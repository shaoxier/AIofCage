# AIofCage

Qiniu Builder Day 2026 赛道一参赛作品：桌面级 AI Agent 安全沙箱。

题目原文要求：
- 核心是安全，其次是最终用户的友好性，不能是太技术化的配置
- 支持在沙箱内跑主流 agent 应用，如 claude code、codex、dsh 等
- 所有主流 MCP 配置时均无需在沙箱内填写任何敏感信息（api key、password 等）

## 架构（已定）

- Go 单仓库，桌面界面用 Wails。
- **隔离**：Docker 容器，项目目录按相同路径挂载进去，其他什么都不挂。
- **强制出网走门卫**：agent 容器只接入 `--internal` 网络；门卫代理是一个边车容器，同时接入 internal 网络和外网；agent 容器的 `HTTP(S)_PROXY` 指向门卫。
- **门卫代理**：用 Go + elazarl/goproxy 做 HTTPS 中间人代理。
  - 自签 CA，装进 agent 容器的系统信任库，同时设置 `NODE_EXTRA_CA_CERTS`。
  - 按域名白名单放行。
  - 对配置了密钥的域名，把请求头里的占位符替换成真 key。
  - 记录日志，统计用量。
- **密钥**：只存在 macOS 钥匙串里。桌面应用读取后，通过带 token 的管理接口下发给门卫，只保存在门卫内存中。
- **MCP**：HTTP 类的 MCP 在沙箱里只配占位符，由门卫注入真密钥。非 HTTP 的凭证（比如数据库密码）不在第一版范围内。
- **参考**：mattolson/agent-sandbox（MIT 协议）的“边车代理 + 防火墙”设计；Docker Sandboxes（sbx）的占位符方案。

## 已验证的结论（2026-10-22，在 Docker sbx 里实测）

- 占位符替换可行：沙箱的环境变量里只有占位符，代理对 xapi.qiniu.io 替换后调用成功；发往白名单以外的域名返回 403；在沙箱的文件和环境变量里搜真 key，0 处。
- Codex 0.149.1 不认识 gpt-6 系列（报 `Model metadata … not found`，`/model` 列不出模型），需要 ≥ 0.161.0。Codex 的模型列表来自程序自带的目录，不读中转站的 `/v1/models`。
- Codex 自定义 provider 的写法：`[model_providers.xxx]` 写 `base_url`、`env_key`、`wire_api = "responses"`。Codex 要信任代理的 CA（sbx 的做法是把 CA 装进系统信任库）。
- 比赛用的模型接口：`https://xapi.qiniu.io/v1`，OpenAI 兼容，`/chat/completions` 和 `/responses` 都能用。模型有 gpt-5.5、gpt-5.6-sol、gpt-5.6-terra、gpt-6-astra、gpt-6-sol、gpt-6.1-sol。
- 比赛 key 存在钥匙串里，service 名是 `sbx-builderday-key`，读取命令：`security find-generic-password -a "$USER" -s sbx-builderday-key -w`。**不要写进代码、仓库或日志。**

## 待验证

- Claude Code 在沙箱里怎么认证：考虑用 `CLAUDE_CODE_OAUTH_TOKEN` 放占位符，由门卫对 api.anthropic.com 注入。
- dsh 用哪个 provider、从哪里读 key。
- 在 Docker Desktop for Mac 上，“internal 网络 + 边车容器”能不能真正挡住直接出网。

## MVP 范围（按这个顺序做）

1. 核心链路：容器 + 边车门卫 + 占位符注入，让 Codex 用 xapi 跑通；验证外传 key 会失败。
2. 桌面应用：密钥管理（钥匙串）、沙箱启停、一键打开 Agent 终端、实时请求日志。
3. MCP（演示 GitHub MCP 不用 token 也能用）、Claude Code 和 dsh 支持。
4. 加分项：用量上限和告警、“模拟攻击”演示按钮。

## 约定

- 文档、README 和界面文案都用中文。
- 永远不要提交密钥；提交前扫描 `sk-`、`ghp_` 这类字符串。
- README 里的功能清单，完成一项勾一项。
- Git：一个功能一个分支一个 PR，不直接往 `main` 推。分支名用 `feat/xxx`、`fix/xxx`、`docs/xxx`，合并用 squash。队友不 fork，直接在本仓库开分支。
