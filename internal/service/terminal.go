package service

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/creack/pty"

	"github.com/shaoxier/AIofCage/internal/runtime"
	"github.com/shaoxier/AIofCage/internal/sandbox"
	"github.com/shaoxier/AIofCage/internal/session"
)

// The built-in terminal runs `docker exec -it` against the sandbox under a PTY owned by the
// desktop app and streams it to xterm.js in the window. It is the same access as the system
// terminal: the agent user inside the sandbox, with the project folder as working directory.

const (
	maxTerminals     = 12
	terminalReadSize = 32 << 10
)

var terminalIDPattern = regexp.MustCompile(`^t-[a-z0-9]{8,32}$`)

type terminals struct {
	mu      sync.Mutex
	items   map[string]*terminal
	closing bool
}

type terminal struct {
	cmd       *exec.Cmd
	pty       *os.File
	sandboxID string
	id        string
	agent     session.Agent
	closed    atomic.Bool
	writeMu   sync.Mutex
	closeOnce sync.Once
}

// terminalEnv makes full-screen agents (Codex, Claude Code, dsh) render colours and Chinese text.
var terminalEnv = []string{"env", "TERM=xterm-256color", "COLORTERM=truecolor", "LANG=C.UTF-8", "LC_ALL=C.UTF-8"}

// terminalCommand returns what runs in the sandbox for a terminal: the agent's CLI, or a shell.
func terminalCommand(sbx sandbox.Sandbox, agent session.Agent) ([]string, error) {
	if agent == "" {
		return []string{"bash"}, nil
	}
	for _, installed := range sbx.Agents() {
		if installed == agent {
			if spec, ok := runtime.Spec(agent); ok {
				if agent == session.DSH {
					return []string{"python3", "/home/agent/.aiofcage/dsh-console.py"}, nil
				}
				return []string{spec.Command}, nil
			}
		}
	}
	return nil, fmt.Errorf("沙箱里没有安装 %s", agent)
}

// StartTerminal opens a terminal in the sandbox. The window chooses the ID so that it can route
// output that arrives before this call returns. Output is sent as "terminal:data" events
// ({id, data: base64}); "terminal:exit" ({id, code}) follows when the process ends.
func (a *App) StartTerminal(id, sandboxID string, agent session.Agent, cols, rows int) error {
	if err := a.runtimeReady(); err != nil {
		return err
	}
	if !terminalIDPattern.MatchString(id) {
		return errors.New("终端 ID 无效")
	}
	sbx, err := a.store.Sandbox(sandboxID)
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
	a.terms.mu.Lock()
	if a.terms.closing {
		a.terms.mu.Unlock()
		return errors.New("应用正在退出")
	}
	if a.terms.items == nil {
		a.terms.items = map[string]*terminal{}
	}
	if _, exists := a.terms.items[id]; exists {
		a.terms.mu.Unlock()
		return errors.New("终端 ID 重复")
	}
	if len(a.terms.items) >= maxTerminals {
		a.terms.mu.Unlock()
		return fmt.Errorf("最多同时打开 %d 个终端，请先关掉一些", maxTerminals)
	}
	t := &terminal{sandboxID: sbx.ID, id: id, agent: agent}

	// 唯一会话标记供关闭时定位容器内的进程及子进程，避免只结束宿主 Docker 客户端。
	args := append([]string{"env", "AIOFCAGE_TERMINAL_ID=" + id}, terminalEnv...)
	cmd := a.manager.TerminalCommand(sbx, append(args, command...)...)
	file, err := pty.StartWithSize(cmd, terminalSize(cols, rows))
	if err != nil {
		a.terms.mu.Unlock()
		return fmt.Errorf("无法打开终端：%w", err)
	}
	// 完成进程启动后再发布会话，关闭与退出不会漏过尚未赋值的 PTY。
	t.cmd, t.pty = cmd, file
	a.terms.items[id] = t
	a.terms.mu.Unlock()
	go a.pumpTerminal(id, t)
	return nil
}

func terminalSize(cols, rows int) *pty.Winsize {
	clamp := func(v, lo, hi int) uint16 { return uint16(min(max(v, lo), hi)) }
	return &pty.Winsize{Cols: clamp(cols, 20, 500), Rows: clamp(rows, 5, 200)}
}

func (a *App) pumpTerminal(id string, t *terminal) {
	buf := make([]byte, terminalReadSize)
	for {
		n, err := t.pty.Read(buf)
		if n > 0 {
			a.emit("terminal:data", map[string]string{"id": id, "data": base64.StdEncoding.EncodeToString(buf[:n])})
		}
		if err != nil {
			break // EIO once the process has exited
		}
	}
	code := 0
	if err := t.cmd.Wait(); err != nil {
		var exit *exec.ExitError
		code = -1
		if errors.As(err, &exit) {
			code = exit.ExitCode()
		}
	}
	t.closed.Store(true)
	t.pty.Close()
	a.dropTerminal(id)
	a.emit("terminal:exit", map[string]any{"id": id, "code": code})
}

func (a *App) dropTerminal(id string) {
	a.terms.mu.Lock()
	delete(a.terms.items, id)
	a.terms.mu.Unlock()
}

func (a *App) lookupTerminal(id string) (*terminal, error) {
	a.terms.mu.Lock()
	defer a.terms.mu.Unlock()
	t := a.terms.items[id]
	if t == nil || t.pty == nil {
		return nil, errors.New("终端已关闭")
	}
	return t, nil
}

// WriteTerminal sends keystrokes or pasted text to a terminal.
func (a *App) WriteTerminal(id, data string) error {
	t, err := a.lookupTerminal(id)
	if err != nil {
		return err
	}
	t.writeMu.Lock()
	defer t.writeMu.Unlock()
	if t.closed.Load() {
		return errors.New("终端已关闭")
	}
	_, err = t.pty.WriteString(data)
	return err
}

// ResizeTerminal follows the window; the docker CLI passes the new size into the sandbox.
func (a *App) ResizeTerminal(id string, cols, rows int) error {
	t, err := a.lookupTerminal(id)
	if err != nil {
		return err
	}
	return pty.Setsize(t.pty, terminalSize(cols, rows))
}

// CloseTerminal 同时结束容器内该会话的进程和宿主 Docker 客户端。
func (a *App) CloseTerminal(id string) error {
	t, err := a.lookupTerminal(id)
	if err != nil {
		return nil // already gone
	}
	a.closeTerminal(t)
	return nil
}

func (a *App) closeTerminal(t *terminal) {
	t.closeOnce.Do(func() {
		t.closed.Store(true)
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
		defer cancel()
		_ = a.manager.EndProcesses(ctx, t.sandboxID, t.id)
		if t.cmd.Process != nil {
			_ = t.cmd.Process.Signal(syscall.SIGHUP)
			process := t.cmd.Process
			go func() {
				time.Sleep(time.Second)
				_ = process.Kill()
			}()
		}
	})
}

// CloseAllTerminals is called when the app quits.
func (a *App) CloseAllTerminals() {
	a.closeConversations()
	a.terms.mu.Lock()
	a.terms.closing = true
	open := make([]*terminal, 0, len(a.terms.items))
	for _, t := range a.terms.items {
		if t.pty != nil {
			open = append(open, t)
		}
	}
	a.terms.mu.Unlock()
	var closing sync.WaitGroup
	for _, t := range open {
		closing.Add(1)
		go func(t *terminal) { defer closing.Done(); a.closeTerminal(t) }(t)
	}
	closing.Wait()
}
