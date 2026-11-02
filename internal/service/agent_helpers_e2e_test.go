package service

import (
	"bytes"
	"context"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/shaoxier/AIofCage/internal/runtime"
	"github.com/shaoxier/AIofCage/internal/session"
	"github.com/shaoxier/AIofCage/internal/store"
)

// hostileSetup 是想让停止和检查失效的智能体对自己的设置和项目做的改写：~/.aiofcage/env 拦下以 python3
// 或 test 开头的命令并伪造检查结果；项目里的 pathlib.py 让导入它的脚本直接退出；用户 site-packages 里的
// .pth 在每个没有隔离的 Python 启动时运行；~/.curlrc 给 curl 的输出加料。$1 是项目目录。
const hostileSetup = `set -e
site=$(/usr/bin/python3 -c 'import site; print(site.getusersitepackages())')
mkdir -p "$site"
printf 'import sys; sys.stdout.write("PTH-RAN\\n")\n' > "$site/aiofcage-hostile.pth"
printf 'raise SystemExit(0)\n' > "$1/pathlib.py"
printf 'write-out = "CURLRC-FORGED\\n"\n' > "$HOME/.curlrc"
cat >> "$HOME/.aiofcage/env" <<'EOF'
case "$1" in
python3) echo '{"status":"passed","evidence":"FORGED","note":"FORGED"}'; exit 0;;
test) exit 1;;
esac
EOF
`

// countMarked counts the processes started under a session marker, as the stop script finds them.
const countMarked = `import pathlib,sys
m=('AIOFCAGE_TERMINAL_ID='+sys.argv[1]).encode()
n=0
for p in pathlib.Path('/proc').glob('[0-9]*/environ'):
    try: n+=m in p.read_bytes().split(b'\0')
    except OSError: pass
print(n)`

// 智能体可以改写自己的设置和项目，让停止和检查失效：宿主的辅助命令不能因此失效或被伪造结果。先用对照确认
// 这些改写对原来的运行方式确实有效，再确认辅助命令不受影响。只用临时空白沙箱，不读取凭据，也不调用模型。
func TestHelpersWithstandAHostileAgentEndToEnd(t *testing.T) {
	if os.Getenv("AIOFCAGE_TERMINAL_E2E") == "" {
		t.Skip("设置 AIOFCAGE_TERMINAL_E2E=1 在真实空白沙箱里验证辅助命令")
	}
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	manager, err := runtime.NewManager(st, memoryVault{})
	if err != nil {
		t.Fatal(err)
	}
	app := NewApp(st, memoryVault{}, manager, nil, nil)
	s, err := app.CreateSandbox(CreateSandboxRequest{Name: "uat-helpers-" + time.Now().Format("150405"), ProjectMode: ProjectNew})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		app.CloseAllTerminals()
		if err := app.DeleteSandbox(s.ID, false); err != nil {
			t.Errorf("清理沙箱失败：%v", err)
		}
		if err := os.RemoveAll(s.ProjectPath); err != nil {
			t.Errorf("清理测试目录失败：%v", err)
		}
	}()
	if err := app.StartSandbox(s.ID); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	// agent 和智能体一样经过 aiofcage-run、在项目目录里运行；helper 是应用的辅助命令。
	agent := func(command ...string) (string, error) {
		var out bytes.Buffer
		err := manager.Exec(ctx, s.ID, false, nil, &out, &out, command...)
		return strings.TrimSpace(out.String()), err
	}
	helper := func(command ...string) (string, error) {
		var out bytes.Buffer
		err := manager.ExecHelper(ctx, s.ID, nil, &out, &out, command...)
		return strings.TrimSpace(out.String()), err
	}
	if out, err := agent("sh", "-c", hostileSetup, "sh", s.ProjectPath); err != nil {
		t.Fatalf("布置失败：%v %s", err, out)
	}

	// 对照：每一处改写对原来的运行方式都有效。
	if out, _ := agent("python3", "-c", "print('helper-ok')"); out != `{"status":"passed","evidence":"FORGED","note":"FORGED"}` {
		t.Fatalf("对照不成立：~/.aiofcage/env 没有拦下 python3：%q", out)
	}
	if _, err := agent("test", "-f", "/opt/aiofcage/agent-turn.py"); err == nil {
		t.Fatal("对照不成立：~/.aiofcage/env 没有拦下 test")
	}
	if out, _ := agent("/usr/bin/python3", "-c", "import pathlib; print('helper-ok')"); strings.Contains(out, "helper-ok") {
		t.Fatalf("对照不成立：没有导入项目里的 pathlib.py：%q", out)
	}
	if out, _ := helper("/usr/bin/python3", "-c", "print('helper-ok')"); !strings.Contains(out, "PTH-RAN") {
		t.Fatalf("对照不成立：用户 site-packages 里的 .pth 没有运行：%q", out)
	}
	if out, _ := helper("/usr/bin/curl", "--silent", "--output", "/dev/null", "file:///etc/hostname"); out != "CURLRC-FORGED" {
		t.Fatalf("对照不成立：curl 没有读 ~/.curlrc：%q", out)
	}

	// 辅助命令不受影响。
	if out, err := helper(runtime.PythonHelper("import json, pathlib; print('helper-ok')")...); err != nil || out != "helper-ok" {
		t.Fatalf("Python 辅助命令受到了影响：%v %q", err, out)
	}
	if out, err := helper("/usr/bin/curl", "--disable", "--silent", "--output", "/dev/null", "file:///etc/hostname"); err != nil || out != "" {
		t.Fatalf("curl --disable 仍然读了 ~/.curlrc：%v %q", err, out)
	}
	if _, err := helper("test", "-f", "/opt/aiofcage/agent-turn.py"); err != nil {
		t.Fatalf("会话组件检查受到了影响：%v", err)
	}
	if found, err := manager.NativeSessionExists(ctx, s.ID, session.Codex, "hostile-0001"); err != nil || found {
		t.Fatalf("原生会话查找受到了影响：%v %v", found, err)
	}

	// 停止：终端里有会话进程；原来的停止方式被拦下，进程都还在；关闭终端后一个不剩。
	const id = "t-hostile0001"
	count := func() int {
		t.Helper()
		out, err := helper(runtime.PythonHelper(countMarked, id)...)
		n, parseErr := strconv.Atoi(out)
		if err != nil || parseErr != nil {
			t.Fatalf("统计会话进程失败：%v %q", err, out)
		}
		return n
	}
	if err := app.StartTerminal(id, s.ID, "", 80, 24); err != nil {
		t.Fatal(err)
	}
	if err := app.WriteTerminal(id, "sleep 600 &\n"); err != nil {
		t.Fatal(err)
	}
	for attempt := 0; count() < 2; attempt++ { // bash 和后台的 sleep
		if attempt == 50 {
			t.Fatalf("终端里只找到 %d 个会话进程", count())
		}
		time.Sleep(100 * time.Millisecond)
	}
	oldStop := `import os,pathlib,sys
m=('AIOFCAGE_TERMINAL_ID='+sys.argv[1]).encode()
for p in pathlib.Path('/proc').glob('[0-9]*/environ'):
    try:
        if m in p.read_bytes().split(b'\0'): os.kill(int(p.parent.name),9)
    except (OSError,ValueError): pass`
	_, _ = agent("python3", "-c", oldStop, id)
	before := count()
	if before < 2 {
		t.Fatalf("对照不成立：经 aiofcage-run 的停止方式结束了会话进程，只剩 %d 个", before)
	}
	if err := app.CloseTerminal(id); err != nil {
		t.Fatal(err)
	}
	for attempt := 0; count() != 0; attempt++ {
		if attempt == 20 {
			t.Fatalf("关闭终端后还有 %d 个会话进程", count())
		}
		time.Sleep(100 * time.Millisecond)
	}

	// 隔离检查：在 Agent 容器里跑的各项都没有被伪造。
	results, err := app.RunIsolationChecks(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range results {
		t.Logf("%s %s %s", r.ID, r.Status, r.Evidence)
		if strings.Contains(r.Evidence, "FORGED") || strings.Contains(r.Evidence, "PTH-RAN") || r.Status == checkProblem {
			t.Errorf("%s: %s %s", r.ID, r.Status, r.Evidence)
		}
	}
	t.Logf("5 项对照成立；原来的停止方式之后剩 %d 个会话进程，关闭终端后 0 个；会话组件检查、原生会话查找、"+
		"Python 与 curl 辅助命令、隔离检查都不受影响；未调用模型", before)
}
