import { useEffect, useRef, useState } from "preact/hooks";
import { html, Icon, agentName } from "./core.js";
import { activitySummary } from "./conversation-state.js";

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

// 复制回复正文：优先用 Wails 的剪贴板接口，应用里的 WebKit 不一定开放 navigator.clipboard。
function CopyButton({ text }) {
  const [copied, setCopied] = useState(false);
  const timer = useRef(0);
  useEffect(() => () => clearTimeout(timer.current), []);
  const copy = async () => {
    try {
      const done = window.runtime?.ClipboardSetText ? await window.runtime.ClipboardSetText(text) : (await navigator.clipboard.writeText(text), true);
      if (!done) return;
      setCopied(true);
      clearTimeout(timer.current);
      timer.current = setTimeout(() => setCopied(false), 1500);
    } catch { /* 复制失败时按钮保持原样，用户可以手动选中复制 */ }
  };
  return html`<button type="button" class="icon-button message-copy" aria-label=${copied ? "已复制" : "复制回复"} data-tip=${copied ? "已复制" : "复制"}
    onClick=${copy}><${Icon} name=${copied ? "check" : "copy"} size=${15} /></button>`;
}

// 回复上方的一行灰字：用时、工具活动、思考过程，点开看详情（照 ChatGPT 桌面端的“Worked for 2m 30s”）。
function Activity({ message, busy }) {
  const summary = activitySummary(message, busy);
  if (!summary) return null;
  const head = html`${busy && html`<span class="activity-dot" aria-hidden="true"></span>`}<span>${summary}</span>`;
  if (!message.thinking && !message.tools?.length) return html`<p class="message-activity">${head}</p>`;
  return html`<details class="message-activity">
    <summary>${head}<${Icon} name="chevronRight" size=${14} class="activity-chevron" /></summary>
    ${message.thinking && html`<div class="activity-section"><strong>思考过程</strong><pre>${message.thinking}</pre></div>`}
    ${message.tools?.length > 0 && html`<div class="activity-section"><strong>工具活动</strong><ul>${message.tools.map((tool, index) => html`<li key=${index}>${tool}</li>`)}</ul></div>`}
  </details>`;
}

export function ConversationView({ session }) {
  const viewport = useRef(null);
  const follow = useRef(session.viewport?.follow ?? true);
  const message = session.messages[session.messages.length - 1];
  useEffect(() => { if (session.viewport && viewport.current) viewport.current.scrollTop = session.viewport.top; }, []);
  useEffect(() => { if (follow.current && viewport.current) viewport.current.scrollTop = viewport.current.scrollHeight; }, [session.messages.length, message?.text, message?.thinking, message?.tools?.length, session.busy]);
  const name = agentName(session.agent);
  return html`<div class="conversation-container">
    <div class="conversation-messages" ref=${viewport} role="region" aria-label="会话消息" tabindex="0"
      onScroll=${() => { const element = viewport.current; follow.current = element.scrollHeight - element.scrollTop - element.clientHeight < 100; session.viewport = { top: element.scrollTop, follow: follow.current }; }}>
      <div class="conversation-column">
        ${session.error && html`<div class="conversation-notice error" role="alert">${session.error}</div>`}
        ${session.diagnostics && html`<div class="conversation-diagnostics" role="status"><strong>链路诊断${session.diagnostics.ready ? "通过" : "未通过"}</strong>
          ${session.diagnostics.checks.map((check) => html`<div class="conversation-check"><span class=${`pill ${check.status === "passed" ? "ok" : "bad"}`}>${check.status === "passed" ? "通过" : "需处理"}</span><span><strong>${check.name}</strong> · ${check.detail}</span></div>`)}</div>`}
        ${!session.messages.length && !session.error && html`<div class="conversation-welcome">
          <span class="conversation-mark"><${Icon} name="sparkles" size=${24} /></span>
          <h3>${session.resumed ? `继续和 ${name} 的会话` : `要让 ${name} 做什么？`}</h3>
          <p>${session.resumed ? "已恢复原生会话。之前的消息正文不保存，直接发送新任务即可。" : "智能体在当前沙箱的权限范围内执行工具。"}</p></div>`}
        ${session.messages.map((item, index) => {
          const working = session.busy && index === session.messages.length - 1;
          return item.role === "user"
            ? html`<article key=${index} class="conversation-message user" aria-label="你的消息">
                ${(item.images > 0 || item.files?.length > 0) && html`<div class="message-attachments">
                  ${item.images > 0 && html`<span><${Icon} name="image" size=${15} />${item.images} 张图片</span>`}
                  ${item.files?.map((name, i) => html`<span key=${i} title=${name}><${Icon} name="file" size=${15} />${name}</span>`)}</div>`}
                <${MessageBody} text=${item.text} /></article>`
            : html`<article key=${index} class="conversation-message assistant" aria-label=${`${name} 回复`}>
                <${Activity} message=${item} busy=${working} />
                <${MessageBody} text=${item.text} />
                ${item.error && html`<p class="message-error" role="alert">${item.error}</p>`}
                ${item.truncated && html`<small class="message-truncated">显示内容已达上限，完整记录保留在智能体原生会话中。</small>`}
                ${!working && item.text && html`<div class="message-actions"><${CopyButton} text=${item.text} /></div>`}
              </article>`;
        })}
      </div>
    </div>
    <span class="sr-only" role="status">${session.status}</span>
  </div>`;
}
