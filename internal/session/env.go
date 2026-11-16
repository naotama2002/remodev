package session

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/naotama2002/remodev/internal/docker"
	"github.com/naotama2002/remodev/internal/github"
)

// EnvDir is the directory in a repository that defines its sandbox environment:
//
//	.remodev/Dockerfile  optional; must start FROM ${REMODEV_BASE}
//	.remodev/setup.sh    optional; runs inside the sandbox after the first clone
//	                     (through the egress proxy, e.g. dependency install)
//	.remodev/*           anything else is available as build context
const EnvDir = ".remodev"

// EnvBuilder turns a repository's .remodev directory into an image.
type EnvBuilder interface {
	// EnsureEnvImage returns an image for the given build context, building it
	// only when no image with the same content hash exists.
	EnsureEnvImage(ctx context.Context, files []github.File, base string, onLine func(string)) (image string, built bool, err error)
}

// envHash is the cache key: base image ID + every file path, mode and content.
func envHash(baseID string, files []github.File) string {
	h := sha256.New()
	fmt.Fprintf(h, "base=%s\n", baseID)
	for _, f := range files {
		fmt.Fprintf(h, "%s %o %d\n", f.Path, f.Mode, len(f.Data))
		h.Write(f.Data)
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

func hasDockerfile(files []github.File) bool {
	for _, f := range files {
		if f.Path == "Dockerfile" {
			return true
		}
	}
	return false
}

func (r *DockerRuntime) EnsureEnvImage(ctx context.Context, files []github.File, base string, onLine func(string)) (string, bool, error) {
	baseID, err := r.D.ImageID(ctx, base)
	if err != nil {
		return "", false, fmt.Errorf("base image %s: %w", base, err)
	}
	tag := "remodev-env:" + envHash(baseID, files)
	if ok, err := r.D.ImageExists(ctx, tag); err != nil {
		return "", false, err
	} else if ok {
		return tag, false, nil
	}
	tf := make([]docker.File, 0, len(files))
	for _, f := range files {
		if strings.Contains(f.Path, "..") || strings.HasPrefix(f.Path, "/") {
			return "", false, fmt.Errorf("invalid path in %s: %s", EnvDir, f.Path)
		}
		tf = append(tf, docker.File{Name: f.Path, Mode: f.Mode, Data: f.Data})
	}
	tarball, err := docker.TarFiles(tf...)
	if err != nil {
		return "", false, err
	}
	if err := r.D.Build(ctx, tarball, tag, map[string]string{"REMODEV_BASE": base}, onLine); err != nil {
		return "", false, err
	}
	return tag, true, nil
}

// resolveImage picks the sandbox image for a session: the repository's
// .remodev/Dockerfile (cached by content hash) or the default sandbox image.
func (m *Manager) resolveImage(ctx context.Context, id, repo, ref string) (string, error) {
	b, ok := m.RT.(EnvBuilder)
	if !ok {
		return m.Cfg.SandboxImage, nil
	}
	files, err := m.GH.Dir(ctx, repo, EnvDir, ref)
	if errors.Is(err, github.ErrNotFound) || (err == nil && !hasDockerfile(files)) {
		return m.Cfg.SandboxImage, nil
	}
	if err != nil {
		return "", fmt.Errorf("read %s: %w", EnvDir, err)
	}
	var lines []string
	img, built, err := b.EnsureEnvImage(ctx, files, m.Cfg.SandboxImage, func(l string) {
		lines = append(lines, l)
		if strings.HasPrefix(l, "Step ") {
			m.event(id, "system", "build: "+l, nil)
		}
	})
	if err != nil {
		return "", fmt.Errorf("build %s/Dockerfile: %w\n%s", EnvDir, err, tail(lines, 15))
	}
	if built {
		m.event(id, "system", "built environment image "+img, nil)
	} else {
		m.event(id, "system", "using cached environment image "+img, nil)
	}
	return img, nil
}
