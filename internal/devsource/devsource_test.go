package devsource

import (
	"fmt"
	"strings"
	"testing"

	"github.com/shaoxier/AIofCage/internal/policy"
)

func TestValidate(t *testing.T) {
	if err := Validate([]string{"npm-mirror", "github"}); err != nil {
		t.Fatal(err)
	}
	for _, bad := range [][]string{{"pypi"}, {"github", "github"}, {"npm", "npm-mirror"}} {
		if Validate(bad) == nil {
			t.Errorf("accepted %v", bad)
		}
	}
}

// The presets open downloads only: publishing, pushing and other GitHub pages stay closed.
func TestRulesOpenDownloadsOnly(t *testing.T) {
	snapshot := policy.Snapshot{Version: 1, Rules: Rules([]string{"npm", "github"})}
	if err := snapshot.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		host, method, path string
		allowed            bool
	}{
		{"registry.npmjs.org", "GET", "/@types/node", true},
		{"registry.npmjs.org", "GET", "/left-pad/-/left-pad-1.3.0.tgz", true},
		{"registry.npmjs.org", "PUT", "/left-pad", false},
		{"registry.npmjs.org", "POST", "/-/npm/v1/security/advisories/bulk", false},
		{"github.com", "GET", "/octo/demo.git/info/refs", true},
		{"github.com", "POST", "/octo/demo.git/git-upload-pack", true},
		{"github.com", "POST", "/octo/demo.git/git-receive-pack", false},
		{"github.com", "GET", "/octo/demo/releases/download/v1.0/tool.tar.gz", true},
		{"github.com", "GET", "/octo/demo", false},
		{"github.com", "GET", "/login", false},
		{"api.github.com", "GET", "/repos/octo/demo", false},
		{"codeload.github.com", "GET", "/octo/demo/tar.gz/refs/heads/main", true},
		{"registry.npmmirror.com", "GET", "/@types/node", false},
	} {
		d := snapshot.Evaluate(policy.Request{Host: c.host, Method: c.method, Path: c.path})
		if d.Allowed != c.allowed || (d.Allowed && !d.Anonymous) {
			t.Errorf("%s %s%s: %+v", c.method, c.host, c.path, d)
		}
	}
	if d := snapshot.Evaluate(policy.Request{Host: "registry.npmjs.org", Method: "PUT", Path: "/left-pad"}); d.ReasonCode != "rule_denied" {
		t.Fatalf("publishing must hit the explicit deny, not just the default: %+v", d)
	}
	ids := map[string]bool{}
	for _, rule := range Rules([]string{"npm-mirror", "github"}) {
		if ids[rule.ID] || !strings.HasPrefix(rule.ID, "dev-") {
			t.Fatalf("rule IDs must be unique and marked: %s", rule.ID)
		}
		ids[rule.ID] = true
	}
}

func TestEnv(t *testing.T) {
	if got := fmt.Sprint(Env([]string{"npm-mirror", "github"})); !strings.Contains(got, "[NPM_CONFIG_REGISTRY https://registry.npmmirror.com/]") ||
		!strings.Contains(got, "[GIT_TERMINAL_PROMPT 0]") || !strings.Contains(got, "[NPM_CONFIG_AUDIT false]") {
		t.Fatalf("env %s", got)
	}
	if len(Env(nil)) != 0 {
		t.Fatal("no source, no settings")
	}
}
