package runtime

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/shaoxier/AIofCage/internal/sandbox"
	"github.com/shaoxier/AIofCage/internal/session"
)

// 增装只追加智能体，供应商由独立绑定流程设置。
func addedBindings(current []sandbox.Binding, agents []session.Agent) ([]sandbox.Binding, error) {
	result := append([]sandbox.Binding{}, current...)
	seen := map[session.Agent]bool{}
	for _, b := range current {
		seen[b.Agent] = true
	}
	if len(agents) == 0 {
		return nil, errors.New("请选择要添加的智能体")
	}
	for _, agent := range agents {
		if !agent.Valid() {
			return nil, fmt.Errorf("不支持的智能体：%s", agent)
		}
		if !seen[agent] {
			result = append(result, sandbox.Binding{Agent: agent})
			seen[agent] = true
		}
	}
	return result, nil
}

// AddAgents 在镜像准备完成后才中断运行；失败时恢复配置和原运行状态。
func (m *Manager) AddAgents(ctx context.Context, id string, agents []session.Agent) error {
	m.environmentMu.RLock()
	defer m.environmentMu.RUnlock()
	unlockEnvironment, environmentErr := m.useEnvironment(ctx, false)
	if environmentErr != nil {
		return environmentErr
	}
	defer unlockEnvironment()
	unlock, lockErr := m.lock(ctx, id)
	if lockErr != nil {
		return lockErr
	}
	defer unlock()
	original, err := m.store.Sandbox(id)
	if err != nil {
		return err
	}
	bindings, err := addedBindings(original.Bindings, agents)
	if err != nil {
		return err
	}
	if len(bindings) == len(original.Bindings) {
		return nil
	}
	candidate := original
	candidate.Bindings = bindings
	candidate.AgentVersions = PinnedAgentVersions(original)
	defaults, err := DefaultAgentVersions(m.store)
	if err != nil {
		return err
	}
	for _, agent := range candidate.Agents() {
		if candidate.AgentVersions[agent] == "" {
			candidate.AgentVersions[agent] = defaults[agent]
		}
	}
	return m.replaceAgents(ctx, id, original, candidate)
}

// UpgradeAgents 只更新用户选择的智能体到当前新沙箱默认版本；其他沙箱不受影响。
func (m *Manager) UpgradeAgents(ctx context.Context, id string, agents []session.Agent) error {
	m.environmentMu.RLock()
	defer m.environmentMu.RUnlock()
	unlockEnvironment, environmentErr := m.useEnvironment(ctx, false)
	if environmentErr != nil {
		return environmentErr
	}
	defer unlockEnvironment()
	unlock, lockErr := m.lock(ctx, id)
	if lockErr != nil {
		return lockErr
	}
	defer unlock()
	original, err := m.store.Sandbox(id)
	if err != nil {
		return err
	}
	if len(agents) == 0 {
		return errors.New("请选择要更新的智能体")
	}
	defaults, err := DefaultAgentVersions(m.store)
	if err != nil {
		return err
	}
	candidate := original
	candidate.AgentVersions = PinnedAgentVersions(original)
	changed := false
	for _, agent := range agents {
		current, installed := candidate.AgentVersions[agent]
		if !installed || !agent.Valid() {
			return errors.New("沙箱未安装该智能体")
		}
		if current != defaults[agent] {
			candidate.AgentVersions[agent] = defaults[agent]
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return m.replaceAgents(ctx, id, original, candidate)
}

func (m *Manager) replaceAgents(ctx context.Context, id string, original, candidate sandbox.Sandbox) error {
	build, err := agentContextVersions(candidate.Agents(), PinnedAgentVersions(candidate))
	if err != nil {
		return err
	}
	m.progress(id, "准备智能体镜像，当前沙箱继续运行")
	if err := m.env.Ensure(ctx, func(line string) { m.detail(id, line) }); err != nil {
		return err
	}
	if err := m.ensureImage(ctx, build, id); err != nil {
		return err
	}
	state := m.Status(ctx, id).State
	if state == statePartial {
		return errors.New("沙箱组件状态不一致，请先停止沙箱再更新智能体")
	}
	running := state == stateRunning
	if running {
		m.progress(id, "保存用量并更新容器，终端会话将结束")
		if m.BeforeReplace != nil {
			m.BeforeReplace(id)
		}
		m.saveUsage(ctx, original)
		if err := m.stopContainers(ctx, names(id)); err != nil {
			return err
		}
	}
	if err := m.store.UpdateSandbox(id, func(s *sandbox.Sandbox) error {
		s.Bindings = candidate.Bindings
		s.AgentVersions = candidate.AgentVersions
		return nil
	}); err != nil {
		if running {
			recovery, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cancel()
			if restoreErr := m.startSnapshot(recovery, id, &original); restoreErr != nil {
				return fmt.Errorf("保存版本配置失败：%v；恢复原沙箱失败：%w。项目和主目录仍保留，可重试启动", err, restoreErr)
			}
			return fmt.Errorf("保存版本配置失败，已恢复原沙箱：%w", err)
		}
		return err
	}
	if running {
		if err := m.start(ctx, id); err != nil {
			recovery, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cancel()
			restoreErr := m.store.UpdateSandbox(id, func(s *sandbox.Sandbox) error {
				s.Bindings = original.Bindings
				s.AgentVersions = original.AgentVersions
				return nil
			})
			if restoreErr == nil {
				restoreErr = m.startSnapshot(recovery, id, &original)
			}
			if restoreErr != nil {
				return fmt.Errorf("更新失败：%v；恢复原沙箱失败：%w。项目和主目录仍保留，可重试启动", err, restoreErr)
			}
			return fmt.Errorf("更新失败，已恢复原沙箱：%w", err)
		}
	}
	m.progress(id, "智能体已更新")
	return nil
}
