import { useEffect, useState } from "preact/hooks";
import { html, Icon } from "./core.js";
import { modelOptions } from "./model-list.js";

// ModelPicker 是可以搜索的模型下拉：输入框可以直接填写，下拉列出供应商返回的模型，
// 按输入筛选；onRefresh 存在时带一个“获取模型列表”按钮。供应商表单和终端共用。
// 回车只在用方向键选中某一项后才替换输入，否则保留手填的名称。
export function ModelPicker({ id, value, models, onChange, onRefresh, loading, disabled, required, label = "模型" }) {
  const [open, setOpen] = useState(false);
  const [active, setActive] = useState(-1);
  const { options, more } = modelOptions(models, value);
  const listId = `${id}-options`;
  const shown = open && options.length > 0;
  const highlighted = shown && active >= 0 && active < options.length;
  useEffect(() => { if (highlighted) document.getElementById(`${listId}-${active}`)?.scrollIntoView({ block: "nearest" }); }, [active, highlighted]);
  const show = (next) => { setOpen(next); setActive(-1); };
  const choose = (model) => { if (model !== value) onChange(model); show(false); };
  const onKeyDown = (event) => {
    if (event.key === "ArrowDown" || event.key === "ArrowUp") {
      event.preventDefault();
      if (!options.length) return;
      const down = event.key === "ArrowDown";
      setOpen(true);
      setActive((current) => !shown || current < 0 ? (down ? 0 : options.length - 1) : (current + (down ? 1 : -1) + options.length) % options.length);
    } else if (event.key === "Enter" && highlighted) {
      event.preventDefault();
      choose(options[active]);
    } else if (event.key === "Enter" && open) {
      show(false); // 保留手填的名称；表单照常提交
    } else if (event.key === "Escape" && open) {
      event.preventDefault();
      show(false);
    }
  };
  return html`<div class="model-picker">
    <div class="model-picker-field">
      <input id=${id} class="mono" role="combobox" aria-label=${label} aria-autocomplete="list" aria-expanded=${shown}
        aria-controls=${listId} aria-activedescendant=${highlighted ? `${listId}-${active}` : undefined} autocomplete="off" spellcheck="false"
        required=${required} disabled=${disabled} value=${value}
        onInput=${(event) => { onChange(event.target.value); show(true); }}
        onFocus=${() => models?.length && show(true)} onBlur=${() => show(false)} onKeyDown=${onKeyDown} />
      <button type="button" class="icon-button" aria-label="展开模型列表" title=${models?.length ? `${models.length} 个模型` : "还没有模型列表"}
        disabled=${disabled || !models?.length} onMouseDown=${(event) => event.preventDefault()}
        onClick=${() => { show(!open); document.getElementById(id)?.focus(); }}><${Icon} name="chevron" size=${17} /></button>
      ${onRefresh && html`<button type="button" class="icon-button" aria-label="获取模型列表" title="获取模型列表"
        disabled=${disabled || loading} onClick=${onRefresh}><${Icon} name="refresh" size=${17} /></button>`}
    </div>
    ${shown && html`<div class=${more > 0 ? "model-options has-more" : "model-options"}>
      <ul id=${listId} role="listbox" aria-label="可选模型">
        ${options.map((model, index) => html`<li id=${`${listId}-${index}`} role="option" aria-selected=${index === active}
          class=${index === active ? "active" : ""} onMouseDown=${(event) => { event.preventDefault(); choose(model); }}>${model}</li>`)}
      </ul>
      ${more > 0 && html`<p class="model-options-more">还有 ${more} 个，请输入名称筛选</p>`}
    </div>`}
  </div>`;
}
