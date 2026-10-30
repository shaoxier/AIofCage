package runtime

import (
	"context"
	"strings"
	"testing"

	"github.com/shaoxier/AIofCage/internal/sandbox"
	"github.com/shaoxier/AIofCage/internal/session"
)

func TestDevSourcesReachTheGatekeeperAndTheAgent(t *testing.T) {
	m, _, _ := newRemoteManager(t, nil)
	sbx := sandbox.Sandbox{ID: "sbx-dev00001", Name: "dev", ProjectPath: "/Users/u/CageWorkspace/p",
		Bindings: []sandbox.Binding{{Agent: session.Codex}}, Placeholders: map[string]string{}, DevSources: []string{"npm-mirror", "github"}}
	config, err := m.gateConfig(context.Background(), sbx, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := config.Policy.Validate(); err != nil {
		t.Fatal(err)
	}
	hosts := map[string]bool{}
	for _, rule := range config.Policy.Rules {
		hosts[rule.Host] = true
		if rule.Effect == "allow" && !rule.Anonymous {
			t.Errorf("development sources only open anonymous rules: %+v", rule)
		}
	}
	if !hosts["registry.npmmirror.com"] || !hosts["cdn.npmmirror.com"] || !hosts["codeload.github.com"] || hosts["registry.npmjs.org"] {
		t.Fatalf("rules for the chosen sources only: %v", hosts)
	}
	var env string
	for _, file := range agentFiles(sbx, nil, nil) {
		if file.Path == ".aiofcage/env" {
			env = file.Content
		}
	}
	for _, want := range []string{"export NPM_CONFIG_REGISTRY='https://registry.npmmirror.com/'", "export GIT_TERMINAL_PROMPT='0'", "export NPM_CONFIG_AUDIT='false'"} {
		if !strings.Contains(env, want) {
			t.Errorf("agent env lacks %s:\n%s", want, env)
		}
	}
	none, err := m.gateConfig(context.Background(), sandbox.Sandbox{ID: "sbx-dev00002", Name: "plain"}, nil, nil)
	if err != nil || len(none.Policy.Rules) != 0 {
		t.Fatalf("a sandbox without development sources opens nothing: %+v %v", none.Policy.Rules, err)
	}
}

func TestDevSourceChoicesAreValidated(t *testing.T) {
	m, st, _ := newRemoteManager(t, nil)
	_, err := m.Create(context.Background(), CreateRequest{Name: "both", ProjectPath: sandbox.Workspace() + "/both", DevSources: []string{"npm", "npm-mirror"}})
	if err == nil || !strings.Contains(err.Error(), "只能选一个") {
		t.Fatalf("the registry and its mirror are alternatives: %v", err)
	}
	if err := st.SaveSandbox(sandbox.Sandbox{ID: "sbx-dev00003", Name: "stopped", ProjectPath: "/Users/u/CageWorkspace/p", Placeholders: map[string]string{}}); err != nil {
		t.Fatal(err)
	}
	if err := m.SetDevSources(context.Background(), "sbx-dev00003", []string{"pypi"}); err == nil || !strings.Contains(err.Error(), "不支持") {
		t.Fatalf("unknown sources are refused: %v", err)
	}
}
