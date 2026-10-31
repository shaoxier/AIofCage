import { useEffect, useState } from "preact/hooks";
import { html, call, formatBytes, REASONS, STATES, Icon } from "./core.js";

const KINDS = { agent: "智能体环境", gate: "出网代理", mcp: "MCP 连接器" };
const COMPONENT_STATES = { running: "运行中", exited: "已停止", created: "待启动", restarting: "重启中", paused: "已暂停", dead: "异常退出", missing: "未创建", unknown: "未采集" };
const date = (value) => value && !value.startsWith("0001") ? new Date(value).toLocaleString() : "—";
const bytes = (value) => value ? formatBytes(value) : "0 B";

export function HealthPanel({ sandbox, vm, events = [] }) {
  const [report, setReport] = useState(null);
  const [security, setSecurity] = useState(null);
  const [error, setError] = useState("");
  const [now, setNow] = useState(Date.now());
  const [refresh, setRefresh] = useState(0);
  useEffect(() => {
    let active = true;
    let pending = false;
    const load = async () => {
      if (pending) return;
      pending = true;
      try { const value = await call("SandboxHealth", sandbox.id); if (active) { setReport(value); setError(""); setNow(Date.now()); } }
      catch (e) { if (active) { setError(e.message); setNow(Date.now()); } }
      finally { pending = false; }
    };
    setReport(null);
    setSecurity(null);
    load();
    call("SecurityDiagnostics", sandbox.id).then((value) => active && setSecurity(value)).catch(() => {});
    const off = window.runtime?.EventsOn?.("sandbox:stats", load);
    const timer = setInterval(load, 5000);
    return () => { active = false; clearInterval(timer); off?.(); };
  }, [sandbox.id, refresh]);
  const h = report?.current;
  const s = h?.stats;
  const stale = h && now - new Date(h.collected_at).getTime() > 20000;
  const valid = s?.valid && !stale;
  const recent = events.filter((e) => e.session_id === sandbox.id && (e.model_request || e.action === "mcp.call")).sort((a,b) => new Date(b.ts)-new Date(a.ts)).slice(0,6);
  const state = STATES[h?.state] || STATES.unknown;
  return html`<div class="health-page">
    <div class="section-heading"><div class="health-status"><h3>运行状态</h3>${h && html`<span class=${`pill ${state.tone}`}>${state.label}</span>`}</div><button onClick=${() => setRefresh((n) => n + 1)}><${Icon} name="refresh" size=${18} />刷新</button></div>
    ${error && html`<p class="feedback error" role="alert">${error}</p>`}
    ${!h ? html`<div class="panel empty">正在读取运行状态…</div>` : html`
      <p class="caption health-stamp">${stale ? "样本已过期 · " : ""}采集于 ${date(h.collected_at)} · 耗时 ${(h.duration_ms/1000).toFixed(1)} 秒 · 约每 5 秒更新</p>
      ${(h.error || stale || s.error) && html`<p class="feedback ${h.state === "stopped" || h.state === "not_created" ? "" : "error"}" role="status">${h.error || (stale ? "暂未收到新样本，以下资源值不可用。" : s.error)}</p>`}
      ${security && html`<div class="panel security-diagnostics" aria-label="安全诊断中心">
        <div class="card-top"><h3>安全诊断中心</h3><span class=${`pill ${security.gatekeeper_ready ? "ok" : security.gatekeeper_running ? "warn" : "idle"}`}>${security.gatekeeper_ready ? "门卫已就绪" : security.gatekeeper_running ? "门卫未确认" : "门卫未运行"}</span></div>
        ${security.gatekeeper_error && html`<p class="caption">${security.gatekeeper_error}</p>`}
        <div class="security-grid">${security.controls.map((item) => html`<div class="security-item"><strong>${item.name}</strong><span class="pill idle">边界说明</span><small>${item.detail}</small></div>`)}</div>
        <div class="section-heading compact"><h4>MCP 权限</h4><span class="caption">最近拒绝 ${security.recent_denied} 次</span></div>
        ${security.mcp.length ? html`<div class="security-mcp">${security.mcp.map((item) => html`<div class="security-item"><strong>${item.name}</strong><small>${item.connector ? "连接器" : "远程服务"}工具 ${item.granted_tools.length}/${item.catalog_tools.length}${item.explicit_grant ? "（沙箱专属）" : item.connector ? "（默认目录）" : "（默认只读）"} · ${item.key_saved ? "凭据可用" : "需要登录或配置"}</small></div>`)}</div>` : html`<p class="caption">当前沙箱未启用 MCP 服务。</p>`}
        ${security.last_isolation && html`<p class="caption">最近一次隔离检查：${new Date(security.last_isolation.finished_at).toLocaleString()}，${security.last_isolation.results.filter((item) => item.status === "passed").length}/${security.last_isolation.results.length} 项通过。</p>`}
      </div>`}
      <div class="metric-grid">
        <div class="panel metric"><span><${Icon} name="cpu" />CPU</span><strong>${valid ? `${s.cpu_percent.toFixed(1)}%` : "—"}</strong><small>100% = 1 个核心</small></div>
        <div class="panel metric"><span><${Icon} name="memory" />内存</span><strong>${valid ? bytes(s.memory_bytes) : "—"}</strong><small>所有运行组件之和</small></div>
        <div class="panel metric"><span><${Icon} name="activity" />进程 / 线程</span><strong>${valid ? s.pids : "—"}</strong><small>Docker PIDs</small></div>
        <div class="panel metric"><span><${Icon} name="shield" />代理管理接口</span><strong class="health-value">${stale ? "样本过期" : h.gate_api ? "可响应" : h.gate_error ? "无响应" : "未检测"}</strong><small>不发送模型计费请求</small></div>
      </div>
      <div class="health-charts"><${Trend} history=${report.history} field="cpu_percent" label="CPU 趋势" format=${(v) => `${v.toFixed(1)}%`} minimum=${100} /><${Trend} history=${report.history} field="memory_bytes" label="内存趋势" format=${bytes} minimum=${1 << 20} /></div>
      <div class="section-heading compact"><h3>组件</h3><span class="caption">资源上限按组件独立执行</span></div>
      <div class="panel data-scroll" tabindex="0" role="region" aria-label="沙箱组件状态表格"><table class="data-table component-table"><thead><tr><th>组件 / 状态</th><th>CPU / 上限</th><th>内存 / 上限</th><th>退出与恢复</th><th>健康检查</th></tr></thead><tbody>
        ${h.components.map((c) => html`<tr><td><strong>${KINDS[c.kind] || c.kind}</strong><small class="mono">${c.kind === "mcp" ? c.name.split("-mcp-")[1] : c.id?.slice(0,12)}</small><span class=${`pill ${c.running ? "ok" : c.state === "unknown" ? "warn" : "idle"}`}>${COMPONENT_STATES[c.state] || c.state}</span>${c.error && html`<small class="danger">${c.error}</small>`}</td>
          <td>${c.stats?.valid && !stale ? `${c.stats.cpu_percent.toFixed(1)}%` : "—"}<small>${!c.id ? "—" : c.cpu_limit > 0 ? `${c.cpu_limit} 核` : "未单独限制"}</small></td>
          <td>${c.stats?.valid && !stale ? bytes(c.stats.memory_bytes) : "—"}<small>${!c.id ? "—" : c.memory_limit ? bytes(c.memory_limit) : "未单独限制"}</small>${c.stats?.error && html`<small class="danger">${c.stats.error}</small>`}</td>
          <td>${c.oom_killed ? html`<strong class="danger">内存不足被终止</strong>` : c.running ? "运行中" : c.id ? `退出码 ${c.exit_code}` : "—"}<small>${c.id ? `重启 ${c.restarts} 次` : "暂无容器记录"}</small><details><summary>时间与 I/O</summary><small>启动 ${date(c.started_at)}</small><small>停止 ${date(c.finished_at)}</small><small>PID 上限 ${c.pids_limit > 0 ? c.pids_limit : "未单独限制"}</small><small>网络 接收 / 发送 ${c.stats?.valid && !stale ? `${bytes(c.stats.network_in)} / ${bytes(c.stats.network_out)}` : "—"}</small><small>磁盘 读取 / 写入 ${c.stats?.valid && !stale ? `${bytes(c.stats.block_read)} / ${bytes(c.stats.block_write)}` : "—"}</small></details></td>
          <td>${c.health}</td></tr>`)}
      </tbody></table></div>
      <p class="caption">I/O 为当前容器的累计计数，重建后归零。共享项目的磁盘空间不等于容器磁盘 I/O。</p>
    `}
    <div class="section-heading compact"><h3>最近调用</h3><span class="caption">仅展示真实模型与 MCP 事件</span></div>
    <div class="panel health-calls">${recent.length ? recent.map((e) => html`<div><span><strong>${e.action === "mcp.call" ? `MCP · ${e.service_id}` : e.provider_name || e.secret_ref}</strong><small>${date(e.ts)} · ${e.model || e.tool_id || ""}</small></span><span class=${`pill ${e.decision === "error" || e.status >= 400 ? "warn" : e.decision === "deny" ? "bad" : "ok"}`}>${REASONS[e.reason_code] || e.reason_code}${e.status ? ` · ${e.status}` : ""}</span></div>`) : html`<p class="caption">暂无调用记录。容器运行并不代表模型或 MCP 已连接成功。</p>`}</div>
    ${vm && html`<details class="health-vm"><summary>专用虚拟机 · ${vm.name}</summary><div class="panel"><strong>已配置 ${vm.cpus} 核 · ${bytes(vm.memory)} 内存 · ${bytes(vm.disk)} 磁盘</strong><p class="caption">${vm.status === "Running" ? "运行中" : vm.status} · ${vm.arch} · 所有沙箱共享以上容量。这是虚拟机配置容量，实时使用量见上方沙箱组件。</p></div></details>`}
  </div>`;
}

function Trend({ history = [], field, label, format, minimum }) {
  const available = history.filter((p) => p.valid);
  const max = Math.max(minimum, ...available.map((p) => p[field]));
  const start = history.length ? new Date(history[0].collected_at).getTime() : 0;
  const end = history.length ? new Date(history[history.length-1].collected_at).getTime() : start;
  let path = "", previous = null;
  for (const point of history) {
    if (!point.valid) { previous = null; continue; }
    const time = new Date(point.collected_at).getTime();
    const x = 4 + 392*(time-start)/Math.max(end-start,1);
    const y = 86 - 78*point[field]/max;
    path += `${previous && time-previous <= 12000 ? "L" : "M"}${x.toFixed(2)},${y.toFixed(2)} `;
    // 单个样本也可见；无效样本之间保留断点。
    path += `l0.15,0 `;
    previous = time;
  }
  return html`<div class="panel trend"><div><h4>${label}</h4><span class="caption">0 — ${format(max)}</span></div><svg viewBox="0 0 400 96" preserveAspectRatio="none" role="img" aria-label=${`${label}，${available.length} 个有效样本，缺失处断开`}><path d="M4,86 H396 M4,47 H396 M4,8 H396" class="trend-grid" /><path d=${path} class="trend-line" /></svg><p class="caption">${history.length ? `${new Date(start).toLocaleTimeString()} — ${new Date(end).toLocaleTimeString()}` : "等待采样"} · 最近 ${history.length}/120 个样本 · 应用重开后重新记录</p></div>`;
}
