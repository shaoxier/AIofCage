package runtime

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"
	"time"

	"github.com/shaoxier/AIofCage/internal/audit"
	"github.com/shaoxier/AIofCage/internal/events"
	"github.com/shaoxier/AIofCage/internal/gatekeeper"
	"github.com/shaoxier/AIofCage/internal/sandbox"
)

// gateClient talks to one sandbox's gatekeeper over the host loopback port that Colima forwards.
// It never uses proxy settings from the environment.
type gateClient struct {
	base  string
	token string
	http  *http.Client
}

func newGateClient(port, token string) *gateClient {
	return &gateClient{base: "http://127.0.0.1:" + port, token: token,
		http: &http.Client{Transport: &http.Transport{Proxy: nil}}}
}

func (c *gateClient) do(ctx context.Context, method, path string, body []byte, timeout time.Duration) (*http.Response, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, bytes.NewReader(body))
	if err != nil {
		cancel()
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		cancel()
		return nil, err
	}
	resp.Body = cancelOnClose{resp.Body, cancel}
	return resp, nil
}

type cancelOnClose struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (c cancelOnClose) Close() error {
	err := c.ReadCloser.Close()
	c.cancel()
	return err
}

type gateHealth struct {
	Ready         bool   `json:"ready"`
	Reason        string `json:"reason"`
	RunID         string `json:"run_id"`
	PolicyVersion uint64 `json:"policy_version"`
}

func (c *gateClient) health(ctx context.Context) (gateHealth, error) {
	var health gateHealth
	resp, err := c.do(ctx, "GET", "/health", nil, 5*time.Second)
	if err != nil {
		return health, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 && resp.StatusCode != 503 {
		return health, fmt.Errorf("门卫健康检查返回 %d", resp.StatusCode)
	}
	return health, json.NewDecoder(resp.Body).Decode(&health)
}

func (c *gateClient) putConfig(ctx context.Context, config gatekeeper.Config) error {
	body, err := json.Marshal(config)
	if err != nil {
		return err
	}
	resp, err := c.do(ctx, "PUT", "/config", body, 10*time.Second)
	// Drop our copy of the serialized secrets as soon as possible.
	clear(body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		var problem struct {
			Code   string `json:"code"`
			Detail string `json:"detail"`
		}
		_ = json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&problem)
		if problem.Code == "invalid_json" {
			// The app sends well-formed configuration; an older gatekeeper refuses fields it lacks.
			return errors.New("门卫不认识这份配置（invalid_json），多半是这个沙箱还在用旧版门卫，重启沙箱后会换成新门卫")
		}
		return fmt.Errorf("门卫拒绝了配置：%s %s", problem.Code, problem.Detail)
	}
	return nil
}

func (c *gateClient) ca(ctx context.Context) (string, error) {
	resp, err := c.do(ctx, "GET", "/ca", nil, 5*time.Second)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil || resp.StatusCode != 200 || !strings.HasPrefix(string(data), "-----BEGIN CERTIFICATE-----") {
		return "", errors.New("无法获取门卫 CA 证书")
	}
	return string(data), nil
}

// GateUsage mirrors the gatekeeper's /usage response.
type GateUsage struct {
	RunID         string `json:"run_id"`
	Requests      uint64 `json:"requests"`
	Denied        uint64 `json:"denied"`
	QuotaRequests uint64 `json:"quota_requests"`
	QuotaTokens   uint64 `json:"quota_tokens"`
	Limits        struct {
		MaxRequests uint64 `json:"max_requests"`
		MaxTokens   uint64 `json:"max_tokens"`
	} `json:"limits"`
	Secrets map[string]struct {
		Requests     uint64 `json:"requests"`
		InputTokens  uint64 `json:"input_tokens"`
		OutputTokens uint64 `json:"output_tokens"`
	} `json:"secrets"`
}

// Providers converts the per-secret counters (secret ID = provider ID) to sandbox usage.
func (u GateUsage) Providers() map[string]sandbox.ProviderUsage {
	result := map[string]sandbox.ProviderUsage{}
	for id, stat := range u.Secrets {
		result[id] = sandbox.ProviderUsage{Requests: stat.Requests, InputTokens: stat.InputTokens, OutputTokens: stat.OutputTokens}
	}
	return result
}

func (c *gateClient) usage(ctx context.Context) (GateUsage, error) {
	var usage GateUsage
	resp, err := c.do(ctx, "GET", "/usage", nil, 5*time.Second)
	if err != nil {
		return usage, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return usage, fmt.Errorf("读取用量失败：%d", resp.StatusCode)
	}
	return usage, json.NewDecoder(resp.Body).Decode(&usage)
}

// events streams gatekeeper events until ctx ends. lastID resumes after a known event.
func (c *gateClient) events(ctx context.Context, lastID string, handle events.Handler) error {
	req, err := http.NewRequestWithContext(ctx, "GET", c.base+"/events", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if lastID != "" {
		req.Header.Set("Last-Event-ID", lastID)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("订阅事件失败：%d", resp.StatusCode)
	}
	contentType, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil || contentType != "text/event-stream" {
		return errors.New("门卫未返回审计事件流")
	}
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var event audit.Event
		if json.Unmarshal([]byte(line[len("data: "):]), &event) != nil || event.ID == "" || event.Action == "" {
			// 不跳过无效记录，否则下一条成功事件会使续传游标越过它。
			return errors.New("审计事件格式无效，已中断订阅")
		}
		if err := handle(event); err != nil {
			return fmt.Errorf("审计事件未确认：%w", err)
		}
	}
	if ctx.Err() != nil {
		return nil
	}
	return scanner.Err()
}
