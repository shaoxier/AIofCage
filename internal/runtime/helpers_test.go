package runtime

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/shaoxier/AIofCage/internal/sandbox"
)

// recordingDocker is a Docker on the dedicated context that records the arguments of the one call
// it gets, NUL separated.
func recordingDocker(t *testing.T) (runner, func() []string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "docker")
	script := "#!/bin/sh\nfor a in \"$@\"; do printf '%s\\0' \"$a\"; done > '" + dir + "/args'\nexit 0\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return runner{path: path, args: []string{"--context", DockerContext}}, func() []string {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(dir, "args"))
		if err != nil {
			t.Fatal(err)
		}
		return strings.Split(strings.TrimSuffix(string(data), "\x00"), "\x00")
	}
}

// 应用自己的辅助命令以 Agent 用户运行，但不经过 aiofcage-run、不在项目目录里运行，Python 用镜像自带的
// 解释器和隔离模式，Docker 命令显式指定专用 context。
func TestHelpersSkipTheAgentSettings(t *testing.T) {
	ctx := context.Background()
	prefix := []string{"--context", "colima-aiofcage", "exec", "--interactive", "--user", "1000:1000", "--workdir", "/",
		"aiofcage-sbx-helper01-agent"}
	for name, tt := range map[string]struct {
		run  func(m *Manager) error
		want []string
	}{
		"停止会话": {func(m *Manager) error { return m.EndProcesses(ctx, "sbx-helper01", "t-0123abcd") },
			[]string{"/usr/bin/python3", "-I", "-c", endProcessesScript, "t-0123abcd"}},
		"检查会话组件": {func(m *Manager) error {
			return m.ExecHelper(ctx, "sbx-helper01", nil, io.Discard, io.Discard, "test", "-f", "/opt/aiofcage/agent-turn.py")
		}, []string{"test", "-f", "/opt/aiofcage/agent-turn.py"}},
		"Python 检查": {func(m *Manager) error {
			return m.ExecHelper(ctx, "sbx-helper01", nil, io.Discard, io.Discard, PythonHelper("print(1)", "a b")...)
		}, []string{"/usr/bin/python3", "-I", "-c", "print(1)", "a b"}},
	} {
		docker, recorded := recordingDocker(t)
		m := &Manager{docker: docker}
		if err := tt.run(m); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got := recorded(); !slices.Equal(got, append(slices.Clone(prefix), tt.want...)) {
			t.Errorf("%s: %q", name, got)
		}
	}
	// 停止脚本只按完整的会话标记匹配，不会结束标记只是前缀相同的其他会话。
	if !strings.Contains(endProcessesScript, "marker in path.read_bytes().split(b'\\0')") {
		t.Error("the stop script must match whole environment entries")
	}
}

// 写入 Agent 设置也按辅助命令运行（查找原生会话记录见 TestNativeSessionLookup）。
func TestAgentSettingsAreWrittenByAHelper(t *testing.T) {
	m, _, log := liveSandbox(t, &liveGate{})
	if err := m.Refresh(context.Background(), "sbx-live0001"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	want := "exec --interactive --user 1000:1000 --workdir / aiofcage-sbx-live0001-agent /usr/bin/python3 -I /opt/aiofcage/aiofcage-init.py"
	if !slices.Contains(strings.Split(string(data), "\n"), want) || strings.Contains(string(data), "aiofcage-run") {
		t.Fatalf("settings written by\n%s", data)
	}
}

// 只有需要 Agent 设置的命令经过 aiofcage-run：启动 CLI 和终端。
func TestAgentCommandsLoadTheAgentSettings(t *testing.T) {
	m := &Manager{docker: runner{path: "/usr/local/bin/docker"}}
	sbx := sandbox.Sandbox{ID: "sbx-helper01", ProjectPath: "/Users/u/CageWorkspace/p"}
	args := m.ConversationCommand(context.Background(), sbx, "t-0123abcd").Args
	want := []string{"/usr/local/bin/docker", "--context", "colima-aiofcage", "exec", "--interactive", "--user", "1000:1000",
		"--workdir", sbx.ProjectPath, "aiofcage-" + sbx.ID + "-agent", "aiofcage-run",
		"env", "AIOFCAGE_TERMINAL_ID=t-0123abcd", "python3", "/opt/aiofcage/agent-turn.py"}
	if !slices.Equal(args, want) {
		t.Fatalf("conversation command %q", args)
	}
	if terminal := m.TerminalCommand(sbx).Args; !slices.Contains(terminal, "aiofcage-run") || terminal[len(terminal)-1] != "bash" {
		t.Fatalf("terminal command %q", terminal)
	}
}
