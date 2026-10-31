package store

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shaoxier/AIofCage/internal/sandbox"
)

func TestStoreProcessWriter(t *testing.T) {
	dir := os.Getenv("AIOFCAGE_STORE_PROCESS_TEST")
	if dir == "" {
		t.Skip("仅供进程并发回归调用")
	}
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		if err := s.UpdateSandbox("shared", func(s *sandbox.Sandbox) error { s.Usage.Requests++; return nil }); err != nil {
			t.Fatal(err)
		}
	}
}

// TestStoreProcessCreator saves one sandbox for the overlap regression; a refusal is an expected
// outcome, any other error fails.
func TestStoreProcessCreator(t *testing.T) {
	dir := os.Getenv("AIOFCAGE_STORE_CREATE_TEST")
	if dir == "" {
		t.Skip("仅供进程并发回归调用")
	}
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	// All processes save at once, so that a check outside the lock would let several through.
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(time.Millisecond) {
		if _, err := os.Stat(os.Getenv("AIOFCAGE_STORE_CREATE_SIGNAL")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("没有收到开始信号")
		}
	}
	id := os.Getenv("AIOFCAGE_STORE_CREATE_ID")
	err = s.SaveSandbox(sandbox.Sandbox{ID: id, Name: id, ProjectPath: os.Getenv("AIOFCAGE_STORE_CREATE_PROJECT")})
	if err != nil && !strings.Contains(err.Error(), "沙箱「") {
		t.Fatal(err)
	}
}

// Processes (the desktop app, cagectl, the local AI control) that create overlapping sandboxes at
// the same time: the check runs under the store's lock, so exactly one of them is saved.
func TestStoreRefusesOverlappingProjectsAcrossProcesses(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	projects := []string{"/Users/u/CageWorkspace/race", "/Users/u/CageWorkspace/race/a", "/Users/u/CageWorkspace/race/a/b",
		"/Users/u/CageWorkspace/race/", "/Users/u/CageWorkspace/Race/a/b/c"}
	signal := filepath.Join(t.TempDir(), "go")
	commands := []*exec.Cmd{}
	for i, project := range projects {
		cmd := exec.Command(os.Args[0], "-test.run=^TestStoreProcessCreator$")
		cmd.Env = append(os.Environ(), "AIOFCAGE_STORE_CREATE_TEST="+s.Dir(), "AIOFCAGE_STORE_CREATE_PROJECT="+project,
			fmt.Sprintf("AIOFCAGE_STORE_CREATE_ID=sbx-race%d", i), "AIOFCAGE_STORE_CREATE_SIGNAL="+signal)
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		commands = append(commands, cmd)
	}
	time.Sleep(500 * time.Millisecond) // let every process start and wait for the signal
	if err := os.WriteFile(signal, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, cmd := range commands {
		if err := cmd.Wait(); err != nil {
			t.Fatal(err)
		}
	}
	if items, err := s.Sandboxes(); err != nil || len(items) != 1 {
		t.Fatalf("overlapping sandboxes saved by concurrent processes: %+v %v", items, err)
	}
}

func TestStoreDoesNotLoseConcurrentProcessWrites(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SaveSandbox(sandbox.Sandbox{ID: "shared", Name: "shared"}); err != nil {
		t.Fatal(err)
	}
	commands := []*exec.Cmd{}
	for i := 0; i < 3; i++ {
		cmd := exec.Command(os.Args[0], "-test.run=^TestStoreProcessWriter$")
		cmd.Env = append(os.Environ(), "AIOFCAGE_STORE_PROCESS_TEST="+s.Dir())
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		commands = append(commands, cmd)
	}
	for _, cmd := range commands {
		if err := cmd.Wait(); err != nil {
			t.Fatal(err)
		}
	}
	after, err := s.Sandbox("shared")
	if err != nil || after.Usage.Requests != 60 {
		t.Fatal(fmt.Sprintf("跨进程计数丢失: %d err=%v", after.Usage.Requests, err))
	}
}
