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
