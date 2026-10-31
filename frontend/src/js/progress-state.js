// 长操作进度的文字说明；不依赖界面库，便于单独测试。

// formatDuration 把毫秒写成「12 秒」「3 分 05 秒」。
export function formatDuration(ms) {
  const seconds = Math.max(0, Math.round(ms / 1000));
  if (seconds < 60) return `${seconds} 秒`;
  const minutes = Math.floor(seconds / 60);
  return `${minutes} 分 ${String(seconds % 60).padStart(2, "0")} 秒`;
}

export const time = (value) => (value ? Date.parse(value) : NaN);

// 停止请求在虚拟机启动这一步不会立即生效：中途打断可能损坏虚拟机。
const VM_STEP = "检查专用虚拟机";

export function activityRunning(activity) {
  return Boolean(activity && !activity.outcome);
}

// stoppingText 说明停止请求要等到什么时候生效。
export function stoppingText(activity) {
  const current = activity.steps?.at(-1)?.text;
  return current === VM_STEP ? "正在停止：等虚拟机启动完成后再停（中途打断可能损坏虚拟机）" : "正在停止…";
}

// outcomeText 说明一次操作怎样结束：完成、失败还是被停止，以及用了多久。
export function outcomeText(activity) {
  const took = formatDuration(time(activity.ended_at) - time(activity.started_at));
  if (activity.outcome === "done") return `${activity.label}已完成，用时 ${took}`;
  if (activity.outcome === "stopped") return `${activity.message || `已停止${activity.label}`}（用时 ${took}）`;
  return `${activity.label}失败：${activity.message || "原因未知"}`;
}

