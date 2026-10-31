package service

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shaoxier/AIofCage/internal/runtime"
	"github.com/shaoxier/AIofCage/internal/sandbox"
	"github.com/shaoxier/AIofCage/internal/session"
)

// blocked runs an operation that waits until release closes or its context ends.
func blocked(app *App, id, label string, stoppable bool, release chan struct{}) chan error {
	result := make(chan error, 1)
	started := make(chan struct{})
	go func() {
		result <- app.operation(id, label, time.Minute, stoppable, func(ctx context.Context) error {
			close(started)
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
	}()
	<-started
	return result
}

func TestOperationsAreExclusiveAndVisible(t *testing.T) {
	app, _, st := newApp(t)
	st.SaveSandbox(sandbox.Sandbox{ID: "sbx-a", Name: "alpha", Bindings: []sandbox.Binding{{Agent: session.Codex}}})
	release := make(chan struct{})
	result := blocked(app, "sbx-a", "启动", true, release)
	app.ops.update("sbx-a", runtime.Update{Step: "检查专用虚拟机"})
	app.ops.update("sbx-a", runtime.Update{Step: "正在构建镜像"})
	app.ops.update("sbx-a", runtime.Update{Detail: "第 3/9 步：RUN npm install"})
	if err := app.operation("sbx-a", "启动", time.Minute, true, func(context.Context) error { return nil }); err == nil || !strings.Contains(err.Error(), "正在启动") {
		t.Fatalf("a second start must be refused while the first runs: %v", err)
	}
	views, _ := app.ListSandboxes()
	activity := views[0].Activity
	if views[0].Operation != "启动" || views[0].Progress != "正在构建镜像" || activity == nil || len(activity.Steps) != 2 ||
		activity.Steps[0].Ended == nil || activity.Steps[1].Ended != nil || activity.Detail != "第 3/9 步：RUN npm install" || !activity.Stoppable {
		t.Fatalf("operation must be visible to every page: %+v %+v", views[0], activity)
	}
	close(release)
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	views, _ = app.ListSandboxes()
	if views[0].Operation != "" || views[0].Activity == nil || views[0].Activity.Outcome != "done" || views[0].Activity.Ended == nil {
		t.Fatalf("a finished operation is no longer running but says how it ended: %+v %+v", views[0], views[0].Activity)
	}
}

// A stopped operation ends as stopped, never as a success, and says what it left behind.
func TestStoppedOperationIsNotASuccess(t *testing.T) {
	app, _, st := newApp(t)
	st.SaveSandbox(sandbox.Sandbox{ID: "sbx-a", Name: "alpha"})
	result := blocked(app, "sbx-a", "启动", true, make(chan struct{}))
	if err := app.CancelSandboxOperation("sbx-a"); err != nil {
		t.Fatal(err)
	}
	err := <-result
	activity, _ := app.ops.view("sbx-a")
	if err == nil || !strings.HasPrefix(err.Error(), "已停止启动") || activity.Outcome != "stopped" || activity.Message != err.Error() {
		t.Fatalf("got %v %+v", err, activity)
	}
	if err := app.CancelSandboxOperation("sbx-a"); err == nil {
		t.Fatal("nothing is left to stop")
	}
}

// Stopping a sandbox or deleting it runs to the end; quitting waits for it.
func TestShortOperationsCannotBeStopped(t *testing.T) {
	app, _, st := newApp(t)
	st.SaveSandbox(sandbox.Sandbox{ID: "sbx-a", Name: "alpha"})
	release := make(chan struct{})
	result := blocked(app, "sbx-a", "停止", false, release)
	if err := app.CancelSandboxOperation("sbx-a"); err == nil || !strings.Contains(err.Error(), "不能中途停止") {
		t.Fatalf("got %v", err)
	}
	if app.StopOperations(200 * time.Millisecond) {
		t.Fatal("an operation that cannot be stopped is still running")
	}
	close(release)
	if err := <-result; err != nil || !app.StopOperations(time.Second) {
		t.Fatalf("got %v", err)
	}
}

// Quitting stops every stoppable operation and waits for it to end.
func TestShutdownStopsOperations(t *testing.T) {
	app, _, st := newApp(t)
	st.SaveSandbox(sandbox.Sandbox{ID: "sbx-a", Name: "alpha"})
	st.SaveSandbox(sandbox.Sandbox{ID: "sbx-b", Name: "beta"})
	first := blocked(app, "sbx-a", "启动", true, make(chan struct{}))
	second := blocked(app, "sbx-b", "添加智能体", true, make(chan struct{}))
	if running := app.RunningOperations(); strings.Join(running, "、") != "「alpha」启动、「beta」添加智能体" {
		t.Fatalf("got %v", running)
	}
	app.Shutdown(time.Second)
	for _, result := range []chan error{first, second} {
		if err := <-result; err == nil || !strings.HasPrefix(err.Error(), "已停止") {
			t.Fatalf("got %v", err)
		}
	}
	if len(app.RunningOperations()) != 0 {
		t.Fatal("operations left after shutdown")
	}
}

// Detail updates arrive often (every line of a build); they reach the interface at most every
// detailInterval, and the latest one is never lost.
func TestDetailUpdatesAreBatched(t *testing.T) {
	app, _, st := newApp(t)
	st.SaveSandbox(sandbox.Sandbox{ID: "sbx-a", Name: "alpha"})
	var mu sync.Mutex
	var sent []OperationView
	app.ops.send = func(_ string, view OperationView) {
		mu.Lock()
		sent = append(sent, view)
		mu.Unlock()
	}
	release := make(chan struct{})
	result := blocked(app, "sbx-a", "启动", true, release)
	app.ops.update("sbx-a", runtime.Update{Step: "正在构建镜像"})
	for i := range 50 {
		app.ops.update("sbx-a", runtime.Update{Detail: fmt.Sprintf("第 1/9 步 · 第 %d 行", i)})
	}
	time.Sleep(2 * detailInterval)
	mu.Lock()
	count, latest := len(sent), sent[len(sent)-1].Detail
	mu.Unlock()
	if count > 5 || latest != "第 1/9 步 · 第 49 行" {
		t.Fatalf("%d updates sent, latest %q", count, latest)
	}
	close(release)
	<-result
}

func TestProviderUsageSurvivesSandboxDeletion(t *testing.T) {
	app, _, st := newApp(t)
	usage := map[string]sandbox.ProviderUsage{"ds": {Requests: 3, InputTokens: 100, OutputTokens: 10}}
	st.SaveSandbox(sandbox.Sandbox{ID: "sbx-a", Name: "alpha", ProviderUsage: usage})
	st.SaveSandbox(sandbox.Sandbox{ID: "sbx-b", Name: "beta", ProviderUsage: usage})
	sbx, _ := st.Sandbox("sbx-a")
	if err := app.retireUsage(sbx); err != nil {
		t.Fatal(err)
	}
	st.DeleteSandbox("sbx-a")
	summary, _ := app.UsageSummary()
	if got := summary.Providers["ds"]; got.Requests != 6 || got.InputTokens != 200 || got.OutputTokens != 20 {
		t.Fatalf("deleted sandbox usage lost: %+v", got)
	}
	app.forgetRetiredUsage("ds")
	if summary, _ = app.UsageSummary(); summary.Providers["ds"].Requests != 3 {
		t.Fatalf("retired usage of a deleted provider must go: %+v", summary.Providers["ds"])
	}
}
