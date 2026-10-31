import { h } from "preact";

// 统一使用 24 像素网格的线性图标，颜色跟随文字，不依赖在线字体或图标服务。
const paths = {
  book: "M12 5C9 3 5 3 2 4v15c3-1 7-1 10 1 3-2 7-2 10-1V4c-3-1-7-1-10 1Zm0 0v15",
  box: "m12 3 9 5v8l-9 5-9-5V8l9-5ZM3 8l9 5 9-5M12 13v8M7.5 5.5l9 5",
  layers: "m12 3 10 5-10 5L2 8l10-5ZM2 12l10 5 10-5M2 16l10 5 10-5",
  terminal: "M4 4h16a1 1 0 0 1 1 1v14a1 1 0 0 1-1 1H4a1 1 0 0 1-1-1V5a1 1 0 0 1 1-1Zm3 4 4 4-4 4m7 0h3",
  sparkles: "m12 3 2.5 6.5L21 12l-6.5 2.5L12 21l-2.5-6.5L3 12l6.5-2.5L12 3ZM20 2v4m-2-2h4",
  plug: "m8 3 0 5m8-5v5M6 8h12v3a6 6 0 0 1-12 0V8Zm6 9v5",
  chart: "M4 3v17h17M8 15v-4m5 4V6m5 9V9",
  list: "M8 6h13M8 12h13M8 18h13M3 6h.01M3 12h.01M3 18h.01",
  chevron: "m6 9 6 6 6-6",
  settings: "m9 3-.6 2.1-1.9 1.1-2.2-.5-2 3.4L4 10.7v2.6l-1.7 1.6 2 3.4 2.2-.5 1.9 1.1L9 21h6l.6-2.1 1.9-1.1 2.2.5 2-3.4-1.7-1.6v-2.6l1.7-1.6-2-3.4-2.2.5-1.9-1.1L15 3H9Zm6 9a3 3 0 1 1-6 0 3 3 0 0 1 6 0Z",
  search: "M17 10a7 7 0 1 1-14 0 7 7 0 0 1 14 0Zm-2 5 6 6",
  plus: "M12 5v14M5 12h14",
  user: "M16 7a4 4 0 1 1-8 0 4 4 0 0 1 8 0ZM4 21v-2a8 8 0 0 1 16 0v2",
  chevronDown: "m6 9 6 6 6-6",
  expand: "M8 3H3v5m13-5h5v5M3 16v5h5m13-5v5h-5",
  play: "m8 4 12 8-12 8V4Z",
  stop: "M6 6h12v12H6V6Z",
  arrowLeft: "m11 5-7 7 7 7M4 12h17",
  folder: "M3 7V5a1 1 0 0 1 1-1h5l2 3h9a1 1 0 0 1 1 1v11a1 1 0 0 1-1 1H4a1 1 0 0 1-1-1V7Z",
  more: "M5 12h.01M12 12h.01M19 12h.01",
  x: "m6 6 12 12M6 18 18 6",
  refresh: "M20 7a8 8 0 0 0-14-2L3 8m0-5v5h5M4 17a8 8 0 0 0 14 2l3-3m0 5v-5h-5",
  image: "M4 3h16a1 1 0 0 1 1 1v16a1 1 0 0 1-1 1H4a1 1 0 0 1-1-1V4a1 1 0 0 1 1-1Zm-1 13 5-5 5 5 3-3 5 5M17 7h.01",
  clipboard: "M9 4H5v17h14V4h-4M9 2h6v5H9V2Z",
  send: "m21 3-6 18-4-8-8-4 18-6ZM11 13 21 3",
  copy: "M9 9h12v12H9V9ZM5 15H3V3h12v2",
  edit: "m15 4 5 5-11 11-6 1 1-6L15 4Zm-2 2 5 5",
  trash: "M3 6h18M9 6V3h6v3M5 6l1 15h12l1-15M10 10v7m4-7v7",
  external: "M14 3h7v7m0-7L10 14M10 3H3v18h18v-7",
  cpu: "M7 7h10v10H7V7Zm3 3h4v4h-4v-4ZM9 3v4m6-4v4M9 17v4m6-4v4M3 9h4m-4 6h4M17 9h4m-4 6h4",
  memory: "M3 6h18v11H3V6Zm3 11v3m4-3v3m4-3v3m4-3v3M7 10v3m5-3v3m5-3v3",
  activity: "M2 12h4l3-8 6 16 3-8h4",
  shield: "m12 3 9 3v6c0 5-9 9-9 9s-9-4-9-9V6l9-3Zm-4 9 3 3 5-6",
  coins: "M20 6c0 2-3.6 3-8 3S4 8 4 6s3.6-3 8-3 8 1 8 3Zm0 0v12c0 2-3.6 3-8 3s-8-1-8-3V6m0 6c0 2 3.6 3 8 3s8-1 8-3",
  clock: "M21 12a9 9 0 1 1-18 0 9 9 0 0 1 18 0ZM12 7v5l3 2",
  check: "m5 12 4 4L19 6",
  database: "M21 6c0 2-4 3-9 3S3 8 3 6s4-3 9-3 9 1 9 3Zm0 0v12c0 2-4 3-9 3s-9-1-9-3V6m0 6c0 2 4 3 9 3s9-1 9-3",
};

export const agentIcon = (agent) => ({ codex: "terminal", "claude-code": "sparkles", dsh: "layers" })[agent] || "terminal";

// 图标只作辅助；操作名称由按钮文字或 aria-label 提供。
export function Icon({ name, size = 20, class: className = "" }) {
  return h("svg", { class: `ui-icon ${className}`, width: size, height: size, viewBox: "0 0 24 24", fill: "none",
    stroke: "currentColor", "stroke-width": 1.8, "stroke-linecap": "round", "stroke-linejoin": "round", "aria-hidden": "true", focusable: "false" },
  h("path", { d: paths[name] || paths.box }));
}
