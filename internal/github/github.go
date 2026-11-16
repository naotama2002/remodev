// Package github is a tiny GitHub REST client used by the control plane
// (which holds the real token; sandboxes never do).
package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

type Client struct {
	Token string
	HTTP  *http.Client
	Base  string
}

func New(token string) *Client {
	return &Client{Token: token, HTTP: &http.Client{Timeout: 20 * time.Second}, Base: "https://api.github.com"}
}

// ErrNotFound is returned for 404 responses.
var ErrNotFound = errors.New("github: not found")

func (c *Client) get(ctx context.Context, path string, out any) error {
	return c.getAccept(ctx, path, "application/vnd.github+json", out)
}

func (c *Client) getAccept(ctx context.Context, path, accept string, out any) error {
	req, err := http.NewRequestWithContext(ctx, "GET", c.Base+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Accept", accept)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("github GET %s: %w", path, ErrNotFound)
	}
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("github GET %s: %d %s", path, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	if b, ok := out.(*[]byte); ok {
		*b, err = io.ReadAll(io.LimitReader(resp.Body, maxFile+1))
		if err == nil && len(*b) > maxFile {
			return fmt.Errorf("github GET %s: file larger than %d bytes", path, maxFile)
		}
		return err
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

const (
	maxFile  = 1 << 20
	maxFiles = 64
)

// File is a file fetched from a repository.
type File struct {
	Path string // relative to the requested directory
	Mode int64
	Data []byte
}

// Dir downloads every file under dir (recursively) at ref. Returns
// ErrNotFound when the directory does not exist.
func (c *Client) Dir(ctx context.Context, repo, dir, ref string) ([]File, error) {
	var out []File
	var walk func(sub string) error
	walk = func(sub string) error {
		var entries []struct {
			Name string `json:"name"`
			Path string `json:"path"`
			Type string `json:"type"`
			Size int    `json:"size"`
		}
		p := "/repos/" + repo + "/contents/" + escapePath(sub) + "?ref=" + url.QueryEscape(ref)
		if err := c.get(ctx, p, &entries); err != nil {
			return err
		}
		for _, e := range entries {
			switch e.Type {
			case "dir":
				if err := walk(e.Path); err != nil {
					return err
				}
			case "file":
				if len(out) >= maxFiles {
					return fmt.Errorf("%s: more than %d files", dir, maxFiles)
				}
				var data []byte
				fp := "/repos/" + repo + "/contents/" + escapePath(e.Path) + "?ref=" + url.QueryEscape(ref)
				if err := c.getAccept(ctx, fp, "application/vnd.github.raw+json", &data); err != nil {
					return err
				}
				mode := int64(0o644)
				if strings.HasSuffix(e.Name, ".sh") {
					mode = 0o755
				}
				out = append(out, File{Path: strings.TrimPrefix(e.Path, dir+"/"), Mode: mode, Data: data})
			}
		}
		return nil
	}
	if err := walk(dir); err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

func escapePath(p string) string {
	parts := strings.Split(p, "/")
	for i := range parts {
		parts[i] = url.PathEscape(parts[i])
	}
	return strings.Join(parts, "/")
}

type Repo struct {
	FullName      string `json:"full_name"`
	DefaultBranch string `json:"default_branch"`
	Private       bool   `json:"private"`
	Permissions   struct {
		Push bool `json:"push"`
	} `json:"permissions"`
}

func (c *Client) Repo(ctx context.Context, fullName string) (*Repo, error) {
	var r Repo
	if err := c.get(ctx, "/repos/"+fullName, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

type PR struct {
	HTMLURL string `json:"html_url"`
	State   string `json:"state"`
	Number  int    `json:"number"`
}

// FindPR returns the most recent PR whose head is owner:branch, or nil.
func (c *Client) FindPR(ctx context.Context, fullName, branch string) (*PR, error) {
	owner := strings.SplitN(fullName, "/", 2)[0]
	var prs []PR
	q := url.Values{"head": {owner + ":" + branch}, "state": {"all"}, "per_page": {"1"}}
	if err := c.get(ctx, "/repos/"+fullName+"/pulls?"+q.Encode(), &prs); err != nil {
		return nil, err
	}
	if len(prs) == 0 {
		return nil, nil
	}
	return &prs[0], nil
}
