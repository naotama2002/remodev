package session

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/naotama2002/remodev/internal/docker"
)

// Runtime provisions the isolated environment of one session.
// The Docker implementation is used for local development and for the
// EC2-based AWS deployment; an ECS/Fargate implementation can satisfy the same
// interface later.
type Runtime interface {
	// Up creates (or re-creates) the sandbox + egress proxy. Idempotent.
	Up(ctx context.Context, s Spec) error
	// Down removes containers and network but keeps the workspace volume.
	Down(ctx context.Context, id string) error
	// Destroy removes everything including the workspace.
	Destroy(ctx context.Context, id string) error
	// Exec runs a command in the sandbox as the unprivileged agent user.
	Exec(ctx context.Context, id string, cmd []string, workdir string, onStdout, onStderr func(string)) (int, error)
	// Spawn starts a long-running command in the sandbox and returns at once.
	Spawn(ctx context.Context, id string, cmd []string, workdir string) error
	// Subnets returns CIDRs of the session's private network (used to refuse
	// sandbox-originated requests to the control plane).
	Subnets(ctx context.Context, id string) ([]string, error)
	// Archive returns a tar stream of a path in the sandbox (docker.ErrNotFound if missing).
	Archive(ctx context.Context, id, path string) (io.ReadCloser, error)
	// Alive reports whether the sandbox is running.
	Alive(ctx context.Context, id string) bool
	// FollowEgress streams proxy log lines until ctx ends.
	FollowEgress(ctx context.Context, id string, since time.Time, onLine func(string)) error
	// PreviewTarget returns host:port to reach a port inside the sandbox.
	PreviewTarget(id string, port int) string
}

type Spec struct {
	ID           string
	ProxyPass    string
	PolicyJSON   []byte
	CACert       []byte
	CAKey        []byte
	ProxyEnv     []string // real credentials, proxy only
	SandboxEnv   []string
	Memory       int64
	NanoCPUs     int64
	SandboxImage string
	ProxyImage   string
}

func names(id string) (network, proxy, sandbox, volume string) {
	return "rd-" + id, "rd-" + id + "-proxy", "rd-" + id + "-sbx", "rd-" + id + "-ws"
}

type DockerRuntime struct {
	D             *docker.Client
	EgressNetwork string
	Control       string // control-plane container name (joins session networks for previews)
	OCIRuntime    string
}

func labels(id, role string) map[string]string {
	return map[string]string{"remodev.session": id, "remodev.role": role}
}

func (r *DockerRuntime) Up(ctx context.Context, s Spec) error {
	netName, proxyName, sbxName, vol := names(s.ID)
	if err := r.D.EnsureNetwork(ctx, docker.NetworkSpec{Name: r.EgressNetwork, Labels: map[string]string{"remodev.role": "egress"}}); err != nil {
		return fmt.Errorf("egress network: %w", err)
	}
	// Internal network: no gateway, so the sandbox cannot reach anything but peers on it.
	if err := r.D.EnsureNetwork(ctx, docker.NetworkSpec{Name: netName, Internal: true, Labels: labels(s.ID, "network")}); err != nil {
		return fmt.Errorf("session network: %w", err)
	}
	if err := r.D.EnsureVolume(ctx, vol, labels(s.ID, "workspace")); err != nil {
		return fmt.Errorf("volume: %w", err)
	}
	// Recreate containers from scratch every time; state lives in the volume.
	_ = r.D.RemoveContainer(ctx, proxyName)
	_ = r.D.RemoveContainer(ctx, sbxName)

	// ---- egress proxy (nono) ----
	pid, err := r.D.CreateContainer(ctx, docker.ContainerSpec{
		Name:    proxyName,
		Image:   s.ProxyImage,
		Network: r.EgressNetwork,
		Labels:  labels(s.ID, "proxy"),
		Env:     append([]string{"NONO_PROXY_PASS=" + s.ProxyPass}, s.ProxyEnv...),
		Cmd: []string{"proxy", "--listen", "0.0.0.0", "--port", "8080",
			"--profile", "/etc/nono/profile.json",
			"--proxy-ca-cert", "/etc/nono/ca.crt", "--proxy-ca-key", "/etc/nono/ca.key",
			"--max-connections", "512"},
		Memory:    256 << 20,
		NanoCPUs:  500_000_000,
		PidsLimit: 256,
		CapDrop:   []string{"ALL"},
	})
	if err != nil {
		return fmt.Errorf("create proxy: %w", err)
	}
	tarball, err := docker.TarFiles(
		docker.File{Name: "profile.json", Mode: 0o444, UID: 10001, Data: s.PolicyJSON},
		docker.File{Name: "ca.crt", Mode: 0o444, UID: 10001, Data: s.CACert},
		docker.File{Name: "ca.key", Mode: 0o400, UID: 10001, Data: s.CAKey},
	)
	if err != nil {
		return err
	}
	if err := r.D.PutArchive(ctx, pid, "/etc/nono", tarball); err != nil {
		return fmt.Errorf("proxy files: %w", err)
	}
	if err := r.D.ConnectNetwork(ctx, netName, pid, "proxy"); err != nil {
		return fmt.Errorf("proxy network: %w", err)
	}
	if err := r.D.StartContainer(ctx, pid); err != nil {
		return fmt.Errorf("start proxy: %w", err)
	}

	// ---- sandbox ----
	sid, err := r.D.CreateContainer(ctx, docker.ContainerSpec{
		Name:      sbxName,
		Image:     s.SandboxImage,
		Network:   netName,
		Aliases:   []string{"sandbox"},
		Labels:    labels(s.ID, "sandbox"),
		Env:       s.SandboxEnv,
		Mounts:    map[string]string{vol: "/workspace"},
		Memory:    s.Memory,
		NanoCPUs:  s.NanoCPUs,
		PidsLimit: 4096,
		Runtime:   r.OCIRuntime,
	})
	if err != nil {
		return fmt.Errorf("create sandbox: %w", err)
	}
	caTar, err := docker.TarFiles(docker.File{Name: "ca.crt", Mode: 0o444, Data: s.CACert})
	if err != nil {
		return err
	}
	if err := r.D.PutArchive(ctx, sid, "/etc/remodev", caTar); err != nil {
		return fmt.Errorf("sandbox files: %w", err)
	}
	if err := r.D.StartContainer(ctx, sid); err != nil {
		return fmt.Errorf("start sandbox: %w", err)
	}
	if r.Control != "" {
		if err := r.D.ConnectNetwork(ctx, netName, r.Control); err != nil {
			return fmt.Errorf("join control to session network: %w", err)
		}
	}
	return nil
}

func (r *DockerRuntime) Down(ctx context.Context, id string) error {
	netName, proxyName, sbxName, _ := names(id)
	var errs []string
	for _, c := range []string{sbxName, proxyName} {
		if err := r.D.RemoveContainer(ctx, c); err != nil {
			errs = append(errs, err.Error())
		}
	}
	if r.Control != "" {
		_ = r.D.DisconnectNetwork(ctx, netName, r.Control)
	}
	if err := r.D.RemoveNetwork(ctx, netName); err != nil {
		errs = append(errs, err.Error())
	}
	if len(errs) > 0 {
		return fmt.Errorf("down: %s", strings.Join(errs, "; "))
	}
	return nil
}

func (r *DockerRuntime) Destroy(ctx context.Context, id string) error {
	if err := r.Down(ctx, id); err != nil {
		return err
	}
	_, _, _, vol := names(id)
	return r.D.RemoveVolume(ctx, vol)
}

func (r *DockerRuntime) Exec(ctx context.Context, id string, cmd []string, workdir string, onStdout, onStderr func(string)) (int, error) {
	_, _, sbx, _ := names(id)
	return r.D.Exec(ctx, sbx, docker.ExecSpec{Cmd: cmd, User: "agent", WorkingDir: workdir}, onStdout, onStderr)
}

func (r *DockerRuntime) Archive(ctx context.Context, id, path string) (io.ReadCloser, error) {
	_, _, sbx, _ := names(id)
	return r.D.GetArchive(ctx, sbx, path)
}

func (r *DockerRuntime) Spawn(ctx context.Context, id string, cmd []string, workdir string) error {
	_, _, sbx, _ := names(id)
	return r.D.ExecDetached(ctx, sbx, docker.ExecSpec{Cmd: cmd, User: "agent", WorkingDir: workdir})
}

func (r *DockerRuntime) Subnets(ctx context.Context, id string) ([]string, error) {
	n, _, _, _ := names(id)
	return r.D.NetworkSubnets(ctx, n)
}

func (r *DockerRuntime) Alive(ctx context.Context, id string) bool {
	_, proxy, sbx, _ := names(id)
	for _, c := range []string{sbx, proxy} {
		st, err := r.D.InspectContainer(ctx, c)
		if err != nil || !st.Running {
			return false
		}
	}
	return true
}

func (r *DockerRuntime) FollowEgress(ctx context.Context, id string, since time.Time, onLine func(string)) error {
	_, proxy, _, _ := names(id)
	return r.D.Logs(ctx, proxy, since, onLine)
}

func (r *DockerRuntime) PreviewTarget(id string, port int) string {
	_, _, sbx, _ := names(id)
	return fmt.Sprintf("%s:%d", sbx, port)
}
