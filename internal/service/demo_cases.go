package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/shaoxier/AIofCage/internal/democases"
	"github.com/shaoxier/AIofCage/internal/mcp"
	"github.com/shaoxier/AIofCage/internal/runtime"
	"github.com/shaoxier/AIofCage/internal/sandbox"
	"github.com/shaoxier/AIofCage/internal/session"
)

func (a *App) ListDemoCases() []democases.Case { return democases.List() }

type CreateDemoRequest struct {
	CaseID     string        `json:"case_id"`
	Agent      session.Agent `json:"agent"`
	ProviderID string        `json:"provider_id"`
}

type DemoSandboxResult struct {
	ID          string `json:"id"`
	ProjectPath string `json:"project_path"`
	Task        string `json:"task"`
}

// CreateDemoSandbox 仅创建全新且停止的沙箱，沿用已有供应商；不会替用户启动付费任务。
func (a *App) CreateDemoSandbox(request CreateDemoRequest) (DemoSandboxResult, error) {
	if err := a.runtimeReady(); err != nil {
		return DemoSandboxResult{}, err
	}
	c, err := democases.Find(request.CaseID)
	if err != nil {
		return DemoSandboxResult{}, err
	}
	if !request.Agent.Valid() {
		return DemoSandboxResult{}, errors.New("请选择智能体")
	}
	if request.ProviderID != "" {
		p, err := a.store.Provider(request.ProviderID)
		if err != nil || p.Agent != request.Agent {
			return DemoSandboxResult{}, errors.New("请选择该智能体的供应商")
		}
	}
	services, err := a.store.MCPServices()
	if err != nil {
		return DemoSandboxResult{}, err
	}
	serviceID := ""
	for _, s := range services {
		if s.IsBuiltin() && s.URL == "builtin://"+c.ID {
			serviceID = s.ID
			break
		}
	}
	if serviceID == "" {
		view, err := a.SaveMCPService(SaveMCPServiceRequest{Name: c.Name + "数据", Kind: string(mcp.KindDemo), URL: "builtin://" + c.ID, Preset: "demo-" + c.ID})
		if err != nil {
			return DemoSandboxResult{}, err
		}
		serviceID = view.ID
	}
	name := "案例-" + c.Name + "-" + a.now().Format("0102-150405.000")
	existing, err := a.store.Sandboxes()
	if err != nil {
		return DemoSandboxResult{}, err
	}
	project, err := prepareProject(ProjectNew, "", name, existing)
	if err != nil {
		return DemoSandboxResult{}, err
	}
	if err := os.WriteFile(filepath.Join(project, "TASK.md"), []byte("# "+c.Name+"\n\n"+c.Task+"\n"), 0o644); err != nil {
		_ = os.RemoveAll(project)
		return DemoSandboxResult{}, err
	}
	s, err := a.manager.Create(context.Background(), runtime.CreateRequest{Name: name, ProjectPath: project, Bindings: []sandbox.Binding{{Agent: request.Agent, ProviderID: request.ProviderID}}, MCP: []string{serviceID}, FilesystemMCP: true, Limits: sandbox.Limits{MaxRequests: 200, MaxTokens: 200000}})
	if err != nil {
		_ = os.RemoveAll(project)
		return DemoSandboxResult{}, err
	}
	if err := a.store.UpdateSandbox(s.ID, func(s *sandbox.Sandbox) error { s.DemoCase = c.ID; return nil }); err != nil {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_ = a.manager.Delete(ctx, s.ID, false)
		_ = os.RemoveAll(project)
		return DemoSandboxResult{}, err
	}
	return DemoSandboxResult{ID: s.ID, ProjectPath: project, Task: c.Task}, nil
}
