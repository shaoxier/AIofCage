package runtime

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shaoxier/AIofCage/internal/store"
)

// A stopped build stops in the VM too: no docker client stays on the host, no build step keeps
// running in BuildKit, and no image is left. Runs against the dedicated VM.
func TestStoppedBuildLeavesNothingRunning(t *testing.T) {
	if os.Getenv("AIOFCAGE_E2E") == "" {
		t.Skip("set AIOFCAGE_E2E=1 to run against the real VM")
	}
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	m, err := NewManager(st, nil)
	if err != nil {
		t.Fatal(err)
	}
	base := os.Getenv("AIOFCAGE_E2E_BASE_IMAGE") // a local image with a shell, so nothing is downloaded
	if base == "" {
		t.Skip("set AIOFCAGE_E2E_BASE_IMAGE to a local image")
	}
	marker := fmt.Sprintf("aiofcage-cancel-%d", time.Now().UnixNano())
	// The shell stays the parent of sleep, so the step is found in the VM by its marker.
	build := buildContext{tag: "aiofcage/progress-test:" + marker[len(marker)-12:], files: map[string][]byte{
		"Dockerfile": []byte("FROM " + base + "\nRUN echo first step\nRUN sh -c 'echo " + marker + "; sleep 300; true'\n")}}
	inVM := func() string {
		out, _ := exec.Command("colima", "ssh", "-p", ColimaProfile, "--", "pgrep", "-af", marker).Output()
		return strings.TrimSpace(string(out))
	}
	var before string
	var mu sync.Mutex
	var details []string
	ctx, cancel := context.WithCancel(context.Background())
	m.Progress = func(_ string, update Update) {
		mu.Lock()
		defer mu.Unlock()
		details = append(details, update.Step+update.Detail)
		if strings.HasPrefix(update.Detail, "第 3/3 步") && before == "" {
			before = "?"
			go func() {
				time.Sleep(3 * time.Second) // the long step runs in the VM now
				mu.Lock()
				before = inVM()
				mu.Unlock()
				cancel()
			}()
		}
	}
	started := time.Now()
	err = m.ensureImage(ctx, build, "sbx-progress")
	took := time.Since(started)
	t.Logf("build returned after %s: %v", took.Round(time.Millisecond), err)
	mu.Lock()
	for _, detail := range details {
		t.Log(detail)
	}
	mu.Unlock()
	if err == nil || took > 2*time.Minute {
		t.Fatalf("the build must stop at once: %v after %s", err, took)
	}
	time.Sleep(5 * time.Second) // BuildKit notices the closed session
	if out, _ := exec.Command("pgrep", "-f", build.tag).Output(); len(out) > 0 {
		t.Fatalf("docker client still running on the host: %s", out)
	}
	after := inVM()
	t.Logf("step in the VM before the stop: %q; after: %q", before, after)
	if !strings.Contains(before, marker) {
		t.Fatal("the check must see the running step before the stop")
	}
	if after != "" {
		t.Fatalf("the build step still runs in the VM: %s", after)
	}
	if _, err := m.docker.run(context.Background(), 15*time.Second, nil, "image", "inspect", build.tag); err == nil {
		t.Fatal("a stopped build left an image")
	}
}
