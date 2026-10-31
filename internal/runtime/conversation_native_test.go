package runtime

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shaoxier/AIofCage/internal/session"
)

// The check runs find inside the agent container, with the id as one argument, and says whether
// the agent's own session record is still there.
func TestNativeSessionLookup(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "commands")
	docker := filepath.Join(dir, "docker")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> '" + log + "'\ncase \"$*\" in\n" +
		"*'container inspect'*-agent) [ -f '" + dir + "/stopped' ] && echo 'false img spec' || echo 'true img spec';;\n" +
		"*'find /home/agent/.codex/sessions'*'*present-0001*'*) echo /home/agent/.codex/sessions/2026/10/30/rollout-x-present-0001.jsonl;;\n" +
		"*'find /home/agent/.claude/projects'*) echo \"find: '/home/agent/.claude/projects': No such file or directory\" >&2; exit 1;;\n" +
		"esac\nexit 0\n"
	if err := os.WriteFile(docker, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	m := &Manager{docker: runner{path: docker}}
	ctx := context.Background()
	if found, err := m.NativeSessionExists(ctx, "sbx-native01", session.Codex, "present-0001"); err != nil || !found {
		t.Fatalf("present: %v %v", found, err)
	}
	if found, err := m.NativeSessionExists(ctx, "sbx-native01", session.Codex, "missing-0001"); err != nil || found {
		t.Fatalf("missing: %v %v", found, err)
	}
	if found, err := m.NativeSessionExists(ctx, "sbx-native01", session.ClaudeCode, "missing-0002"); err != nil || found {
		t.Fatalf("no directory at all means no session: %v %v", found, err)
	}
	if _, err := m.NativeSessionExists(ctx, "sbx-native01", session.Codex, "x' -delete '"); err == nil {
		t.Fatal("an id that is not a plain token is not searched")
	}
	os.WriteFile(filepath.Join(dir, "stopped"), nil, 0o600)
	if _, err := m.NativeSessionExists(ctx, "sbx-native01", session.Codex, "present-0001"); err == nil {
		t.Fatal("a stopped sandbox cannot be checked")
	}
	commands, _ := os.ReadFile(log)
	if !strings.Contains(string(commands), "exec --user 1000:1000 aiofcage-sbx-native01-agent find /home/agent/.codex/sessions -maxdepth 6 -name *present-0001* -print -quit") {
		t.Fatalf("commands:\n%s", commands)
	}
}
