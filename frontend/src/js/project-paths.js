// 把项目文件路径插入会话输入框或 Shell 时用到的纯函数。
// 控制字符会被终端当成按键（例如回车、Ctrl+C）或控制序列；不可见的格式字符（双向排版、零宽）
// 能让看到的文件名和实际的不一样。含这些字符的路径不插入。
export const insertable = (path) => Boolean(path) && !/[\p{Cc}\p{Cf}\u2028\u2029]/u.test(path);

// 在光标处插入路径，前后补空格，返回新文本和插入后的光标位置。
export function insertAt(text, insertion, start = text.length, end = start) {
  const before = text.slice(0, start);
  const after = text.slice(end);
  const value = (before && !/\s$/.test(before) ? " " : "") + insertion + (after && /^\s/.test(after) ? "" : " ");
  return { text: before + value + after, cursor: before.length + value.length };
}

// 给 Shell 用的路径：需要时加单引号；以“-”或“+”开头的相对路径加“./”，避免被当成选项。
export function shellQuote(path) {
  const safe = /^[-+]/.test(path) ? `./${path}` : path;
  return /^[A-Za-z0-9_./@%+=:,-]+$/.test(safe) ? safe : `'${safe.replaceAll("'", `'\\''`)}'`;
}
