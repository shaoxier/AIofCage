// Package audit defines redacted events; durable recording is not implemented yet.
package audit

import (
	"context"
	"github.com/shaoxier/AIofCage/internal/provider"
	"time"
)

type Event struct {
	Pricing       *provider.Pricing `json:"pricing,omitempty"`
	EstimatedCost *float64          `json:"estimated_cost,omitempty"`
	CostStatus    string            `json:"cost_status,omitempty"`
	ID            string            `json:"id"`
	Time          time.Time         `json:"ts"`
	Source        string            `json:"source"`
	RunID         string            `json:"gatekeeper_run_id,omitempty"`
	Sequence      uint64            `json:"seq,omitempty"`
	SessionID     string            `json:"session_id"`
	Action        string            `json:"action"`
	Decision      string            `json:"decision"`
	ReasonCode    string            `json:"reason_code"`
	RuleID        string            `json:"rule,omitempty"`
	PolicyVersion uint64            `json:"policy_version,omitempty"`
	ServiceID     string            `json:"service_id,omitempty"`
	ToolID        string            `json:"tool_id,omitempty"`
	// RedirectHost is where an upstream redirect pointed, when it is a valid host name.
	RedirectHost string `json:"redirect_host,omitempty"`

	// Network metadata. Path never includes the query string; SecretRef is a name, never a value.
	Host         string `json:"host,omitempty"`
	Method       string `json:"method,omitempty"`
	Path         string `json:"path,omitempty"`
	SecretRef    string `json:"secret_ref,omitempty"`
	Status       int    `json:"status,omitempty"`
	BytesIn      int64  `json:"bytes_in,omitempty"`
	BytesOut     int64  `json:"bytes_out,omitempty"`
	DurationMS   int64  `json:"duration_ms,omitempty"`
	InputTokens  int64  `json:"input_tokens,omitempty"`
	OutputTokens int64  `json:"output_tokens,omitempty"`
	// 以下均为脱敏用量元数据，不记录请求正文或凭据。
	Model            string `json:"model,omitempty"`
	ConfiguredModel  string `json:"configured_model,omitempty"`
	Agent            string `json:"agent,omitempty"`
	ProviderName     string `json:"provider_name,omitempty"`
	CacheReadTokens  int64  `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens int64  `json:"cache_write_tokens,omitempty"`
	ModelRequest     bool   `json:"model_request,omitempty"`
	// UsageKind is "reported" when tokens came from the upstream response, "unknown" otherwise.
	UsageKind string `json:"usage_kind,omitempty"`
}

// Event values used by the gatekeeper.
const (
	SourceGatekeeper = "gatekeeper"
	SourceApp        = "app"

	ActionConnect      = "net.connect"
	ActionRequest      = "net.request"
	ActionMCPCall      = "mcp.call"
	ActionQuotaBlocked = "quota.blocked"
	ActionQuotaWarning = "quota.warning"
	ActionPolicyChange = "policy.change"
	ActionGap          = "audit.gap"

	DecisionAllow         = "allow"
	DecisionDeny          = "deny"
	DecisionInject        = "inject"
	DecisionError         = "error"
	DecisionNotApplicable = "not-applicable"
)

// Event deliberately has no body, headers, arbitrary payload, tool args, or secret values.
type Recorder interface {
	Append(context.Context, Event) error
}
