package service

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/shaoxier/AIofCage/internal/audit"
	"github.com/shaoxier/AIofCage/internal/events"
	"github.com/shaoxier/AIofCage/internal/runtime"
	"github.com/shaoxier/AIofCage/internal/sandbox"
)

const (
	monitorInterval = 5 * time.Second
	ringCapacity    = 500
	recentLimit     = 300
)

// monitor follows every running sandbox: it subscribes to the gatekeeper event stream, appends
// events to a per-sandbox JSONL audit log, and polls usage and container stats for the UI.
type monitor struct {
	health  map[string]runtime.SandboxHealth
	history map[string][]runtime.ContainerStats
	mu      sync.Mutex
	subs    map[string]context.CancelFunc
	rings   map[string][]audit.Event
	lastID  map[string]string
	live    map[string]runtime.GateUsage
	stats   map[string]runtime.ContainerStats
	// oauthAlerted remembers services whose expired login was already reported.
	oauthAlerted map[string]bool
	// oauthPending remembers services whose renewed token did not reach every sandbox yet.
	oauthPending map[string]bool
	// stale counts retries for sandboxes whose gatekeeper may not match their stored settings.
	stale map[string]int
}

func newMonitor() *monitor {
	return &monitor{health: map[string]runtime.SandboxHealth{}, history: map[string][]runtime.ContainerStats{}, subs: map[string]context.CancelFunc{}, rings: map[string][]audit.Event{},
		lastID: map[string]string{}, live: map[string]runtime.GateUsage{}, stats: map[string]runtime.ContainerStats{},
		oauthAlerted: map[string]bool{}, oauthPending: map[string]bool{}, stale: map[string]int{}}
}

// StartMonitor runs until ctx ends. The desktop app calls it once the window exists.
func (a *App) StartMonitor(ctx context.Context) {
	if a.manager == nil {
		return
	}
	go func() {
		ticker := time.NewTicker(monitorInterval)
		defer ticker.Stop()
		for {
			a.tick(ctx)
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	go func() {
		ticker := time.NewTicker(oauthCheckInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				a.retryStale(ctx, a.manager.Refresh)
				a.refreshOAuth(ctx)
			}
		}
	}()
}

func (a *App) tick(ctx context.Context) {
	sandboxes, err := a.store.Sandboxes()
	if err != nil {
		return
	}
	ids := make([]string, 0, len(sandboxes))
	for _, s := range sandboxes {
		ids = append(ids, s.ID)
	}
	health := a.manager.SampleHealth(ctx, ids)
	stats := map[string]runtime.ContainerStats{}
	for _, s := range sandboxes {
		if health[s.ID].GateRunning() {
			a.subscribe(ctx, s)
			if usage, err := a.manager.Usage(ctx, s.ID); err == nil {
				_ = a.manager.RecordUsage(s.ID, usage)
				a.monitor.mu.Lock()
				a.monitor.live[s.ID] = usage
				a.monitor.mu.Unlock()
				a.emit("sandbox:usage", map[string]any{"id": s.ID, "usage": usage})
				h := health[s.ID]
				h.GateAPI = true
				health[s.ID] = h
			} else {
				h := health[s.ID]
				h.GateError = "代理管理接口暂时无响应"
				health[s.ID] = h
			}
		} else {
			a.unsubscribe(s.ID)
		}
	}
	a.monitor.mu.Lock()
	a.monitor.health = health
	for id, h := range health {
		stats[id] = h.Stats
		points := append(a.monitor.history[id], h.Stats)
		if len(points) > 120 {
			points = append([]runtime.ContainerStats{}, points[len(points)-120:]...)
		}
		a.monitor.history[id] = points
	}
	for id := range a.monitor.history {
		if _, ok := health[id]; !ok {
			delete(a.monitor.history, id)
		}
	}
	a.monitor.stats = stats
	for id := range a.monitor.live {
		if h, ok := health[id]; !ok || !h.GateRunning() {
			delete(a.monitor.live, id)
		}
	}
	a.monitor.mu.Unlock()
	a.emit("sandbox:stats", stats)
}

func (a *App) subscribe(ctx context.Context, s sandbox.Sandbox) {
	a.monitor.mu.Lock()
	defer a.monitor.mu.Unlock()
	if _, ok := a.monitor.subs[s.ID]; ok {
		return
	}
	subCtx, cancel := context.WithCancel(ctx)
	a.monitor.subs[s.ID] = cancel
	go a.follow(subCtx, s, a.manager)
}

func (a *App) unsubscribe(id string) {
	a.monitor.mu.Lock()
	defer a.monitor.mu.Unlock()
	if cancel, ok := a.monitor.subs[id]; ok {
		cancel()
		delete(a.monitor.subs, id)
	}
}

// follow 只从持久化成功的游标续传；跨运行仍保留旧游标，由门卫声明缺口。
func (a *App) follow(ctx context.Context, s sandbox.Sandbox, source events.Source) {
	interrupted := false
	for ctx.Err() == nil {
		if cursor, err := a.cursor(s.ID); err == nil {
			// 流返回（包括正常 EOF）都需要重连；只显示采集状态，不暴露错误正文。
			_ = source.Events(ctx, s.ID, cursor, func(event audit.Event) error {
				if err := a.record(s, event); err != nil {
					return err
				}
				if interrupted {
					a.emit("sandbox:alert", map[string]string{"id": s.ID, "message": "审计采集已恢复", "level": "info"})
					interrupted = false
				}
				return nil
			})
		}
		if ctx.Err() != nil {
			return
		}
		if !interrupted {
			// 不展示上游响应或错误正文；异常时不宣称日志完整。
			a.emit("sandbox:alert", map[string]string{"id": s.ID, "message": "审计采集中断，正在从已保存的位置重试。日志和用量明细可能不完整。"})
			interrupted = true
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
	}
}

func (a *App) cursor(id string) (string, error) {
	a.monitor.mu.Lock()
	cursor, known := a.monitor.lastID[id]
	a.monitor.mu.Unlock()
	if known {
		return cursor, nil
	}
	tail, err := a.loadTail(id, ringCapacity)
	if err != nil {
		return "", err
	}
	for _, event := range tail {
		if event.Action != audit.ActionGap {
			cursor = event.ID
		}
	}
	a.monitor.mu.Lock()
	a.monitor.lastID[id] = cursor
	a.monitor.mu.Unlock()
	return cursor, nil
}

func (a *App) record(s sandbox.Sandbox, event audit.Event) error {
	if event.SessionID != "" && event.SessionID != s.ID {
		return errors.New("审计事件与沙箱不匹配")
	}
	if err := a.appendAudit(s.ID, event); err != nil {
		return err
	}
	a.monitor.mu.Lock()
	if event.Action != audit.ActionGap {
		a.monitor.lastID[s.ID] = event.ID
	}
	ring := append(a.monitor.rings[s.ID], event)
	if len(ring) > ringCapacity {
		ring = append([]audit.Event(nil), ring[len(ring)-ringCapacity:]...)
	}
	a.monitor.rings[s.ID] = ring
	a.monitor.mu.Unlock()

	a.emit("sandbox:event", EventView{Event: event, SandboxName: s.Name})
	return nil
}

func (a *App) auditPath(id string) string {
	return filepath.Join(a.store.Dir(), "audit", id+".jsonl")
}

func (a *App) appendAudit(id string, event audit.Event) error {
	if err := os.MkdirAll(filepath.Dir(a.auditPath(id)), 0o700); err != nil {
		return err
	}
	file, err := os.OpenFile(a.auditPath(id), os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	data, err := json.Marshal(event)
	if err != nil {
		return err
	}
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if info.Size() > 0 {
		var last [1]byte
		if _, err := file.ReadAt(last[:], info.Size()-1); err != nil {
			return err
		}
		if last[0] != '\n' {
			// 保留崩溃留下的残行供查询标记缺口，避免吞掉下一条完整记录。
			data = append([]byte{'\n'}, data...)
		}
	}
	_, err = file.Write(append(data, '\n'))
	if err != nil {
		return err
	}
	return file.Sync()
}

// loadTail reads the last n events of a sandbox's audit log.
func (a *App) loadTail(id string, n int) ([]audit.Event, error) {
	file, err := os.Open(a.auditPath(id))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()
	const window = 512 << 10
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if info.Size() > window {
		if _, err := file.Seek(info.Size()-window, io.SeekStart); err != nil {
			return nil, err
		}
	}
	var events []audit.Event
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	for scanner.Scan() {
		var event audit.Event
		if json.Unmarshal(bytes.TrimSpace(scanner.Bytes()), &event) == nil && event.Action != "" {
			events = append(events, event)
		}
	}
	if len(events) > n {
		events = events[len(events)-n:]
	}
	return events, scanner.Err()
}

// EventView is a gatekeeper event with the sandbox's display name.
type EventView struct {
	audit.Event
	SandboxName string `json:"sandbox_name"`
}

// RecentEvents returns recent events for one sandbox, or for all sandboxes when id is empty.
func (a *App) RecentEvents(id string) ([]EventView, error) {
	sandboxes, err := a.store.Sandboxes()
	if err != nil {
		return nil, err
	}
	var result []EventView
	for _, s := range sandboxes {
		if id != "" && s.ID != id {
			continue
		}
		a.monitor.mu.Lock()
		ring := append([]audit.Event(nil), a.monitor.rings[s.ID]...)
		a.monitor.mu.Unlock()
		if len(ring) == 0 {
			ring, err = a.loadTail(s.ID, ringCapacity)
			if err != nil {
				return nil, err
			}
			a.monitor.mu.Lock()
			if len(a.monitor.rings[s.ID]) == 0 {
				a.monitor.rings[s.ID] = ring
			}
			a.monitor.mu.Unlock()
		}
		for _, event := range ring {
			result = append(result, EventView{Event: event, SandboxName: s.Name})
		}
	}
	sort.SliceStable(result, func(i, j int) bool { return result[i].Time.Before(result[j].Time) })
	if len(result) > recentLimit {
		result = result[len(result)-recentLimit:]
	}
	return result, nil
}

type SandboxUsage struct {
	ID          string                  `json:"id"`
	Name        string                  `json:"name"`
	Requests    uint64                  `json:"requests"`
	Tokens      uint64                  `json:"tokens"`
	Denied      uint64                  `json:"denied"`
	MaxRequests uint64                  `json:"max_requests"`
	MaxTokens   uint64                  `json:"max_tokens"`
	Live        bool                    `json:"live"`
	Stats       *runtime.ContainerStats `json:"stats,omitempty"`
}

type ProviderUsageView struct {
	ID           string   `json:"id"`
	Requests     uint64   `json:"requests"`
	InputTokens  uint64   `json:"input_tokens"`
	OutputTokens uint64   `json:"output_tokens"`
	Sandboxes    []string `json:"sandboxes"`
}

type UsageSummary struct {
	Sandboxes []SandboxUsage               `json:"sandboxes"`
	Providers map[string]ProviderUsageView `json:"providers"`
}

// UsageSummary combines stored cumulative usage with the latest live readings.
func (a *App) UsageSummary() (UsageSummary, error) {
	sandboxes, err := a.store.Sandboxes()
	if err != nil {
		return UsageSummary{}, err
	}
	summary := UsageSummary{Sandboxes: []SandboxUsage{}, Providers: map[string]ProviderUsageView{}}
	a.monitor.mu.Lock()
	defer a.monitor.mu.Unlock()
	for _, s := range sandboxes {
		row := SandboxUsage{ID: s.ID, Name: s.Name, Requests: s.Usage.Requests, Tokens: s.Usage.Tokens,
			MaxRequests: s.Limits.MaxRequests, MaxTokens: s.Limits.MaxTokens}
		if live, ok := a.monitor.live[s.ID]; ok {
			row.Live, row.Denied = true, live.Denied
			row.Requests, row.Tokens = max(row.Requests, live.QuotaRequests), max(row.Tokens, live.QuotaTokens)
		}
		if stats, ok := a.monitor.stats[s.ID]; ok {
			row.Stats = &stats
		}
		summary.Sandboxes = append(summary.Sandboxes, row)
		for id, usage := range s.ProviderUsage {
			view := summary.Providers[id]
			view.ID = id
			view.Requests += usage.Requests
			view.InputTokens += usage.InputTokens
			view.OutputTokens += usage.OutputTokens
			view.Sandboxes = append(view.Sandboxes, s.Name)
			summary.Providers[id] = view
		}
	}
	for id, usage := range a.retiredUsage() {
		view := summary.Providers[id]
		view.ID = id
		view.Requests += usage.Requests
		view.InputTokens += usage.InputTokens
		view.OutputTokens += usage.OutputTokens
		summary.Providers[id] = view
	}
	return summary, nil
}

// RevealAuditLog shows the sandbox's JSONL audit log in Finder.
func (a *App) RevealAuditLog(id string) error {
	path := a.auditPath(id)
	if _, err := os.Stat(path); err != nil {
		return errors.New("这个沙箱还没有审计日志")
	}
	return exec.Command("/usr/bin/open", "-R", path).Run()
}

// ExportAuditTimeline writes the current redacted timeline to a standalone JSON file. It uses the
// same EventView data as the UI, so provider names and sandbox names are included while request
// bodies, MCP arguments and credentials remain absent.
func (a *App) ExportAuditTimeline(id string) (string, error) {
	if _, err := a.store.Sandbox(id); err != nil {
		return "", err
	}
	events, err := a.RecentEvents(id)
	if err != nil {
		return "", err
	}
	data, err := json.MarshalIndent(struct {
		ExportedAt time.Time   `json:"exported_at"`
		SandboxID  string      `json:"sandbox_id"`
		Events     []EventView `json:"events"`
	}{ExportedAt: time.Now(), SandboxID: id, Events: events}, "", "  ")
	if err != nil {
		return "", err
	}
	path := filepath.Join(a.store.Dir(), "audit", id+".timeline.json")
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		return "", err
	}
	return path, nil
}
