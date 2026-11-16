package session

import (
	"testing"

	"github.com/naotama2002/remodev/internal/github"
)

func TestEnvHash(t *testing.T) {
	a := []github.File{{Path: "Dockerfile", Mode: 0o644, Data: []byte("FROM x")}}
	b := []github.File{{Path: "Dockerfile", Mode: 0o644, Data: []byte("FROM y")}}
	if envHash("base1", a) != envHash("base1", a) {
		t.Fatal("hash must be stable")
	}
	if envHash("base1", a) == envHash("base1", b) {
		t.Fatal("content change must change hash")
	}
	if envHash("base1", a) == envHash("base2", a) {
		t.Fatal("base image change must change hash")
	}
	if !hasDockerfile(a) || hasDockerfile([]github.File{{Path: "setup.sh"}}) {
		t.Fatal("hasDockerfile")
	}
}
