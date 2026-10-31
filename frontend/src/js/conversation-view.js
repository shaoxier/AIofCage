import { useEffect, useRef } from "preact/hooks";
import { html, call, Icon, agentName } from "./core.js";
import { ModelPicker } from "./model-picker.js";

// 使用 Preact 文本节点显示 Markdown，代码块单独排版，绝不注入模型返回的 HTML。
function inlineText(text) {
  return text.split(/(\*\*[^*\n]+\*\*|`[^`\n]+`)/g).map((part) => {
    if (part.startsWith("**") && part.endsWith("**")) return html`<strong>${part.slice(2, -2)}</strong>`;
    if (part.startsWith("`") && part.endsWith("`")) return html`<code>${part.slice(1, -1)}</code>`;
    return part;
  });
}

function MessageBody({ text }) {
  const blocks = String(text || "").split(/```[^\n]*\n([\s\S]*?)(?:```|$)/g);
  return html`<div class="message-body">${blocks.map((block, index) => index % 2
    ? html`<pre><code>${block}</code></pre>`
    : block.split(/\n\s*\n/).filter(Boolean).map((paragraph) => html`<p>${inlineText(paragraph)}</p>`))}</div>`;
}

export function ConversationView({ session, changed }) {
  const viewport = useRef(null);
  const follow = useRef(true);
  const message = session.messages[session.messages.length - 1];
  useEffect(() => { if (follow.current && viewport.current) viewport.current.scrollTop = viewport.current.scrollHeight; }, [session.messages.length, message?.text, message?.thinking, message?.tools?.length, session.busy]);
  // 打开会话时用供应商页取过的列表；刷新会重新请求，供应商页的列表也随之更新。
  const loadModels = async (refresh) => {
    session.loadingModels = true; session.modelsError = ""; changed();
    try {
      const result = await call("ConversationModels", session.id, refresh);
      session.models = result.models ?? [];
      session.modelsError = result.ok ? "" : result.message;
    } catch (error) { session.modelsError = error.message; }
    finally { session.loadingModels = false; changed(); }
  };
  useEffect(() => {
    if (!session.info || session.started || session.modelsRequested) return;
    session.modelsRequested = true; // 每个会话只自动读一次，之后靠刷新按钮
    loadModels(false);
  }, [session.info]);
  const unpriced = session.info?.priced && session.model && session.model !== session.info.provider_model;
  return html`<div class="conversation-container">
    <div class="conversation-controls">
      <span class="conversation-provider"><${Icon} name="layers" size=${17} />${session.info?.provider_name || "正在连接…"}</span>
      <span class="conversation-model"><label for=${`model-${session.id}`}>模型</label>
        <${ModelPicker} id=${`model-${session.id}`} label="会话模型" value=${session.model || ""} models=${session.models} loading=${session.loadingModels}
          disabled=${session.started || !session.info} onChange=${(model) => { session.model = model; changed(); }} onRefresh=${() => loadModels(true)} /></span>
      <label>思考强度<select aria-label="会话思考强度" value=${session.effort || ""} disabled=${session.started || !session.info}
        onChange=${(event) => { session.effort = event.target.value; changed(); }}>
        <option value="">模型默认</option><option value="low">低</option><option value="medium">中</option><option value="high">高</option>
      </select></label>
      <span class="conversation-status" role="status">${session.started ? `${session.status} · 新会话可更改模型设置` : "设置在首条消息发送时生效"}</span>
    </div>
    ${session.modelsError && html`<p class="conversation-notice" role="status">${session.modelsError}</p>`}
    ${unpriced && !session.started && html`<p class="conversation-notice" role="status">换成「${session.model}」后用量照常统计，但不估算费用：单价只配置了「${session.info.provider_model}」。</p>`}
    <div class="conversation-messages" ref=${viewport} role="region" aria-label="会话消息" tabindex="0"
      onScroll=${() => { const element = viewport.current; follow.current = element.scrollHeight - element.scrollTop - element.clientHeight < 100; }}>
      ${session.error && html`<div class="conversation-notice error" role="alert">${session.error}</div>`}
      ${session.diagnostics && html`<div class="conversation-diagnostics" role="status"><strong>链路诊断${session.diagnostics.ready ? "通过" : "未通过"}</strong>
        ${session.diagnostics.checks.map((check) => html`<div class="conversation-check"><span class=${`pill ${check.status === "passed" ? "ok" : "bad"}`}>${check.status === "passed" ? "通过" : "需处理"}</span><span><strong>${check.name}</strong> · ${check.detail}</span></div>`)}</div>`}
      ${!session.messages.length && !session.error && html`<div class="conversation-welcome"><span class="conversation-mark"><${Icon} name="sparkles" size=${28} /></span>
        <h3>${agentName(session.agent)}</h3><p>描述任务，或添加图片开始会话。</p><small>智能体可在当前沙箱权限范围内执行工具；模型与思考强度由供应商支持情况决定。</small></div>`}
      ${session.messages.map((item, index) => html`<article key=${index} class=${`conversation-message ${item.role}`} aria-label=${item.role === "user" ? "你的消息" : `${agentName(session.agent)} 回复`}>
        <div class="message-heading"><${Icon} name=${item.role === "user" ? "user" : "sparkles"} size=${17} /><strong>${item.role === "user" ? "你" : agentName(session.agent)}</strong></div>
        ${item.images > 0 && html`<span class="message-attachments"><${Icon} name="image" size=${16} />${item.images} 张图片</span>`}
        ${item.thinking && html`<details class="message-thinking"><summary>思考过程</summary><pre>${item.thinking}</pre></details>`}
        ${item.tools?.length > 0 && html`<details class="message-tools"><summary>${item.tools.length} 次工具活动</summary><ul>${item.tools.map((tool) => html`<li>${tool}</li>`)}</ul></details>`}
        <${MessageBody} text=${item.text} />
        ${item.error && html`<p class="message-error" role="alert">${item.error}</p>`}
        ${item.truncated && html`<small>显示内容已达上限，完整记录保留在智能体原生会话中。</small>`}
        ${session.busy && index === session.messages.length - 1 && html`<span class="message-working" role="status"><span class="dot ok"></span>智能体正在处理…</span>`}
      </article>`)}
    </div>
  </div>`;
}
