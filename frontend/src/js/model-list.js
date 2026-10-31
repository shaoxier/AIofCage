// 模型列表的筛选：不依赖界面库，便于单独测试。

// matchModels 按输入筛选模型：忽略大小写，开头匹配的排在前面，最多返回 limit 个。
// 输入恰好是某个完整的模型名时（例如刚选中），仍显示全部，方便换成别的模型。
export function matchModels(models, query, limit = 50) {
  const list = models ?? [];
  const text = (query ?? "").trim().toLowerCase();
  if (!text || list.some((model) => model.toLowerCase() === text)) return list.slice(0, limit);
  const starts = [], contains = [];
  for (const model of list) {
    const name = model.toLowerCase();
    if (name.startsWith(text)) starts.push(model);
    else if (name.includes(text)) contains.push(model);
  }
  return [...starts, ...contains].slice(0, limit);
}

// modelOptions 返回下拉里显示的模型，以及超出 limit 没有显示的个数。
export function modelOptions(models, query, limit = 50) {
  const all = matchModels(models, query, Infinity);
  return { options: all.slice(0, limit), more: Math.max(all.length - limit, 0) };
}

// withModel 把某个模型放在列表最前面，例如供应商配置的模型不在它返回的列表里时，仍能换回来。
export function withModel(models, model) {
  const list = models ?? [];
  return model && !list.includes(model) ? [model, ...list] : list;
}
