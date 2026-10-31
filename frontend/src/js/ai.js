// AI 安全解说与隔离检查。AI 只负责解释，不参与放行决定。
import { useEffect, useState } from "preact/hooks";
import { html, call, Icon } from "./core.js";
import { CHECK_STATUSES, checkCounts, checkStatus } from "./isolation-summary.js";

const RISK_TONE = { 高: "bad", 中: "warn", 低: "ok" };

export function ExplainDialog({ request, onClose }) {
  const [state, setState] = useState({ loading: true });
  useEffect(() => {
    const job = request.eventId ? call("ExplainEvent", request.sandboxId, request.eventId) : call("SummarizeSandbox", request.sandboxId);
    job.then((result) => setState({ result })).catch((error) => setState({ error: error.message }));
  }, []);
  const r = state.result;
  return html`
    <div class="overlay" onClick=${(e) => e.target === e.currentTarget && onClose()}>
      <div class="dialog" role="dialog" aria-modal="true" aria-label="AI 安全解说">
        <div class="dialog-heading"><h2>${request.eventId ? "AI 解读这条记录" : `AI 总结「${request.sandboxName}」`}</h2>
          <button type="button" class="icon-button" aria-label="关闭" onClick=${onClose}><${Icon} name="x" size=${20} /></button></div>
        ${state.loading && html`<p class="caption" role="status">正在请模型分析（只发送主机、路径、结果等元数据，不发送请求内容和密钥）…</p>`}
        ${state.error && html`<p class="form-error" role="alert">${state.error}</p>
          <p class="caption">可以在“设置”页换一个 AI 解说使用的供应商，或检查网络后重试。</p>`}
        ${r && html`<div class="explain">
          <div class="card-top"><h3>${r.title}</h3><span class=${`pill ${RISK_TONE[r.risk] ?? "idle"}`}>风险：${r.risk}</span></div>
          <p>${r.explanation}</p>
          ${r.similar && html`<p><strong>类似的攻击：</strong>${r.similar}</p>`}
          ${r.advice && html`<p><strong>建议：</strong>${r.advice}</p>`}
          <p class="caption">由「${r.provider}」生成 · ${(r.latency_ms / 1000).toFixed(1)} 秒 · AI 只做解释，放行与否由门卫规则决定。</p>
        </div>`}
      </div>
    </div>`;
}

export function AttackDialog({ sandbox, onClose, onLogs }) {
  const [attacks, setAttacks] = useState([]);
  const [results, setResults] = useState(null);
  const [running, setRunning] = useState(false);
  const [error, setError] = useState("");
  useEffect(() => { call("ListIsolationChecks").then(setAttacks).catch((e) => setError(e.message)); }, []);
  const start = async () => {
    setRunning(true);
    setError("");
    try { setResults(await call("RunIsolationChecks", sandbox.id)); } catch (e) { setError(e.message); } finally { setRunning(false); }
  };
  const list = results ?? attacks;
  return html`
    <div class="overlay" onClick=${(e) => e.target === e.currentTarget && !running && onClose()}>
      <div class="dialog wide" role="dialog" aria-modal="true" aria-label="隔离检查">
        <div class="dialog-heading"><h2>隔离检查 · 「${sandbox.name}」</h2>
          <button type="button" class="icon-button" aria-label="关闭" onClick=${onClose} disabled=${running}><${Icon} name="x" size=${20} /></button></div>
        <p class="caption">使用固定测试数据。检查时会在专用虚拟机里临时启动几个对照容器，并在 ~/CageWorkspace 下临时建一个测试目录，结束后都会删除。对照不成立的项判“无法验证”。结果只覆盖本次检查，不代表完整安全审计。</p>
        ${results && html`<div class="diagnostic-summary" role="status">${checkCounts(results).map((item) => html`<span class=${`pill ${item.tone}`}>${item.label} ${item.count}</span>`)}</div>`}
        <div class="attack-list">
          ${list.map((a) => { const status = CHECK_STATUSES[checkStatus(a)]; return html`<article class=${`attack ${results ? status.tone : ""}`} key=${a.id}>
            <div class="card-top">
              <h3>${a.title}</h3>
              ${results ? html`<span class=${`pill ${status.tone}`}>${status.label}</span>` : html`<span class="pill idle">${a.layer}</span>`}
            </div>
            ${a.evidence && html`<pre class="evidence">${a.evidence}</pre>`}
            ${a.controls?.length > 0 && html`<ul class="check-controls" aria-label="对照">${a.controls.map((c) => html`<li class=${c.ok ? "ok" : "bad"}>
              <${Icon} name=${c.ok ? "check" : "x"} size=${14} /><span>对照${c.ok ? "成立" : "不成立"}：${c.name}${c.detail && html`<small>${c.detail}</small>`}</span></li>`)}</ul>`}
            ${a.note && html`<p class="note">${a.note}</p>`}
          </article>`; })}
        </div>
        ${error && html`<p class="form-error" role="alert">${error}</p>`}
        <div class="dialog-actions">
          ${results && html`<button onClick=${onLogs}>查看日志里的拦截记录</button>`}
          <button class="primary" onClick=${start} disabled=${running}>${running ? "正在检查…" : results ? "重新检查" : "开始检查"}</button>
        </div>
      </div>
    </div>`;
}

export function ExplainerSettings({ providers, notify }) {
  const [current, setCurrent] = useState("");
  useEffect(() => { call("ExplainerProvider").then((p) => setCurrent(p.id)).catch(() => setCurrent("")); }, [providers.length]);
  const usable = providers.filter((p) => p.key_saved);
  const change = async (e) => {
    try { await call("SetExplainerProvider", e.target.value); setCurrent(e.target.value); notify("已更新 AI 解说使用的供应商"); }
    catch (err) { notify(err.message, true); }
  };
  return html`<div><dt>AI 安全解说使用的供应商</dt><dd>
    ${usable.length
      ? html`<select value=${current} onChange=${change}>${usable.map((p) => html`<option value=${p.id}>${p.name}（${p.model}）</option>`)}</select>
        <p class="caption">只发送门卫记录的主机、路径、结果等元数据，不发送请求内容和任何密钥；由桌面应用直接调用。</p>`
      : "先在“供应商”页添加一个供应商并保存密钥"}
  </dd></div>`;
}
