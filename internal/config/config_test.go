package config

import "testing"

func TestRepoAllowed(t *testing.T) {
	tests := []struct {
		name    string
		allowed []string
		repo    string
		want    bool
	}{
		{"exact match", []string{"myorg/app"}, "myorg/app", true},
		{"exact mismatch", []string{"myorg/app"}, "myorg/other", false},
		{"owner wildcard", []string{"myorg/*"}, "myorg/anything", true},
		{"owner wildcard other owner", []string{"myorg/*"}, "other/app", false},
		{"case insensitive repo", []string{"myorg/app"}, "MyOrg/App", true},
		{"case insensitive pattern", []string{"MyOrg/*"}, "myorg/app", true},
		{"multiple patterns", []string{"a/x", "b/*"}, "b/y", true},
		{"no patterns", nil, "myorg/app", false},
		{"no slash", []string{"*"}, "myorg", false},
		{"too many slashes", []string{"myorg/*"}, "myorg/app/extra", false},
		{"empty repo", []string{"*/*"}, "", false},
		{"wildcard does not cross slash", []string{"*"}, "myorg/app", false},
		{"full wildcard", []string{"*/*"}, "any/thing", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &Config{AllowedRepos: tt.allowed}
			if got := c.RepoAllowed(tt.repo); got != tt.want {
				t.Errorf("RepoAllowed(%q) with %v = %v, want %v", tt.repo, tt.allowed, got, tt.want)
			}
		})
	}
}

func TestAgent(t *testing.T) {
	c := &Config{Agents: []Agent{
		{Name: "default", Description: "def"},
		{Name: "reviewer", Description: "rev", Model: "m"},
	}}

	t.Run("empty name resolves to default", func(t *testing.T) {
		a, ok := c.Agent("")
		if !ok || a.Name != "default" {
			t.Errorf("got (%+v, %v), want default agent", a, ok)
		}
	})
	t.Run("named agent", func(t *testing.T) {
		a, ok := c.Agent("reviewer")
		if !ok || a.Model != "m" {
			t.Errorf("got (%+v, %v), want reviewer agent", a, ok)
		}
	})
	t.Run("unknown agent", func(t *testing.T) {
		a, ok := c.Agent("nope")
		if ok || a.Name != "" {
			t.Errorf("got (%+v, %v), want zero value and false", a, ok)
		}
	})
	t.Run("no agents configured", func(t *testing.T) {
		if _, ok := (&Config{}).Agent(""); ok {
			t.Error("expected not found")
		}
	})
}
