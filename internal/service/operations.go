package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/shaoxier/AIofCage/internal/runtime"
	"github.com/shaoxier/AIofCage/internal/sandbox"
)

// operations remembers lifecycle operations in progress, with their steps and the latest detail
// of the current one, so every page (and a reloaded window) shows the same state, a second click
// cannot start the same sandbox twice, and a long operation can be stopped. The last finished
// operation of each sandbox stays until the next one begins, so the interface can say how it ended.
type operations struct {
	mu    sync.Mutex
	items map[string]*operation
	last  map[string]OperationView
	// closed refuses new operations once the app is quitting.
	closed bool
	now    func() time.Time
	// send delivers a sandbox's operation to the interface; updates of the detail are batched.
	send func(id string, view OperationView)
}

type operation struct {
	name, label string
	cancel      context.CancelFunc // nil for operations that must run to the end
	started     time.Time
	steps       []StepView
	detail      string
	stopping    bool
	sent        time.Time
	queued      bool
}

// OperationView is a long sandbox operation as the interface shows it: the steps so far with when
// each began and ended, the detail of the current one, and, once it is over, how it ended.
type OperationView struct {
	Label     string     `json:"label"`
	Started   time.Time  `json:"started_at"`
	Steps     []StepView `json:"steps"`
	Detail    string     `json:"detail,omitempty"`
	Stoppable bool       `json:"stoppable,omitempty"`
	Stopping  bool       `json:"stopping,omitempty"`
	// Outcome is empty while running, then done, failed or stopped; Message says more.
	Outcome string     `json:"outcome,omitempty"`
	Message string     `json:"message,omitempty"`
	Ended   *time.Time `json:"ended_at,omitempty"`
}

type StepView struct {
	Text    string     `json:"text"`
	Started time.Time  `json:"started_at"`
	Ended   *time.Time `json:"ended_at,omitempty"`
}

// detailInterval is how often detail updates (build output, for instance) reach the interface.
const detailInterval = 300 * time.Millisecond

func (o *operations) clock() time.Time {
	if o.now != nil {
		return o.now()
	}
	return time.Now()
}

// begin marks an operation as running; cancel, if set, lets it be stopped.
func (o *operations) begin(id, name, label string, cancel context.CancelFunc) error {
	o.mu.Lock()
	if o.items == nil {
		o.items, o.last = map[string]*operation{}, map[string]OperationView{}
	}
	if current := o.items[id]; current != nil {
		o.mu.Unlock()
		return fmt.Errorf("「%s」正在%s，请等它完成", name, current.label)
	}
	if o.closed {
		o.mu.Unlock()
		return errors.New("应用正在退出，不能开始新的操作")
	}
	o.items[id] = &operation{name: name, label: label, cancel: cancel, started: o.clock()}
	delete(o.last, id)
	view := o.viewLocked(id)
	o.mu.Unlock()
	o.deliver(id, view)
	return nil
}

// end records how an operation ended and keeps it for display.
func (o *operations) end(id, outcome, message string) {
	o.mu.Lock()
	current := o.items[id]
	if current == nil {
		o.mu.Unlock()
		return
	}
	view := o.viewLocked(id)
	ended := o.clock()
	view.Outcome, view.Message, view.Ended, view.Stoppable, view.Stopping = outcome, message, &ended, false, false
	if n := len(view.Steps); n > 0 && view.Steps[n-1].Ended == nil {
		view.Steps[n-1].Ended = &ended
	}
	delete(o.items, id)
	o.last[id] = view
	o.mu.Unlock()
	o.deliver(id, view)
}

// update applies a progress report from the runtime: a new step, or a detail of the current one.
func (o *operations) update(id string, update runtime.Update) {
	o.mu.Lock()
	current := o.items[id]
	if current == nil {
		o.mu.Unlock()
		return
	}
	if update.Final {
		current.cancel = nil // the rest runs to its end; a stop no longer applies
	}
	if update.Step != "" {
		now := o.clock()
		if n := len(current.steps); n > 0 && current.steps[n-1].Ended == nil {
			current.steps[n-1].Ended = &now
		}
		current.steps = append(current.steps, StepView{Text: update.Step, Started: now})
		current.detail = ""
	} else {
		current.detail = update.Detail
		if wait := detailInterval - o.clock().Sub(current.sent); wait > 0 {
			if !current.queued {
				current.queued = true
				time.AfterFunc(wait, func() { o.flush(id) })
			}
			o.mu.Unlock()
			return
		}
	}
	current.sent = o.clock()
	view := o.viewLocked(id)
	o.mu.Unlock()
	o.deliver(id, view)
}

func (o *operations) flush(id string) {
	o.mu.Lock()
	current := o.items[id]
	if current == nil || !current.queued {
		o.mu.Unlock()
		return
	}
	current.queued, current.sent = false, o.clock()
	view := o.viewLocked(id)
	o.mu.Unlock()
	o.deliver(id, view)
}

// stop asks a running operation to stop. It ends on its own once its current step allows.
func (o *operations) stop(id string) error {
	o.mu.Lock()
	current := o.items[id]
	switch {
	case current == nil:
		o.mu.Unlock()
		return errors.New("这个沙箱没有正在进行的操作")
	case current.cancel == nil:
		o.mu.Unlock()
		return fmt.Errorf("「%s」的%s不能中途停止，请等它完成", current.name, current.label)
	}
	current.stopping = true
	current.cancel()
	view := o.viewLocked(id)
	o.mu.Unlock()
	o.deliver(id, view)
	return nil
}

// stopAll asks every operation that can be stopped to stop, and refuses new ones: the app quits.
func (o *operations) stopAll() {
	o.mu.Lock()
	o.closed = true
	var ids []string
	for id, current := range o.items {
		if current.cancel != nil {
			ids = append(ids, id)
		}
	}
	o.mu.Unlock()
	for _, id := range ids {
		_ = o.stop(id)
	}
}

func (o *operations) stopping(id string) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	current := o.items[id]
	return current != nil && current.stopping
}

// running describes the operations in progress, e.g. 「t4-dev」启动.
func (o *operations) running() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	var names []string
	for _, current := range o.items {
		names = append(names, fmt.Sprintf("「%s」%s", current.name, current.label))
	}
	sort.Strings(names)
	return names
}

// wait returns once no operation is running, or false when timeout passes first.
func (o *operations) wait(timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		o.mu.Lock()
		idle := len(o.items) == 0
		o.mu.Unlock()
		if idle {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// settingsOperations only deliver new settings to a running sandbox; sessions may go on meanwhile.
var settingsOperations = map[string]bool{"更新 MCP": true, "更新开发环境": true}

// changingContainers returns the label of a running operation that starts, stops or replaces the
// sandbox's containers, during which sessions cannot open or receive messages.
func (o *operations) changingContainers(id string) string {
	label, _ := o.get(id)
	if settingsOperations[label] {
		return ""
	}
	return label
}

// get returns the running operation's label and current step.
func (o *operations) get(id string) (string, string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if current := o.items[id]; current != nil {
		step := ""
		if n := len(current.steps); n > 0 {
			step = current.steps[n-1].Text
		}
		return current.label, step
	}
	return "", ""
}

// view returns the running operation, or else the last finished one.
func (o *operations) view(id string) (OperationView, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.items[id] != nil {
		return o.viewLocked(id), true
	}
	view, ok := o.last[id]
	return view, ok
}

func (o *operations) viewLocked(id string) OperationView {
	current := o.items[id]
	return OperationView{Label: current.label, Started: current.started, Steps: slices.Clone(current.steps),
		Detail: current.detail, Stoppable: current.cancel != nil, Stopping: current.stopping}
}

func (o *operations) deliver(id string, view OperationView) {
	if o.send != nil {
		o.send(id, view)
	}
}

// retiredUsageKey keeps the provider usage of deleted sandboxes, so a provider's cumulative usage
// does not drop when a sandbox is removed.
const retiredUsageKey = "retired_provider_usage"

func (a *App) retiredUsage() map[string]sandbox.ProviderUsage {
	usage := map[string]sandbox.ProviderUsage{}
	if raw := a.store.Setting(retiredUsageKey); raw != "" {
		_ = json.Unmarshal([]byte(raw), &usage)
	}
	return usage
}

func (a *App) retireUsage(sbx sandbox.Sandbox) error {
	a.usageMu.Lock()
	defer a.usageMu.Unlock()
	usage := a.retiredUsage()
	for id, u := range sbx.ProviderUsage {
		total := usage[id]
		total.Requests += u.Requests
		total.InputTokens += u.InputTokens
		total.OutputTokens += u.OutputTokens
		usage[id] = total
	}
	data, err := json.Marshal(usage)
	if err != nil {
		return err
	}
	return a.store.SetSetting(retiredUsageKey, string(data))
}

func (a *App) forgetRetiredUsage(providerID string) {
	a.usageMu.Lock()
	defer a.usageMu.Unlock()
	usage := a.retiredUsage()
	if _, ok := usage[providerID]; !ok {
		return
	}
	delete(usage, providerID)
	if data, err := json.Marshal(usage); err == nil {
		_ = a.store.SetSetting(retiredUsageKey, string(data))
	}
}
