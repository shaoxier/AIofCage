// 隔离检查的状态与计数。“无法验证”和“跳过”单独计数，不算通过；未知状态按“无法验证”算。
export const CHECK_STATUSES = {
  passed: { label: "通过", tone: "ok" },
  problem: { label: "发现问题", tone: "bad" },
  inconclusive: { label: "无法验证", tone: "warn" },
  skipped: { label: "跳过", tone: "idle" },
};

export const checkStatus = (result) => (CHECK_STATUSES[result?.status] ? result.status : "inconclusive");

export function checkCounts(results = []) {
  return Object.entries(CHECK_STATUSES).map(([status, value]) => ({ status, ...value,
    count: results.filter((result) => checkStatus(result) === status).length }));
}

export const checkCountsText = (results) => checkCounts(results).map((item) => `${item.label} ${item.count}`).join(" · ");
