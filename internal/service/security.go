package service

import (
	"context"
	"time"

	"github.com/shaoxier/AIofCage/internal/audit"
	"github.com/shaoxier/AIofCage/internal/mcp"
)

type SecurityControlView struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail"`
}

type SecurityMCPView struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	Kind          string   `json:"kind"`
	KeySaved      bool     `json:"key_saved"`
	Connector     bool     `json:"connector"`
	ExplicitGrant bool     `json:"explicit_grant"`
	CatalogTools  []string `json:"catalog_tools"`
	GrantedTools  []string `json:"granted_tools"`
}

type SecurityIsolationView struct {
	FinishedAt time.Time      `json:"finished_at"`
	Results    []AttackResult `json:"results"`
}

type SecurityDiagnosticsView struct {
	SandboxID         string                 `json:"sandbox_id"`
	SandboxName       string                 `json:"sandbox_name"`
	State             string                 `json:"state"`
	GatekeeperRunning bool                   `json:"gatekeeper_running"`
	GatekeeperReady   bool                   `json:"gatekeeper_ready"`
	GatekeeperError   string                 `json:"gatekeeper_error,omitempty"`
	RecentDenied      int                    `json:"recent_denied"`
	Controls          []SecurityControlView  `json:"controls"`
	MCP               []SecurityMCPView      `json:"mcp"`
	LastIsolation     *SecurityIsolationView `json:"last_isolation,omitempty"`
}

func (a *App) SecurityDiagnostics(id string) (SecurityDiagnosticsView, error) {
	sbx, err := a.store.Sandbox(id)
	if err != nil {
		return SecurityDiagnosticsView{}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	view := SecurityDiagnosticsView{SandboxID: sbx.ID, SandboxName: sbx.Name, State: "unknown", MCP: []SecurityMCPView{}, Controls: []SecurityControlView{
		{ID: "project", Name: "项目文件", Status: "configured", Detail: "仅共享到沙箱项目目录；项目文件仍可被 Agent 修改。"},
		{ID: "network", Name: "网络出口", Status: "configured", Detail: "Agent 不直接接出网，模型和 MCP 请求经过门卫。"},
		{ID: "credentials", Name: "凭据隔离", Status: "configured", Detail: "上游密钥保留在宿主、门卫或可信连接器，沙箱只得到占位符。"},
		{ID: "audit", Name: "审计记录", Status: "configured", Detail: "记录脱敏请求元数据；请求正文、MCP 参数和结果不写入审计。"},
	}}
	if a.manager == nil {
		view.State = "unavailable"
		view.GatekeeperError = "运行环境不可用"
	} else {
		state := a.manager.Status(ctx, id)
		view.State = state.State
		view.GatekeeperRunning = state.GateRunning
	}
	a.monitor.mu.Lock()
	if health, ok := a.monitor.health[id]; ok {
		view.GatekeeperReady = health.GateAPI && health.GateRunning()
		view.GatekeeperError = health.GateError
	}
	a.monitor.mu.Unlock()
	if events, err := a.RecentEvents(id); err == nil {
		for _, event := range events {
			if event.Decision == audit.DecisionDeny {
				view.RecentDenied++
			}
		}
	}
	for _, id := range sbx.MCP {
		service, err := a.store.MCPService(id)
		if err != nil {
			return SecurityDiagnosticsView{}, err
		}
		item := SecurityMCPView{ID: service.ID, Name: service.Name, Kind: string(service.Kind), Connector: service.IsConnector(),
			CatalogTools: append([]string{}, service.Tools()...), GrantedTools: []string{}}
		if service.IsBuiltin() || service.Auth == mcp.AuthNone {
			item.KeySaved = true
		} else {
			secret, readErr := a.vault.Read(ctx, service.KeyRef)
			item.KeySaved = readErr == nil
			clear(secret)
		}
		if tools, ok := sbx.MCPPermissions[service.ID]; ok {
			item.ExplicitGrant = true
			item.GrantedTools = append([]string{}, tools...)
		} else {
			item.GrantedTools = service.DefaultTools()
		}
		view.MCP = append(view.MCP, item)
	}
	a.securityMu.Lock()
	if run, ok := a.isolationRuns[id]; ok {
		copyRun := SecurityIsolationView{FinishedAt: run.FinishedAt, Results: append([]AttackResult(nil), run.Results...)}
		view.LastIsolation = &copyRun
	}
	a.securityMu.Unlock()
	return view, nil
}

func (a *App) rememberIsolation(id string, results []AttackResult) {
	a.securityMu.Lock()
	defer a.securityMu.Unlock()
	if a.isolationRuns == nil {
		a.isolationRuns = map[string]SecurityIsolationRun{}
	}
	a.isolationRuns[id] = SecurityIsolationRun{FinishedAt: a.now(), Results: append([]AttackResult(nil), results...)}
}

type SecurityIsolationRun struct {
	FinishedAt time.Time
	Results    []AttackResult
}
