package service

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/shaoxier/AIofCage/internal/oauth"
	"github.com/shaoxier/AIofCage/internal/provider"
	"github.com/shaoxier/AIofCage/internal/runtime"
	"github.com/shaoxier/AIofCage/internal/sandbox"
	"github.com/shaoxier/AIofCage/internal/session"
)

type ConversationInfo struct {
	ID              string        `json:"id"`
	Agent           session.Agent `json:"agent"`
	ProviderName    string        `json:"provider_name"`
	ProviderID      string        `json:"provider_id"`
	Model           string        `json:"model"`
	Effort          string        `json:"effort"`
	ImageInput      bool          `json:"image_input"`
	Resumed         bool          `json:"resumed"`
	NativeSessionID string        `json:"native_session_id,omitempty"`
}

type ConversationOptions struct {
	Model  string `json:"model"`
	Effort string `json:"effort"`
}

type conversationSessions struct {
	mu        sync.Mutex
	items     map[string]*conversation
	closing   bool
	persistMu sync.Mutex
}

type conversation struct {
	id       string
	mu       sync.Mutex
	sandbox  sandbox.Sandbox
	provider provider.Provider
	agent    session.Agent
	model    string
	effort   string
	nativeID string
	busy     bool
	closed   bool
	stop     func()
	done     chan struct{}
	images   []string
}

func (a *App) StartConversation(id, sandboxID string, agent session.Agent) (ConversationInfo, error) {
	return a.startConversation(id, sandboxID, agent, nil)
}

func (a *App) startConversation(id, sandboxID string, agent session.Agent, recovery *conversationRecovery) (ConversationInfo, error) {
	if err := a.runtimeReady(); err != nil {
		return ConversationInfo{}, err
	}
	if !terminalIDPattern.MatchString(id) || !agent.Valid() {
		return ConversationInfo{}, errors.New("会话或智能体无效")
	}
	sbx, err := a.store.Sandbox(sandboxID)
	if err != nil {
		return ConversationInfo{}, err
	}
	if a.manager.Status(context.Background(), sandboxID).State != "running" {
		return ConversationInfo{}, errors.New("请先启动沙箱")
	}
	if a.ops.changingContainers(sbx.ID) != "" {
		return ConversationInfo{}, errors.New("沙箱正在更新，请完成后再打开会话")
	}
	bindings := sbx.Bindings
	if sbx.AppliedProviders != nil {
		bindings = sbx.AppliedBindings
	}
	var p provider.Provider
	for _, binding := range bindings {
		if binding.Agent == agent && binding.ProviderID != "" {
			if sbx.AppliedProviders != nil {
				p = sbx.AppliedProviders[binding.ProviderID]
			} else {
				p, err = a.store.Provider(binding.ProviderID)
			}
		}
	}
	if err != nil || p.ID == "" {
		return ConversationInfo{}, errors.New("请先为该智能体选择供应商，并重新启动沙箱应用配置")
	}
	if recovery != nil && p.ID != recovery.ProviderID {
		return ConversationInfo{}, errors.New("供应商配置已变化，不能继续原生会话，请新建会话")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := a.manager.Exec(ctx, sandboxID, false, nil, io.Discard, io.Discard,
		"test", "-f", "/opt/aiofcage/agent-turn.py"); err != nil {
		return ConversationInfo{}, errors.New("沙箱需要更新会话组件，请停止后重新启动沙箱")
	}
	a.conversations.mu.Lock()
	defer a.conversations.mu.Unlock()
	if a.conversations.closing {
		return ConversationInfo{}, errors.New("应用正在退出")
	}
	if a.conversations.items == nil {
		a.conversations.items = make(map[string]*conversation)
	}
	if a.conversations.items[id] != nil || len(a.conversations.items) >= maxTerminals {
		return ConversationInfo{}, errors.New("会话重复或数量达到上限，请先关闭部分标签")
	}
	nativeID, model, effort := "", "", ""
	if recovery != nil {
		nativeID, model, effort = recovery.NativeSessionID, recovery.Model, recovery.Effort
	}
	a.conversations.items[id] = &conversation{id: id, sandbox: sbx, provider: p, agent: agent, nativeID: nativeID, model: model, effort: effort}
	if model == "" {
		model = p.Model
	}
	return ConversationInfo{ID: id, Agent: agent, ProviderName: p.Name, ProviderID: p.ID, Model: model, Effort: effort,
		ImageInput: agent != session.DSH || p.ImageInput, Resumed: recovery != nil, NativeSessionID: nativeID}, nil
}

func (a *App) conversation(id string) (*conversation, error) {
	a.conversations.mu.Lock()
	defer a.conversations.mu.Unlock()
	c := a.conversations.items[id]
	if c == nil {
		return nil, errors.New("会话已关闭")
	}
	return c, nil
}

func validateConversationOptions(options ConversationOptions) error {
	if len(options.Model) > 200 || strings.TrimSpace(options.Model) == "" {
		return errors.New("请输入模型名称，最多 200 字节")
	}
	for _, r := range options.Model {
		if unicode.IsControl(r) {
			return errors.New("模型名称包含无效字符")
		}
	}
	switch options.Effort {
	case "", "low", "medium", "high":
		return nil
	default:
		return errors.New("不支持的思考强度")
	}
}

// SendConversationMessage 在启动成功后返回；输出通过事件发送，正文不进入终端或审计。
func (a *App) SendConversationMessage(id, text string, images []TerminalImage, options ConversationOptions) error {
	c, err := a.conversation(id)
	if err != nil {
		return err
	}
	if a.ops.changingContainers(c.sandbox.ID) != "" {
		return errors.New("沙箱正在更新，请稍后重试")
	}
	if err := validateConversationOptions(options); err != nil {
		return err
	}
	files, err := decodeTerminalImages(images)
	if err != nil {
		return err
	}
	var pending []string
	if len(files) > 0 {
		pending = []string{"/pending.png"}
	}
	if _, err := terminalMessage(c.agent, text, pending); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || c.busy {
		return errors.New("会话已关闭或仍在处理上一条任务")
	}
	if len(files) > 0 && len(c.images) >= 32 {
		return errors.New("当前会话的图片附件已达上限，请新建会话")
	}
	if c.nativeID != "" && (c.model != options.Model || c.effort != options.Effort) {
		return errors.New("请新建会话后更改模型或思考强度，原会话仍保留")
	}
	if len(files) > 0 && c.agent == session.DSH && !c.provider.ImageInput {
		return errors.New("当前供应商未声明图片能力，请在供应商设置中启用并重启沙箱")
	}
	batchID := rand.Text()
	ctx, cancel := context.WithCancel(context.Background())
	var paths []string
	cleanup := func() {
		cancel()
		if len(paths) > 0 {
			cleanupContext, stop := context.WithTimeout(context.Background(), 5*time.Second)
			defer stop()
			_ = a.manager.RemoveTerminalImages(cleanupContext, c.sandbox.ID, batchID)
		}
	}
	if len(files) > 0 {
		uploadContext, stop := context.WithTimeout(ctx, 30*time.Second)
		paths, err = a.manager.WriteTerminalImages(uploadContext, c.sandbox.ID, batchID, files)
		stop()
		if err != nil {
			cleanup()
			return err
		}
	}
	if strings.TrimSpace(text) == "" {
		text = "请查看这些图片。"
	}
	payload, err := json.Marshal(map[string]any{"agent": c.agent, "text": text, "images": paths,
		"model": options.Model, "effort": options.Effort, "resume_id": c.nativeID,
		"dsh_patch": runtime.DSHConversationPatch(c.provider, options.Model, options.Effort)})
	if err != nil {
		cleanup()
		return err
	}
	// 每轮独立标记，停止旧任务时不会误伤刚开始的下一轮。
	marker := id + "-" + batchID
	cmd := a.manager.ConversationCommand(ctx, c.sandbox, marker)
	cmd.Stdin = bytes.NewReader(payload)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cleanup()
		return err
	}
	// 原生 CLI 诊断只保留有限尾部，不写入审计或日志。
	diagnostics := &tailBuffer{limit: 2048}
	cmd.Stderr = diagnostics
	if err := cmd.Start(); err != nil {
		cleanup()
		return errors.New("无法启动智能体，请检查沙箱状态")
	}
	var stopOnce sync.Once
	stopTurn := func() {
		stopOnce.Do(func() {
			killCtx, killCancel := context.WithTimeout(context.Background(), 4*time.Second)
			defer killCancel()
			_ = a.manager.Exec(killCtx, c.sandbox.ID, false, nil, io.Discard, io.Discard, "python3", "-c", endTerminalProcesses, marker)
			cancel()
		})
	}
	done := make(chan struct{})
	c.model, c.effort, c.busy, c.stop, c.done = options.Model, options.Effort, true, stopTurn, done
	if len(paths) > 0 {
		c.images = append(c.images, batchID)
	}
	a.emit("conversation:event", map[string]any{"id": id, "type": "user", "text": text, "images": len(images)})
	go func() {
		defer close(done)
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 32<<10), 8<<20)
		for scanner.Scan() {
			var event struct {
				Type string `json:"type"`
				Text string `json:"text"`
			}
			if json.Unmarshal(scanner.Bytes(), &event) != nil {
				continue
			}
			if event.Type == "session" {
				a.noteNativeSession(c, event.Text)
				continue
			}
			switch event.Type {
			case "text", "delta", "thinking", "tool", "final", "error":
				a.emit("conversation:event", map[string]string{"id": id, "type": event.Type, "text": event.Text})
			}
		}
		if scanner.Err() != nil {
			stopTurn()
		}
		runErr := cmd.Wait()
		stopped := ctx.Err() != nil
		cancel()
		if scanner.Err() != nil {
			a.emit("conversation:event", map[string]string{"id": id, "type": "error", "text": "智能体输出过大或中断，请重试"})
		} else if runErr != nil && !stopped {
			detail := a.redactConversationDiagnostic(ctx, c, string(diagnostics.data))
			a.emit("conversation:event", map[string]string{"id": id, "type": "error", "text": classifyConversationFailure(detail)})
		}
		state := "done"
		if stopped {
			state = "stopped"
		}
		c.mu.Lock()
		c.busy, c.stop = false, nil
		a.emit("conversation:event", map[string]string{"id": id, "type": state})
		c.mu.Unlock()
	}()
	return nil
}

type tailBuffer struct {
	data  []byte
	limit int
}

func (a *App) redactConversationDiagnostic(ctx context.Context, c *conversation, detail string) string {
	readCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	secrets, _ := a.realSecrets(readCtx, c.sandbox)
	for _, id := range c.sandbox.MCP {
		service, err := a.store.MCPService(id)
		if err != nil || !service.IsOAuth() {
			continue
		}
		raw, err := a.vault.Read(readCtx, service.KeyRef)
		if err != nil {
			continue
		}
		creds, err := oauth.Parse(raw)
		clear(raw)
		if err == nil {
			secrets = append(secrets, creds.AccessToken, creds.RefreshToken)
		}
	}
	for _, secret := range secrets {
		if len(secret) >= 8 {
			detail = strings.ReplaceAll(detail, secret, "[已脱敏]")
		}
	}
	return strings.TrimSpace(detail)
}

func (b *tailBuffer) Write(data []byte) (int, error) {
	n := len(data)
	b.data = append(b.data, data...)
	if len(b.data) > b.limit {
		b.data = b.data[len(b.data)-b.limit:]
	}
	return n, nil
}

func (a *App) StopConversation(id string) error {
	c, err := a.conversation(id)
	if err != nil {
		return nil
	}
	c.mu.Lock()
	stop := c.stop
	c.mu.Unlock()
	if stop != nil {
		stop()
	}
	return nil
}

func (a *App) CloseConversation(id string) error {
	return a.closeConversation(id, true)
}

func (a *App) closeConversation(id string, forget bool) error {
	c, err := a.conversation(id)
	if err != nil {
		if forget {
			_ = a.removeConversationRecovery(id)
		}
		return nil
	}
	c.mu.Lock()
	c.closed = true
	stop, done, batches := c.stop, c.done, c.images
	c.mu.Unlock()
	if stop != nil {
		stop()
	}
	if done != nil {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
	}
	// 附件在会话期间供原生历史引用，关闭标签时清理。
	for _, batch := range batches {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = a.manager.RemoveTerminalImages(ctx, c.sandbox.ID, batch)
		cancel()
	}
	a.conversations.mu.Lock()
	delete(a.conversations.items, id)
	a.conversations.mu.Unlock()
	if forget {
		_ = a.removeConversationRecovery(id)
	}
	return nil
}

func (a *App) closeConversations() {
	a.conversations.mu.Lock()
	a.conversations.closing = true
	var ids []string
	for id := range a.conversations.items {
		ids = append(ids, id)
	}
	a.conversations.mu.Unlock()
	var closing sync.WaitGroup
	for _, id := range ids {
		closing.Add(1)
		go func(id string) { defer closing.Done(); _ = a.closeConversation(id, false) }(id)
	}
	closing.Wait()
}

func (a *App) closeSandboxConversations(id string) {
	sbx, err := a.store.Sandbox(id)
	if err != nil {
		return
	}
	a.conversations.mu.Lock()
	var ids []string
	for key, c := range a.conversations.items {
		if c.sandbox.ID == sbx.ID {
			ids = append(ids, key)
		}
	}
	a.conversations.mu.Unlock()
	var closing sync.WaitGroup
	for _, key := range ids {
		closing.Add(1)
		go func(key string) {
			defer closing.Done()
			_ = a.closeConversation(key, false)
			a.emit("conversation:event", map[string]string{"id": key, "type": "closed"})
		}(key)
	}
	closing.Wait()
}

const conversationRecoverySetting = "conversation.recovery"

type conversationRecovery struct {
	ID              string        `json:"id"`
	SandboxID       string        `json:"sandbox_id"`
	Agent           session.Agent `json:"agent"`
	ProviderID      string        `json:"provider_id"`
	Model           string        `json:"model"`
	Effort          string        `json:"effort"`
	NativeSessionID string        `json:"native_session_id"`
	UpdatedAt       time.Time     `json:"updated_at"`
}

type ConversationRecoveryView struct {
	ID              string        `json:"id"`
	SandboxID       string        `json:"sandbox_id"`
	SandboxName     string        `json:"sandbox_name"`
	Agent           session.Agent `json:"agent"`
	ProviderName    string        `json:"provider_name"`
	Model           string        `json:"model"`
	Effort          string        `json:"effort"`
	NativeSessionID string        `json:"native_session_id"`
	UpdatedAt       time.Time     `json:"updated_at"`
	Ready           bool          `json:"ready"`
	Reason          string        `json:"reason,omitempty"`
}

// noteNativeSession records the agent's own session id for recovery. Claude Code repeats the id on
// every event; only a new one is written, and reported to the interface.
func (a *App) noteNativeSession(c *conversation, nativeID string) bool {
	c.mu.Lock()
	if c.closed || nativeID == "" || c.nativeID == nativeID {
		c.mu.Unlock()
		return false
	}
	c.nativeID = nativeID
	recovery := a.conversationRecoveryFrom(c)
	c.mu.Unlock()
	return a.saveConversationRecovery(recovery) == nil
}

func (a *App) conversationRecoveryFrom(c *conversation) conversationRecovery {
	return conversationRecovery{ID: cID(c), SandboxID: c.sandbox.ID, Agent: c.agent, ProviderID: c.provider.ID, Model: c.model, Effort: c.effort, NativeSessionID: c.nativeID, UpdatedAt: a.now().UTC()}
}

// cID is filled by the map key when recovery is saved; the conversation itself intentionally does
// not retain the desktop tab ID in its runtime state.
func cID(c *conversation) string {
	return c.id
}

func (a *App) recoveryRecords() (map[string]conversationRecovery, error) {
	raw := a.store.Setting(conversationRecoverySetting)
	if raw == "" {
		return map[string]conversationRecovery{}, nil
	}
	items := map[string]conversationRecovery{}
	if err := json.Unmarshal([]byte(raw), &items); err != nil {
		return nil, fmt.Errorf("会话恢复记录格式无效：%w", err)
	}
	return items, nil
}

func (a *App) saveConversationRecovery(item conversationRecovery) error {
	if item.ID == "" || item.NativeSessionID == "" {
		return nil
	}
	a.conversations.persistMu.Lock()
	defer a.conversations.persistMu.Unlock()
	items, err := a.recoveryRecords()
	if err != nil {
		return err
	}
	items[item.ID] = item
	return a.writeRecoveryRecords(items)
}

// writeRecoveryRecords saves the records and tells the interface to read the recoverable list
// again. The caller holds persistMu.
func (a *App) writeRecoveryRecords(items map[string]conversationRecovery) error {
	data, err := json.Marshal(items)
	if err != nil {
		return err
	}
	if err := a.store.SetSetting(conversationRecoverySetting, string(data)); err != nil {
		return err
	}
	a.emit("conversation:recovery", nil)
	return nil
}

func (a *App) removeConversationRecovery(id string) error {
	a.conversations.persistMu.Lock()
	defer a.conversations.persistMu.Unlock()
	items, err := a.recoveryRecords()
	if err != nil {
		return err
	}
	if _, ok := items[id]; !ok {
		return nil
	}
	delete(items, id)
	return a.writeRecoveryRecords(items)
}

func (a *App) removeSandboxConversationRecoveries(sandboxID string) error {
	a.conversations.persistMu.Lock()
	defer a.conversations.persistMu.Unlock()
	items, err := a.recoveryRecords()
	if err != nil {
		return err
	}
	changed := false
	for id, item := range items {
		if item.SandboxID == sandboxID {
			delete(items, id)
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return a.writeRecoveryRecords(items)
}

func (a *App) ListRecoverableConversations() ([]ConversationRecoveryView, error) {
	items, err := a.recoveryRecords()
	if err != nil {
		return nil, err
	}
	result := []ConversationRecoveryView{}
	var gone []string
	for _, item := range items {
		sbx, err := a.store.Sandbox(item.SandboxID)
		if err != nil {
			gone = append(gone, item.ID) // the sandbox was deleted; so is its session history
			continue
		}
		view := ConversationRecoveryView{ID: item.ID, SandboxID: item.SandboxID, SandboxName: sbx.Name, Agent: item.Agent, Model: item.Model, Effort: item.Effort, NativeSessionID: item.NativeSessionID, UpdatedAt: item.UpdatedAt, Ready: true}
		binding := sbx.ProviderFor(item.Agent)
		if sbx.AppliedProviders != nil {
			binding = ""
			for _, current := range sbx.AppliedBindings {
				if current.Agent == item.Agent {
					binding = current.ProviderID
					break
				}
			}
		}
		if binding == "" || binding != item.ProviderID {
			view.Ready = false
			view.Reason = "供应商配置已变化，请重新启动沙箱后新建会话"
		} else if p, providerErr := a.store.Provider(binding); providerErr == nil {
			view.ProviderName = p.Name
		} else {
			view.Ready = false
			view.Reason = "原供应商配置不可用"
		}
		result = append(result, view)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].UpdatedAt.After(result[j].UpdatedAt) })
	for _, id := range gone {
		_ = a.removeConversationRecovery(id)
	}
	return result, nil
}

func (a *App) RecoverConversation(id string) (ConversationInfo, error) {
	if err := a.runtimeReady(); err != nil {
		return ConversationInfo{}, err
	}
	items, err := a.recoveryRecords()
	if err != nil {
		return ConversationInfo{}, err
	}
	recovery, ok := items[id]
	if !ok || recovery.NativeSessionID == "" {
		return ConversationInfo{}, errors.New("找不到可恢复的会话")
	}
	if !recovery.Agent.Valid() {
		return ConversationInfo{}, errors.New("恢复会话的智能体无效")
	}
	// The record outlives the agent's own session files (a reset without keeping home, a manual
	// cleanup): check them first rather than fail at the first message.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	exists, err := a.nativeSessionExists(ctx, recovery.SandboxID, recovery.Agent, recovery.NativeSessionID)
	cancel()
	if err == nil && !exists {
		_ = a.removeConversationRecovery(id)
		return ConversationInfo{}, errors.New("沙箱里已经没有这个会话的记录（可能重置时清掉了），已移除这条恢复记录，请新建会话")
	}
	return a.startConversation(recovery.ID, recovery.SandboxID, recovery.Agent, &recovery)
}

// ConversationModels 只读取当前会话供应商的模型列表；密钥不会返回前端。
func (a *App) ConversationModels(id string) ([]string, error) {
	c, err := a.conversation(id)
	if err != nil {
		return nil, err
	}
	p := c.provider
	if err := p.KeyRefAllowed(); err != nil {
		return nil, err
	}
	host, base, err := p.Endpoint()
	if err != nil {
		return nil, err
	}
	path := base + "/models"
	if p.Protocol == provider.Anthropic {
		path = base + "/v1/models"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	key, err := a.vault.Read(ctx, p.KeyRef)
	if err != nil {
		return nil, errors.New("供应商密钥不可用")
	}
	defer func() { clear(key) }()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+host+path, nil)
	if err != nil {
		return nil, err
	}
	header, prefix := p.Injection()
	req.Header.Set(header, prefix+string(key))
	req.Header.Set("anthropic-version", "2023-06-01")
	response, err := a.client.Do(req)
	if err != nil {
		return nil, errors.New("读取模型列表失败，可手动填写模型名称")
	}
	defer response.Body.Close()
	var result struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if response.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(response.Body, 2<<20)).Decode(&result) != nil {
		return nil, errors.New("供应商未提供模型列表，可手动填写模型名称")
	}
	models := []string{p.Model}
	for _, model := range result.Data {
		if model.ID != "" && model.ID != p.Model && len(models) < 500 {
			models = append(models, model.ID)
		}
	}
	return models, nil
}
