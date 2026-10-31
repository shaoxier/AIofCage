// 沙箱详情页（参考 Docker Desktop 点开容器）：终端、概览、日志三个标签。
import { useEffect, useRef, useState } from "preact/hooks";
import { DevSourceLine } from "./dev-sources.js";
import { html, call, AGENTS, agentName, STATES, ConfirmDialog, Icon, agentIcon } from "./core.js";
import { BindingSelects, MCPLine, UsageStats } from "./sandboxes.js";
import { HealthPanel } from "./health.js";
import { UsagePage } from "./usage.js";
import { LogsPage } from "./logs.js";
import { TerminalView, openTerminal, closeTerminal, useTerminals, updateTerminalDraft, addTerminalImages, handleTerminalPaste, handleTerminalImageKey } from "./terminal.js";
import { IMAGE_ACCEPT, removeImage, sendDraft } from "./terminal-draft.js";
import { SandboxAgentUpdates } from "./agent-versions.js";
import { OperationProgress } from "./progress.js";
import { DemoTask } from "./demo-cases.js";
import { placeTerminalMenu, terminalMenuKey } from "./terminal-menu.js";

const activeTerminals = new Map();

const TABS = [
  { id: "overview", label: "概览", icon: "box" },
  { id: "agents", label: "智能体", icon: "cpu" },
  { id: "terminal", label: "终端", icon: "terminal" },
  { id: "usage", label: "用量", icon: "chart" },
  { id: "health", label: "运行状态", icon: "activity" },
  { id: "logs", label: "日志", icon: "list" },
];

export function SandboxDetail({ sandbox, detail, setDetail, providers, services, recoverable, events, usage, vm, busy, run, notify,
  onAddAgents, onEditMCP, onAttack, onReset, onDelete, onLogs, dialogs, devSources, onEditDev }) {
  const [restartConfirm, setRestartConfirm] = useState(false);
  const tab = detail.tab ?? "overview";
  const state = STATES[sandbox.state] ?? STATES.unknown;
  const running = sandbox.state === "running";
  return html`
    <section class="page detail">
      <button class="link back" onClick=${() => setDetail(null)}><${Icon} name="arrowLeft" size=${18} />全部沙箱</button>
      <div class="detail-heading">
        <div class="sandbox-title" data-guide="sandbox-status">
          <span class=${`status-dot ${state.tone}`} aria-hidden="true"></span>
          <h1>${sandbox.name}</h1>
          <span class=${`pill ${busy ? "warn" : state.tone}`}>${busy ? `${busy}中…` : state.label}</span>
        </div>
        <div class="row-actions">
          ${running
            ? html`<button onClick=${() => run(sandbox, "停止", "StopSandbox", sandbox.id)} disabled=${Boolean(busy)}><${Icon} name="stop" size=${18} />停止</button>`
            : html`<button class="primary" data-guide="sandbox-start" onClick=${() => run(sandbox, "启动", "StartSandbox", sandbox.id)} disabled=${Boolean(busy)}><${Icon} name="play" size=${18} />启动</button>`}
          ${sandbox.config_pending && html`<button class="primary" disabled=${Boolean(busy)} onClick=${() => setRestartConfirm(true)}>重启以应用</button>`}
          <details class="action-menu"><summary aria-label="更多沙箱操作"><${Icon} name="more" /></summary><div class="menu-items">
            <button onClick=${onAttack} disabled=${!running || Boolean(busy)}>隔离检查</button>
            <button onClick=${onReset} disabled=${Boolean(busy)}>重置运行环境…</button>
            <button class="danger" onClick=${onDelete} disabled=${Boolean(busy)}>删除沙箱…</button>
          </div></details>
        </div>
      </div>
      <button class="link mono" onClick=${() => call("OpenProjectFolder", sandbox.id).catch((e) => notify(e.message, true))}
        title="在访达中打开">${sandbox.project_path}</button>
      <${OperationProgress} sandboxId=${sandbox.id} activity=${sandbox.activity} notify=${notify} />
      ${sandbox.demo_case && tab === "overview" && html`<${DemoTask} caseID=${sandbox.demo_case} notify=${notify} />`}
      <div class="tabs" role="tablist">
        ${TABS.map((t) => html`<button role="tab" aria-selected=${t.id === tab} tabindex=${t.id === tab ? 0 : -1} class=${t.id === tab ? "tab active" : "tab"}
          onClick=${() => setDetail({ ...detail, tab: t.id, open: undefined })}><${Icon} name=${t.icon} size=${18} />${t.label}</button>`)}
      </div>
      ${tab === "terminal" && html`<${TerminalTab} sandbox=${sandbox} request=${detail.open} recoverable=${recoverable} running=${running} busy=${busy}
        run=${run} notify=${notify} />`}
      ${tab === "agents" && html`<div class="panel detail-overview">
        <div class="section-heading"><div><h3>已安装智能体</h3><p class="caption">为每个智能体独立选择供应商。</p></div>
          <button onClick=${onAddAgents} disabled=${Boolean(busy) || sandbox.bindings.length === AGENTS.length}><${Icon} name="plus" size=${18} />添加智能体</button></div>
        <${BindingSelects} sandbox=${sandbox} providers=${providers} busy=${busy} run=${run} notify=${notify} />
        <${SandboxAgentUpdates} sandbox=${sandbox} busy=${busy} run=${run} />
      </div>`}
      ${tab === "overview" && html`<div class="panel detail-overview">
        <div><h3>智能体与供应商</h3><button class="link" onClick=${() => setDetail({ ...detail, tab: "agents" })}>管理智能体</button><p class="caption">选择在下次重启沙箱后应用。</p>
          <${BindingSelects} sandbox=${sandbox} providers=${providers} busy=${busy} run=${run} notify=${notify} /></div>
        <div><h3>MCP 服务</h3><${MCPLine} sandbox=${sandbox} services=${services} busy=${busy} onEditMCP=${onEditMCP} /></div>
        <div><h3>开发环境</h3><${DevSourceLine} sandbox=${sandbox} sources=${devSources ?? []} busy=${busy} onEdit=${onEditDev} /></div>
        <div><h3>用量</h3><${UsageStats} sandbox=${sandbox} usage=${usage} /></div>
      </div>`}
      ${tab === "health" && html`<${HealthPanel} sandbox=${sandbox} vm=${vm} events=${events} />`}
      ${tab === "usage" && html`<${UsagePage} sandboxId=${sandbox.id} providers=${providers} embedded=${true} />`}
      ${tab === "logs" && html`<${LogsPage} events=${events} sandboxes=${[sandbox]} providers=${providers} notify=${notify}
        sandboxFilter=${sandbox.id} setSandboxFilter=${() => {}} embedded=${true} />`}
      ${restartConfirm && html`<${ConfirmDialog} title="重启并应用配置？" message="当前终端会话将结束，项目文件和智能体历史会保留。" confirmLabel="重启并应用" onClose=${() => setRestartConfirm(false)} onConfirm=${async () => {
        setRestartConfirm(false);
        if (await run(sandbox, "停止", "StopSandbox", sandbox.id)) await run(sandbox, "启动", "StartSandbox", sandbox.id);
      }} />`}
      ${dialogs}
    </section>`;
}

function TerminalTab({ sandbox, request, recoverable = [], running, busy, run, notify }) {
  const sessions = useTerminals(sandbox.id);
  const [active, updateActive] = useState(() => activeTerminals.get(sandbox.id));
  const [dragging, setDragging] = useState(false);
  const [creating, setCreating] = useState(false);
  const menu = useRef(null);
  useEffect(() => {
    const dismiss = (event) => { if (menu.current && !menu.current.contains(event.target)) menu.current.open = false; };
    const reposition = () => placeTerminalMenu(menu.current);
    const observer = new ResizeObserver(reposition);
    if (menu.current) observer.observe(menu.current.closest(".terminal-panel"));
    document.addEventListener("pointerdown", dismiss);
    window.addEventListener("resize", reposition);
    return () => {
      document.removeEventListener("pointerdown", dismiss);
      window.removeEventListener("resize", reposition);
      observer.disconnect();
    };
  }, []);
  const setActive = (id) => { activeTerminals.set(sandbox.id, id); updateActive(id); };
  const open = async (agent) => {
    if (creating) return;
    setCreating(true);
    if (menu.current?.open) { menu.current.open = false; menu.current.querySelector("summary").focus(); }
    try { setActive(await openTerminal(sandbox, agent)); }
    finally { setCreating(false); }
  };
  const recover = async (item) => {
    if (creating || !item.ready) return;
    if (!running && !await run(sandbox, "启动", "StartSandbox", sandbox.id)) return;
    setCreating(true);
    try { setActive(await openTerminal(sandbox, item.agent, item)); }
    finally { setCreating(false); }
  };
  const pendingRecoveries = (recoverable ?? []).filter((item) => item.sandbox_id === sandbox.id && !sessions.some((session) => session.id === item.id && !session.exited));
  const recoveryPanel = pendingRecoveries.length > 0 && html`<div class="conversation-recovery panel" role="status">
    <div><strong>可恢复会话</strong><p class="caption">这些记录只保存原生会话元数据，不保存消息正文。启动沙箱后可继续。</p></div>
    ${pendingRecoveries.map((item) => html`<div class="conversation-recovery-row" key=${item.id}>
      <span><${Icon} name="sparkles" size=${17} />${agentName(item.agent)} · ${item.provider_name || "供应商不可用"} · ${item.model}</span>
      ${item.ready ? html`<button class="primary" onClick=${() => recover(item)} disabled=${Boolean(busy) || creating || !running && Boolean(busy)}>恢复</button>`
        : html`<span class="caption">${item.reason}</span>`}
    </div>`)}
  </div>`;

  // 从沙箱列表点“打开终端”过来时，按选中的 Agent 新开一个标签。
  useEffect(() => {
    if (request && running) {
      const existing = request.reuse && sessions.findLast((s) => !s.exited && s.agent === request.agent && s.info?.provider_id === sandbox.bindings.find((b) => b.agent === request.agent)?.active_provider_id);
      if (existing) setActive(existing.id);
      else open(request.agent);
    }
  }, [request?.nonce]);

  const current = sessions.find((s) => s.id === active) ?? sessions[sessions.length - 1];
  const close = (id) => {
    closeTerminal(id);
    if (id === current?.id) setActive(null);
  };
  const choices = [...sandbox.bindings.map((b) => ({ agent: b.agent, label: agentName(b.agent) })), { agent: "", label: "Shell" }];

  if (!running && sessions.length === 0) {
    return html`<div>${recoveryPanel}<div class="panel empty">
      <span class="empty-icon"><${Icon} name="terminal" size=${40} /></span><h3>沙箱还没有启动</h3>
      <p>启动沙箱后，打开智能体或 Shell 开始工作。</p>
      <button class="primary" onClick=${() => run(sandbox, "启动", "StartSandbox", sandbox.id)} disabled=${Boolean(busy)}>启动沙箱</button>
    </div></div>`;
  }
  return html`<div class=${`terminal-panel ${dragging ? "dragging-image" : ""}`}
    onPasteCapture=${(e) => handleTerminalPaste(current, e)}
    onKeyDownCapture=${(e) => handleTerminalImageKey(current, e)}
    onDragOver=${(e) => { if (e.dataTransfer.types.includes("Files")) { e.preventDefault(); e.dataTransfer.dropEffect = "copy"; setDragging(true); } }}
    onDragLeave=${(e) => { if (!e.currentTarget.contains(e.relatedTarget)) setDragging(false); }}
    onDrop=${(e) => { e.preventDefault(); setDragging(false); if (current?.agent && !current.exited) addTerminalImages(current, e.dataTransfer.files); }}>
    ${recoveryPanel}
    <div class="terminal-tabs">
      ${sessions.length > 0 && html`<div class="terminal-session-tabs" role="tablist" aria-label="终端会话">${sessions.map((s) => html`<div class=${`terminal-tab ${s.id === current?.id ? "active" : ""} ${s.exited ? "exited" : ""}`} key=${s.id}>
        <button role="tab" aria-selected=${s.id === current?.id} tabindex=${s.id === current?.id ? 0 : -1} onClick=${() => setActive(s.id)}><${Icon} name=${agentIcon(s.agent)} size=${16} />${s.title}${s.exited ? "（已结束）" : ""}</button>
        <button class="close" aria-label=${`关闭 ${s.title}`} title="关闭" onClick=${() => close(s.id)}><${Icon} name="x" size=${16} /></button>
      </div>`)}</div>`}
      <div class="terminal-new">
        <details class="terminal-new-menu" ref=${menu} onKeyDown=${terminalMenuKey} onToggle=${(event) => placeTerminalMenu(event.currentTarget)}
          onFocusOut=${(event) => { if (!event.currentTarget.contains(event.relatedTarget)) event.currentTarget.open = false; }}>
          <summary aria-label="新建会话" data-guide="new-conversation"><${Icon} name="plus" size=${18} /><span>新建</span><${Icon} name="chevronDown" size=${15} /></summary>
          <div class="terminal-menu-choices">${choices.map((choice) => html`<button onClick=${() => open(choice.agent)} disabled=${!running || creating}><${Icon} name=${agentIcon(choice.agent)} size=${18} />${choice.label}</button>`)}</div>
        </details>
      </div>
      <button class="link system-terminal" disabled=${!running} title="用 macOS 自带的“终端”打开"
        onClick=${() => call("OpenTerminal", sandbox.id, current?.agent ?? sandbox.bindings[0]?.agent ?? "").catch((e) => notify(e.message, true))}>
        在系统终端中打开</button>
    </div>
    ${current
      ? html`<${TerminalView} key=${current.id} id=${current.id} />
          ${current.exited && running && html`<div class="terminal-exited">
            <button onClick=${() => { const agent = current.agent; close(current.id); open(agent); }}>重新打开 ${current.title}</button></div>`}`
      : html`<div class="panel empty terminal-empty"><h3>开始工作</h3>
          <p>打开智能体，或使用 Shell 管理项目。</p>
          <div class="terminal-choices">${choices.map((c) => html`<button class="primary" onClick=${() => open(c.agent)}><${Icon} name=${agentIcon(c.agent)} size=${18} />${c.label}</button>`)}</div>
        </div>`}
    ${current?.agent && current.info && !current.exited && running && html`<${TerminalComposer} key=${current.id} session=${current} />`}
    ${dragging && html`<div class="terminal-drop-hint" aria-hidden="true">松开以添加图片</div>`}
  </div>`;
}

function TerminalComposer({ session }) {
  const picker = useRef(null);
  const draft = session.draft;
  const inputID = `terminal-message-${session.id}`;
  const errorID = `terminal-message-error-${session.id}`;
  const send = () => {
    if (session.busy) return;
    return sendDraft(draft, (text, images) => call("SendConversationMessage", session.id, text, images, { model: session.model, effort: session.effort }), updateTerminalDraft);
  };
  return html`<form class="terminal-composer" onSubmit=${(e) => { e.preventDefault(); send(); }}>
    ${draft.images.length > 0 && html`<ul class="terminal-attachments" aria-label="待发送的图片">
      ${draft.images.map((item, index) => html`<li key=${item.id}>
        <img src=${item.url} alt=${`图片 ${index + 1}：${item.file.name}`} />
        <span title=${item.file.name}>${item.file.name}</span>
        <button type="button" class="attachment-remove" aria-label=${`移除图片 ${index + 1}：${item.file.name}`} disabled=${draft.busy}
          onClick=${() => { removeImage(draft, item.id); updateTerminalDraft(); }}><${Icon} name="x" size=${16} /></button>
      </li>`)}
    </ul>`}
    <div class="terminal-input-row">
      <label class="sr-only" for=${inputID}>发送给 ${session.title}</label>
      <textarea id=${inputID} data-guide="message-input" rows="2" value=${draft.text} placeholder=${session.agent ? "输入任务，或粘贴图片…" : "输入 Shell 命令…"}
        disabled=${draft.busy} aria-invalid=${Boolean(draft.error)} aria-describedby=${draft.error ? errorID : undefined}
        onInput=${(e) => { draft.text = e.target.value; draft.error = ""; updateTerminalDraft(); }}
        onKeyDown=${(e) => { if (e.key === "Enter" && !e.shiftKey && !e.isComposing && e.keyCode !== 229) { e.preventDefault(); send(); } }}></textarea>
    </div>
    <div class="terminal-composer-actions">
      <div class="terminal-attachment-actions">
        ${session.agent && html`<input ref=${picker} type="file" accept=${IMAGE_ACCEPT} multiple hidden aria-label="选择图片"
          onChange=${(e) => { addTerminalImages(session, e.target.files); e.target.value = ""; }} />
          <button type="button" disabled=${draft.busy || draft.reading} onClick=${() => picker.current?.click()}><${Icon} name="image" size=${18} />添加图片</button>
          `}
        <span class="terminal-input-hint">${draft.images.length ? `${draft.images.length}/4 张 · 所选模型需支持图片` : "Shift + Enter 换行 · ⌘V 粘贴图片"}</span>
      </div>
      ${session.busy ? html`<button type="button" class="terminal-send" onClick=${() => call("StopConversation", session.id).catch((error) => { draft.error = error.message; updateTerminalDraft(); })}><${Icon} name="stop" size=${18} />停止</button>`
        : html`<button type="submit" class="terminal-send" disabled=${draft.busy || draft.reading}><${Icon} name="send" size=${18} />${draft.busy ? "正在发送…" : "发送 ↵"}</button>`}
    </div>
    <span class="sr-only" role="status">${draft.status}</span>
    <div class="terminal-input-error" id=${errorID} role="alert">${draft.error}</div>
  </form>`;
}
