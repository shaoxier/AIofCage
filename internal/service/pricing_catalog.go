package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/shaoxier/AIofCage/internal/provider"
)

const pricingCatalogURL = "https://models.dev/api.json"

type PricingSuggestion struct {
	Provider string           `json:"provider"`
	Model    string           `json:"model"`
	Pricing  provider.Pricing `json:"pricing"`
}

type pricingCatalog struct {
	mu      sync.Mutex
	items   []PricingSuggestion
	fetched time.Time
}

// PricingSuggestions 只获取公开目录，不发送模型配置、密钥或请求正文；精确匹配，供用户手动采用。
func (a *App) PricingSuggestions(model string) ([]PricingSuggestion, error) {
	model = strings.TrimSpace(model)
	if model == "" || len(model) > 200 {
		return nil, errors.New("请先填写模型名称")
	}
	a.prices.mu.Lock()
	defer a.prices.mu.Unlock()
	if a.prices.items == nil || a.now().Sub(a.prices.fetched) > 24*time.Hour {
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, pricingCatalogURL, nil)
		client := *a.client
		client.Timeout = 45 * time.Second
		client.CheckRedirect = nil // a public catalog fetched without credentials may follow redirects
		response, err := client.Do(req)
		if err != nil {
			return nil, errors.New("读取公开价格失败，请手动填写供应商报价")
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusOK {
			return nil, errors.New("价格目录暂不可用，请手动填写供应商报价")
		}
		data, err := io.ReadAll(io.LimitReader(response.Body, (16<<20)+1))
		if err != nil || len(data) > 16<<20 {
			return nil, errors.New("价格目录内容异常")
		}
		items, err := parsePricingCatalog(data, a.now())
		if err != nil {
			return nil, err
		}
		a.prices.items, a.prices.fetched = items, a.now()
	}
	result := []PricingSuggestion{}
	for _, item := range a.prices.items {
		if item.Model == model {
			result = append(result, item)
		}
		if len(result) >= 100 {
			break
		}
	}
	return result, nil
}

func parsePricingCatalog(data []byte, fetched time.Time) ([]PricingSuggestion, error) {
	var catalog map[string]struct {
		Name   string `json:"name"`
		Models map[string]struct {
			ID         string `json:"id"`
			Status     string `json:"status"`
			Modalities struct {
				Output []string `json:"output"`
			} `json:"modalities"`
			Cost *struct {
				Input  *float64 `json:"input"`
				Output *float64 `json:"output"`
				Read   *float64 `json:"cache_read"`
				Write  *float64 `json:"cache_write"`
			} `json:"cost"`
		} `json:"models"`
	}
	if json.Unmarshal(data, &catalog) != nil {
		return nil, errors.New("价格目录格式无效")
	}
	items := []PricingSuggestion{}
	for id, entry := range catalog {
		for key, model := range entry.Models {
			if model.Status == "deprecated" || model.Cost == nil || model.Cost.Input == nil || model.Cost.Output == nil {
				continue
			}
			if len(model.Modalities.Output) > 0 && (len(model.Modalities.Output) != 1 || model.Modalities.Output[0] != "text") {
				continue
			}
			name := model.ID
			if name == "" {
				name = key
			}
			price := provider.Pricing{Currency: "USD", Input: *model.Cost.Input, Output: *model.Cost.Output, Source: "models.dev/" + id + "/" + name, AsOf: fetched.UTC().Format(time.RFC3339)}
			// 目录缺少缓存价时按普通输入估算，绝不能把缺失误算成免费。
			price.CacheRead, price.CacheWrite = price.Input, price.Input
			if model.Cost.Read != nil {
				price.CacheRead = *model.Cost.Read
			}
			if model.Cost.Write != nil {
				price.CacheWrite = *model.Cost.Write
			}
			if price.Validate() != nil {
				continue
			}
			label := entry.Name
			if label == "" {
				label = id
			}
			items = append(items, PricingSuggestion{Provider: label, Model: name, Pricing: price})
		}
	}
	if len(items) == 0 {
		return nil, errors.New("目录没有可用的文本模型价格")
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Pricing.Source < items[j].Pricing.Source })
	return items, nil
}
