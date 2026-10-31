package service

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shaoxier/AIofCage/internal/runtime"
	"github.com/shaoxier/AIofCage/internal/sandbox"
	"github.com/shaoxier/AIofCage/internal/session"
	"github.com/shaoxier/AIofCage/internal/store"
)

// A start stopped while it builds the agent image ends as stopped, not as a success; the sandbox
// is not running and no build step keeps running in the VM. Quitting the app does the same.
// Runs against the dedicated VM and downloads the pinned Codex from npm.
func TestStoppedStartLeavesNothingRunning(t *testing.T) {
	if os.Getenv("AIOFCAGE_PROGRESS_E2E") != "1" {
		t.Skip("设置 AIOFCAGE_PROGRESS_E2E=1 在专用虚拟机里验证停止启动")
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
	var mu sync.Mutex
	var latest OperationView
	app.ops.send = func(_ string, view OperationView) {
		mu.Lock()
		latest = view
		mu.Unlock()
	}
	s, err := app.CreateSandbox(CreateSandboxRequest{Name: "progress-e2e-" + time.Now().Format("150405"), ProjectMode: ProjectNew,
		Bindings: []sandbox.Binding{{Agent: session.Codex}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		_ = manager.Delete(ctx, s.ID, false)
		_ = os.RemoveAll(s.ProjectPath)
	})
	// A version no local image has, so the start must build; the VM finds its npm step by name.
	const pinned = "0.150.0"
	if err := st.UpdateSandbox(s.ID, func(x *sandbox.Sandbox) error {
		x.AgentVersions = map[session.Agent]string{session.Codex: pinned}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	inVM := func() string {
		out, _ := exec.Command("colima", "ssh", "-p", runtime.ColimaProfile, "--", "pgrep", "-af", "codex@"+pinned).Output()
		return strings.TrimSpace(string(out))
	}
	for _, quit := range []bool{false, true} {
		result := make(chan error, 1)
		go func() { result <- app.StartSandbox(s.ID) }()
		deadline := time.Now().Add(10 * time.Minute)
		for {
			mu.Lock()
			detail := latest.Detail
			mu.Unlock()
			if strings.Contains(detail, "codex@"+pinned) {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("the build never reached the Codex step; last detail %q", detail)
			}
			time.Sleep(200 * time.Millisecond)
		}
		time.Sleep(3 * time.Second)
		before := inVM()
		if quit {
			go app.Shutdown(time.Minute)
		} else if err := app.CancelSandboxOperation(s.ID); err != nil {
			t.Fatal(err)
		}
		err := <-result
		time.Sleep(5 * time.Second)
		after := inVM()
		view, _ := app.ops.view(s.ID)
		state := manager.Status(context.Background(), s.ID).State
		host, _ := exec.Command("sh", "-c", "ps -axo pid,ppid,pgid,etime,command | grep 'aiofcage-build-' | grep -v grep").Output()
		t.Logf("quit=%v: %v; outcome %s; state %s; step in the VM before %q, after %q; host builds %q", quit, err, view.Outcome, state, before, after, host)
		if err == nil || !strings.HasPrefix(err.Error(), "已停止启动") || view.Outcome != "stopped" || state == "running" {
			t.Fatalf("a stopped start must end as stopped with the sandbox not running")
		}
		if !strings.Contains(before, "codex@"+pinned) || after != "" || len(host) > 0 {
			t.Fatal("the build must be running before the stop and gone after it")
		}
	}
}
