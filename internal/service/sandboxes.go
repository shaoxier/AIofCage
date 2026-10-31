package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"github.com/shaoxier/AIofCage/internal/devsource"
	"github.com/shaoxier/AIofCage/internal/runtime"
	"github.com/shaoxier/AIofCage/internal/sandbox"
	"github.com/shaoxier/AIofCage/internal/session"
)

// Emitter delivers events to the frontend (Wails EventsEmit in the desktop app).
type Emitter func(event string, data any)

// TerminalOpener opens a terminal window running the given shell command.
type TerminalOpener func(command string) error

// SetEmitter is called once the desktop window exists. Long sandbox operations report progress
// through the "sandbox:progress" event.
func (a *App) SetEmitter(emit Emitter) {
	a.emit = emit
}

func (a *App) runtimeReady() error {
	if a.manager == nil {
		if a.managerErr != nil {
			return fmt.Errorf("运行环境不可用：%w", a.managerErr)
		}
		return errors.New("运行环境不可用")
	}
	return nil
}

// Project modes for CreateSandbox.
const (
	ProjectUse  = "use"  // the folder is already inside ~/CageWorkspace
	ProjectCopy = "copy" // copy the folder into ~/CageWorkspace first
	ProjectNew  = "new"  // create an empty folder in ~/CageWorkspace
)

type CreateSandboxRequest struct {
	Name           string              `json:"name"`
	ProjectPath    string              `json:"project_path"`
	ProjectMode    string              `json:"project_mode"`
	Bindings       []sandbox.Binding   `json:"bindings"`
	MaxRequests    uint64              `json:"max_requests"`
	MaxTokens      uint64              `json:"max_tokens"`
	MCP            []string            `json:"mcp"`
	MCPPermissions map[string][]string `json:"mcp_permissions"`
	FilesystemMCP  bool                `json:"filesystem_mcp"`
	DevSources     []string            `json:"dev_sources"`
}

func (a *App) CreateSandbox(request CreateSandboxRequest) (SandboxView, error) {
	if err := a.runtimeReady(); err != nil {
		return SandboxView{}, err
	}
	name := strings.TrimSpace(request.Name)
	if name == "" {
		return SandboxView{}, errors.New("请填写沙箱名称")
	}
	if _, err := a.store.Sandbox(name); err == nil {
		return SandboxView{}, fmt.Errorf("已有同名沙箱：%s", name)
	}
	project, err := prepareProject(request.ProjectMode, request.ProjectPath, name)
	if err != nil {
		return SandboxView{}, err
	}
	sbx, err := a.manager.Create(context.Background(), runtime.CreateRequest{Name: name, ProjectPath: project,
		Bindings: request.Bindings, Limits: sandbox.Limits{MaxRequests: request.MaxRequests, MaxTokens: request.MaxTokens},
		MCP: request.MCP, MCPPermissions: request.MCPPermissions, FilesystemMCP: request.FilesystemMCP, DevSources: request.DevSources})
	if err != nil {
		return SandboxView{}, err
	}
	views, err := a.ListSandboxes()
	if err != nil {
		return SandboxView{}, err
	}
	for _, view := range views {
		if view.ID == sbx.ID {
			return view, nil
		}
	}
	return SandboxView{}, errors.New("沙箱已创建，但读取列表失败")
}

func (a *App) StartSandbox(id string) error {
	if err := a.runtimeReady(); err != nil {
		return err
	}
	return a.operation(id, "启动", 40*time.Minute, true, func(ctx context.Context) error {
		return a.manager.Start(ctx, id)
	})
}

// operation runs a lifecycle operation of a sandbox and records it for the interface. Its context
// derives from the app's root context, so quitting the app cancels it; a stoppable one can also be
// stopped with CancelSandboxOperation. A stopped operation fails with a message saying what state
// it left the sandbox in, never as a success.
func (a *App) operation(id, label string, timeout time.Duration, stoppable bool, run func(context.Context) error) error {
	sbx, err := a.store.Sandbox(id)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(a.root, timeout)
	defer cancel()
	var stop context.CancelFunc
	if stoppable {
		stop = cancel
	}
	if err := a.ops.begin(sbx.ID, sbx.Name, label, stop); err != nil {
		return err
	}
	err = run(ctx)
	switch {
	case err == nil:
		a.ops.end(sbx.ID, "done", "")
	case a.ops.stopping(sbx.ID) || a.root.Err() != nil:
		message := a.stoppedState(sbx.ID, label)
		a.ops.end(sbx.ID, "stopped", message)
		return errors.New(message)
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		message := fmt.Sprintf("%s超时（超过 %d 分钟）：%v", label, int(timeout.Minutes()), err)
		a.ops.end(sbx.ID, "failed", message)
		return errors.New(message)
	default:
		a.ops.end(sbx.ID, "failed", err.Error())
	}
	return err
}

// stoppedState says how a stopped operation left the sandbox.
func (a *App) stoppedState(id, label string) string {
	if a.manager == nil {
		return "已停止" + label
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	switch a.manager.Status(ctx, id).State {
	case "running":
		return "已停止" + label + "；沙箱仍在运行"
	case "partial":
		return "已停止" + label + "；沙箱的组件状态不一致，建议重新启动"
	}
	return "已停止" + label + "；沙箱没有运行"
}

// CancelSandboxOperation stops a sandbox's long operation (a start, reset or agent install). It
// returns at once; the operation ends when its current step allows: a build stops right away, a
// VM start finishes first.
func (a *App) CancelSandboxOperation(id string) error {
	return a.ops.stop(id)
}

// RunningOperations describes the operations in progress, for a confirmation before quitting.
func (a *App) RunningOperations() []string { return a.ops.running() }

// StopOperations stops every operation that can be stopped and waits up to timeout for all of
// them to end. It reports whether none is left.
func (a *App) StopOperations(timeout time.Duration) bool {
	a.ops.stopAll()
	return a.ops.wait(timeout)
}

// Shutdown runs when the app quits: operations stop, and anything still running after timeout is
// cancelled, so no build or other command the app started keeps running after it.
func (a *App) Shutdown(timeout time.Duration) {
	a.StopOperations(timeout)
	a.rootCancel()
	a.ops.wait(5 * time.Second)
}

func (a *App) StopSandbox(id string) error {
	if err := a.runtimeReady(); err != nil {
		return err
	}
	return a.operation(id, "停止", 2*time.Minute, false, func(ctx context.Context) error {
		a.closeSandboxConversations(id)
		return a.manager.Stop(ctx, id)
	})
}

// ResetSandbox rebuilds the isolated runtime objects while keeping the sandbox configuration
// and project directory. keepHome controls Agent settings and session history; restart restores a
// sandbox that was running before the reset.
func (a *App) ResetSandbox(id string, keepHome, restart bool) error {
	if err := a.runtimeReady(); err != nil {
		return err
	}
	return a.operation(id, "重置", 40*time.Minute, true, func(ctx context.Context) error {
		a.closeSandboxConversations(id)
		if err := a.manager.Reset(ctx, id, keepHome, restart); err != nil {
			return err
		}
		if !keepHome {
			return a.removeSandboxConversationRecoveries(id)
		}
		return nil
	})
}

// DeleteSandbox removes containers and networks. The project folder is never deleted; keepHome
// keeps the agent's home volume (its settings and session history).
func (a *App) DeleteSandbox(id string, keepHome bool) error {
	if err := a.runtimeReady(); err != nil {
		return err
	}
	return a.operation(id, "删除", 2*time.Minute, false, func(ctx context.Context) error {
		a.closeSandboxConversations(id)
		sbx, err := a.store.Sandbox(id)
		if err != nil {
			return err
		}
		if err := a.manager.Delete(ctx, id, keepHome); err != nil {
			return err
		}
		_ = a.removeSandboxConversationRecoveries(sbx.ID)
		return a.retireUsage(sbx)
	})
}

// BindProvider 保存供应商选择，运行中的沙箱在重启后应用。
func (a *App) BindProvider(id string, agent session.Agent, providerID string) error {
	if err := a.runtimeReady(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	return a.manager.Bind(ctx, id, agent, providerID)
}

// OpenTerminal opens Terminal.app inside the sandbox, running the agent (or a shell when agent
// is empty) as the unprivileged agent user in the project folder.
func (a *App) OpenTerminal(id string, agent session.Agent) error {
	if err := a.runtimeReady(); err != nil {
		return err
	}
	sbx, err := a.store.Sandbox(id)
	if err != nil {
		return err
	}
	if a.manager.Status(context.Background(), sbx.ID).State != "running" {
		return errors.New("请先启动沙箱")
	}
	command, err := terminalCommand(sbx, agent)
	if err != nil {
		return err
	}
	return a.terminal(systemTerminalScript(sbx.Name, a.manager.ExecArgs(sbx, true, command...)))
}

// terminalCommand builds the shell command typed into Terminal: a window title, then the
// docker exec invocation with every argument single-quoted.
// systemTerminalScript is the command line typed into macOS Terminal.
func systemTerminalScript(name string, args []string) string {
	quoted := make([]string, len(args))
	for i, arg := range args {
		quoted[i] = shellQuote(arg)
	}
	title := "AIofCage · " + strings.Map(func(r rune) rune {
		if r < 0x20 || r == '\'' || r == '\\' || r == 0x7f {
			return -1
		}
		return r
	}, name)
	return "printf '\\033]0;%s\\007' " + shellQuote(title) + "; clear; " + strings.Join(quoted, " ")
}

// appleScriptString escapes a value for use inside an AppleScript string literal.
func appleScriptString(value string) string {
	return `"` + strings.ReplaceAll(strings.ReplaceAll(value, `\`, `\\`), `"`, `\"`) + `"`
}

// OpenProjectFolder reveals the project folder in Finder.
func (a *App) OpenProjectFolder(id string) error {
	sbx, err := a.store.Sandbox(id)
	if err != nil {
		return err
	}
	return exec.Command("/usr/bin/open", sbx.ProjectPath).Run()
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'"'"'`) + "'"
}

// openInTerminal runs command in a new Terminal.app window. macOS asks the user once to allow
// AIofCage to control Terminal.
func openInTerminal(command string) error {
	out, err := exec.Command("/usr/bin/osascript",
		"-e", `tell application "Terminal"`, "-e", "activate", "-e", "do script "+appleScriptString(command), "-e", "end tell").CombinedOutput()
	if err != nil {
		if strings.Contains(string(out), "-1743") {
			return errors.New("没有控制“终端”的权限：请在 系统设置 → 隐私与安全性 → 自动化 中允许 AIofCage 控制“终端”")
		}
		return errors.New("无法打开终端")
	}
	return nil
}

const (
	maxCopyBytes = 2 << 30
	maxCopyFiles = 200_000
)

// prepareProject returns a folder inside ~/CageWorkspace for the sandbox, copying or creating it
// if needed. Only the workspace is visible to the VM, so other folders must be imported.
func prepareProject(mode, path, name string) (string, error) {
	workspace := sandbox.Workspace()
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		return "", err
	}
	switch mode {
	case ProjectUse, "":
		path = filepath.Clean(path)
		if err := sandbox.ValidateProject(path); err != nil {
			return "", fmt.Errorf("%w（不在工作区里的文件夹请选择“复制到工作区”）", err)
		}
		return path, nil
	case ProjectNew:
		target := uniqueDir(workspace, slug(name))
		return target, os.Mkdir(target, 0o755)
	case ProjectCopy:
		source, err := filepath.EvalSymlinks(filepath.Clean(path))
		if err != nil || !filepath.IsAbs(path) {
			return "", errors.New("要复制的文件夹不存在")
		}
		home, _ := os.UserHomeDir()
		resolvedWorkspace, _ := filepath.EvalSymlinks(workspace)
		if source == "/" || source == home || source == resolvedWorkspace || strings.HasPrefix(resolvedWorkspace+"/", source+"/") {
			return "", errors.New("不能复制这个文件夹，请选择具体的项目文件夹")
		}
		if err := checkCopySize(source); err != nil {
			return "", err
		}
		target := uniqueDir(workspace, slug(filepath.Base(source)))
		if err := copyTree(source, target); err != nil {
			return "", fmt.Errorf("复制项目失败：%w", err)
		}
		return target, nil
	}
	return "", errors.New("未知的项目导入方式")
}

func slug(name string) string {
	var b strings.Builder
	for _, r := range strings.TrimSpace(name) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '_':
			b.WriteRune(r)
		case r == ' ' || r == '.':
			b.WriteRune('-')
		}
	}
	result := strings.Trim(b.String(), "-")
	if result == "" {
		return "project"
	}
	return result
}

func uniqueDir(parent, base string) string {
	candidate := filepath.Join(parent, base)
	for i := 2; ; i++ {
		if _, err := os.Lstat(candidate); errors.Is(err, fs.ErrNotExist) {
			return candidate
		}
		candidate = filepath.Join(parent, fmt.Sprintf("%s-%d", base, i))
	}
}

func checkCopySize(source string) error {
	var size int64
	files := 0
	err := filepath.WalkDir(source, func(_ string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		files++
		if entry.Type().IsRegular() {
			info, err := entry.Info()
			if err != nil {
				return err
			}
			size += info.Size()
		}
		if size > maxCopyBytes || files > maxCopyFiles {
			return errors.New("too large")
		}
		return nil
	})
	if err != nil {
		return errors.New("文件夹太大（超过 2 GB 或 20 万个文件），请先清理依赖目录或直接把项目放进 ~/CageWorkspace")
	}
	return nil
}

// copyTree copies regular files, folders and symlinks (as links, never following them).
func copyTree(source, target string) error {
	return filepath.WalkDir(source, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		destination := filepath.Join(target, rel)
		info, err := entry.Info()
		if err != nil {
			return err
		}
		switch {
		case entry.IsDir():
			return os.MkdirAll(destination, info.Mode().Perm()|0o700)
		case entry.Type()&fs.ModeSymlink != 0:
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			return os.Symlink(link, destination)
		case entry.Type().IsRegular():
			return copyFile(path, destination, info.Mode().Perm())
		}
		return nil // sockets, devices and pipes are skipped
	})
}

func copyFile(source, destination string, mode fs.FileMode) error {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode|0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// AddSandboxAgents 通过桌面操作增装智能体，沿用生命周期互斥和进度通知。
func (a *App) AddSandboxAgents(id string, agents []session.Agent) error {
	if err := a.runtimeReady(); err != nil {
		return err
	}
	return a.operation(id, "添加智能体", 40*time.Minute, true, func(ctx context.Context) error {
		return a.manager.AddAgents(ctx, id, agents)
	})
}

// ListDevSources lists the development sources a sandbox can open.
func (a *App) ListDevSources() []devsource.Source { return devsource.Catalog() }

// SetSandboxDevSources chooses the development sources a sandbox may download from; a running
// sandbox is updated at once.
func (a *App) SetSandboxDevSources(id string, sources []string) error {
	if err := a.runtimeReady(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	return a.markStale(id, a.manager.SetDevSources(ctx, id, sources))
}
