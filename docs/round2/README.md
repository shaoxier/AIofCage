# 第二轮迭代资料

决赛第二轮的计划、代码排查记录和方向调研，2026-10-30 整理。

## 先看哪份

1. [第二轮迭代 todo 清单](todo.md)：全员先看第一节总览、第二节分工和第八节第一轮评审意见。各条线再看第三节里自己负责的条目；第五节是还没答复的问题。
2. [排查记录](audits/)：写清单时对代码的逐项排查，动手做对应条目时再查。
3. [第二轮方向建议](direction-proposal.pdf)：会议之前的方向提案，清单里 P0、P1 编号的出处。
4. [AI Agent 安全沙箱真实案例与需求](agent-sandbox-cases-and-needs.md)：方向建议依据的调研报告。

## 排查记录

| 文件 | 内容 | 对应条目 |
|---|---|---|
| [1-first-run.md](audits/1-first-run.md) | 首次使用链路：下载清单、失败点、教程 bug、长操作的进度和取消 | 1、2、6 |
| [2-terminal-and-providers.md](audits/2-terminal-and-providers.md) | 终端与供应商：恢复会话、多对话、模型列表、七牛预设、文件栏的接入点 | 3、4、5、11 |
| [3-devenv-gatekeeper-isolation.md](audits/3-devenv-gatekeeper-isolation.md) | 开发环境、门卫规则与隔离自检：镜像现状、P0-2 的落点、P0-4 的方案 | 7、8 |
| [4-external-research.md](audits/4-external-research.md) | 外部调研：文件栏、供应商预设、七牛接口，附来源链接 | 3、4、5 |
| [5-qiniu-api-probe.md](audits/5-qiniu-api-probe.md) | llmapi 和 xapi 的接口实测结果 | 3、5 |

## 说明

- 这些是计划和调研，不代表功能已经完成。完成情况以[功能清单](../features.md)和[验收记录](../validation/README.md)为准。
- 文中的代码位置（如 `proxy.go:236`）对应 `main` 的 4f75106，代码改动后行号可能变化。
- 标了“推测”“待复现”“需实测”的结论还没有验证，引用前先复现。
