// 只保存页面位置和标签元数据；消息正文、附件和密钥不写入浏览器存储。
const tabs = new Set(["overview", "agents", "terminal", "usage", "health", "logs"]);
const pages = new Set(["sandboxes", "providers", "mcp", "usage", "logs", "settings"]);
const agents = new Set(["codex", "claude-code", "dsh", ""]);
function read(key) {
  try { return JSON.parse(globalThis.localStorage?.getItem(key) ?? "null"); } catch { return null; }
}
function write(key, value) {
  try { globalThis.localStorage?.setItem(key, JSON.stringify(value)); } catch { /* 存储不可用时仍可正常工作 */ }
}

export function readWorkspace(id) {
  const value = read(`aiofcage.workspace.${id}`);
  return {
    tab: tabs.has(value?.tab) ? value.tab : null,
    active: typeof value?.active === "string" ? value.active : null,
    terminals: Array.isArray(value?.terminals) ? value.terminals.filter((t) => t && typeof t.id === "string" && agents.has(t.agent) && typeof t.title === "string") : [],
  };
}
export function saveWorkspace(id, patch) {
  write(`aiofcage.workspace.${id}`, { ...readWorkspace(id), ...patch });
}
export function readAppView() {
  const value = read("aiofcage.view");
  const page = pages.has(value?.page) ? value.page : "sandboxes";
  const detail = page === "sandboxes" && typeof value?.detail?.id === "string" && tabs.has(value.detail.tab)
    ? { id: value.detail.id, tab: value.detail.tab } : null;
  return { page, detail };
}
export function saveAppView(page, detail) {
  write("aiofcage.view", { page, detail: page === "sandboxes" && detail ? { id: detail.id, tab: detail.tab } : null });
}

export function orderedRecoveries(records, sandboxId, workspace) {
  const mine = records.filter((r) => r.sandbox_id === sandboxId && r.ready);
  const order = workspace.terminals.map((t) => t.id);
  return mine.sort((a, b) => {
    const ai = order.indexOf(a.id), bi = order.indexOf(b.id);
    return (ai < 0 ? order.length : ai) - (bi < 0 ? order.length : bi);
  });
}
