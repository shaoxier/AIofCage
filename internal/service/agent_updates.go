package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/shaoxier/AIofCage/internal/runtime"
	"github.com/shaoxier/AIofCage/internal/session"
)

type AgentUpdate struct {
	Agent   session.Agent `json:"agent"`
	Name    string        `json:"name"`
	Package string        `json:"package"`
	BuiltIn string        `json:"built_in"`
	Default string        `json:"default_version"`
	Latest  string        `json:"latest"`
	Checked string        `json:"checked"`
	Error   string        `json:"error"`
	Newer   bool          `json:"newer"`
}

const agentChecksKey = "agent_update_checks"

func (a *App) AgentUpdates() ([]AgentUpdate, error) {
	a.updatesMu.Lock()
	defer a.updatesMu.Unlock()
	return a.agentUpdates()
}

func (a *App) agentUpdates() ([]AgentUpdate, error) {
	defaults, err := runtime.DefaultAgentVersions(a.store)
	if err != nil {
		return nil, err
	}
	var cache []AgentUpdate
	if raw := a.store.Setting(agentChecksKey); raw != "" {
		_ = json.Unmarshal([]byte(raw), &cache)
	}
	items := []AgentUpdate{}
	for _, spec := range runtime.Catalog() {
		item := AgentUpdate{Agent: spec.Agent, Name: spec.Name, Package: spec.Package, BuiltIn: spec.Version, Default: defaults[spec.Agent]}
		for _, previous := range cache {
			if previous.Agent == spec.Agent {
				item.Latest, item.Checked, item.Error = previous.Latest, previous.Checked, previous.Error
			}
		}
		item.Newer = versionNewer(item.Latest, item.Default)
		items = append(items, item)
	}
	return items, nil
}

// CheckAgentUpdates 只读取固定官方 npm 包的 latest 元数据，不安装、不运行包或改变沙箱。
func (a *App) CheckAgentUpdates() ([]AgentUpdate, error) {
	a.updatesMu.Lock()
	defer a.updatesMu.Unlock()
	items, err := a.agentUpdates()
	if err != nil {
		return nil, err
	}
	var workers sync.WaitGroup
	for index := range items {
		workers.Add(1)
		go func(index int) {
			defer workers.Done()
			item := &items[index]
			item.Error = ""
			ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
			defer cancel()
			req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://registry.npmjs.org/"+url.PathEscape(item.Package)+"/latest", nil)
			response, err := a.client.Do(req)
			if err != nil {
				item.Error = "检查失败，请稍后重试"
				return
			}
			defer response.Body.Close()
			var meta struct {
				Name    string `json:"name"`
				Version string `json:"version"`
			}
			if response.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(response.Body, 2<<20)).Decode(&meta) != nil || meta.Name != item.Package || !runtime.ValidAgentVersion(meta.Version) {
				item.Error = "官方仓库未返回有效版本"
				return
			}
			item.Latest, item.Checked = meta.Version, a.now().UTC().Format(time.RFC3339)
			item.Newer = versionNewer(item.Latest, item.Default)
		}(index)
	}
	workers.Wait()
	raw, _ := json.Marshal(items)
	if err := a.store.SetSetting(agentChecksKey, string(raw)); err != nil {
		return nil, err
	}
	return items, nil
}

func (a *App) SetAgentDefaultVersion(agent session.Agent, version string) error {
	a.updatesMu.Lock()
	defer a.updatesMu.Unlock()
	if !agent.Valid() || !runtime.ValidAgentVersion(version) {
		return errors.New("智能体或版本无效")
	}
	items, err := a.agentUpdates()
	if err != nil {
		return err
	}
	allowed := false
	for _, item := range items {
		if item.Agent == agent && (version == item.BuiltIn || version == item.Default || (item.Error == "" && version == item.Latest)) {
			allowed = true
		}
	}
	if !allowed {
		return errors.New("请先检查更新，或选择应用内置版本")
	}
	defaults, err := runtime.DefaultAgentVersions(a.store)
	if err != nil {
		return err
	}
	defaults[agent] = version
	raw, _ := json.Marshal(defaults)
	return a.store.SetSetting(runtime.AgentDefaultsKey, string(raw))
}

func (a *App) UpgradeSandboxAgents(id string, agents []session.Agent) error {
	if err := a.runtimeReady(); err != nil {
		return err
	}
	return a.operation(id, "更新智能体", 40*time.Minute, true, func(ctx context.Context) error {
		return a.manager.UpgradeAgents(ctx, id, agents)
	})
}

// 版本只接受 runtime 校验的 SemVer 子集；数字比较，正式版优先于同号预发布。
func versionNewer(candidate, current string) bool {
	if !runtime.ValidAgentVersion(candidate) || !runtime.ValidAgentVersion(current) {
		return false
	}
	a, ap, _ := strings.Cut(candidate, "-")
	b, bp, _ := strings.Cut(current, "-")
	aa, bb := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < 3; i++ {
		av, _ := strconv.Atoi(aa[i])
		bv, _ := strconv.Atoi(bb[i])
		if av != bv {
			return av > bv
		}
	}
	if ap == bp {
		return false
	}
	if ap == "" {
		return true
	}
	if bp == "" {
		return false
	}
	aids, bids := strings.Split(ap, "."), strings.Split(bp, ".")
	for i := 0; i < min(len(aids), len(bids)); i++ {
		if aids[i] == bids[i] {
			continue
		}
		av, ae := strconv.Atoi(aids[i])
		bv, be := strconv.Atoi(bids[i])
		if ae == nil && be == nil {
			return av > bv
		}
		if ae == nil {
			return false
		}
		if be == nil {
			return true
		}
		return aids[i] > bids[i]
	}
	return len(aids) > len(bids)
}
