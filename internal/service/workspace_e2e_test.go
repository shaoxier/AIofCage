package service

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shaoxier/AIofCage/internal/provider"
	"github.com/shaoxier/AIofCage/internal/runtime"
	"github.com/shaoxier/AIofCage/internal/sandbox"
	"github.com/shaoxier/AIofCage/internal/session"
	"github.com/shaoxier/AIofCage/internal/store"
	"github.com/shaoxier/AIofCage/internal/vault"
)

// 可选真实验证只创建 uat- 资源，读取现有凭据引用，不修改用户供应商或沙箱。
func TestWorkspaceEndToEnd(t *testing.T) {
	if os.Getenv("AIOFCAGE_WORKSPACE_E2E") == "" {
		t.Skip("设置 AIOFCAGE_WORKSPACE_E2E=1 验证专用实例与三个真实智能体")
	}
	dir, _ := store.DefaultDir()
	existing, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for source, target := range map[string]string{"xapi": "uat-codex-a", "deepseek-cc": "uat-claude", "deepseek-dsh": "uat-dsh"} {
		p, err := existing.Provider(source)
		if err != nil {
			t.Fatal(err)
		}
		p.ID = target
		p.Name = target
		p.Pricing = &provider.Pricing{Currency: "CNY", Input: 2, Output: 10, CacheRead: 0.2, CacheWrite: 2, Version: "uat-synthetic-rate"}
		if err := st.SaveProvider(p); err != nil {
			t.Fatal(err)
		}
	}
	p, _ := st.Provider("uat-codex-a")
	p.ID = "uat-codex-b"
	p.Name = p.ID
	if err := st.SaveProvider(p); err != nil {
		t.Fatal(err)
	}
	keychain := vault.NewKeychain()
	manager, err := runtime.NewManager(st, keychain)
	if err != nil {
		t.Fatal(err)
	}
	app := NewApp(st, keychain, manager, nil, nil)
	manager.Progress = func(id string, update runtime.Update) { t.Logf("%s %s %s", id, update.Step, update.Detail) }
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	s, err := app.CreateSandbox(CreateSandboxRequest{Name: "uat-engine-" + time.Now().Format("150405"), ProjectMode: ProjectNew, MaxRequests: 30, MaxTokens: 60000})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { app.CloseAllTerminals(); _ = app.DeleteSandbox(s.ID, false); _ = os.RemoveAll(s.ProjectPath) }()
	if len(s.Bindings) != 0 {
		t.Fatal("默认不是空白沙箱")
	}
	if err := app.StartSandbox(s.ID); err != nil {
		t.Fatal(err)
	}
	var shell bytes.Buffer
	if err := manager.Exec(ctx, s.ID, false, nil, &shell, &shell, "bash", "-c", "id -u; printf 'retained\\n' > uat-retained.txt"); err != nil || !strings.Contains(shell.String(), "1000") {
		t.Fatalf("空白 Shell 失败 %v", err)
	}
	if err := app.AddSandboxAgents(s.ID, []session.Agent{session.Codex, session.ClaudeCode}); err != nil {
		t.Fatal(err)
	}
	if err := app.BindProvider(s.ID, session.Codex, "uat-codex-a"); err != nil {
		t.Fatal(err)
	}
	if err := app.BindProvider(s.ID, session.ClaudeCode, "uat-claude"); err != nil {
		t.Fatal(err)
	}
	if err := app.StopSandbox(s.ID); err != nil {
		t.Fatal(err)
	}
	if err := app.StartSandbox(s.ID); err != nil {
		t.Fatal(err)
	}
	// 让增装应用一份不可用的待生效配置，验证失败后可按原快照恢复运行。
	original, _ := st.Provider("uat-codex-a")
	broken := original
	broken.KeyRef = "aiofcage-uat-missing-key"
	_ = st.SaveProvider(broken)
	err = app.AddSandboxAgents(s.ID, []session.Agent{session.DSH})
	if err == nil || !strings.Contains(err.Error(), "已恢复原沙箱") || manager.Status(ctx, s.ID).State != "running" {
		t.Fatalf("增装失败恢复不符合预期：%v", err)
	}
	saved, _ := st.Sandbox(s.ID)
	if len(saved.Bindings) != 2 {
		t.Fatal("失败增装没有恢复智能体组合")
	}
	_ = st.SaveProvider(original)
	if err := app.AddSandboxAgents(s.ID, []session.Agent{session.DSH}); err != nil {
		t.Fatal(err)
	}
	if err := app.BindProvider(s.ID, session.DSH, "uat-dsh"); err != nil {
		t.Fatal(err)
	}
	// 独立的第二个沙箱选择另一份 Codex 配置，验证不会串配置。
	other, err := app.CreateSandbox(CreateSandboxRequest{Name: "uat-binding-" + time.Now().Format("150405"), ProjectMode: ProjectNew, Bindings: []sandbox.Binding{{Agent: session.Codex, ProviderID: "uat-codex-b"}}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = app.DeleteSandbox(other.ID, false); _ = os.RemoveAll(other.ProjectPath) }()
	if err := app.StartSandbox(other.ID); err != nil {
		t.Fatal(err)
	}
	if err := app.StopSandbox(s.ID); err != nil {
		t.Fatal(err)
	}
	if err := app.StartSandbox(s.ID); err != nil {
		t.Fatal(err)
	}
	a, _ := st.Sandbox(s.ID)
	b, _ := st.Sandbox(other.ID)
	if a.AppliedBindings[0].ProviderID != "uat-codex-a" || b.AppliedBindings[0].ProviderID != "uat-codex-b" || a.Placeholders["uat-codex-a"] == b.Placeholders["uat-codex-b"] {
		t.Fatal("供应商绑定或占位符串沙箱")
	}
	app.tick(ctx)
	checkBuiltInTerminal(t, app, s.ID)
	secrets, err := app.realSecrets(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	for _, task := range []struct {
		name     string
		provider string
		args     []string
		file     string
	}{
		{"Codex", "uat-codex-a", []string{"codex", "exec", "--skip-git-repo-check", "Create uat-codex.txt containing exactly codex-ok and stop. Do not inspect other files."}, "uat-codex.txt"},
		{"Claude Code", "uat-claude", []string{"claude", "-p", "--permission-mode", "bypassPermissions", "Create uat-claude.txt containing exactly claude-ok and stop. Do not inspect other files."}, "uat-claude.txt"},
		{"DeepSeek Harness", "uat-dsh", []string{"dsh", "headless", "--json", "Create uat-dsh.txt containing exactly dsh-ok and stop. Do not inspect other files."}, "uat-dsh.txt"},
	} {
		t.Run(task.name, func(t *testing.T) {
			// 无网络响应时明确跳过该供应商任务；HTTP 错误和任务错误仍使验证失败。
			probe, err := app.TestProvider(task.provider)
			if err != nil {
				t.Fatal(err)
			}
			if probe.Status == 0 {
				t.Skipf("供应商连通性受限：%s", probe.Message)
			}
			taskCtx, taskCancel := context.WithTimeout(ctx, 3*time.Minute)
			defer taskCancel()
			var output bytes.Buffer
			err = manager.Exec(taskCtx, s.ID, false, nil, &output, &output, task.args...)
			if err != nil {
				diagnostic := output.String()
				for _, secret := range secrets {
					if secret != "" {
						diagnostic = strings.ReplaceAll(diagnostic, secret, "[已脱敏]")
					}
				}
				if len(diagnostic) > 6000 {
					diagnostic = diagnostic[len(diagnostic)-6000:]
				}
				t.Fatalf("真实任务失败：%v\n%s", err, diagnostic)
			}
			content, err := os.ReadFile(filepath.Join(s.ProjectPath, task.file))
			if err != nil || !strings.Contains(string(content), strings.TrimPrefix(strings.TrimSuffix(task.file, ".txt"), "uat-")+"-ok") {
				t.Fatalf("未生成预期文件 %v", err)
			}
			t.Log("真实文件任务通过")
		})
	}
	time.Sleep(time.Second)
	report, err := app.QueryUsage(UsageQuery{SandboxID: s.ID, Period: "all"})
	if err != nil || report.Requests < 3 {
		t.Fatalf("持久化用量缺失 %d %v", report.Requests, err)
	}
	t.Logf("模型请求=%d 未知=%d 估算=%v 未计价=%d", report.Requests, report.Unknown, report.Costs, report.Unpriced)
	for _, g := range report.Groups {
		t.Logf("供应商=%s 模型=%s 输入=%d 输出=%d 缓存=%d 费用=%v", g.ProviderID, g.Model, g.InputTokens, g.OutputTokens, g.CacheReadTokens, g.Costs)
	}
	retained, err := os.ReadFile(filepath.Join(s.ProjectPath, "uat-retained.txt"))
	if err != nil || string(retained) != "retained\n" {
		t.Fatal("增装或重启丢失项目文件")
	}
	fresh := NewApp(st, keychain, manager, nil, nil)
	again, err := fresh.QueryUsage(UsageQuery{SandboxID: s.ID, Period: "all"})
	if err != nil || again.Requests != report.Requests {
		t.Fatal("应用重开用量不一致")
	}
	app.tick(ctx)
	h, err := app.SandboxHealth(s.ID)
	if err != nil || !h.Current.Stats.Valid || len(h.Current.Components) != 2 {
		t.Fatalf("真实健康样本失败 %v", err)
	}
}
