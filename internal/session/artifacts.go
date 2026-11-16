package session

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/naotama2002/remodev/internal/docker"
	"github.com/naotama2002/remodev/internal/github"
)

// Artifacts are files the agent writes to /workspace/artifacts in the sandbox
// (screenshots, HTML reports, ...). The control plane copies them to
// <data>/artifacts/<session>/ so they outlive the sandbox and the workspace.
const sandboxArtifacts = "/workspace/artifacts"

const (
	maxArtifactSize  = 20 << 20
	maxArtifactCount = 200
)

var artifactNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

type Artifact struct {
	Name    string    `json:"name"`
	Size    int64     `json:"size"`
	Type    string    `json:"type"`
	ModTime time.Time `json:"modTime"`
}

func (m *Manager) artifactDir(id string) string {
	return filepath.Join(m.ArtifactsDir, id)
}

// ValidArtifactName reports whether name is a safe flat file name.
func ValidArtifactName(name string) bool {
	return artifactNameRe.MatchString(name) && !strings.Contains(name, "..")
}

// syncArtifacts copies new or changed files from the sandbox and emits an
// "artifact" event for each. Subdirectories are flattened with "_".
func (m *Manager) syncArtifacts(ctx context.Context, id string) {
	if m.ArtifactsDir == "" {
		return
	}
	rc, err := m.RT.Archive(ctx, id, sandboxArtifacts)
	if errors.Is(err, docker.ErrNotFound) {
		return
	}
	if err != nil {
		m.Log.Warn("artifacts", "session", id, "err", err)
		return
	}
	defer rc.Close()
	dir := m.artifactDir(id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		m.Log.Error("artifacts dir", "err", err)
		return
	}
	existing, _ := os.ReadDir(dir)
	count := len(existing)
	tr := tar.NewReader(rc)
	for {
		h, err := tr.Next()
		if err != nil {
			if !errors.Is(err, io.EOF) {
				m.Log.Warn("artifacts tar", "session", id, "err", err)
			}
			return
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		// "artifacts/sub/a.png" -> "sub_a.png"
		rel := strings.TrimPrefix(path.Clean(h.Name), "artifacts/")
		name := strings.ReplaceAll(rel, "/", "_")
		if !ValidArtifactName(name) || h.Size > maxArtifactSize {
			continue
		}
		dst := filepath.Join(dir, name)
		if st, err := os.Stat(dst); err == nil && st.Size() == h.Size && !h.ModTime.After(st.ModTime()) {
			continue
		} else if err != nil && count >= maxArtifactCount {
			continue
		} else if err != nil {
			count++
		}
		tmp := dst + ".tmp"
		f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
		if err != nil {
			continue
		}
		_, cerr := io.Copy(f, io.LimitReader(tr, maxArtifactSize))
		f.Close()
		if cerr != nil || os.Rename(tmp, dst) != nil {
			os.Remove(tmp)
			continue
		}
		_ = os.Chtimes(dst, h.ModTime, h.ModTime)
		m.event(id, "artifact", name, Artifact{Name: name, Size: h.Size, Type: artifactType(name), ModTime: h.ModTime})
	}
}

func artifactType(name string) string {
	t := mime.TypeByExtension(strings.ToLower(filepath.Ext(name)))
	if t == "" {
		return "application/octet-stream"
	}
	return t
}

// Artifacts lists stored artifacts, newest first.
func (m *Manager) Artifacts(id string) ([]Artifact, error) {
	entries, err := os.ReadDir(m.artifactDir(id))
	if errors.Is(err, os.ErrNotExist) {
		return []Artifact{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := []Artifact{}
	for _, e := range entries {
		if e.IsDir() || strings.HasSuffix(e.Name(), ".tmp") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, Artifact{Name: e.Name(), Size: info.Size(), Type: artifactType(e.Name()), ModTime: info.ModTime()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ModTime.After(out[j].ModTime) })
	return out, nil
}

// ArtifactPath returns the on-disk path of an artifact.
func (m *Manager) ArtifactPath(id, name string) (string, error) {
	if !ValidArtifactName(name) {
		return "", fmt.Errorf("invalid artifact name")
	}
	return filepath.Join(m.artifactDir(id), name), nil
}

// ---- diff & checks ---------------------------------------------------------

type Diff struct {
	Source    string   `json:"source"` // sandbox | github | none
	Diff      string   `json:"diff"`
	Untracked []string `json:"untracked"`
	Truncated bool     `json:"truncated"`
}

const maxDiff = 2 << 20

// Diff returns the changes of the session branch against its base. While the
// sandbox runs it includes uncommitted work; afterwards it uses GitHub.
func (m *Manager) Diff(ctx context.Context, id string) (*Diff, error) {
	s, ok := m.Store.Get(id)
	if !ok {
		return nil, ErrNotFound
	}
	if m.RT.Alive(ctx, s.ID) {
		d := &Diff{Source: "sandbox", Untracked: []string{}}
		var b strings.Builder
		code, err := m.RT.Exec(ctx, id, []string{"git", "-c", "core.quotepath=off", "diff", "--no-color", "--no-ext-diff", "origin/" + s.BaseBranch}, repoDir,
			func(l string) {
				if b.Len() > maxDiff {
					d.Truncated = true
					return
				}
				b.WriteString(l)
				b.WriteByte('\n')
			}, nil)
		if err == nil && code == 0 {
			d.Diff = b.String()
			_, _ = m.RT.Exec(ctx, id, []string{"git", "-c", "core.quotepath=off", "ls-files", "--others", "--exclude-standard"}, repoDir,
				func(l string) {
					if l != "" && len(d.Untracked) < 500 {
						d.Untracked = append(d.Untracked, l)
					}
				}, nil)
			return d, nil
		}
	}
	diff, err := m.GH.CompareDiff(ctx, s.Repo, s.BaseBranch, s.Branch)
	if errors.Is(err, github.ErrNotFound) {
		return &Diff{Source: "none", Untracked: []string{}}, nil
	}
	if err != nil {
		return nil, err
	}
	return &Diff{Source: "github", Diff: diff, Untracked: []string{}}, nil
}

type Checks struct {
	PR   *github.PR        `json:"pr"`
	Runs []github.CheckRun `json:"runs"`
}

// Checks returns the session PR and the CI check runs of its head commit.
func (m *Manager) Checks(ctx context.Context, id string) (*Checks, error) {
	s, ok := m.Store.Get(id)
	if !ok {
		return nil, ErrNotFound
	}
	pr, err := m.GH.FindPR(ctx, s.Repo, s.Branch)
	if err != nil || pr == nil {
		return &Checks{Runs: []github.CheckRun{}}, err
	}
	runs, err := m.GH.CheckRuns(ctx, s.Repo, pr.Head.SHA)
	if err != nil {
		return nil, err
	}
	if runs == nil {
		runs = []github.CheckRun{}
	}
	return &Checks{PR: pr, Runs: runs}, nil
}
