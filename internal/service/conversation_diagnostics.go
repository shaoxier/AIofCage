package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/shaoxier/AIofCage/internal/provider"
	"github.com/shaoxier/AIofCage/internal/session"
)

type ConversationCheck struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail"`
}

type ConversationDiagnosticsView struct {
	SandboxID string              `json:"sandbox_id"`
	Agent     session.Agent       `json:"agent"`
	Checks    []ConversationCheck `json:"checks"`
	Ready     bool                `json:"ready"`
}

// DiagnoseConversation validates the local chain without sending a billable model request.
func (a *App) DiagnoseConversation(sandboxID string, agent session.Agent) (ConversationDiagnosticsView, error) {
	view := ConversationDiagnosticsView{SandboxID: sandboxID, Agent: agent, Checks: []ConversationCheck{}}
	add := func(id, name, status, detail string) {
		view.Checks = append(view.Checks, ConversationCheck{ID: id, Name: name, Status: status, Detail: detail})
	}
	if !agent.Valid() {
		return view, errors.New("智能体无效")
	}
	if a.manager == nil {
		detail := "Docker / Colima 不可用"
		if a.managerErr != nil {
			detail += "：" + a.managerErr.Error()
		}
		add("runtime", "运行环境", "problem", detail)
		return view, nil
	}
	sbx, err := a.store.Sandbox(sandboxID)
	if err != nil {
		return view, err
	}
	state := a.manager.Status(context.Background(), sandboxID)
	if state.State != "running" {
		add("runtime", "运行环境", "problem", "沙箱当前不是运行状态，请先启动沙箱。")
		return view, nil
	}
	add("runtime", "运行环境", "passed", "沙箱和门卫容器正在运行。")
	bindings := sbx.Bindings
	providers := sbx.AppliedProviders
	if providers != nil {
		bindings = sbx.AppliedBindings
	}
	var selected provider.Provider
	for _, binding := range bindings {
		if binding.Agent != agent || binding.ProviderID == "" {
			continue
		}
		if providers != nil {
			selected = providers[binding.ProviderID]
		} else {
			selected, err = a.store.Provider(binding.ProviderID)
		}
		break
	}
	if err != nil || selected.ID == "" {
		add("provider", "供应商绑定", "problem", "没有找到该智能体的已应用供应商，请在沙箱设置中选择并重启。")
		return view, nil
	}
	if err := selected.Validate(); err != nil {
		add("provider", "供应商配置", "problem", "供应商配置无效："+err.Error())
	} else {
		add("provider", "供应商配置", "passed", selected.Name+" · "+selected.Model)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	key, keyErr := a.vault.Read(ctx, selected.KeyRef)
	clear(key)
	if keyErr != nil {
		add("credential", "供应商凭据", "problem", "钥匙串中没有可用密钥，请重新保存供应商配置。")
	} else {
		add("credential", "供应商凭据", "passed", "宿主已找到密钥，沙箱只使用占位符。")
	}
	if err := a.manager.ExecHelper(ctx, sandboxID, nil, io.Discard, io.Discard, "test", "-f", "/opt/aiofcage/agent-turn.py"); err != nil {
		add("session", "会话组件", "problem", "沙箱会话组件不可用，请停止后重新启动以更新镜像。")
	} else {
		add("session", "会话组件", "passed", "会话桥接组件已安装。")
	}
	if !state.GateRunning {
		add("gate", "门卫接口", "problem", "门卫容器未运行，无法建立模型请求链路。")
	} else {
		add("gate", "门卫接口", "passed", "模型请求将经过本沙箱门卫。")
	}
	view.Ready = true
	for _, check := range view.Checks {
		if check.Status != "passed" {
			view.Ready = false
		}
	}
	return view, nil
}

func conversationFailureHint(detail string) string {
	lower := strings.ToLower(detail)
	hint := "任务未完成，请检查模型、供应商和沙箱状态"
	switch {
	case strings.Contains(lower, "401") || strings.Contains(lower, "403") || strings.Contains(lower, "unauthorized") || strings.Contains(lower, "forbidden"):
		hint = "供应商认证失败，请检查 API Key、协议和供应商配置"
	case strings.Contains(lower, "429") || strings.Contains(lower, "rate limit") || strings.Contains(lower, "too many requests"):
		hint = "供应商限流，请稍后重试或切换供应商"
	case strings.Contains(lower, "timeout") || strings.Contains(lower, "timed out") || strings.Contains(lower, "deadline"):
		hint = "供应商请求超时，请检查网络出口和上游服务状态"
	case strings.Contains(lower, "connection refused") || strings.Contains(lower, "network is unreachable") || strings.Contains(lower, "no route"):
		hint = "无法连接供应商，请检查门卫状态、域名策略和网络连通性"
	}
	return hint
}

func classifyConversationFailure(detail string) string {
	detail = strings.TrimSpace(detail)
	hint := conversationFailureHint(detail)
	if detail == "" {
		return hint
	}
	if len(detail) > 800 {
		detail = detail[len(detail)-800:]
	}
	return fmt.Sprintf("%s：%s", hint, detail)
}
