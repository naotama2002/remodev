// Package github is a tiny GitHub REST client used by the control plane
// (which holds the real token; sandboxes never do).
package github

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
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

func (c *Client) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, "GET", c.Base+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("github GET %s: %d %s", path, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return json.NewDecoder(resp.Body).Decode(out)
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
