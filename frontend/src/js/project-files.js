// 终端右侧的项目文件栏：只读浏览、纯文本预览、把项目内的路径插入当前会话。
// 文件内容由 Agent 控制：预览一律当纯文本显示，不渲染 HTML、SVG 或 Markdown，也不用其他程序打开。
import { Component } from "preact";
import { useEffect, useRef, useState } from "preact/hooks";
import { html, call, formatBytes, Icon } from "./core.js";
import { insertable } from "./project-paths.js";

const size = (bytes) => (bytes < 1024 ? `${bytes} 字节` : formatBytes(bytes));

const NOTES = { binary: "二进制文件，不预览。", large: "图片太大或读不出尺寸，不预览。", link: "符号链接，不跟随也不预览。", other: "特殊文件（如管道或设备），不读取。" };

// 对话流式输出时终端页会频繁重绘；文件栏的属性没变就不跟着重绘整棵树。
export class ProjectFiles extends Component {
  shouldComponentUpdate(next) {
    return Object.keys(next).some((key) => next[key] !== this.props[key]);
  }
  render(props) {
    return html`<${FilePanel} ...${props} />`;
  }
}

function FilePanel({ sandboxId, busy, canInsert, onInsert, onClose, notify }) {
  const [hidden, setHidden] = useState(false);
  // 用 Map：文件夹可能就叫 constructor、__proto__ 这类对象自带的名字。
  const [folders, setFolders] = useState(() => new Map());
  const [open, setOpen] = useState(() => new Set([""]));
  const [selected, setSelected] = useState(null);
  const [preview, setPreview] = useState(null);
  const generation = useRef(0);
  const previewing = useRef(0);
  const idle = useRef(busy);

  const store = (path, value) => setFolders((prev) => new Map(prev).set(path, value));
  const load = (paths, turn = generation.current) => paths.forEach((path) =>
    call("ListProjectEntries", sandboxId, path, hidden)
      .then((listing) => turn === generation.current && store(path, listing))
      .catch((error) => turn === generation.current && store(path, { error: error.message })));
  const show = async (path, keep = false) => {
    const turn = ++previewing.current;
    setSelected(path);
    // 刷新时保留旧预览和滚动位置，等新内容到了再替换。
    if (!keep) setPreview(null);
    try {
      const file = await call("ReadProjectFile", sandboxId, path);
      if (turn === previewing.current) setPreview(file);
    } catch (error) { if (turn === previewing.current) setPreview({ error: error.message }); }
  };
  const reload = () => {
    generation.current++;
    load([...open]);
    if (selected) show(selected, true);
  };
  useEffect(() => { generation.current++; setFolders(new Map()); load([...open]); }, [hidden]);
  // 每轮对话结束时刷新一次，看到智能体刚改过的文件。
  useEffect(() => {
    if (busy < idle.current) reload();
    idle.current = busy;
  }, [busy]);

  const toggle = (path) => {
    const next = new Set(open);
    if (next.has(path)) next.delete(path);
    else { next.add(path); load([path]); }
    setOpen(next);
  };
  const copy = async (path) => {
    try {
      if (window.runtime?.ClipboardSetText) await window.runtime.ClipboardSetText(path);
      else await navigator.clipboard.writeText(path);
      notify("已复制路径");
    } catch { notify("复制失败", true); }
  };
  const unsafe = "文件名里有控制字符或不可见字符，不能插入或复制";
  const insertButton = (path) => html`<button type="button" class="project-insert" onClick=${() => onInsert(path)}
    disabled=${!canInsert || !insertable(path)} title=${!insertable(path) ? unsafe : !canInsert ? "先打开一个会话" : `插入 ${path}`}
    aria-label=${`插入路径：${path}`}><${Icon} name="insert" size=${15} /></button>`;

  const rows = (path, depth) => {
    const folder = folders.get(path);
    const indent = `--depth: ${depth}`;
    if (!folder) return html`<li class="project-note" style=${indent}>正在读取…</li>`;
    if (folder.error) return html`<li class="project-note error" style=${indent}>${folder.error}</li>`;
    if (!folder.entries.length && !folder.hidden) return html`<li class="project-note" style=${indent}>空文件夹</li>`;
    return html`${folder.entries.map((entry) => {
      const dir = entry.kind === "dir";
      const expanded = dir && open.has(entry.path);
      return html`<li key=${entry.path}>
        <div class=${`project-row ${entry.path === selected ? "selected" : ""}`}>
          <button type="button" class="project-entry" style=${indent} title=${entry.path} aria-expanded=${dir ? expanded : undefined}
            aria-current=${entry.path === selected ? "true" : undefined} onClick=${() => dir ? toggle(entry.path) : show(entry.path)}>
            ${dir ? html`<${Icon} name="chevron" size=${14} class=${expanded ? "" : "collapsed"} />` : html`<span class="project-spacer"></span>`}
            <${Icon} name=${dir ? "folder" : entry.kind === "link" ? "link" : "file"} size=${15} />
            <span class="project-name">${entry.name}</span>
          </button>
          ${insertButton(entry.path)}
        </div>
        ${expanded && html`<ul>${rows(entry.path, depth + 1)}</ul>`}
      </li>`;
    })}
    ${folder.more && html`<li class="project-note" style=${indent}>条目太多，只列出了一部分</li>`}
    ${folder.hidden > 0 && html`<li class="project-note" style=${indent}>${folder.hidden} 个隐藏项（以“.”开头）</li>`}`;
  };

  const name = selected?.split("/").pop();
  return html`<aside class="project-files" aria-label="项目文件">
    <div class="project-files-head">
      <strong>项目文件</strong>
      <label class="project-hidden-toggle"><input type="checkbox" checked=${hidden} onChange=${(e) => setHidden(e.target.checked)} />显示隐藏项</label>
      <button type="button" class="icon-button" aria-label="刷新文件列表" title="刷新" onClick=${reload}><${Icon} name="refresh" size=${16} /></button>
      <button type="button" class="icon-button" aria-label="收起项目文件" title="收起" onClick=${onClose}><${Icon} name="x" size=${16} /></button>
    </div>
    <ul class="project-tree" aria-label="项目文件列表">${rows("", 0)}</ul>
    ${selected && html`<section class="project-preview" aria-label=${`预览 ${name}`}>
      <div class="project-preview-head">
        <span class="project-preview-title" title=${selected}><strong>${name}</strong>${preview?.size !== undefined && html`<small>${size(preview.size)}</small>`}</span>
        ${insertButton(selected)}
        <button type="button" class="icon-button" aria-label=${`复制路径：${selected}`} title=${insertable(selected) ? "复制路径" : unsafe}
          disabled=${!insertable(selected)} onClick=${() => copy(selected)}><${Icon} name="copy" size=${15} /></button>
        <button type="button" class="icon-button" aria-label="关闭预览" title="关闭预览" onClick=${() => { previewing.current++; setSelected(null); }}><${Icon} name="x" size=${15} /></button>
      </div>
      ${!preview ? html`<p class="project-note">正在读取…</p>`
        : preview.error ? html`<p class="project-note error">${preview.error}</p>`
        : preview.kind === "text" ? html`<pre class="project-text" tabindex="0">${preview.text}</pre>
            ${(preview.truncated || preview.clipped) && html`<p class="project-note">${[preview.truncated && "只显示前 512 KB", preview.clipped && "过长的行已截断"].filter(Boolean).join("；")}。</p>`}`
        : preview.kind === "image" ? html`<div class="project-image"><img src=${preview.image} alt=${name} /></div>`
        : html`<p class="project-note">${NOTES[preview.kind] ?? "不预览。"}</p>`}
    </section>`}
  </aside>`;
}
