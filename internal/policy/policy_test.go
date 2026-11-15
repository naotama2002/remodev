package policy

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRenderScopesWritesToRepo(t *testing.T) {
	b, err := Render(Input{SessionID: "abc", Repo: "Owner/Repo", NetworkProfile: "developer"})
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, want := range []string{
		`"/repos/owner/repo/pulls"`,
		`"/owner/repo.git/git-receive-pack"`,
		`"env://REMODEV_GITHUB_TOKEN"`,
		`"x-api-key"`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("profile missing %s", want)
		}
	}
	if strings.Contains(s, `"/graphql"`) {
		t.Error("graphql must be off by default")
	}
	var v map[string]any
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
}

func TestRenderOAuth(t *testing.T) {
	b, _ := Render(Input{SessionID: "abc", Repo: "o/r", AnthropicOAuth: true})
	if !strings.Contains(string(b), `"Bearer {}"`) || !strings.Contains(string(b), "CLAUDE_CODE_OAUTH_TOKEN") {
		t.Error("oauth route not rendered")
	}
}
