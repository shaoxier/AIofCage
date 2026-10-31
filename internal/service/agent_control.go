package service

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/shaoxier/AIofCage/internal/control"
	"github.com/shaoxier/AIofCage/internal/runtime"
	"github.com/shaoxier/AIofCage/internal/sandbox"
	"github.com/shaoxier/AIofCage/internal/session"
	"github.com/shaoxier/AIofCage/internal/store"
	"github.com/shaoxier/AIofCage/internal/vault"
)

type AgentControlConfig struct {
	Command string   `json:"command"`
	Args    []string `json:"args"`
}

func (a *App) AgentControlConfig() (AgentControlConfig, error) {
	path, err := os.Executable()
	return AgentControlConfig{Command: path, Args: []string{"--mcp"}}, err
}

// RunAgentControl 由桌面二进制 --mcp 或 cagectl ai serve 显式启动；不常驻、不打开 TCP 端口。
func RunAgentControl(ctx context.Context, input io.Reader, output io.Writer) error {
	dir, err := store.DefaultDir()
	if err != nil {
		return err
	}
	st, err := store.Open(dir)
	if err != nil {
		return err
	}
	keys := vault.NewKeychain()
	manager, managerErr := runtime.NewManager(st, keys)
	a := NewApp(st, keys, manager, managerErr, nil)
	return control.Serve(ctx, input, output, newAgentController(ctx, a))
}

type controlTask struct {
	ID             string        `json:"id"`
	SandboxID      string        `json:"sandbox_id"`
	SandboxName    string        `json:"sandbox_name,omitempty"`
	Agent          session.Agent `json:"agent,omitempty"`
	ProviderID     string        `json:"provider_id,omitempty"`
	ProviderName   string        `json:"provider_name,omitempty"`
	Model          string        `json:"model,omitempty"`
	Effort         string        `json:"effort,omitempty"`
	State          string        `json:"state"`
	Text           string        `json:"text"`
	OutputSummary  string        `json:"output_summary,omitempty"`
	Tools          []string      `json:"tools"`
	Error          string        `json:"error,omitempty"`
	Truncated      bool          `json:"truncated"`
	StartedAt      time.Time     `json:"started_at,omitempty"`
	FinishedAt     time.Time     `json:"finished_at,omitempty"`
	DurationMS     int64         `json:"duration_ms,omitempty"`
	Requests       uint64        `json:"requests,omitempty"`
	Tokens         uint64        `json:"tokens,omitempty"`
	FallbackUsed   bool          `json:"fallback_used,omitempty"`
	FallbackReason string        `json:"fallback_reason,omitempty"`
	cancel         context.CancelFunc
	providerChain  []string
	modelOverride  bool
}

type agentController struct {
	app    *App
	ctx    context.Context
	cancel context.CancelFunc
	mu     sync.Mutex
	owned  map[string]bool
	tasks  map[string]*controlTask
	wg     sync.WaitGroup
	closed bool
}

func newAgentController(parent context.Context, a *App) *agentController {
	ctx, cancel := context.WithCancel(parent)
	return &agentController{app: a, ctx: ctx, cancel: cancel, owned: map[string]bool{}, tasks: map[string]*controlTask{}}
}

func (*agentController) Tools() []any {
	str := func(description string) any { return map[string]any{"type": "string", "description": description} }
	tool := func(name, description string, props map[string]any, required []string, readOnly bool) any {
		return map[string]any{"name": name, "description": description, "inputSchema": map[string]any{"type": "object", "properties": props, "required": required, "additionalProperties": false}, "annotations": map[string]bool{"readOnlyHint": readOnly, "destructiveHint": false, "openWorldHint": !readOnly}}
	}
	return []any{
		tool("list_providers", "列出供应商的 ID、名称、智能体与模型；不返回密钥、钥匙串引用或接口地址", map[string]any{}, []string{}, true),
		tool("list_sandboxes", "只列出本次控制会话创建的沙箱", map[string]any{}, []string{}, true),
		tool("create_sandbox", "在新项目中创建停止的沙箱，按供应商确定智能体。每会话最多8个；默认100次请求/100000 token，可调低。可选合成案例 sales/incident/release", map[string]any{"name": str("简短任务名，最多40字"), "provider_id": str("list_providers 返回的 ID"), "case_id": map[string]any{"type": "string", "enum": []string{"sales", "incident", "release"}}, "max_requests": map[string]any{"type": "integer", "minimum": 1, "maximum": 100}, "max_tokens": map[string]any{"type": "integer", "minimum": 1, "maximum": 100000}}, []string{"name", "provider_id"}, false),
		tool("run_task", "异步启动沙箱并执行官方 CLI 任务，返回 task_id；每会话最多2个并发、16个任务，不复用订阅或其他沙箱凭据。可选传入同一 Agent 的备用供应商顺序；仅认证、限流、超时或网络失败时切换。可用 get_task 查询，cancel_task 取消", map[string]any{"sandbox_id": str("本会话创建的沙箱 ID"), "text": str("任务内容，最多32000字节"), "model": str("可选，覆盖该供应商默认模型"), "effort": map[string]any{"type": "string", "enum": []string{"low", "medium", "high"}}, "timeout_seconds": map[string]any{"type": "integer", "minimum": 10, "maximum": 600}, "fallback_provider_ids": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "maxItems": 3}}, []string{"sandbox_id", "text"}, false),
		tool("get_task", "查询任务状态、最终文本、工具名称与错误；最多保留256KiB答案，超出时标记 truncated", map[string]any{"task_id": str("任务 ID")}, []string{"task_id"}, true), tool("compare_tasks", "比较本控制会话中多个已完成或进行中的任务，返回 Agent、供应商、模型、耗时、用量、状态和输出摘要；不跨控制会话", map[string]any{"task_ids": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "maxItems": 16}}, []string{}, true),
		tool("cancel_task", "取消本控制会话的任务", map[string]any{"task_id": str("任务 ID")}, []string{"task_id"}, false),
		tool("sandbox_usage", "查询本会话沙箱的实时模型用量", map[string]any{"sandbox_id": str("沙箱 ID")}, []string{"sandbox_id"}, true),
		tool("stop_sandbox", "停止本会话的空闲沙箱。先取消仍在运行的任务；项目保留", map[string]any{"sandbox_id": str("沙箱 ID")}, []string{"sandbox_id"}, false),
	}
}

func controlArgs(raw json.RawMessage, dst any) error {
	if len(raw) == 0 {
		raw = []byte("{}")
	}
	if len(bytes.TrimSpace(raw)) == 0 || bytes.TrimSpace(raw)[0] != '{' {
		return errors.New("参数须为 JSON 对象")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(dst) != nil || d.Decode(new(any)) != io.EOF {
		return errors.New("工具参数格式错误或包含未支持字段")
	}
	return nil
}

func (c *agentController) own(id string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.owned[id] {
		return errors.New("只能操作本控制会话创建的沙箱")
	}
	return nil
}

func (c *agentController) Call(ctx context.Context, name string, raw json.RawMessage) (any, error) {
	switch name {
	case "list_providers":
		if err := controlArgs(raw, &struct{}{}); err != nil {
			return nil, err
		}
		items, err := c.app.store.Providers()
		if err != nil {
			return nil, err
		}
		out := []map[string]string{}
		for _, p := range items {
			out = append(out, map[string]string{"id": p.ID, "name": p.Name, "agent": string(p.Agent), "model": p.Model})
		}
		return map[string]any{"providers": out}, nil
	case "list_sandboxes":
		if err := controlArgs(raw, &struct{}{}); err != nil {
			return nil, err
		}
		items, err := c.app.store.Sandboxes()
		if err != nil {
			return nil, err
		}
		out := []map[string]string{}
		for _, s := range items {
			if c.own(s.ID) == nil {
				out = append(out, map[string]string{"id": s.ID, "name": s.Name, "project_path": s.ProjectPath})
			}
		}
		return map[string]any{"sandboxes": out}, nil
	case "create_sandbox":
		return c.create(raw)
	case "run_task":
		return c.run(raw)
	case "get_task", "cancel_task":
		var args struct {
			ID string `json:"task_id"`
		}
		if err := controlArgs(raw, &args); err != nil {
			return nil, err
		}
		c.mu.Lock()
		task := c.tasks[args.ID]
		if task == nil {
			c.mu.Unlock()
			return nil, errors.New("本控制会话中没有该任务")
		}
		if name == "cancel_task" {
			task.cancel()
		}
		snapshot := *task
		snapshot.Tools = append([]string{}, task.Tools...)
		c.mu.Unlock()
		return snapshot, nil
	case "compare_tasks":
		var args struct {
			IDs []string `json:"task_ids"`
		}
		if err := controlArgs(raw, &args); err != nil {
			return nil, err
		}
		if len(args.IDs) > 16 {
			return nil, errors.New("一次最多比较16个任务")
		}
		c.mu.Lock()
		seen := map[string]bool{}
		var selected []*controlTask
		if len(args.IDs) == 0 {
			for _, task := range c.tasks {
				selected = append(selected, task)
			}
		} else {
			for _, id := range args.IDs {
				if seen[id] {
					continue
				}
				seen[id] = true
				task := c.tasks[id]
				if task == nil {
					c.mu.Unlock()
					return nil, errors.New("本控制会话中没有该任务")
				}
				selected = append(selected, task)
			}
		}
		rows := make([]controlTask, 0, len(selected))
		for _, task := range selected {
			row := *task
			row.Text = ""
			row.Tools = append([]string(nil), task.Tools...)
			rows = append(rows, row)
		}
		c.mu.Unlock()
		sort.SliceStable(rows, func(i, j int) bool { return rows[i].StartedAt.Before(rows[j].StartedAt) })
		return map[string]any{"tasks": rows}, nil

	case "sandbox_usage", "stop_sandbox":
		var args struct {
			ID string `json:"sandbox_id"`
		}
		if err := controlArgs(raw, &args); err != nil {
			return nil, err
		}
		if err := c.own(args.ID); err != nil {
			return nil, err
		}
		if err := c.app.runtimeReady(); err != nil {
			return nil, err
		}
		if name == "sandbox_usage" {
			return c.app.manager.Usage(ctx, args.ID)
		}
		c.mu.Lock()
		active := c.activeIn(args.ID)
		c.mu.Unlock()
		if active {
			return nil, errors.New("请先取消这个沙箱的运行任务")
		}
		return map[string]bool{"stopped": true}, c.app.manager.Stop(ctx, args.ID)
	}
	return nil, errors.New("不支持该工具")
}

func (c *agentController) create(raw json.RawMessage) (any, error) {
	var args struct {
		Name       string  `json:"name"`
		ProviderID string  `json:"provider_id"`
		CaseID     string  `json:"case_id"`
		Requests   *uint64 `json:"max_requests"`
		Tokens     *uint64 `json:"max_tokens"`
	}
	if err := controlArgs(raw, &args); err != nil {
		return nil, err
	}
	if strings.TrimSpace(args.Name) == "" || len([]rune(args.Name)) > 40 {
		return nil, errors.New("任务名须为1至40字")
	}
	limits := sandbox.Limits{MaxRequests: 100, MaxTokens: 100000}
	if args.Requests != nil {
		limits.MaxRequests = *args.Requests
	}
	if args.Tokens != nil {
		limits.MaxTokens = *args.Tokens
	}
	if limits.MaxRequests < 1 || limits.MaxRequests > 100 || limits.MaxTokens < 1 || limits.MaxTokens > 100000 {
		return nil, errors.New("控制会话额度须为1至100次请求、1至100000 token")
	}
	p, err := c.app.store.Provider(args.ProviderID)
	if err != nil {
		return nil, err
	}
	if err := c.app.runtimeReady(); err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || len(c.owned) >= 8 {
		return nil, errors.New("控制会话已关闭或沙箱达到8个上限")
	}
	var id, path string
	if args.CaseID != "" {
		result, err := c.app.CreateDemoSandbox(CreateDemoRequest{CaseID: args.CaseID, Agent: p.Agent, ProviderID: p.ID})
		if err != nil {
			return nil, err
		}
		id, path = result.ID, result.ProjectPath
	} else {
		name := "ai-" + strings.TrimSpace(args.Name) + "-" + rand.Text()[:6]
		existing, err := c.app.store.Sandboxes()
		if err != nil {
			return nil, err
		}
		path, err = prepareProject(ProjectNew, "", name, existing)
		if err != nil {
			return nil, err
		}
		s, err := c.app.manager.Create(c.ctx, runtime.CreateRequest{Name: name, ProjectPath: path, Bindings: []sandbox.Binding{{Agent: p.Agent, ProviderID: p.ID}}, Limits: limits})
		if err != nil {
			_ = os.RemoveAll(path)
			return nil, err
		}
		id = s.ID
	}
	c.owned[id] = true
	if err := c.app.store.UpdateSandbox(id, func(s *sandbox.Sandbox) error { s.Limits = limits; return nil }); err != nil {
		return nil, err
	}
	return map[string]any{"sandbox_id": id, "project_path": path, "agent": p.Agent, "provider_id": p.ID, "limits": limits, "state": "not_created"}, nil
}

// activeIn 仅在持锁时读取；单个沙箱一次只运行一个任务。
func (c *agentController) activeIn(id string) bool {
	for _, task := range c.tasks {
		if task.SandboxID == id && (task.State == "starting" || task.State == "running") {
			return true
		}
	}
	return false
}

func (c *agentController) run(raw json.RawMessage) (any, error) {
	var args struct {
		ID          string   `json:"sandbox_id"`
		Text        string   `json:"text"`
		Model       string   `json:"model"`
		Effort      string   `json:"effort"`
		Timeout     int      `json:"timeout_seconds"`
		FallbackIDs []string `json:"fallback_provider_ids"`
	}
	if err := controlArgs(raw, &args); err != nil {
		return nil, err
	}
	if err := c.own(args.ID); err != nil {
		return nil, err
	}
	if strings.TrimSpace(args.Text) == "" || len(args.Text) > 32000 {
		return nil, errors.New("任务内容须为1至32000字节")
	}
	if args.Timeout == 0 {
		args.Timeout = 300
	}
	if args.Timeout < 10 || args.Timeout > 600 {
		return nil, errors.New("超时须为10至600秒")
	}
	s, err := c.app.store.Sandbox(args.ID)
	if err != nil {
		return nil, err
	}
	if len(s.Bindings) != 1 {
		return nil, errors.New("控制沙箱需要唯一的智能体绑定")
	}
	p, err := c.app.store.Provider(s.Bindings[0].ProviderID)
	if err != nil {
		return nil, err
	}
	providerChain := []string{p.ID}
	seenProviders := map[string]bool{p.ID: true}
	if len(args.FallbackIDs) > 3 {
		return nil, errors.New("最多配置3个备用供应商")
	}
	for _, providerID := range args.FallbackIDs {
		if providerID == "" || seenProviders[providerID] {
			continue
		}
		fallback, fallbackErr := c.app.store.Provider(providerID)
		if fallbackErr != nil {
			return nil, fallbackErr
		}
		if fallback.Agent != p.Agent {
			return nil, errors.New("备用供应商必须属于同一个 Agent")
		}
		seenProviders[providerID] = true
		providerChain = append(providerChain, providerID)
	}
	modelOverride := args.Model != ""
	if args.Model == "" {
		args.Model = p.Model
	}
	if err := validateConversationOptions(ConversationOptions{Model: args.Model, Effort: args.Effort}); err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	active := 0
	for _, task := range c.tasks {
		if task.State == "starting" || task.State == "running" {
			active++
		}
	}
	if c.closed || active >= 2 || len(c.tasks) >= 16 || c.activeIn(s.ID) {
		return nil, errors.New("每会话最多2个并发、16个任务，每个沙箱仅一个运行任务")
	}
	ctx, cancel := context.WithTimeout(c.ctx, time.Duration(args.Timeout)*time.Second)
	task := &controlTask{ID: "job-" + rand.Text(), SandboxID: s.ID, SandboxName: s.Name, Agent: p.Agent, ProviderID: p.ID, ProviderName: p.Name, Model: args.Model, Effort: args.Effort,
		State: "starting", Tools: []string{}, StartedAt: time.Now().UTC(), cancel: cancel, providerChain: providerChain, modelOverride: modelOverride}
	c.tasks[task.ID] = task
	c.wg.Add(1)
	go c.execute(ctx, task, s, args.Text, args.Model, args.Effort)
	return map[string]string{"task_id": task.ID, "state": task.State}, nil
}

func (c *agentController) execute(ctx context.Context, task *controlTask, s sandbox.Sandbox, text, model, effort string) {
	defer c.wg.Done()
	defer task.cancel()
	defer func() {
		c.mu.Lock()
		if task.FinishedAt.IsZero() {
			task.FinishedAt = time.Now().UTC()
		}
		if !task.StartedAt.IsZero() {
			task.DurationMS = task.FinishedAt.Sub(task.StartedAt).Milliseconds()
		}
		if task.OutputSummary == "" {
			task.OutputSummary = outputSummary(task.Text)
		}
		c.mu.Unlock()
	}()
	if len(task.providerChain) == 0 {
		task.providerChain = []string{s.ProviderFor(task.Agent)}
	}
	for index, providerID := range task.providerChain {
		if index > 0 {
			if err := c.app.manager.Stop(ctx, s.ID); err != nil {
				c.failTask(task, "切换备用供应商前停止沙箱失败："+err.Error(), ctx)
				return
			}
			if err := c.app.manager.Bind(ctx, s.ID, task.Agent, providerID); err != nil {
				c.failTask(task, "切换备用供应商失败："+err.Error(), ctx)
				return
			}
			c.mu.Lock()
			task.FallbackUsed = true
			c.mu.Unlock()
		}
		attemptModel := model
		if !task.modelOverride {
			if fallback, err := c.app.store.Provider(providerID); err == nil {
				attemptModel = fallback.Model
			}
		}
		result := c.executeAttempt(ctx, task, s, text, attemptModel, effort)
		if result.err == nil {
			c.mu.Lock()
			task.State = "done"
			c.mu.Unlock()
			return
		}
		if ctx.Err() != nil || !result.retryable || index == len(task.providerChain)-1 {
			c.failTask(task, result.reason, ctx)
			return
		}
		c.mu.Lock()
		task.FallbackReason = result.reason
		task.Text = ""
		task.Tools = nil
		task.Error = ""
		c.mu.Unlock()
	}
}

type taskAttemptResult struct {
	err       error
	retryable bool
	reason    string
}

func (c *agentController) executeAttempt(ctx context.Context, task *controlTask, s sandbox.Sandbox, text, model, effort string) taskAttemptResult {
	if err := c.app.manager.Start(ctx, s.ID); err != nil {
		return taskAttemptResult{err: err, retryable: retryableProviderFailure(err.Error()), reason: "启动沙箱失败：" + err.Error()}
	}
	if ctx.Err() != nil {
		return taskAttemptResult{err: ctx.Err(), reason: "任务已取消"}
	}
	initialUsage, _ := c.app.manager.Usage(context.Background(), s.ID)
	current, err := c.app.store.Sandbox(s.ID)
	if err != nil {
		return taskAttemptResult{err: err, reason: "读取沙箱失败"}
	}
	if len(current.AppliedBindings) != 1 {
		return taskAttemptResult{err: errors.New("运行配置变化"), reason: "沙箱运行配置已变化，请重新运行任务"}
	}
	binding := current.AppliedBindings[0]
	p := current.AppliedProviders[binding.ProviderID]
	if p.ID == "" {
		return taskAttemptResult{err: errors.New("供应商不可用"), reason: "沙箱运行供应商不可用"}
	}
	c.mu.Lock()
	task.ProviderID, task.ProviderName, task.Model = p.ID, p.Name, model
	task.State = "running"
	c.mu.Unlock()
	payload, _ := json.Marshal(map[string]any{"agent": binding.Agent, "text": text, "images": []string{}, "model": model, "effort": effort, "dsh_patch": runtime.DSHConversationPatch(p, model, effort)})
	marker := "t-control-" + task.ID
	cmd := c.app.manager.ConversationCommand(ctx, current, marker)
	cmd.Stdin = bytes.NewReader(payload)
	diagnostics := &tailBuffer{limit: 2048}
	cmd.Stderr = diagnostics
	out, err := cmd.StdoutPipe()
	if err != nil {
		return taskAttemptResult{err: err, reason: "无法读取智能体输出"}
	}
	if err := cmd.Start(); err != nil {
		return taskAttemptResult{err: err, retryable: retryableProviderFailure(err.Error()), reason: "无法启动智能体"}
	}
	finished := make(chan struct{})
	killerDone := make(chan struct{})
	go func() {
		defer close(killerDone)
		select {
		case <-finished:
			return
		case <-ctx.Done():
		}
		killCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = c.app.manager.Exec(killCtx, s.ID, false, nil, io.Discard, io.Discard, "python3", "-c", endTerminalProcesses, marker)
	}()
	scanner := bufio.NewScanner(out)
	var reportedError string
	scanner.Buffer(make([]byte, 32768), 8<<20)
	for scanner.Scan() {
		var event struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(scanner.Bytes(), &event) != nil {
			continue
		}
		c.mu.Lock()
		switch event.Type {
		case "text", "delta", "final":
			if event.Type == "final" {
				task.Text = ""
			}
			room := (256 << 10) - len(task.Text)
			if len(event.Text) > room {
				task.Truncated = true
				event.Text = event.Text[:max(room, 0)]
			}
			task.Text += event.Text
		case "tool":
			if len(task.Tools) < 100 {
				task.Tools = append(task.Tools, event.Text)
			}
		case "error":
			task.Error = "智能体报告任务失败，请在沙箱中检查模型与供应商"
			reportedError = event.Text
		}
		c.mu.Unlock()
	}
	if scanner.Err() != nil {
		task.cancel()
	}
	runErr := cmd.Wait()
	close(finished)
	<-killerDone
	finalUsage, usageErr := c.app.manager.Usage(context.Background(), s.ID)
	if usageErr == nil {
		c.mu.Lock()
		task.Requests += deltaCounter(finalUsage.QuotaRequests, initialUsage.QuotaRequests)
		task.Tokens += deltaCounter(finalUsage.QuotaTokens, initialUsage.QuotaTokens)
		c.mu.Unlock()
	}
	if ctx.Err() != nil {
		return taskAttemptResult{err: ctx.Err(), reason: "任务已取消"}
	}
	if scanner.Err() != nil || runErr != nil || task.Error != "" {
		detail := strings.TrimSpace(string(diagnostics.data) + "\n" + reportedError)
		reason := conversationFailureHint(detail)
		if task.Error != "" && detail == "" {
			reason = task.Error
		}
		return taskAttemptResult{err: errors.New(reason), retryable: retryableProviderFailure(detail), reason: reason}
	}
	return taskAttemptResult{}
}

func (c *agentController) failTask(task *controlTask, message string, ctx context.Context) {
	c.mu.Lock()
	defer c.mu.Unlock()
	task.State = "failed"
	task.Error = message
	if ctx.Err() != nil {
		task.State = "cancelled"
	}
}

func retryableProviderFailure(detail string) bool {
	detail = strings.ToLower(detail)
	for _, marker := range []string{"401", "403", "unauthorized", "forbidden", "429", "rate limit", "timeout", "deadline", "connection refused", "no route", "network is unreachable", "dns"} {
		if strings.Contains(detail, marker) {
			return true
		}
	}
	return false
}

func deltaCounter(current, previous uint64) uint64 {
	if current < previous {
		return current
	}
	return current - previous
}

func outputSummary(text string) string {
	text = strings.Join(strings.Fields(text), " ")
	runes := []rune(text)
	if len(runes) > 500 {
		return string(runes[:500]) + "…"
	}
	return text
}

func (c *agentController) Close() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	c.cancel()
	ids := []string{}
	for id := range c.owned {
		ids = append(ids, id)
	}
	c.mu.Unlock()
	c.wg.Wait()
	if c.app.manager != nil {
		for _, id := range ids {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			_ = c.app.manager.Stop(ctx, id)
			cancel()
		}
	}
}
