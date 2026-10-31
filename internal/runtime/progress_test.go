package runtime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shaoxier/AIofCage/internal/sandbox"
	"github.com/shaoxier/AIofCage/internal/session"
	"github.com/shaoxier/AIofCage/internal/store"
)

func script(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tool")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// Output arrives line by line, including lines a progress display redraws with \r, and the error
// keeps the last line.
func TestRunLinesHandsOverEachLine(t *testing.T) {
	tool := script(t, `printf 'first\n\n  second  \r third\n' ; printf 'oops: 失败的原因\n' >&2; exit 3`)
	var lines []string
	err := (runner{path: tool}).runLines(context.Background(), 10*time.Second, func(line string) { lines = append(lines, line) })
	if strings.Join(lines, "|") != "first|second|third|oops: 失败的原因" {
		t.Fatalf("lines %q", lines)
	}
	if err == nil || !strings.HasSuffix(err.Error(), "oops: 失败的原因") {
		t.Fatalf("got %v", err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := (runner{path: script(t, "sleep 5")}).runLines(cancelled, 10*time.Second, nil); err == nil || !strings.Contains(err.Error(), "已取消") {
		t.Fatalf("got %v", err)
	}
}

func TestClipKeepsWholeCharacters(t *testing.T) {
	if got := clip("沙箱策略拦截", 7); got != "沙箱…" {
		t.Fatalf("got %q", got)
	}
}

// docker build --progress=plain output becomes "step k of N" and the latest line of that step.
func TestBuildStepsFollowPlainProgress(t *testing.T) {
	var steps buildSteps
	var details []string
	for _, line := range []string{
		"#0 building with \"default\" instance using docker driver",
		"#1 [internal] load build definition from Dockerfile",
		"#5 [1/9] FROM public.ecr.aws/docker/library/node:22-bookworm-slim@sha256:0123",
		"#5 sha256:4f4fb700ef54461cfa02571ae0db9a0dc1e0cdb5577484a6d75e68dc38e8acc1 12.58MB / 49.55MB 3.2s",
		"#7 [builder 3/9] RUN npm install -g @openai/codex@0.161.0",
		"#7 12.345 added 120 packages in 11s",
		"#7 12.345 added 120 packages in 11s",
		"#7 DONE 12.4s",
	} {
		if detail, changed := steps.feed(line); changed {
			details = append(details, detail)
		}
	}
	want := []string{
		"第 1/9 步：FROM public.ecr.aws/docker/library/node:22-bookworm-slim@sha256:0123",
		"第 1/9 步：FROM public.ecr.aws/docker/library/node:22-bookworm-slim@sha256:0123 · 下载基础镜像 12.58MB / 49.55MB",
		"第 3/9 步：RUN npm install -g @openai/codex@0.161.0",
		"第 3/9 步：RUN npm install -g @openai/codex@0.161.0 · added 120 packages in 11s",
	}
	if strings.Join(details, "\n") != strings.Join(want, "\n") {
		t.Fatalf("details:\n%s", strings.Join(details, "\n"))
	}
}

func TestVMLinesAreDescribed(t *testing.T) {
	for line, want := range map[string]string{
		`time="2026-10-30T18:37:42-07:00" level=info msg="downloading disk image ..." context=vm`:     "正在下载虚拟机系统镜像（首次约 300 MB，网络慢时要等较久）",
		`time="…" level=info msg="[hostagent] Waiting for the essential requirement 2 of 3: \"ssh\""`: "等待虚拟机就绪（2/3）",
		`time="…" level=info msg="done"`:            "虚拟机已启动",
		`time="…" level=info msg="starting colima"`: "starting colima",
		`plain line from a tool`:                    "plain line from a tool",
	} {
		if got := vmLine(line); got != want {
			t.Errorf("%s: got %q", line, got)
		}
	}
}

// fakeColima reports the VM stopped until a start (which takes a moment) has finished, and counts
// the starts.
func fakeColima(t *testing.T) (*Environment, string) {
	t.Helper()
	dir := t.TempDir()
	colima := script(t, `dir='`+dir+`'
case "$1" in
 list) if [ -f "$dir/running" ]; then s=Running; else s=Stopped; fi
       printf '{"name":"aiofcage","status":"%s","arch":"aarch64","cpus":4,"memory":6442450944,"disk":42949672960}\n' "$s" ;;
 start) echo start >> "$dir/starts"; echo 'time="x" level=info msg="starting colima"'; sleep 1; touch "$dir/running"
        echo 'time="x" level=info msg="done"' ;;
 *) exit 1 ;;
esac
`)
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return &Environment{colima: runner{path: colima}, store: st}, dir
}

// A stop during the VM start waits for the start to finish: colima is not killed half-way.
func TestStopWaitsForTheVMStart(t *testing.T) {
	env, dir := fakeColima(t)
	ctx, cancel := context.WithCancel(context.Background())
	var mu sync.Mutex
	var details []string
	err := env.startVM(ctx, t.TempDir(), func(line string) {
		mu.Lock()
		details = append(details, line)
		mu.Unlock()
		if line == "starting colima" {
			cancel() // colima is running now
		}
	})
	if err != context.Canceled {
		t.Fatalf("the stop takes effect after the start: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "running")); err != nil {
		t.Fatal("colima was interrupted before the VM was up")
	}
	mu.Lock()
	defer mu.Unlock()
	if joined := strings.Join(details, "|"); !strings.Contains(joined, "等虚拟机启动完成后再停止") || !strings.Contains(joined, "虚拟机已启动") {
		t.Fatalf("details %q", details)
	}
}

// A stop that arrives before colima runs needs no wait: nothing is started.
func TestStopBeforeTheVMStartStartsNothing(t *testing.T) {
	env, dir := fakeColima(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := env.startVM(ctx, t.TempDir(), nil); err == nil {
		t.Fatal("a stopped operation must not go on")
	}
	if _, err := os.Stat(filepath.Join(dir, "starts")); err == nil {
		t.Fatal("colima was started after the stop")
	}
}

// Two sandboxes starting together start the VM once.
func TestVMStartsOnceForConcurrentStarts(t *testing.T) {
	env, dir := fakeColima(t)
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = env.startVM(context.Background(), t.TempDir(), nil)
		}()
	}
	wg.Wait()
	starts, _ := os.ReadFile(filepath.Join(dir, "starts"))
	if errs[0] != nil || errs[1] != nil || strings.Count(string(starts), "start") != 1 {
		t.Fatalf("errors %v, starts %q", errs, starts)
	}
}

// The VM start writes to a file, not to a pipe of this process: if the app quits during the start,
// colima finishes instead of dying of SIGPIPE. Lines still arrive while it runs.
func TestLoggedCommandOutlivesItsReader(t *testing.T) {
	tool := script(t, `if [ -p /dev/stdout ]; then echo pipe; else echo file; fi; sleep 0.3; printf 'last line\n' >&2; exit 2`)
	var lines []string
	err := (runner{path: tool}).runLogged(context.Background(), 10*time.Second, filepath.Join(t.TempDir(), "out.log"),
		func(line string) { lines = append(lines, line) })
	if strings.Join(lines, "|") != "file|last line" || err == nil || !strings.HasSuffix(err.Error(), "last line") {
		t.Fatalf("lines %q, err %v", lines, err)
	}
}

// A command ended by a stop says so in a way callers can test, unlike one that failed.
func TestCancelledCommandsAreRecognisable(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	tool := script(t, "sleep 5")
	if _, err := (runner{path: tool}).run(cancelled, time.Minute, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("run: %v", err)
	}
	if err := (runner{path: tool}).runLines(cancelled, time.Minute, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("runLines: %v", err)
	}
	if err := (runner{path: script(t, "exit 3")}).runLines(context.Background(), time.Minute, nil); errors.Is(err, context.Canceled) {
		t.Fatalf("a failure is not a cancellation: %v", err)
	}
}

// A reset stopped during its teardown finishes the teardown, so the sandbox is not left cut off
// yet running, and does not restart.
func TestStoppedResetFinishesTheTeardown(t *testing.T) {
	gate := &liveGate{}
	m, _, log := liveSandbox(t, gate)
	if err := m.Refresh(context.Background(), "sbx-live0001"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.Progress = func(_ string, update Update) {
		if update.Step == "停止并删除旧的容器和网络" {
			cancel()
		}
	}
	err := m.Reset(ctx, "sbx-live0001", true, true)
	commands, _ := os.ReadFile(log)
	if !errors.Is(err, context.Canceled) || !gate.empty() {
		t.Fatalf("the reset ends as stopped after clearing the gatekeeper: %v %v", err, gate.rules)
	}
	if !strings.Contains(string(commands), "rm --force aiofcage-sbx-live0001-agent") || strings.Contains(string(commands), "start aiofcage-sbx-live0001") {
		t.Fatalf("the teardown must finish and the restart must not run:\n%s", commands)
	}
}

// A stop that lands just after the new agent image is built changes nothing: the running
// containers stay, and the new version is not recorded.
func TestStopAfterTheBuildChangesNothing(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	workspace := filepath.Join(home, "CageWorkspace")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	resolved, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(t.TempDir(), "docker.log")
	colima := script(t, `case "$1" in
 list) echo '{"name":"aiofcage","status":"Running","arch":"aarch64","cpus":4,"memory":6442450944,"disk":42949672960}' ;;
 ssh) echo "mount0 `+resolved+` virtiofs rw 0 0" ;;
esac
`)
	limactl := script(t, `echo '{"config":{"vmType":"vz","mountType":"virtiofs","mounts":[{"location":"`+workspace+`","writable":true}],"ssh":{"forwardAgent":false}}}'
`)
	docker := script(t, `printf '%s\n' "$*" >> '`+log+`'
case "$3 $4" in
 "context inspect") echo '[{"Endpoints":{"docker":{"Host":"unix://`+home+`/.colima/aiofcage/docker.sock"}}}]' ;;
 "image inspect") exit 1 ;;
 "container inspect") echo "true aiofcage/agent:old 2" ;;
esac
case "$3" in
 build) echo '#1 [1/1] FROM base'; echo '#1 DONE 0.1s' ;;
esac
exit 0
`)
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sbx := sandbox.Sandbox{ID: "sbx-after001", Name: "after", ProjectPath: filepath.Join(workspace, "after"),
		Bindings: []sandbox.Binding{{Agent: session.Codex, ProviderID: "p"}}, AgentVersions: map[session.Agent]string{session.Codex: "0.150.0"},
		Placeholders: map[string]string{}}
	if err := st.SaveSandbox(sbx); err != nil {
		t.Fatal(err)
	}
	dockerRunner := runner{path: docker, args: []string{"--context", DockerContext}}
	m := &Manager{store: st, docker: dockerRunner, gates: map[string]*gateClient{}, locks: map[string]*sync.Mutex{}, connectorTokens: map[string]string{},
		env: &Environment{colima: runner{path: colima}, limactl: limactl, docker: dockerRunner, store: st}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.Progress = func(_ string, update Update) {
		if strings.HasPrefix(update.Detail, "第 1/1 步") {
			time.Sleep(300 * time.Millisecond) // the build has finished; 停止 is pressed now
			cancel()
		}
	}
	err = m.UpgradeAgents(ctx, sbx.ID, []session.Agent{session.Codex})
	saved, _ := st.Sandbox(sbx.ID)
	calls, _ := os.ReadFile(log)
	if !errors.Is(err, context.Canceled) || saved.AgentVersions[session.Codex] != "0.150.0" || strings.Contains(string(calls), "stop --time") {
		t.Fatalf("got %v, versions %v, calls:\n%s", err, saved.AgentVersions, calls)
	}
}
