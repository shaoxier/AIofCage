// Package devsource describes the package and code sources a sandbox can open for development.
// Each source is opened for anonymous downloads only: its rules allow reading, refuse publishing
// explicitly, and refuse requests that carry their own credentials. Opening a source does not
// stop data from leaving in request URLs; it only keeps writes and accounts out.
package devsource

import (
	"fmt"
	"slices"
	"sort"

	"github.com/shaoxier/AIofCage/internal/policy"
)

// Source is one preset the user can tick when creating or editing a sandbox. Sources that share a
// Group are alternatives (the npm registry or its mirror), and at most one of them is enabled.
type Source struct {
	ID    string   `json:"id"`
	Name  string   `json:"name"`
	Note  string   `json:"note"`
	Group string   `json:"group,omitempty"`
	Hosts []string `json:"hosts"`
	// Env configures the clients inside the sandbox for this source.
	Env   map[string]string `json:"-"`
	rules []policy.Rule
}

var (
	read  = []string{"GET", "HEAD"}
	write = []string{"PUT", "POST", "PATCH", "DELETE"}
	// npm prints warnings for audit and update checks that only work against the official API.
	npmQuiet = map[string]string{"NPM_CONFIG_AUDIT": "false", "NPM_CONFIG_FUND": "false", "NPM_CONFIG_UPDATE_NOTIFIER": "false"}
)

func allow(host, path string, methods ...string) policy.Rule {
	return policy.Rule{Effect: policy.Allow, Host: host, Path: path, Methods: methods, Anonymous: true}
}

func deny(host, path string, methods ...string) policy.Rule {
	return policy.Rule{Effect: policy.Deny, Host: host, Path: path, Methods: methods}
}

var catalog = []Source{
	{ID: "npm", Name: "npm 官方源", Group: "npm", Hosts: []string{"registry.npmjs.org"},
		Note: "npm install 从 registry.npmjs.org 下载包；不能发布或改动包",
		Env:  merge(npmQuiet, map[string]string{"NPM_CONFIG_REGISTRY": "https://registry.npmjs.org/"}),
		rules: []policy.Rule{
			deny("registry.npmjs.org", "/*", write...),
			allow("registry.npmjs.org", "/*", read...),
		}},
	{ID: "npm-mirror", Name: "npm 国内镜像（npmmirror）", Group: "npm", Hosts: []string{"registry.npmmirror.com", "cdn.npmmirror.com"},
		Note: "npm install 从 registry.npmmirror.com 下载包，国内网络更快；不能发布",
		Env:  merge(npmQuiet, map[string]string{"NPM_CONFIG_REGISTRY": "https://registry.npmmirror.com/"}),
		rules: []policy.Rule{
			deny("registry.npmmirror.com", "/*", write...),
			allow("registry.npmmirror.com", "/*", read...),
			allow("cdn.npmmirror.com", "/*", read...),
		}},
	{ID: "github", Name: "GitHub 只读（公开仓库）",
		Hosts: []string{"github.com", "codeload.github.com", "raw.githubusercontent.com", "objects.githubusercontent.com", "release-assets.githubusercontent.com"},
		Note:  "git clone 公开仓库，下载源码包、Release 文件和原始文件；不能推送，也不开放 GitHub 的其他页面和接口",
		// A private repository answers 401; git must fail at once instead of waiting for a password.
		Env: map[string]string{"GIT_TERMINAL_PROMPT": "0"},
		rules: []policy.Rule{
			deny("github.com", "/*/*/git-receive-pack"),
			allow("github.com", "/*/*/info/refs", read...),
			allow("github.com", "/*/*/git-upload-pack", "POST"),
			allow("github.com", "/*/*/archive/*", read...),
			allow("github.com", "/*/*/releases/download/*", read...),
			allow("github.com", "/*/*/raw/*", read...),
			allow("codeload.github.com", "/*", read...),
			allow("raw.githubusercontent.com", "/*", read...),
			allow("objects.githubusercontent.com", "/*", read...),
			allow("release-assets.githubusercontent.com", "/*", read...),
		}},
}

func merge(maps ...map[string]string) map[string]string {
	result := map[string]string{}
	for _, m := range maps {
		for key, value := range m {
			result[key] = value
		}
	}
	return result
}

// Catalog lists the sources in display order.
func Catalog() []Source { return slices.Clone(catalog) }

func find(id string) (Source, bool) {
	for _, source := range catalog {
		if source.ID == id {
			return source, true
		}
	}
	return Source{}, false
}

// Validate accepts known, distinct sources with at most one per group.
func Validate(ids []string) error {
	seen, groups := map[string]bool{}, map[string]string{}
	for _, id := range ids {
		source, ok := find(id)
		if !ok {
			return fmt.Errorf("不支持的开发环境：%s", id)
		}
		if seen[id] {
			return fmt.Errorf("开发环境重复：%s", source.Name)
		}
		seen[id] = true
		if source.Group != "" {
			if other, taken := groups[source.Group]; taken {
				return fmt.Errorf("「%s」和「%s」只能选一个", other, source.Name)
			}
			groups[source.Group] = source.Name
		}
	}
	return nil
}

// Rules returns the gatekeeper rules of the enabled sources; rule IDs are unique per source.
func Rules(ids []string) []policy.Rule {
	var rules []policy.Rule
	for _, id := range ids {
		source, ok := find(id)
		if !ok {
			continue
		}
		for i, rule := range source.rules {
			rule.ID = fmt.Sprintf("dev-%s-%d", source.ID, i+1)
			rules = append(rules, rule)
		}
	}
	return rules
}

// Env returns the client settings of the enabled sources, sorted by name.
func Env(ids []string) [][2]string {
	values := map[string]string{}
	for _, id := range ids {
		if source, ok := find(id); ok {
			for key, value := range source.Env {
				values[key] = value
			}
		}
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([][2]string, 0, len(keys))
	for _, key := range keys {
		result = append(result, [2]string{key, values[key]})
	}
	return result
}
