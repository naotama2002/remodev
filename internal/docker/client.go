// Package docker is a minimal Docker Engine API client built on net/http.
// It intentionally avoids the official SDK to keep the dependency tree empty.
package docker

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const apiVersion = "v1.44"

// ErrNotFound is returned when the Docker API responds 404.
var ErrNotFound = errors.New("docker: not found")

type Client struct {
	http *http.Client
	base string
}

// New connects to the Docker daemon at a unix socket path (e.g. /var/run/docker.sock).
func New(socket string) *Client {
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		},
		MaxIdleConns: 16,
	}
	return &Client{http: &http.Client{Transport: tr}, base: "http://docker/" + apiVersion}
}

func (c *Client) do(ctx context.Context, method, path string, body any, out any) error {
	resp, err := c.raw(ctx, method, path, body, "application/json")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

func (c *Client) raw(ctx context.Context, method, path string, body any, ctype string) (*http.Response, error) {
	var r io.Reader
	switch b := body.(type) {
	case nil:
	case []byte:
		r = bytes.NewReader(b)
	case io.Reader:
		r = b
	default:
		buf, err := json.Marshal(b)
		if err != nil {
			return nil, err
		}
		r = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, r)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", ctype)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusNotFound {
		resp.Body.Close()
		return nil, fmt.Errorf("%s %s: %w", method, path, ErrNotFound)
	}
	if resp.StatusCode >= 300 && resp.StatusCode != http.StatusNotModified {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		return nil, fmt.Errorf("docker %s %s: %d %s", method, path, resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	return resp, nil
}

func labelFilter(labels map[string]string) string {
	var l []string
	for k, v := range labels {
		l = append(l, k+"="+v)
	}
	b, _ := json.Marshal(map[string][]string{"label": l})
	return url.QueryEscape(string(b))
}

// ---- images -----------------------------------------------------------------

func (c *Client) ImageExists(ctx context.Context, ref string) (bool, error) {
	err := c.do(ctx, "GET", "/images/"+url.PathEscape(ref)+"/json", nil, nil)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}

// ---- networks ---------------------------------------------------------------

type NetworkSpec struct {
	Name     string
	Internal bool
	Labels   map[string]string
}

func (c *Client) EnsureNetwork(ctx context.Context, s NetworkSpec) error {
	var info struct{ ID string }
	err := c.do(ctx, "GET", "/networks/"+url.PathEscape(s.Name), nil, &info)
	if err == nil {
		return nil
	}
	if !errors.Is(err, ErrNotFound) {
		return err
	}
	return c.do(ctx, "POST", "/networks/create", map[string]any{
		"Name": s.Name, "Driver": "bridge", "Internal": s.Internal, "Labels": s.Labels, "CheckDuplicate": true,
	}, nil)
}

// NetworkSubnets returns the IPAM subnets of a network.
func (c *Client) NetworkSubnets(ctx context.Context, name string) ([]string, error) {
	var info struct {
		IPAM struct{ Config []struct{ Subnet string } }
	}
	if err := c.do(ctx, "GET", "/networks/"+url.PathEscape(name), nil, &info); err != nil {
		return nil, err
	}
	var out []string
	for _, cfg := range info.IPAM.Config {
		if cfg.Subnet != "" {
			out = append(out, cfg.Subnet)
		}
	}
	return out, nil
}

func (c *Client) RemoveNetwork(ctx context.Context, name string) error {
	err := c.do(ctx, "DELETE", "/networks/"+url.PathEscape(name), nil, nil)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

func (c *Client) ConnectNetwork(ctx context.Context, network, container string, aliases ...string) error {
	body := map[string]any{"Container": container}
	if len(aliases) > 0 {
		body["EndpointConfig"] = map[string]any{"Aliases": aliases}
	}
	err := c.do(ctx, "POST", "/networks/"+url.PathEscape(network)+"/connect", body, nil)
	if err != nil && strings.Contains(err.Error(), "already exists") {
		return nil
	}
	return err
}

func (c *Client) DisconnectNetwork(ctx context.Context, network, container string) error {
	err := c.do(ctx, "POST", "/networks/"+url.PathEscape(network)+"/disconnect", map[string]any{"Container": container, "Force": true}, nil)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

// ---- volumes ----------------------------------------------------------------

func (c *Client) EnsureVolume(ctx context.Context, name string, labels map[string]string) error {
	return c.do(ctx, "POST", "/volumes/create", map[string]any{"Name": name, "Labels": labels}, nil)
}

func (c *Client) RemoveVolume(ctx context.Context, name string) error {
	err := c.do(ctx, "DELETE", "/volumes/"+url.PathEscape(name)+"?force=true", nil, nil)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

// ---- containers -------------------------------------------------------------

type ContainerSpec struct {
	Name       string
	Image      string
	Cmd        []string
	Entrypoint []string
	Env        []string
	User       string
	WorkingDir string
	Labels     map[string]string
	Network    string            // primary network
	Aliases    []string          // DNS aliases on the primary network
	Mounts     map[string]string // volume name -> container path
	Memory     int64             // bytes, 0 = unlimited
	NanoCPUs   int64
	PidsLimit  int64
	CapDrop    []string
	ReadOnly   bool
	Tmpfs      map[string]string
	Runtime    string // optional OCI runtime (e.g. runsc)
}

// File is an entry for TarFiles.
type File struct {
	Name string
	Mode int64
	UID  int
	Data []byte
}

// TarFiles builds an in-memory tar archive (for PutArchive).
func TarFiles(files ...File) ([]byte, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, f := range files {
		h := &tar.Header{Name: f.Name, Mode: f.Mode, Size: int64(len(f.Data)), Uid: f.UID, Gid: f.UID, ModTime: time.Now()}
		if err := tw.WriteHeader(h); err != nil {
			return nil, err
		}
		if _, err := tw.Write(f.Data); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func (c *Client) CreateContainer(ctx context.Context, s ContainerSpec) (string, error) {
	var mounts []map[string]any
	for vol, target := range s.Mounts {
		mounts = append(mounts, map[string]any{"Type": "volume", "Source": vol, "Target": target})
	}
	host := map[string]any{
		"Mounts":         mounts,
		"NetworkMode":    s.Network,
		"Memory":         s.Memory,
		"NanoCpus":       s.NanoCPUs,
		"CapDrop":        s.CapDrop,
		"ReadonlyRootfs": s.ReadOnly,
		"Tmpfs":          s.Tmpfs,
		"SecurityOpt":    []string{"no-new-privileges:true"},
		"Init":           true,
	}
	if s.PidsLimit > 0 {
		host["PidsLimit"] = s.PidsLimit
	}
	if s.Runtime != "" {
		host["Runtime"] = s.Runtime
	}
	body := map[string]any{
		"Image":      s.Image,
		"Cmd":        s.Cmd,
		"Env":        s.Env,
		"User":       s.User,
		"WorkingDir": s.WorkingDir,
		"Labels":     s.Labels,
		"HostConfig": host,
	}
	if s.Entrypoint != nil {
		body["Entrypoint"] = s.Entrypoint
	}
	if s.Network != "" {
		body["NetworkingConfig"] = map[string]any{
			"EndpointsConfig": map[string]any{s.Network: map[string]any{"Aliases": s.Aliases}},
		}
	}
	var out struct{ Id string }
	err := c.do(ctx, "POST", "/containers/create?name="+url.QueryEscape(s.Name), body, &out)
	return out.Id, err
}

func (c *Client) StartContainer(ctx context.Context, id string) error {
	return c.do(ctx, "POST", "/containers/"+url.PathEscape(id)+"/start", nil, nil)
}

func (c *Client) RemoveContainer(ctx context.Context, id string) error {
	err := c.do(ctx, "DELETE", "/containers/"+url.PathEscape(id)+"?force=true&v=false", nil, nil)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

type ContainerState struct {
	Running bool
	Status  string
}

func (c *Client) InspectContainer(ctx context.Context, id string) (*ContainerState, error) {
	var info struct{ State ContainerState }
	if err := c.do(ctx, "GET", "/containers/"+url.PathEscape(id)+"/json", nil, &info); err != nil {
		return nil, err
	}
	return &info.State, nil
}

type ContainerSummary struct {
	ID     string `json:"Id"`
	Names  []string
	Labels map[string]string
	State  string
}

func (c *Client) ListContainers(ctx context.Context, labels map[string]string) ([]ContainerSummary, error) {
	var out []ContainerSummary
	err := c.do(ctx, "GET", "/containers/json?all=true&filters="+labelFilter(labels), nil, &out)
	return out, err
}

// PutArchive uploads a tar archive and extracts it at path inside the container.
func (c *Client) PutArchive(ctx context.Context, id, path string, tarball []byte) error {
	resp, err := c.raw(ctx, "PUT", "/containers/"+url.PathEscape(id)+"/archive?path="+url.QueryEscape(path), tarball, "application/x-tar")
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// ---- exec -------------------------------------------------------------------

type ExecSpec struct {
	Cmd        []string
	User       string
	Env        []string
	WorkingDir string
}

// Exec runs a command and calls onLine for every complete stdout / stderr line.
// It blocks until the process exits and returns its exit code.
func (c *Client) Exec(ctx context.Context, container string, s ExecSpec, onStdout, onStderr func(string)) (int, error) {
	var created struct{ Id string }
	err := c.do(ctx, "POST", "/containers/"+url.PathEscape(container)+"/exec", map[string]any{
		"Cmd": s.Cmd, "User": s.User, "Env": s.Env, "WorkingDir": s.WorkingDir,
		"AttachStdout": true, "AttachStderr": true,
	}, &created)
	if err != nil {
		return -1, err
	}
	resp, err := c.raw(ctx, "POST", "/exec/"+created.Id+"/start", map[string]any{"Detach": false, "Tty": false}, "application/json")
	if err != nil {
		return -1, err
	}
	derr := Demux(resp.Body, onStdout, onStderr)
	resp.Body.Close()
	// Body ends when the process exits; poll briefly until Docker records the exit code.
	for i := 0; i < 50; i++ {
		var info struct {
			Running  bool
			ExitCode int
		}
		if err := c.do(context.WithoutCancel(ctx), "GET", "/exec/"+created.Id+"/json", nil, &info); err != nil {
			return -1, err
		}
		if !info.Running {
			if derr != nil && !errors.Is(derr, io.EOF) && ctx.Err() == nil {
				return info.ExitCode, derr
			}
			return info.ExitCode, nil
		}
		if ctx.Err() != nil {
			return -1, ctx.Err()
		}
		time.Sleep(100 * time.Millisecond)
	}
	return -1, errors.New("exec did not finish")
}

// ExecDetached starts a command in the background and returns immediately.
func (c *Client) ExecDetached(ctx context.Context, container string, s ExecSpec) error {
	var created struct{ Id string }
	err := c.do(ctx, "POST", "/containers/"+url.PathEscape(container)+"/exec", map[string]any{
		"Cmd": s.Cmd, "User": s.User, "Env": s.Env, "WorkingDir": s.WorkingDir,
	}, &created)
	if err != nil {
		return err
	}
	return c.do(ctx, "POST", "/exec/"+created.Id+"/start", map[string]any{"Detach": true, "Tty": false}, nil)
}

// Logs follows container logs (stdout+stderr) until ctx is cancelled.
func (c *Client) Logs(ctx context.Context, container string, since time.Time, onLine func(string)) error {
	q := fmt.Sprintf("?follow=true&stdout=true&stderr=true&since=%d", since.Unix())
	resp, err := c.raw(ctx, "GET", "/containers/"+url.PathEscape(container)+"/logs"+q, nil, "")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return Demux(resp.Body, onLine, onLine)
}

// Demux splits Docker's multiplexed stdout/stderr stream into lines.
func Demux(r io.Reader, onStdout, onStderr func(string)) error {
	br := bufio.NewReaderSize(r, 64*1024)
	var outBuf, errBuf bytes.Buffer
	flush := func(buf *bytes.Buffer, fn func(string), final bool) {
		for {
			i := bytes.IndexByte(buf.Bytes(), '\n')
			if i < 0 {
				break
			}
			line := string(buf.Next(i + 1))
			if fn != nil {
				fn(strings.TrimRight(line, "\r\n"))
			}
		}
		if final && buf.Len() > 0 && fn != nil {
			fn(buf.String())
			buf.Reset()
		}
	}
	hdr := make([]byte, 8)
	for {
		if _, err := io.ReadFull(br, hdr); err != nil {
			flush(&outBuf, onStdout, true)
			flush(&errBuf, onStderr, true)
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return nil
			}
			return err
		}
		n := binary.BigEndian.Uint32(hdr[4:])
		target, fn := &outBuf, onStdout
		if hdr[0] == 2 {
			target, fn = &errBuf, onStderr
		}
		if _, err := io.CopyN(target, br, int64(n)); err != nil {
			flush(&outBuf, onStdout, true)
			flush(&errBuf, onStderr, true)
			return err
		}
		flush(target, fn, false)
	}
}
