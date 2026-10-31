// 沙箱页：参考 Docker Desktop 的容器列表——创建、启停、打开终端、切换供应商、删除。
import { useState } from "preact/hooks";
import { html, call, AGENTS, agentName, STATES, formatTokens, formatBytes, Icon, agentIcon, PageTitle } from "./core.js";
import { cleanGrants, MCPChoices } from "./mcp.js";
import { DevSourceChoices, DevSourceDialog, useDevSources } from "./dev-sources.js";
import { AttackDialog } from "./ai.js";
import { useTerminals } from "./terminal.js";
import { SandboxDetail } from "./sandbox-detail.js";
import { OperationProgress } from "./progress.js";

export function SandboxesPage({ sandboxes, providers, services, recoverable, overview, summary, events, reload, notify, onAddProvider, onLogs, detail, setDetail, tutorial }) {
  const [query, setQuery] = useState("");
  const [filter, setFilter] = useState("all");
  const [addingAgents, setAddingAgents] = useState(null);
  const [editingMCP, setEditingMCP] = useState(null);
  const [editingDev, setEditingDev] = useState(null);
  const devSources = useDevSources();
  const [attacking, setAttacking] = useState(null);
  const [creating, setCreating] = useState(false);
  const [busy, setBusy] = useState({}); // id → 正在进行的操作
  const [deleting, setDeleting] = useState(null);
  const [resetting, setResetting] = useState(null);

  const run = async (sandbox, label, method, ...args) => {
    setBusy((current) => ({ ...current, [sandbox.id]: label }));
    try {
      await call(method, ...args);
      return true;
    } catch (error) {
      // 按要求停止的操作不算失败；后端会说明沙箱停在什么状态。
      const stopped = error.message.startsWith("已停止");
      notify(stopped ? `「${sandbox.name}」${error.message}` : `「${sandbox.name}」${label}失败：${error.message}`, !stopped);
      return false;
    } finally {
      setBusy((current) => ({ ...current, [sandbox.id]: undefined }));
      await reload();
    }
  };

  const dialogs = html`
      ${addingAgents && html`<${AddAgentsDialog} sandbox=${addingAgents} onClose=${() => setAddingAgents(null)} onAdd=${async (agents) => {
        const target = addingAgents; setAddingAgents(null);
        if (await run(target, "添加智能体", "AddSandboxAgents", target.id, agents)) notify("智能体已添加，请选择供应商");
      }} />`}
      ${attacking && html`<${AttackDialog} sandbox=${attacking} onClose=${() => setAttacking(null)}
        onLogs=${() => { const id = attacking.id; setAttacking(null); onLogs(id, "deny"); }} />`}
      ${editingMCP && html`<${MCPDialog} sandbox=${editingMCP} services=${services} onClose=${() => setEditingMCP(null)}
        onSave=${async (ids, filesystem, permissions) => {
          const target = editingMCP;
          setEditingMCP(null);
          if (await run(target, "更新 MCP", "SetSandboxMCPPermissions", target.id, ids, filesystem, permissions)) notify(`已更新「${target.name}」的 MCP 服务和工具权限`);
        }} />`}
      ${editingDev && html`<${DevSourceDialog} sandbox=${editingDev} sources=${devSources} onClose=${() => setEditingDev(null)}
        onSave=${async (sources) => {
          const target = editingDev;
          setEditingDev(null);
          if (await run(target, "更新开发环境", "SetSandboxDevSources", target.id, sources)) notify(`已更新「${target.name}」的开发环境`);
        }} />`}
      ${creating && html`<${CreateSandboxDialog} providers=${providers} services=${services} devSources=${devSources} overview=${overview} onAddProvider=${onAddProvider} tutorial=${tutorial}
        onClose=${() => setCreating(false)} onCreated=${async (created, start) => {
          setCreating(false);
          notify(`已创建沙箱「${created.name}」`);
          await reload();
          if (tutorial) await tutorial.sandboxCreated(created);
          setDetail({ id: created.id, tab: start ? "terminal" : "overview" });
          if (start && await run(created, "启动", "StartSandbox", created.id)) setDetail({ id: created.id, tab: "terminal", open: { agent: created.bindings[0]?.agent ?? "", nonce: Date.now() } });
        }} />`}
      ${resetting && html`<${ResetDialog} sandbox=${resetting} onClose=${() => setResetting(null)} onConfirm=${async (keepHome, restart) => {
          const target = resetting;
          setResetting(null);
          if (await run(target, "重置", "ResetSandbox", target.id, keepHome, restart)) {
            notify(restart ? `已重置并恢复运行「${target.name}」` : `已重置「${target.name}」，项目文件夹保留`);
          }
        }} />`}
      ${deleting && html`<${DeleteDialog} sandbox=${deleting} onClose=${() => setDeleting(null)} onConfirm=${async (keepHome) => {
          const target = deleting;
          setDeleting(null);
          if (await run(target, "删除", "DeleteSandbox", target.id, keepHome)) {
            notify(`已删除沙箱「${target.name}」，项目文件夹保留`);
            if (detail?.id === target.id) setDetail(null);
          }
        }} />`}`;

  const current = detail && sandboxes.find((s) => s.id === detail.id);
  if (current) {
    return html`<${SandboxDetail} sandbox=${current} detail=${detail} setDetail=${setDetail} providers=${providers}
      services=${services} recoverable=${recoverable} events=${events} vm=${overview?.environment?.vm} usage=${summary?.sandboxes?.find((u) => u.id === current.id)}
      busy=${busy[current.id] ?? current.operation} run=${run} notify=${notify}
      onAddAgents=${() => setAddingAgents(current)} onEditMCP=${() => setEditingMCP(current)} onAttack=${() => setAttacking(current)}
      devSources=${devSources} onEditDev=${() => setEditingDev(current)}
      onReset=${() => setResetting(current)} onDelete=${() => setDeleting(current)} onLogs=${(filter) => onLogs(current.id, filter)} dialogs=${dialogs} />`;
  }

  const visible = sandboxes.filter((s) => `${s.name} ${s.project_path}`.toLowerCase().includes(query.toLowerCase()) && (filter === "all" || (filter === "running" ? s.state === "running" : s.state !== "running")));
  return html`
    <section class="page">
      <div class="section-heading">
        <${PageTitle} icon="box" title="沙箱" description="管理项目、智能体和运行状态。" />
        <button class="primary" data-guide="create-sandbox" onClick=${() => setCreating(true)} disabled=${!overview || tutorial?.saving}><${Icon} name="plus" />创建沙箱</button>
      </div>
      ${sandboxes.length > 0 && html`<div class="toolbar sandbox-toolbar">
        <div class="search-field"><${Icon} name="search" /><input type="search" aria-label="搜索沙箱" placeholder="搜索名称或项目目录" value=${query} onInput=${(e) => setQuery(e.target.value)} /></div>
        <div class="segmented" aria-label="沙箱状态">
          ${[{ id: "all", label: `全部 ${sandboxes.length}` }, { id: "running", label: `运行中 ${sandboxes.filter((s) => s.state === "running").length}` }, { id: "stopped", label: "未运行" }].map((f) => html`<button aria-pressed=${filter === f.id} class=${filter === f.id ? "active" : ""} onClick=${() => setFilter(f.id)}>${f.label}</button>`)}
        </div></div>`}
      ${sandboxes.length === 0
        ? html`<div class="panel empty">
            <span class="empty-icon"><${Icon} name="box" size=${40} /></span>
            <h3>还没有沙箱</h3>
            <p>创建一个工作区，按需添加智能体。</p>
            <button class="primary" onClick=${() => setCreating(true)} disabled=${!overview}>创建第一个沙箱</button>
          </div>`
        : html`<div class="sandbox-list">
            ${visible.map((s) => html`<${SandboxRow} key=${s.id} sandbox=${s} providers=${providers}
              usage=${summary?.sandboxes?.find((u) => u.id === s.id)} onLogs=${() => onLogs(s.id)}
              services=${services} onEditMCP=${() => setEditingMCP(s)} onAttack=${() => setAttacking(s)}
              busy=${busy[s.id] ?? s.operation} run=${run} notify=${notify}
              onReset=${() => setResetting(s)} onDelete=${() => setDeleting(s)} onOpen=${(view) => setDetail({ id: s.id, ...view })} />`)}
            ${!visible.length && html`<div class="panel empty"><span class="empty-icon"><${Icon} name="search" size=${40} /></span><h3>没有匹配的沙箱</h3><p>试试其他名称或状态。</p><button onClick=${() => { setQuery(""); setFilter("all"); }}>重置筛选</button></div>`}
          </div>`}
      <${EnvironmentPanel} environment=${overview?.environment} />
      ${dialogs}
    </section>`;
}

// 沙箱行与详情页共用的几块内容。
export function BindingSelects({ sandbox, providers, busy, run, notify }) {
  const running = sandbox.state === "running";
  const bind = (agent) => async (event) => {
    const providerID = event.target.value;
    if (await run(sandbox, "切换供应商", "BindProvider", sandbox.id, agent, providerID)) {
      notify(running ? `已切换：${agentName(agent)} 下次重启沙箱后使用新的供应商` : `已切换 ${agentName(agent)} 的供应商`);
    }
  };
  return html`<div class="bindings">
    ${!sandbox.bindings.length && html`<span class="caption">空白沙箱 · 可使用 Shell</span>`}
    ${sandbox.bindings.map((b) => html`<label class="binding-select">
      <span>${agentName(b.agent)}${b.version && html`<small class="version">${b.version}</small>`}</span>
      <select value=${b.provider_id} onChange=${bind(b.agent)} disabled=${Boolean(busy)}>
        <option value="">未配置供应商</option>
        ${providers.filter((p) => p.agent === b.agent).map((p) => html`<option value=${p.id}>${p.name}</option>`)}
      </select>
    </label>`)}
    ${sandbox.config_pending && html`<p class="caption pending">供应商选择已保存，重启沙箱后生效。当前任务继续使用原配置。</p>`}
  </div>`;
}

export function MCPLine({ sandbox, services, busy, onEditMCP }) {
  return html`<div class="mcp-line">
    <span class="faint">MCP</span>
    ${sandbox.filesystem_mcp && html`<span class="binding">filesystem</span>`}
    ${sandbox.mcp.map((id) => html`<span class="binding">${services.find((s) => s.id === id)?.name ?? id}</span>`)}
    ${!sandbox.filesystem_mcp && sandbox.mcp.length === 0 && html`<span class="faint">未启用</span>`}
    <button class="link" onClick=${onEditMCP} disabled=${Boolean(busy)}>设置</button>
  </div>`;
}

export function UsageStats({ sandbox, usage, onLogs }) {
  const maxRequests = usage?.max_requests || sandbox.limits.max_requests;
  const maxTokens = usage?.max_tokens || sandbox.limits.max_tokens;
  return html`<div class="usage">
    <span><strong>${usage?.requests ?? sandbox.usage.requests}</strong> 次请求${maxRequests ? ` / ${maxRequests}` : ""}
      ${quotaPercent(usage?.requests, maxRequests)}</span>
    <span><strong>${formatTokens(usage?.tokens ?? sandbox.usage.tokens)}</strong> token${maxTokens ? ` / ${formatTokens(maxTokens)}` : ""}
      ${quotaPercent(usage?.tokens, maxTokens)}</span>
    ${usage?.stats?.valid && html`<span class="resource-usage"><span>CPU ${usage.stats.cpu_percent.toFixed(1)}%</span><span>内存 ${formatBytes(usage.stats.memory_bytes)}</span></span>`}
    ${onLogs && html`<button class="link" onClick=${onLogs}>${usage?.denied ? html`<span class="count bad">拦截 ${usage.denied}</span> ` : ""}查看日志</button>`}
  </div>`;
}

function SandboxRow({ sandbox, providers, usage, busy, run, notify, onReset, onDelete, onOpen }) {
  const state = STATES[sandbox.state] ?? STATES.unknown;
  const running = sandbox.state === "running";
  const openTerminals = useTerminals(sandbox.id).filter((t) => !t.exited);
  const enter = async () => {
    if (!running && !await run(sandbox, "启动", "StartSandbox", sandbox.id)) return;
    onOpen({ tab: "terminal", open: openTerminals.length ? undefined : { agent: sandbox.bindings.find((b) => b.provider_id)?.agent ?? "", nonce: Date.now() } });
  };
  return html`<article class="panel sandbox-row">
    <div class="sandbox-identity"><span class=${`sandbox-avatar ${running ? "running" : ""}`}><${Icon} name="box" size=${28} /></span><div class="sandbox-main">
      <div class="sandbox-title"><span class=${`status-dot ${state.tone}`} aria-hidden="true"></span>
        <button class="title-link" onClick=${() => onOpen({ tab: "overview" })}><h3>${sandbox.name}</h3></button>
        <span class=${`pill ${busy ? "warn" : state.tone}`}>${busy ? `${busy}中…` : state.label}</span></div>
      <p class="mono project-path" title=${sandbox.project_path}>${sandbox.project_path}</p>
      <div class="agent-summary">${sandbox.bindings.length ? sandbox.bindings.map((b) => html`<span class="binding"><${Icon} name=${agentIcon(b.agent)} size=${16} />${agentName(b.agent)}<span class="muted"> · ${b.provider_name || "待配置"}</span></span>`) : html`<span class="caption">空白沙箱</span>`}</div>
      ${sandbox.config_pending && html`<span class="caption">配置待重启生效</span>`}
      ${sandbox.activity ? html`<${OperationProgress} sandboxId=${sandbox.id} activity=${sandbox.activity} compact notify=${notify} />`
        : busy && html`<p class="progress" role="status">正在处理…</p>`}
    </div></div>
    <${UsageStats} sandbox=${sandbox} usage=${usage} />
    <div class="row-actions">
      <button class="primary" onClick=${enter} disabled=${Boolean(busy)}><${Icon} name=${running ? "terminal" : "play"} size=${18} />${running ? (openTerminals.length ? "继续工作" : "打开") : "启动并打开"}</button>
      <details class="action-menu"><summary aria-label=${`更多操作：${sandbox.name}`}><${Icon} name="more" /></summary><div class="menu-items">
        <button onClick=${() => onOpen({ tab: "agents" })}><${Icon} name="cpu" size=${18} />管理智能体</button>
        <button onClick=${() => onOpen({ tab: "overview" })}><${Icon} name="box" size=${18} />沙箱详情</button>
        <button onClick=${() => call("OpenProjectFolder", sandbox.id).catch((e) => notify(e.message, true))}><${Icon} name="folder" size=${18} />在访达中打开</button>
        ${running && html`<button disabled=${Boolean(busy)} onClick=${() => run(sandbox, "停止", "StopSandbox", sandbox.id)}><${Icon} name="stop" size=${18} />停止沙箱</button>`}
        <button disabled=${Boolean(busy)} onClick=${onReset}>重置运行环境…</button>
        <button class="danger" disabled=${Boolean(busy)} onClick=${onDelete}><${Icon} name="trash" size=${18} />删除沙箱…</button>
      </div></details>
    </div></article>`;
}

function CreateSandboxDialog({ providers, services, devSources, overview, onClose, onCreated, onAddProvider, tutorial }) {
  const workspace = overview?.workspace ?? "";
  const [name, setName] = useState("");
  const [path, setPath] = useState("");
  const [mode, setMode] = useState("new");
  const [agents, setAgents] = useState(() => tutorial ? { [tutorial.state.agent]: true } : {});
  const [bindings, setBindings] = useState(() => {
    const first = (agent) => providers.find((p) => p.agent === agent)?.id ?? "";
    return Object.fromEntries(AGENTS.map((a) => [a.id, tutorial?.state.agent === a.id ? tutorial.state.provider_id : first(a.id)]));
  });
  const [mcp, setMcp] = useState([]);
  const [mcpPermissions, setMcpPermissions] = useState({});
  const [filesystem, setFilesystem] = useState(false);
  const [dev, setDev] = useState([]);
  const [maxRequests, setMaxRequests] = useState("");
  const [maxTokens, setMaxTokens] = useState("");
  const [error, setError] = useState("");
  const [saving, setSaving] = useState(false);
  const inside = path !== "" && (path === workspace || path.startsWith(workspace + "/"));

  const choose = async () => {
    try {
      const picked = await call("SelectProject");
      if (!picked) return;
      setPath(picked);
      const isInside = picked.startsWith(workspace + "/");
      setMode(isInside ? "use" : "copy");
      if (!name) setName(picked.split("/").filter(Boolean).at(-1) ?? "");
    } catch (e) {
      setError(e.message);
    }
  };
  const submit = async (start) => {
    setSaving(true);
    setError("");
    try {
      const selected = AGENTS.filter((a) => agents[a.id]);
      if (tutorial && (!agents[tutorial.state.agent] || bindings[tutorial.state.agent] !== tutorial.state.provider_id)) {
        throw new Error(`请为本次教程选择 ${agentName(tutorial.state.agent)}，并绑定刚才的供应商。`);
      }
      const created = await call("CreateSandbox", {
        name, project_path: mode === "new" ? "" : path, project_mode: mode,
        bindings: selected.map((a) => ({ agent: a.id, provider_id: bindings[a.id] })),
        max_requests: Number(maxRequests) || 0, max_tokens: Number(maxTokens) || 0, mcp, mcp_permissions: mcpPermissions, filesystem_mcp: filesystem,
        dev_sources: dev,
      });
      onCreated(created, start);
    } catch (e) {
      setError(e.message);
    } finally {
      setSaving(false);
    }
  };
  return html`
    <div class="overlay" onClick=${(e) => e.target === e.currentTarget && !saving && onClose()}>
      <div class="dialog wide" role="dialog" aria-modal="true" aria-label="创建沙箱">
        <div class="dialog-heading"><h2>创建沙箱</h2>
          <button type="button" class="icon-button" aria-label="关闭" disabled=${saving} onClick=${onClose}><${Icon} name="x" size=${20} /></button></div>
        ${tutorial && html`<p class="caption">已选好本次教程的智能体和供应商，填写名称即可创建。下一步会引导启动。</p>`}
        <form onSubmit=${(e) => { e.preventDefault(); submit(!tutorial); }}>
          <label for="s-name">名称</label>
          <input id="s-name" required maxlength="60" value=${name} onInput=${(e) => setName(e.target.value)} placeholder="例如：我的网站" />

          <label>项目文件夹</label>
          <div class="choice-group">
            <label class="check"><input type="radio" name="mode" checked=${mode === "new"} onChange=${() => setMode("new")} />
              在工作区新建一个空文件夹</label>
            <label class="check"><input type="radio" name="mode" checked=${mode !== "new"} onChange=${() => setMode(inside ? "use" : "copy")} />
              使用已有文件夹</label>
          </div>
          ${mode !== "new" && html`<div class="input-row">
              <input readonly class="mono" value=${path} placeholder="请选择文件夹" />
              <button type="button" onClick=${choose}>选择…</button>
            </div>
            ${path && !inside && html`<p class="caption">这个文件夹不在工作区里。沙箱只能看到 <span class="mono">~/CageWorkspace</span>，
              所以会先<strong>复制一份</strong>进去，原文件夹不受影响。</p>`}
            ${path && inside && html`<p class="caption">直接使用这个文件夹，Agent 可以修改其中的文件。</p>`}`}

          <label>智能体（可选，之后也能添加）</label>
          <div class="agent-pick">
            ${AGENTS.map((a) => {
              const options = providers.filter((p) => p.agent === a.id);
              return html`<div class="agent-line">
                <label class="check"><input type="checkbox" checked=${Boolean(agents[a.id])}
                  onChange=${(e) => setAgents({ ...agents, [a.id]: e.target.checked })} />${a.name}</label>
                ${agents[a.id] && (options.length
                  ? html`<select aria-label=${`${a.name} 的供应商`} value=${bindings[a.id]} onChange=${(e) => setBindings({ ...bindings, [a.id]: e.target.value })}>
                      <option value="">暂不配置</option>
                      ${options.map((p) => html`<option value=${p.id}>${p.name}</option>`)}
                    </select>`
                  : html`<button type="button" class="link" onClick=${onAddProvider}>先去添加 ${a.name} 的供应商</button>`)}
              </div>`;
            })}
          </div>

          <label>开发环境（可选）</label>
          <${DevSourceChoices} sources=${devSources} selected=${dev} onChange=${setDev} />

          <label>工具连接（可选）</label>
          <${MCPChoices} services=${services} selected=${mcp} permissions=${mcpPermissions} filesystem=${filesystem}
            onChange=${(ids, fs, grants) => { setMcp(ids); setFilesystem(fs); setMcpPermissions(grants); }} />

          <details>
            <summary>用量上限（可选）</summary>
            <div class="form-row">
              <div><label for="s-req">最多请求次数</label>
                <input id="s-req" type="number" min="0" placeholder="不限" value=${maxRequests} onInput=${(e) => setMaxRequests(e.target.value)} /></div>
              <div><label for="s-tok">最多 token</label>
                <input id="s-tok" type="number" min="0" placeholder="不限" value=${maxTokens} onInput=${(e) => setMaxTokens(e.target.value)} /></div>
            </div>
            <p class="caption">由沙箱的门卫执行，达到上限后模型请求会被拒绝；到 80% 时会提醒。</p>
          </details>

          ${error && html`<p class="form-error" role="alert">${error}</p>`}
          <p class="caption">首次启动需要准备镜像，可在沙箱列表查看进度。</p>
          <div class="dialog-actions">
            ${!tutorial && html`<button type="button" onClick=${() => submit(false)} disabled=${saving}>只创建</button>`}
            <button class="primary" type="submit" disabled=${saving}>${saving ? "处理中…" : tutorial ? "创建沙箱" : "创建并启动"}</button>
          </div>
        </form>
      </div>
    </div>`;
}

function ResetDialog({ sandbox, onClose, onConfirm }) {
  const [keepHome, setKeepHome] = useState(true);
  const [restart, setRestart] = useState(sandbox.state === "running");
  return html`
    <div class="overlay" onClick=${(e) => e.target === e.currentTarget && onClose()}>
      <div class="dialog" role="alertdialog" aria-modal="true" aria-label="重置运行环境">
        <div class="dialog-heading"><h2>重置「${sandbox.name}」的运行环境？</h2></div>
        <p>会重建 Agent、门卫、连接器和网络。<strong>项目文件夹不会被删除：</strong><span class="mono">${sandbox.project_path}</span></p>
        <label class="check"><input type="checkbox" checked=${keepHome} onChange=${(e) => setKeepHome(e.target.checked)} />
          保留 Agent 的配置和会话记录</label>
        <label class="check"><input type="checkbox" checked=${restart} onChange=${(e) => setRestart(e.target.checked)} />
          重置完成后恢复运行${sandbox.state === "running" ? "（当前沙箱正在运行）" : ""}</label>
        <p class="caption">配置、供应商绑定、MCP 权限和项目文件会保留。重置失败时会保留这些配置，并显示失败原因。</p>
        <div class="dialog-actions">
          <button data-dismiss onClick=${onClose}>取消</button>
          <button class="primary" onClick=${() => onConfirm(keepHome, restart)}>重置</button>
        </div>
      </div>
    </div>`;
}

function DeleteDialog({ sandbox, onClose, onConfirm }) {
  const [keepHome, setKeepHome] = useState(false);
  return html`
    <div class="overlay" onClick=${(e) => e.target === e.currentTarget && onClose()}>
      <div class="dialog" role="alertdialog" aria-modal="true" aria-label="删除沙箱">
        <div class="dialog-heading"><h2>删除沙箱「${sandbox.name}」？</h2></div>
        <p>会删除它的容器和网络。<strong>项目文件夹不会被删除：</strong><span class="mono">${sandbox.project_path}</span></p>
        <label class="check"><input type="checkbox" checked=${keepHome} onChange=${(e) => setKeepHome(e.target.checked)} />
          保留 Agent 的配置和会话记录（下次用同名沙箱时不会自动恢复）</label>
        <div class="dialog-actions">
          <button data-dismiss onClick=${onClose}>取消</button>
          <button class="danger-solid" onClick=${() => onConfirm(keepHome)}>删除</button>
        </div>
      </div>
    </div>`;
}

const CHECK_LABELS = { ready: "就绪", detected: "已检测", missing: "缺失", stopped: "未运行", error: "异常", unsupported: "不支持" };

export function EnvironmentPanel({ environment }) {
  if (!environment) return null;
  return html`
    <details class="environment-details" data-guide="environment"><summary>运行环境 · ${environment.ready ? "就绪" : "需要检查"}</summary>
    <div class="panel">
      ${environment.checks.map(
        (check) => html`<div class="check-row" key=${check.id}>
          <div><h3>${check.name}</h3><p>${check.detail}</p></div>
          <span class=${`pill ${check.status === "ready" || check.status === "detected" ? "ok" : check.status === "error" || check.status === "missing" ? "warn" : "idle"}`}>
            ${CHECK_LABELS[check.status] ?? check.status}</span>
        </div>`
      )}
    </div></details>`;
}

function quotaPercent(used, limit) {
  if (!limit || !used) return "";
  const percent = Math.min(100, Math.round((used / limit) * 100));
  return html`<span class=${percent >= 100 ? "quota bad" : percent >= 80 ? "quota warn" : "quota"}>${percent}%</span>`;
}

function MCPDialog({ sandbox, services, onClose, onSave }) {
  const [selected, setSelected] = useState(sandbox.mcp);
  const [permissions, setPermissions] = useState(() => {
    const grants = cleanGrants(services, sandbox.mcp_permissions ?? {});
    services.forEach((service) => {
      if (selected.includes(service.id) && service.tools?.length && !Object.prototype.hasOwnProperty.call(grants, service.id)) {
        grants[service.id] = [...(service.default_tools ?? service.tools)];
      }
    });
    return grants;
  });
  const [filesystem, setFilesystem] = useState(sandbox.filesystem_mcp);
  return html`
    <div class="overlay" onClick=${(e) => e.target === e.currentTarget && onClose()}>
      <div class="dialog" role="dialog" aria-modal="true" aria-label="MCP 设置">
        <div class="dialog-heading"><h2>「${sandbox.name}」的 MCP 服务</h2>
          <button type="button" class="icon-button" aria-label="关闭" onClick=${onClose}><${Icon} name="x" size=${20} /></button></div>
        <${MCPChoices} services=${services} selected=${selected} permissions=${permissions} filesystem=${filesystem}
          onChange=${(ids, fs, grants) => { setSelected(ids); setFilesystem(fs); setPermissions(grants); }} />
        <p class="caption">运行中的沙箱会立即更新配置，Agent 下次启动时生效。</p>
        <div class="dialog-actions">
          <button onClick=${onClose}>取消</button>
          <button class="primary" onClick=${() => onSave(selected, filesystem, permissions)}>保存</button>
        </div>
      </div>
    </div>`;
}

function AddAgentsDialog({ sandbox, onClose, onAdd }) {
  const [selected, setSelected] = useState({});
  const available = AGENTS.filter((a) => !sandbox.bindings.some((b) => b.agent === a.id));
  return html`<div class="overlay" onClick=${(e) => e.target === e.currentTarget && onClose()}>
    <div class="dialog" role="dialog" aria-modal="true" aria-label="添加智能体">
      <div class="dialog-heading"><h2>添加智能体</h2><button class="icon-button" aria-label="关闭" onClick=${onClose}><${Icon} name="x" size=${20} /></button></div>
      ${available.map((a) => html`<label class="check"><input type="checkbox" checked=${Boolean(selected[a.id])}
        onChange=${(e) => setSelected({ ...selected, [a.id]: e.target.checked })} />${a.name}</label>`)}
      <p class="caption">项目文件和主目录会保留。${sandbox.state === "running" ? "镜像准备完成后会重建运行容器，当前终端会话将结束。" : "首次准备镜像可能需要几分钟。"}</p>
      <div class="dialog-actions"><button data-dismiss onClick=${onClose}>取消</button>
        <button class="primary" disabled=${!Object.values(selected).some(Boolean)} onClick=${() => onAdd(available.filter((a) => selected[a.id]).map((a) => a.id))}>添加</button></div>
    </div></div>`;
}
