// 公共工具：Preact/htm、后端调用与中文标签。
import { h } from "preact";
import htm from "htm";
import { Icon } from "./icons.js";
export { Icon, agentIcon } from "./icons.js";

export const html = htm.bind(h);

export function PageTitle({ icon, title, description }) {
  return html`<div class="page-title"><span class="page-icon"><${Icon} name=${icon} size=${26} /></span>
    <div><h1>${title}</h1>${description && html`<p>${description}</p>`}</div></div>`;
}

// 调用 Wails 绑定的 Go 方法；错误统一转成可直接展示的中文字符串。
export async function call(method, ...args) {
  const app = window.go?.service?.App;
  if (!app) throw new Error("应用连接中断，请重新打开 Ai of Cage。");
  try {
    return await app[method](...args);
  } catch (error) {
    throw new Error(error instanceof Error ? error.message : String(error));
  }
}

export const AGENTS = [
  { id: "codex", name: "Codex" },
  { id: "claude-code", name: "Claude Code" },
  { id: "dsh", name: "DeepSeek Harness" },
];

export const agentName = (id) => AGENTS.find((agent) => agent.id === id)?.name ?? id;

export const PROTOCOLS = {
  "openai-responses": "OpenAI Responses",
  "openai-chat": "OpenAI Chat Completions",
  anthropic: "Anthropic Messages",
};

// 与后端 provider.Compatible 保持一致。
export const COMPATIBLE = {
  codex: ["openai-responses"],
  "claude-code": ["anthropic"],
  dsh: ["openai-chat", "openai-responses", "anthropic"],
};

export const AUTHS = { bearer: "Authorization: Bearer", "x-api-key": "x-api-key 请求头" };

export const STATES = {
  running: { label: "运行中", tone: "ok" },
  stopped: { label: "已停止", tone: "idle" },
  not_created: { label: "未启动过", tone: "idle" },
  partial: { label: "状态异常", tone: "warn" },
  unknown: { label: "未知", tone: "idle" },
};

// 门卫事件的中文说明（reason_code → 文案）。
export const REASONS = {
  allowed: "已放行", completed: "完成", config_applied: "策略与密钥已下发",
  default_deny: "不在允许列表，已拦截", rule_denied: "命中拒绝规则", credential_foreign: "携带了外来密钥（可能是外传）",
  credential_missing: "缺少占位符密钥", credential_mismatch: "密钥请求头格式不符", connection_header_denied: "可疑的 Connection 请求头",
  target_mismatch: "请求目标与隧道不一致", plain_http_denied: "禁止明文 HTTP", port_denied: "只允许 443 端口",
  ip_literal_denied: "禁止直接访问 IP 地址", body_too_large: "请求体过大", redirect_denied: "拦截了跳转",
  request_quota_exceeded: "已达到请求次数上限", token_quota_exceeded: "已达到 token 上限",
  request_quota_80: "请求次数已用到 80%", token_quota_80: "token 已用到 80%",
  upstream_failed: "连不上上游服务", destination_denied: "目标不是公网地址", upstream_status: "上游返回了错误",
  closed_early: "连接提前结束", stream_interrupted: "数据流中断", not_configured: "门卫尚未配置",
  cursor_from_other_run: "日志有缺口（门卫已重启）", buffer_overflow: "日志有缺口（应用离线太久）",
  mcp_forwarded: "经门卫转给 MCP 服务", mcp_tool_denied: "工具不在允许列表，已拦截", mcp_method_denied: "不允许的 MCP 方法，已拦截",
  mcp_unknown_service: "未启用的 MCP 服务", mcp_invalid_message: "MCP 消息格式错误或有歧义，已拦截",
  mcp_invalid_reply: "MCP 服务的回复无法检查，没有转交", upstream_unauthorized: "MCP 服务不接受保存的凭据，需要重新登录或更新令牌",
  mcp_token_in_reply: "MCP 服务的回复里带有服务令牌，已拦下",
  client_closed: "Agent 没等回答就断开了（通常是正在退出）",
};

export const DECISIONS = {
  inject: { label: "已注入密钥", tone: "ok" }, allow: { label: "放行", tone: "ok" }, deny: { label: "拦截", tone: "bad" },
  error: { label: "出错", tone: "warn" }, "not-applicable": { label: "系统", tone: "idle" },
};

export const formatTokens = (n) => (n >= 1e6 ? `${(n / 1e6).toFixed(1)}M` : n >= 1e4 ? `${(n / 1e3).toFixed(1)}k` : String(n ?? 0));
export const formatBytes = (n) => (n >= 1 << 30 ? `${(n / (1 << 30)).toFixed(1)} GiB` : n >= 1 << 20 ? `${Math.round(n / (1 << 20))} MiB` : `${Math.round((n ?? 0) / 1024)} KB`);

// 确认对话框。桌面窗口（WKWebView）不支持浏览器自带的 confirm()，它会直接返回“取消”。
export function ConfirmDialog({ title, message, confirmLabel = "删除", busy, onConfirm, onClose }) {
  return html`
    <div class="overlay" onClick=${(e) => e.target === e.currentTarget && !busy && onClose()}>
      <div class="dialog" role="alertdialog" aria-modal="true" aria-label=${title}>
        <div class="dialog-heading"><h2>${title}</h2></div>
        <p>${message}</p>
        <div class="dialog-actions">
          <button data-dismiss onClick=${onClose} disabled=${busy}>取消</button>
          <button class="danger-solid" onClick=${onConfirm} disabled=${busy}>${busy ? "处理中…" : confirmLabel}</button>
        </div>
      </div>
    </div>`;
}
