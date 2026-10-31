// Ai of Cage 桌面界面：左侧导航 + 各页面。样式见 main.css。
import "./js/frames.js";
import { render } from "preact";
import { useEffect, useState, useCallback, useRef } from "preact/hooks";
import { html, call, Icon } from "./js/core.js";
import { ProvidersPage } from "./js/providers.js";
import { SandboxesPage } from "./js/sandboxes.js";
import { UsagePage } from "./js/usage.js";
import { SettingsPage } from "./js/settings.js";
import { LogsPage } from "./js/logs.js";
import { MCPPage } from "./js/mcp.js";
import { Onboarding, useOnboarding } from "./js/onboarding.js";

const PAGES = [
  { id: "sandboxes", title: "沙箱", icon: "box" },
  { id: "providers", title: "模型供应商", icon: "layers" },
  { id: "mcp", title: "工具连接", icon: "plug" },
  { id: "usage", title: "用量与费用", icon: "chart" },
  { id: "logs", title: "请求日志", icon: "list" },
  { id: "settings", title: "设置", icon: "settings" },
];

function App() {
  const [page, setPage] = useState(() => PAGES.some((p) => `#${p.id}` === location.hash) ? location.hash.slice(1) : "sandboxes");
  const [overview, setOverview] = useState(null);
  const [providers, setProviders] = useState([]);
  const [presets, setPresets] = useState([]);
  const [sandboxes, setSandboxes] = useState([]);
  const [toast, setToast] = useState(null);
  const [quitting, setQuitting] = useState(false);
  const reloads = useRef(0); // 只采用最近一次刷新的结果，旧的响应晚到时不覆盖新状态
  const [events, setEvents] = useState([]);
  const [summary, setSummary] = useState({ sandboxes: [], providers: {} });
  const [logSandbox, setLogSandbox] = useState("");
  const [logFilter, setLogFilter] = useState("all");
  const [detail, setDetail] = useState(null); // 打开的沙箱详情：{ id, tab, open? }
  const [services, setServices] = useState([]);
  const [mcpPresets, setMcpPresets] = useState([]);
  const [recoverable, setRecoverable] = useState([]);
  const [dataReady, setDataReady] = useState(false);
  const navigate = useCallback((nextPage, nextDetail = null) => {
    setPage(nextPage); setDetail(nextDetail); location.hash = nextPage;
  }, []);

  const notify = useCallback((message, isError = false) => {
    setToast({ message, isError });
    if (!isError) setTimeout(() => setToast((current) => (current?.message === message ? null : current)), 5000);
  }, []);

  const reload = useCallback(async () => {
    const turn = ++reloads.current;
    try {
      const [p, s, m, r] = await Promise.all([call("ListProviders"), call("ListSandboxes"), call("ListMCPServices"), call("ListRecoverableConversations")]);
      if (turn !== reloads.current) return;
      setProviders(p);
      setSandboxes(s);
      setServices(m);
      setRecoverable(r ?? []);
      setDataReady(true);
    } catch (error) {
      if (turn !== reloads.current) return;
      setDataReady(false);
      notify(error.message, true);
    }
  }, [notify]);

  const tutorial = useOnboarding({ providers, sandboxes, dataReady, navigate, reload });

  const refreshSummary = useCallback(() => call("UsageSummary").then(setSummary).catch(() => {}), []);

  useEffect(() => {
    call("ListPresets").then(setPresets).catch((error) => notify(error.message, true));
    call("ListMCPPresets").then(setMcpPresets).catch(() => {});
    reload();
    refreshSummary();
    call("RecentEvents", "").then(setEvents).catch(() => {});
    const alerted = {};
    const offEvent = window.runtime?.EventsOn?.("sandbox:event", (event) => {
      setEvents((current) => [...current.slice(-499), event]);
      // 额度提醒：同一沙箱 30 秒内只提醒一次。
      if (event.action === "quota.warning" || event.action === "quota.blocked") {
        const key = `${event.session_id}-${event.action}`;
        if (!alerted[key] || Date.now() - alerted[key] > 30000) {
          alerted[key] = Date.now();
          notify(event.action === "quota.warning"
            ? `「${event.sandbox_name}」的用量已到上限的 80%`
            : `「${event.sandbox_name}」已达到用量上限，模型请求会被拒绝`, event.action === "quota.blocked");
        }
      }
    });
    const offStats = window.runtime?.EventsOn?.("sandbox:stats", () => refreshSummary());
    const offAlert = window.runtime?.EventsOn?.("sandbox:alert", (alert) => notify(alert.message, alert.level !== "info"));
    const offQuitting = window.runtime?.EventsOn?.("app:quitting", () => setQuitting(true));
    // 进度只更新对应沙箱的操作，不重读全部数据；操作结束时沙箱状态会变，再刷新一次列表。
    // 后端也记着进度，切换页面回来照样能看到正在进行的操作。
    const offProgress = window.runtime?.EventsOn?.("sandbox:progress", ({ id, activity }) => {
      setSandboxes((list) => list.map((s) => s.id !== id ? s : { ...s, activity,
        operation: activity.outcome ? "" : activity.label, progress: activity.outcome ? "" : activity.steps?.at(-1)?.text ?? "" }));
      if (activity.outcome) reload();
    });
    // 弹窗焦点与键盘导航统一处理，兼容现有原生表单。
    let topDialog = null;
    let returnFocus = null;
    const focusable = (root) => [...root.querySelectorAll('button:not(:disabled), input:not(:disabled), select:not(:disabled), textarea:not(:disabled), a[href], [tabindex="0"]')].filter((el) => el.getClientRects().length);
    const syncDialog = () => {
      const dialogs = document.querySelectorAll('[aria-modal="true"]');
      const next = dialogs[dialogs.length - 1] ?? null;
      if (next === topDialog) return;
      if (next) {
        if (!topDialog) returnFocus = document.activeElement;
        topDialog = next;
        topDialog.tabIndex = -1;
        (focusable(next)[0] ?? next).focus();
      } else {
        topDialog = null;
        if (returnFocus?.isConnected) returnFocus.focus();
        returnFocus = null;
      }
    };
    const observer = new MutationObserver(syncDialog);
    observer.observe(document.body, { childList: true, subtree: true });
    const onKey = (e) => {
      // 对话框里的控件（例如模型下拉）已经用 Esc 收起了自己，就不再关闭对话框。
      if (e.key === "Escape" && topDialog && !e.defaultPrevented) {
        topDialog.querySelector("[data-dismiss], [aria-label='关闭']")?.click();
        e.preventDefault();
      }
      if (e.key === "Tab" && topDialog) {
        const items = focusable(topDialog);
        if (!items.length) { e.preventDefault(); topDialog.focus(); return; }
        const first = items[0], last = items[items.length - 1];
        if (e.shiftKey && (document.activeElement === first || !topDialog.contains(document.activeElement))) { e.preventDefault(); last.focus(); }
        else if (!e.shiftKey && (document.activeElement === last || !topDialog.contains(document.activeElement))) { e.preventDefault(); first.focus(); }
      }
      // WKWebView 不会为所有可聚焦容器提供默认横向滚动，表格区域显式支持方向键。
      if (["ArrowLeft", "ArrowRight"].includes(e.key) && e.target.matches(".data-scroll, .log-table")) {
        e.preventDefault();
        e.target.scrollBy({ left: e.key === "ArrowRight" ? 160 : -160 });
      }
      if (["ArrowLeft", "ArrowRight", "Home", "End"].includes(e.key) && e.target.getAttribute("role") === "tab") {
        const items = [...e.target.closest('[role="tablist"]').querySelectorAll('[role="tab"]')];
        const index = items.indexOf(e.target);
        const next = e.key === "Home" ? 0 : e.key === "End" ? items.length - 1 : (index + (e.key === "ArrowRight" ? 1 : -1) + items.length) % items.length;
        e.preventDefault(); items[next].focus(); items[next].click();
      }
    };
    document.addEventListener("keydown", onKey);
    return () => { offEvent?.(); offStats?.(); offAlert?.(); offQuitting?.(); offProgress?.(); observer.disconnect(); document.removeEventListener("keydown", onKey); };

  }, []);

  useEffect(() => { reload(); }, [page]);

  useEffect(() => {
    // 环境检查要访问虚拟机，单独加载，避免拖慢首屏。
    call("GetOverview").then(setOverview).catch((error) => notify(error.message, true));
  }, []);

  const content = {
    sandboxes: html`<${SandboxesPage} sandboxes=${sandboxes} providers=${providers} services=${services} recoverable=${recoverable} overview=${overview} summary=${summary}
      events=${events} detail=${detail} setDetail=${setDetail}
      tutorial=${tutorial.active && tutorial.step === 2 ? tutorial : null}
      reload=${async () => { await reload(); refreshSummary(); }} notify=${notify} onAddProvider=${() => setPage("providers")}
      onLogs=${(id, filter = "all") => { setLogSandbox(id); setLogFilter(filter); setPage("logs"); }} />`,
    providers: html`<${ProvidersPage} providers=${providers} presets=${presets} summary=${summary} reload=${reload} notify=${notify}
      tutorial=${tutorial.active && tutorial.step === 1 ? tutorial : null} />`,
    mcp: html`<${MCPPage} services=${services} presets=${mcpPresets} providers=${providers} reload=${reload} notify=${notify}
      onCreated=${(id) => { setPage("sandboxes"); setDetail({ id, tab: "overview" }); }} />`,
    usage: html`<${UsagePage} sandboxes=${sandboxes} providers=${providers} />`,
    logs: html`<${LogsPage} events=${events} sandboxes=${sandboxes} providers=${providers} notify=${notify} initialFilter=${logFilter}
      sandboxFilter=${logSandbox} setSandboxFilter=${setLogSandbox} />`,
    settings: html`<${SettingsPage} overview=${overview} providers=${providers} notify=${notify}
      onEnvironmentChanged=${async () => setOverview(await call("GetOverview"))} />`,
  }[page];

  return html`
    <a class="skip-link" href="#main-content" onClick=${(e) => { e.preventDefault(); document.getElementById("main-content").focus(); }}>跳到主要内容</a>
    <aside class="sidebar">
      <div class="brand"><img class="brand-icon" src="/assets/logo.svg" alt="" aria-hidden="true" width="48" height="48" /><span class="brand-name">Ai of Cage<small>笼中 AI · 沙箱</small></span></div>
      <nav aria-label="主导航">
        ${PAGES.map((item) => html`<button class=${item.id === page ? "nav-item active" : "nav-item"}
          title=${item.title} aria-label=${item.title} aria-current=${item.id === page ? "page" : undefined}
          onClick=${() => { setLogFilter("all"); setDetail(null); setPage(item.id); location.hash = item.id; }}>
          <${Icon} class="nav-icon" name=${item.icon} size=${22} /><span class="nav-label">${item.title}</span></button>`)}
      </nav>
      <div class="sidebar-footer"><button class="nav-item tutorial-entry" onClick=${tutorial.open} title="使用教程" aria-label="使用教程"><${Icon} name="book" size=${22} /><span class="nav-label">使用教程</span></button>
        <span class=${overview?.environment?.ready ? "dot ok" : "dot"}></span>
        <span class="environment-label">${overview ? (overview.environment.ready ? "运行环境就绪" : "运行环境未就绪") : "正在检查环境…"}</span>
        <small>${overview ? `v${overview.version}` : ""}</small></div>
    </aside>
    <main id="main-content" tabindex="-1">
      ${quitting && html`<div class="feedback" role="status"><span>正在停止进行中的操作，停完后自动退出。</span></div>`}
      ${toast && html`<div class=${toast.isError ? "feedback error" : "feedback"} role=${toast.isError ? "alert" : "status"}>
        <span>${toast.message}</span><button class="icon-button" aria-label="关闭通知" onClick=${() => setToast(null)}><${Icon} name="x" /></button></div>`}
      <${Onboarding} tutorial=${tutorial} page=${page} detail=${detail} notify=${notify} />
      ${content}
    </main>`;
}

render(html`<${App} />`, document.getElementById("app"));
