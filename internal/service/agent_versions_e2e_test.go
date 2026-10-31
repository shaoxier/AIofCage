package service

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/shaoxier/AIofCage/internal/runtime"
	"github.com/shaoxier/AIofCage/internal/sandbox"
	"github.com/shaoxier/AIofCage/internal/session"
	"github.com/shaoxier/AIofCage/internal/store"
)

// 两个官方固定版本在一次性沙箱中迁移；不调用模型，不使用用户凭据。
func TestAgentVersionUpgradeEndToEnd(t *testing.T) {
	if os.Getenv("AIOFCAGE_VERSION_E2E") != "1" {
		t.Skip("设置 AIOFCAGE_VERSION_E2E=1 验证真实容器版本迁移")
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
	s, err := app.CreateSandbox(CreateSandboxRequest{Name: "review-versions-" + time.Now().Format("150405"), ProjectMode: ProjectNew, Bindings: []sandbox.Binding{{Agent: session.Codex}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = app.DeleteSandbox(s.ID, false); _ = os.RemoveAll(s.ProjectPath) })
	manager.Progress = func(_ string, update runtime.Update) { t.Log(update.Step, update.Detail) }
	startCtx, startCancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer startCancel()
	if err := manager.Start(startCtx, s.ID); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if err := manager.Exec(ctx, s.ID, false, nil, &bytes.Buffer{}, &bytes.Buffer{}, "bash", "-c", "echo preserved > /home/agent/version-marker"); err != nil {
		t.Fatal(err)
	}
	// 临时存储中的受控测试目标，不影响用户默认版本。
	if err := st.SetSetting(runtime.AgentDefaultsKey, `{"codex":"0.163.0"}`); err != nil {
		t.Fatal(err)
	}
	if err := manager.UpgradeAgents(ctx, s.ID, []session.Agent{session.Codex}); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := manager.Exec(ctx, s.ID, false, nil, &out, &out, "bash", "-c", "codex --version; cat /home/agent/version-marker"); err != nil || !strings.Contains(out.String(), "0.163.0") || !strings.Contains(out.String(), "preserved") {
		t.Fatalf("迁移或 home 保留失败: %v %s", err, out.String())
	}
	if err := st.SetSetting(runtime.AgentDefaultsKey, `{"codex":"999999.0.0-review"}`); err != nil {
		t.Fatal(err)
	}
	failureCtx, failureCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer failureCancel()
	if err := manager.UpgradeAgents(failureCtx, s.ID, []session.Agent{session.Codex}); err == nil {
		t.Fatal("不存在的 npm 版本安装成功")
	}
	after, _ := st.Sandbox(s.ID)
	if runtime.PinnedAgentVersions(after)[session.Codex] != "0.163.0" || manager.Status(context.Background(), s.ID).State != "running" {
		t.Fatal("候选构建失败影响了原运行容器")
	}
	t.Log("真实版本切换、home 保留、候选失败不中断旧容器通过")
}
