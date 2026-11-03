import { useEffect, useState } from "preact/hooks";
import { html, call, agentName, formatTokens, Icon, PageTitle } from "./core.js";

export const money = (values) => Object.entries(values || {}).map(([currency,value]) => `${currency} ${Number(value).toFixed(4)}`).join(" · ") || "—";
const COST_STATES = { "price-missing":"未设置单价", "usage-unknown":"用量未知", "model-mismatch":"返回模型与定价模型不一致", "usage-invalid":"缓存用量不完整" };

export function UsagePage({ sandboxId = "", sandboxes = [], providers = [], embedded = false }) {
  const [period, setPeriod] = useState("today");
  const [sandbox, setSandbox] = useState(sandboxId);
  const [provider, setProvider] = useState("");
  const [report, setReport] = useState(null);
  const [error, setError] = useState("");
  const [refresh, setRefresh] = useState(0);
  useEffect(() => {
    let current = true;
    setError("");
    call("QueryUsage", { sandbox_id: sandboxId || sandbox, provider_id: provider, period }).then((r) => { if (current) setReport(r); }).catch((e) => { if (current) setError(e.message); });
    return () => { current = false; };
  }, [sandboxId, sandbox, provider, period, refresh]);
  useEffect(() => window.runtime?.EventsOn?.("sandbox:event", () => setRefresh((n) => n + 1)), []);
  return html`<section class="page usage-page">
    ${!embedded && html`<div class="section-heading"><${PageTitle} icon="chart" title="用量与费用" description="查看模型消耗与费用估算。" /></div>`}
    <div class="toolbar"><div class="segmented">${[{id:"today",label:"今日"},{id:"week",label:"近七天"},{id:"all",label:"全部"}].map((p) => html`<button aria-pressed=${period === p.id} class=${period === p.id ? "active" : ""} onClick=${() => setPeriod(p.id)}>${p.label}</button>`)}</div>
      ${!sandboxId && html`<select aria-label="按沙箱筛选" value=${sandbox} onChange=${(e) => setSandbox(e.target.value)}><option value="">全部沙箱（含已删除）</option>${sandboxes.map((s) => html`<option value=${s.id}>${s.name}</option>`)}</select>`}
      <select aria-label="按供应商筛选" value=${provider} onChange=${(e) => setProvider(e.target.value)}><option value="">全部供应商</option>${providers.map((p) => html`<option value=${p.id}>${p.name}</option>`)}</select>
      <button onClick=${() => setRefresh((n) => n + 1)}><${Icon} name="refresh" size=${18} />刷新</button></div>
    ${error && html`<p class="feedback error" role="alert">${error}</p>`}
    ${!report ? html`<div class="panel empty">正在读取用量…</div>` : html`
      <div class="metric-grid"><div class="panel metric"><span><${Icon} name="activity" />模型请求</span><strong>${report.requests}</strong></div><div class="panel metric"><span><${Icon} name="layers" />已知 token</span><strong>${formatTokens(report.groups.reduce((n,g) => n + g.input_tokens + g.output_tokens,0))}</strong></div><div class="panel metric"><span><${Icon} name="coins" />已估算费用</span><strong class="cost">${money(report.costs)}</strong></div><div class="panel metric"><span><${Icon} name="clock" />待补全</span><strong>${report.unpriced}</strong><small>含用量未知 ${report.unknown} 次</small></div></div>
      ${report.legacy && html`<p class="caption">旧累计仍保留在沙箱概览；以下按本版本记录的请求明细统计，不能代表完整历史账单。</p>`}
      <div class="section-heading compact"><h3>供应商与模型</h3><span class="caption">缓存已包含在总输入中 · 费用按币种分别汇总</span></div>
      <div class="panel data-scroll" tabindex="0" role="region" aria-label="供应商与模型用量表格"><table class="data-table"><thead><tr><th>供应商 / 模型</th><th>请求</th><th>输入 / 输出</th><th>缓存读 / 写</th><th>估算费用</th></tr></thead><tbody>
        ${report.groups.map((g) => html`<tr><td><strong>${g.provider_name || g.provider_id}</strong><small>${agentName(g.agent)} · ${g.model || "未知模型"}</small></td><td>${g.requests}</td><td>${g.unknown === g.requests ? "用量未知" : `${formatTokens(g.input_tokens)} / ${formatTokens(g.output_tokens)}`}${g.unknown > 0 && g.unknown < g.requests && html`<small>另有 ${g.unknown} 次用量未知</small>`}</td><td>${g.unknown === g.requests ? "—" : `${formatTokens(g.cache_read_tokens)} / ${formatTokens(g.cache_write_tokens)}`}</td><td>${money(g.costs)}${g.unpriced > 0 && html`<small>${g.unpriced} 次未估算</small>`}</td></tr>`)}
        ${!report.groups.length && html`<tr><td colspan="5" class="empty">此范围暂无模型请求。智能体完成任务后会在这里显示。</td></tr>`}</tbody></table></div>
      <details class="usage-records"><summary>最近请求明细（最多 100 条）</summary><div class="panel data-scroll" tabindex="0" role="region" aria-label="最近请求用量明细表格"><table class="data-table"><thead><tr><th>时间 / 沙箱</th><th>供应商 / 模型</th><th>输入 / 输出</th><th>结果</th><th>估算费用</th></tr></thead><tbody>${report.rows.map((r) => html`<tr><td>${new Date(r.ts).toLocaleString()}<small>${r.sandbox_name}</small></td><td>${r.provider_name || r.secret_ref}<small>${r.model}</small></td><td>${r.usage_kind === "reported" ? `${formatTokens(r.input_tokens)} / ${formatTokens(r.output_tokens)}` : "用量未知"}</td><td>${r.status || "—"}<small>${r.reason_code === "completed" ? "完成" : r.reason_code}</small></td><td>${r.estimated_cost != null ? `${r.pricing.currency} ${r.estimated_cost.toFixed(6)}` : COST_STATES[r.cost_status] || "未估算"}</td></tr>`)}</tbody></table></div></details>
      <p class="caption">费用基于请求当时的单价估算，不含税费、套餐折扣或供应商账单调整。</p>`}
  </section>`;
}
