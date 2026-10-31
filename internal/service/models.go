package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/shaoxier/AIofCage/internal/provider"
)

// ModelList is what a provider's model list endpoint returned. It serves the provider form, the
// terminal's model picker and the connection test. Models may be empty: a model name can always be
// typed by hand.
type ModelList struct {
	Models    []string  `json:"models"`
	OK        bool      `json:"ok"` // the endpoint answered with a usable list
	Status    int       `json:"status"`
	Message   string    `json:"message"`
	LatencyMS int64     `json:"latency_ms"`
	FetchedAt time.Time `json:"fetched_at"`
	Cached    bool      `json:"cached"`
}

const (
	maxModels = 500
	// modelsTTL keeps a provider's list; a refresh replaces it, saving or deleting the provider drops it.
	modelsTTL = 20 * time.Minute
	// anthropicPages bounds paging through Anthropic's model list.
	anthropicPages = 5
)

// modelIDPattern accepts the model names providers use (gpt-6.1-sol, claude-sonnet-4-5,
// deepseek-ai/DeepSeek-V3, qwen:72b) and nothing that could be markup or a control character.
var modelIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/@+-]{0,199}$`)

// modelCache keeps model lists of saved providers, keyed by what decides the answer: address,
// protocol, authentication and Keychain item. The key itself is never part of it.
type modelCache struct {
	mu    sync.Mutex
	items map[string]ModelList
}

func modelCacheKey(p provider.Provider) string {
	return strings.Join([]string{p.BaseURL, string(p.Protocol), string(p.Auth), p.KeyRef}, "\n")
}

func (c *modelCache) get(key string, now time.Time) (ModelList, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	list, ok := c.items[key]
	if !ok || now.Sub(list.FetchedAt) > modelsTTL {
		return ModelList{}, false
	}
	list.Models = append([]string(nil), list.Models...)
	return list, true
}

func (c *modelCache) put(key string, list ModelList) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.items == nil {
		c.items = map[string]ModelList{}
	}
	list.Models = append([]string(nil), list.Models...)
	c.items[key] = list
}

// forget drops every list fetched with a Keychain item, whose key may just have changed.
func (c *modelCache) forget(keyRef string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for key := range c.items {
		if strings.HasSuffix(key, "\n"+keyRef) {
			delete(c.items, key)
		}
	}
}

// ProviderModels lists a saved provider's models, from the cache unless refresh is set.
func (a *App) ProviderModels(id string, refresh bool) (ModelList, error) {
	p, err := a.store.Provider(id)
	if err != nil {
		return ModelList{}, err
	}
	return a.providerModels(p, refresh), nil
}

func (a *App) providerModels(p provider.Provider, refresh bool) ModelList {
	cacheKey := modelCacheKey(p)
	if !refresh {
		if list, ok := a.models.get(cacheKey, a.now()); ok {
			list.Cached = true
			return list
		}
	}
	if err := p.KeyRefAllowed(); err != nil {
		return ModelList{Message: err.Error()}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	key, err := a.vault.Read(ctx, p.KeyRef)
	if err != nil {
		return ModelList{Message: "钥匙串中没有这个供应商的密钥"}
	}
	defer clear(key)
	list := a.fetchModels(ctx, p, key)
	if list.OK {
		a.models.put(cacheKey, list)
	}
	return list
}

// PreviewProviderModels lists models for a provider that is being added or edited, before it is
// saved: with the key typed in the form, a preset's Keychain item, or the saved key of the provider
// being edited. A preset's item only goes to the preset's host. Nothing is stored or cached.
func (a *App) PreviewProviderModels(request SaveProviderRequest) (ModelList, error) {
	p := provider.Provider{ID: request.ID, Name: request.Name, Agent: request.Agent, Protocol: request.Protocol,
		BaseURL: request.BaseURL, Model: request.Model, Auth: request.Auth, Preset: request.Preset}
	p.Normalize()
	if _, _, err := p.Endpoint(); err != nil {
		return ModelList{Message: err.Error()}, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	key := []byte(strings.TrimSpace(request.APIKey))
	if len(key) == 0 {
		switch {
		case request.ExistingKeyRef != "":
			if !allowedExistingKeyRef(request.ExistingKeyRef, request.ID) {
				return ModelList{}, errors.New("只能使用 AIofCage 保存的密钥，或预设中列出的钥匙串条目")
			}
			p.KeyRef = request.ExistingKeyRef
		case request.ID != "":
			saved, err := a.store.Provider(request.ID)
			if err != nil {
				return ModelList{}, err
			}
			p.KeyRef = saved.KeyRef
		default:
			return ModelList{Message: "请先填写 API Key"}, nil
		}
		if err := p.KeyRefAllowed(); err != nil {
			return ModelList{Message: err.Error()}, nil
		}
		saved, err := a.vault.Read(ctx, p.KeyRef)
		if err != nil {
			return ModelList{Message: "钥匙串中没有这个条目，请填写 API Key"}, nil
		}
		key = saved
	}
	defer clear(key)
	return a.fetchModels(ctx, p, key), nil
}

// fetchModels asks a provider for its models with the given key. Only the provider's own host is
// contacted and redirects are refused (a.client), so the key never travels anywhere else.
func (a *App) fetchModels(ctx context.Context, p provider.Provider, key []byte) ModelList {
	host, base, err := p.Endpoint()
	if err != nil {
		return ModelList{Message: err.Error()}
	}
	list := ModelList{FetchedAt: a.now()}
	path, query := base+"/models", ""
	if p.Protocol == provider.Anthropic {
		path, query = base+"/v1/models", "limit=1000"
	}
	seen := map[string]bool{}
	started := a.now()
	for page := 0; ; page++ {
		target := "https://" + host + path
		if query != "" {
			target += "?" + query
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
		if err != nil {
			return ModelList{Message: err.Error()}
		}
		header, prefix := p.Injection()
		req.Header.Set(header, prefix+string(key))
		if p.Protocol == provider.Anthropic {
			req.Header.Set("anthropic-version", "2023-06-01")
		}
		resp, err := a.client.Do(req)
		if page == 0 {
			list.LatencyMS = a.now().Sub(started).Milliseconds()
		}
		if err != nil {
			list.Message = "无法连接到 " + host + "（" + networkReason(err) + "），可以手动填写模型名称"
			return list
		}
		list.Status = resp.StatusCode
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			list.Message = modelStatusMessage(resp.StatusCode)
			return list
		}
		var body struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
			HasMore bool   `json:"has_more"`
			LastID  string `json:"last_id"`
		}
		err = json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&body)
		resp.Body.Close()
		if err != nil {
			list.Message = "服务可达，但模型列表的格式无法识别，可以手动填写模型名称"
			return list
		}
		for _, model := range body.Data {
			if modelIDPattern.MatchString(model.ID) && !seen[model.ID] && len(list.Models) < maxModels {
				seen[model.ID] = true
				list.Models = append(list.Models, model.ID)
			}
		}
		if p.Protocol != provider.Anthropic || !body.HasMore || body.LastID == "" || page+1 >= anthropicPages || len(list.Models) >= maxModels {
			break
		}
		query = "limit=1000&after_id=" + url.QueryEscape(body.LastID)
	}
	if len(list.Models) == 0 {
		list.Message = "服务可达，但返回的模型列表为空，可以手动填写模型名称"
		return list
	}
	sort.Strings(list.Models)
	list.OK = true
	list.Message = fmt.Sprintf("连接正常，取得 %d 个模型", len(list.Models))
	return list
}

// modelStatusMessage explains an answer other than 200 from a model list endpoint.
func modelStatusMessage(status int) string {
	switch {
	case status >= 300 && status < 400:
		return fmt.Sprintf("服务返回了跳转（%d）。为保护密钥，AIofCage 不跟随跳转，请检查接口地址", status)
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return "服务可达，但密钥无效或没有权限"
	case status == http.StatusNotFound || status == http.StatusMethodNotAllowed:
		return "服务可达，但不提供模型列表接口，请手动填写模型名称"
	case status == http.StatusTooManyRequests:
		return "服务可达，但当前被限流，稍后再试或手动填写模型名称"
	}
	return fmt.Sprintf("服务返回了 %d，可以手动填写模型名称", status)
}
