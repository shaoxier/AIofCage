import { useEffect, useState } from "preact/hooks";
import { html, call } from "./core.js";
import { activityRunning, formatDuration, outcomeText, stoppingText, time } from "./progress-state.js";

// 长操作（启动、重置、增装智能体）的进度：已完成的步骤和用时、当前步骤的细节、停止按钮；
// 结束后说明结果。数据来自后端的 activity，总量未知的步骤不显示百分比。

function useNow(running) {
  const [now, setNow] = useState(Date.now());
  useEffect(() => {
    if (!running) return undefined;
    const timer = setInterval(() => setNow(Date.now()), 1000);
    return () => clearInterval(timer);
  }, [running]);
  return now;
}

// OperationProgress 显示一个沙箱的 activity；compact 只显示当前步骤，用在列表行里。
// 已成功结束的操作不再显示，失败和被停止的结果保留到下一次操作。
export function OperationProgress({ sandboxId, activity, compact, notify }) {
  const running = activityRunning(activity);
  const now = useNow(running);
  if (!activity || activity.outcome === "done") return null;
  const stop = () => call("CancelSandboxOperation", sandboxId).catch((error) => notify?.(error.message, true));
  if (!running) {
    return html`<p class=${activity.outcome === "failed" ? "form-error op-result" : "caption op-result"} role="status">${outcomeText(activity)}</p>`;
  }
  const steps = activity.steps ?? [];
  const current = steps.at(-1);
  const elapsed = formatDuration(now - time(activity.started_at));
  const stopButton = activity.stoppable && html`<button class="op-stop" onClick=${stop} disabled=${activity.stopping}>${activity.stopping ? "正在停止…" : "停止"}</button>`;
  if (compact) {
    return html`<div class="op-progress compact" role="status" aria-live="polite">
      <p><strong>${activity.label}中</strong> · ${current?.text ?? "准备中"} · 已用 ${elapsed}</p>
      ${activity.detail && html`<p class="caption op-detail">${activity.detail}</p>`}
      ${activity.stopping && stoppingText(activity) !== "正在停止…" && html`<p class="caption">${stoppingText(activity)}</p>`}
      ${stopButton}</div>`;
  }
  return html`<div class="op-progress" role="status" aria-live="polite">
    <div class="op-head"><strong>${activity.label}中</strong><span class="caption">已用 ${elapsed}</span>${stopButton}</div>
    <ol class="op-steps">${steps.map((step) => {
      const done = Boolean(step.ended_at);
      const took = formatDuration((done ? time(step.ended_at) : now) - time(step.started_at));
      return html`<li class=${done ? "done" : "current"}><span>${step.text}</span><span class="caption">${took}</span>
        ${!done && activity.detail && html`<p class="caption op-detail">${activity.detail}</p>`}</li>`;
    })}</ol>
    ${activity.stopping && stoppingText(activity) !== "正在停止…" && html`<p class="caption">${stoppingText(activity)}</p>`}</div>`;
}
