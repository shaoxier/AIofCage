package runtime

import (
	"context"
	"errors"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/shaoxier/AIofCage/internal/provider"
	"github.com/shaoxier/AIofCage/internal/sandbox"
	"github.com/shaoxier/AIofCage/internal/session"
)

// ConversationCommand 只在容器内启动官方 CLI，正文及公开配置通过标准输入交付。
func (m *Manager) ConversationCommand(ctx context.Context, sbx sandbox.Sandbox, id string) *exec.Cmd {
	args := m.ExecArgs(sbx, false, "env", "AIOFCAGE_TERMINAL_ID="+id, "python3", "/opt/aiofcage/agent-turn.py")
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Env = childEnv()
	return cmd
}

// DSHConversationPatch 为单次进程配置模型，不改写其他会话的全局设置。
func DSHConversationPatch(p provider.Provider, model, effort string) string {
	p.Model = model
	return dshConfigWithEffort(p, effort)
}

// nativeSessionDirs is where each agent keeps its own session records in the agent's home. DeepSeek
// Harness also keeps a cache named after the session elsewhere; only its sessions directory counts.
var nativeSessionDirs = map[session.Agent]string{
	session.Codex:      "/home/agent/.codex/sessions",
	session.ClaudeCode: "/home/agent/.claude/projects",
	session.DSH:        "/home/agent/.dsh/sessions",
}

var nativeSessionID = regexp.MustCompile(`^[A-Za-z0-9._-]{4,128}$`)

// NativeSessionExists reports whether an agent's own record of a session is still in the running
// sandbox: Codex keeps sessions/<date>/rollout-…-<id>.jsonl, Claude Code projects/<project>/<id>.jsonl,
// DeepSeek Harness sessions/<project>/session-<id>/. An id that cannot be searched safely, or a
// sandbox that is not running, is an error: the caller then lets the agent decide.
func (m *Manager) NativeSessionExists(ctx context.Context, id string, agent session.Agent, nativeID string) (bool, error) {
	dir, ok := nativeSessionDirs[agent]
	if !ok || !nativeSessionID.MatchString(nativeID) {
		return false, errors.New("无法检查这个会话")
	}
	if !m.container(ctx, names(id).agent).running {
		return false, errors.New("沙箱未运行")
	}
	// No shell: the id is a single find argument. A missing directory means no session either.
	out, err := m.docker.run(ctx, 15*time.Second, nil, "exec", "--user", agentUser, names(id).agent,
		"find", dir, "-maxdepth", "6", "-name", "*"+nativeID+"*", "-print", "-quit")
	if err != nil && len(out) == 0 && !strings.Contains(err.Error(), "No such file") {
		return false, err
	}
	return strings.TrimSpace(string(out)) != "", nil
}
