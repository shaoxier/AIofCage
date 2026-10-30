# 早期工程骨架与模块实施记录

> 历史记录：部分版本、命令、状态和接入方式已被后续 PR 替换。当前模块职责见[当前架构](architecture-current.md)，构建与检查见[开发指南](development.md)。产品接口请结合 `internal/service` 代码及[工作区规格](product-workspace.md)阅读，不按本文的早期状态判断当前完成度。

当前交付的是桌面入口、内存会话、模块接口，以及可配置、可转发的门卫（S2）。目的是让队友可以在同一工程里分模块开发；隔离、Keychain、模型、MCP 和审计采集仍未实现，G0—G5 也没有因此变成已验证。

## 入口与调用关系

```text
main.go / Wails
    ├── frontend/src：Preact + htm（vendor 目录内固定版本，无 npm 构建）
    │     沙箱 / 供应商 / MCP / 日志 / 设置
    └── internal/service.App（唯一暴露给前端的 API，不返回任何密钥）
          ├── store：供应商与沙箱元数据
          ├── vault.Keychain：密钥只写入、只由运行时读取
          └── runtime.Manager：专用虚拟机与沙箱生命周期

cmd/cagectl ──> runtime.Manager（与桌面应用共用）
cmd/gatekeeper（Linux 版嵌入运行时，在每个沙箱里运行）
    └── internal/gatekeeper：管理 / 代理 / MCP 三个监听
```

## 模块边界与实际状态

| 模块 | 已有内容 | 下一步及限制 |
|---|---|---|
| `service` | 环境概览；供应商增删改、预设、连通性测试（宿主直连，不经沙箱）；沙箱创建（含项目导入）、启停、终端、切换供应商、删除；进度事件；事件订阅、JSONL 审计、用量与资源监视 | `existing_key_ref` 只允许 AIofCage 自己的条目和预设列出的条目，防止界面被用来指向任意钥匙串秘密 |
| `control` | 本机 stdio MCP 协议，无网络监听；每个控制会话只能操作自己创建的沙箱；任务支持异步执行、取消、用量元数据和同会话多 Agent 对比 | 不跨控制会话比较，不接管已有沙箱 |
| `session` | Agent 枚举（codex / claude-code / dsh） | 原“会话草稿”已由 `sandbox` 取代 |
| `runtime` | `Environment`：创建/启动专用 Colima 实例并核对实际共享与 context；`Manager`：每个沙箱独立的内部网络、出网网络、门卫与 Agent 容器，镜像按内容摘要构建并缓存，门卫先配置再启动 Agent，切换供应商热更新，停止时保存累计用量，重置时销毁并重建运行对象 | 永不回退宿主执行；重置不删除项目目录，Agent home 是否保留由用户选择 |
| `provider` | 供应商元数据（接口、协议、模型、认证方式、钥匙串条目）、协议兼容性、按协议生成最小放行规则 | Codex 只支持 Responses API |
| `sandbox` | 沙箱元数据、Agent→供应商绑定、每个沙箱+供应商一个占位符、项目必须位于 `~/CageWorkspace` 且无符号链接 | 改变已装 Agent 需重建 Agent 容器（home 数据卷保留） |
| `store` | 供应商与沙箱的 JSON 持久化（`~/Library/Application Support/AIofCage`，0600，原子写入） | 不存任何密钥 |
| `gatekeeper` | 三个独立监听；`PUT /config` 原子下发策略、密钥与额度；自签 CA + MITM；先授权再校验占位符，配了密钥的域名拒绝外来 key；只改请求头；不跟随重定向；上游错误正文脱敏；按沙箱的请求数/token 额度与 80% 告警；SSE 事件流（序号、补发、缺口）；用量统计 | 只支持 443 端口 HTTPS；MCP 入口只转发到已配置的可信连接器；配置不落盘，门卫重启后需应用重新下发（额度用基线延续） |
| `policy` | 规则校验、规范化、deny 优先、默认拒绝；精确路径或 `/*` 前缀；deny 规则可用 `*.` 通配 | 只覆盖 HTTP 方法与路径，不是 MCP 工具权限 |
| `vault` | 接口 + `Keychain` 实现：经 `/usr/bin/security` 读写登录钥匙串，写入时密钥走标准输入（不出现在进程列表），写后回读校验。普通 API key 原样保存；含空格、引号的密码和 OAuth 授权（JSON）以 base64 保存，超过约 3.4 KB 时拆成 `<条目>-partN` 多个条目（`security -i` 每行约 4 KB 上限） | 不绑定到前端；读方法只供运行时下发给门卫 |
| `oauth` | MCP 标准 OAuth：受保护资源元数据（RFC 9728）→ 授权服务器元数据（RFC 8414）→ 动态注册客户端（RFC 7591）→ 浏览器授权（PKCE S256、`resource` 参数）→ 回调到 `127.0.0.1` 随机端口（校验 state）→ 换取令牌；按条目串行续期（兼容轮换的刷新令牌） | 只支持能自动注册客户端的服务；授权只在宿主进行，刷新令牌从不离开宿主 |
| `audit` / `events` | 脱敏事件结构（含主机、方法、路径、状态、字节、token）；门卫 SSE；应用侧订阅、续传、JSONL 落盘、缺口事件 | 审计日志不做防篡改（hash 链在路线图） |
| `mcp` | 远程 streamable HTTP MCP（静态 Bearer 令牌或 OAuth 登录得到的令牌，门卫注入）与连接器服务（PostgreSQL：连接串不含密码，密码进钥匙串）；预设、放行规则、连接串校验（只允许 sslmode 等安全参数）、连接器目录（包和版本固定、允许的工具） | 连接器目前只有 PostgreSQL；dsh 暂不配置 MCP |
| `oauth` | 宿主授权/续期/撤销接口，只有秘密引用 | PKCE、发现、注册、回调、串行刷新及 Keychain |
| `connector` | 可信连接器：在独立容器里按会话启动 stdio MCP 服务，转成 streamable HTTP，只接受门卫的连接器令牌；启动配置经 `docker exec` 标准输入写入 tmpfs，读后即删；子进程日志脱敏 | 只运行连接器目录里固定的命令，不运行传入的命令或镜像；连接器容器可以出网（要连数据库），这是它作为可信组件的残余风险 |
| `buildinfo` | 桌面与门卫共用的版本与 `skeleton` 阶段 | 发布时注入真实版本 |

## 桌面开发接口

前端通过 Wails 注入的 `window.go.service.App` 调用，所有方法返回 Promise。

| 方法 | 说明 |
|---|---|
| `GetOverview()` | 版本、运行环境检查（会访问专用虚拟机）、可安装的 Agent 及版本、数据目录与工作区 |
| `ListPresets()` | 供应商预设（`internal/provider/presets.json`，模型名可能过时，可随时修改） |
| `ListProviders()` | 供应商列表，只返回“密钥是否已保存”和使用它的沙箱，不返回密钥 |
| `SaveProvider(request)` | 新建或编辑；`api_key` 只写入钥匙串，编辑时留空表示不改密钥；修改在用密钥前须停止引用它的沙箱，其他配置保存为下次重启生效 |
| `DeleteProvider(id)` | 仍有沙箱在用时拒绝；同时删除 AIofCage 保存的钥匙串条目 |
| `TestProvider(id)` | 由桌面进程直连供应商的模型列表接口，返回是否可达、密钥是否有效与耗时 |
| `ListSandboxes()` | 沙箱、实际容器状态、Agent→供应商、累计用量，以及正在进行的操作（启动/停止/删除）和最近一条进度。操作由后端记住，切换页面或重开窗口都能看到；同一沙箱的操作进行中时再次点击会被拒绝并说明原因 |
| `CreateSandbox(request)` | 名称、项目（`use` 工作区内已有文件夹 / `copy` 复制进工作区 / `new` 新建空文件夹）、Agent→供应商、可选的请求数与 token 上限 |
| `StartSandbox(id)` / `StopSandbox(id)` | 启动过程通过 `sandbox:progress` 事件推送进度；首次启动会构建镜像 |
| `BindProvider(id, agent, providerID)` | 保存某个 Agent 的供应商选择，重启沙箱后生效；当前运行快照保持原供应商 |
| `OpenTerminal(id, agent)` | 在“终端”里以 Agent 用户进入项目目录，运行该 Agent（`agent` 为空时开 Shell）；第一次会请求“自动化”权限 |
| `StartTerminal(termID, id, agent, cols, rows)` / `WriteTerminal(termID, data)` / `ResizeTerminal(termID, cols, rows)` / `CloseTerminal(termID)` | 应用内置终端（沙箱详情页的“终端”标签）：后端用 PTY 运行 `docker exec -it`，与系统终端的权限相同（Agent 用户、项目目录）；输出以 `terminal:data` 事件（base64）推给 xterm.js，进程结束发 `terminal:exit`；终端 ID 由窗口生成，避免启动输出丢失；最多同时 12 个，退出应用时全部关闭。切换页面时终端继续运行，回来时原样显示 |
| `StartConversation(id, sandboxID, agent)` / `ListRecoverableConversations()` / `RecoverConversation(id)` | 结构化会话启动、恢复和列表；恢复只依赖非敏感原生 session 元数据，要求沙箱运行、桥接组件存在且供应商配置匹配，不返回消息正文或凭据 |
| `DiagnoseConversation(sandboxID, agent)` | 检查本机环境、供应商绑定、钥匙串凭据是否存在、门卫容器与会话组件，不发送模型计费请求，也不等于上游模型已经可用 |
| `OpenProjectFolder(id)` | 在访达中打开项目文件夹 |
| `ResetSandbox(id, keepHome, restart)` | 清空门卫授权，删除连接器、Agent、门卫和网络，再按选项重建；项目与沙箱配置保留，`keepHome` 决定是否保留 Agent home，`restart` 决定是否自动恢复运行 |
| `DeleteSandbox(id, keepHome)` | 删除容器与网络；项目文件夹永远保留，`keepHome` 决定是否保留 Agent 的 home 数据卷 |
| `SelectProject()` | 系统目录选择框 |
| `RecentEvents(id)` | 最近的门卫事件（`id` 为空表示全部沙箱，最多 300 条），附沙箱名称 |
| `UsageSummary()` | 每个沙箱的累计请求/token、实时拒绝数与 CPU/内存，以及按供应商汇总的累计用量（包括已删除沙箱的用量；删除供应商时一并清掉） |
| `RevealAuditLog(id)` | 在访达中显示该沙箱的 JSONL 审计日志 |
| `ExportAuditTimeline(id)` | 导出该沙箱最近最多 300 条脱敏事件到数据目录 `audit/<id>.timeline.json`，权限 0600；每次覆盖同名快照，不是完整历史归档 |
| `ListMCPPresets()` / `ListMCPServices()` | MCP 预设与已保存的服务（不含令牌） |
| `SaveMCPService(request)` / `DeleteMCPService(id)` / `TestMCPService(id)` | 令牌或数据库密码只写入钥匙串（连接串里带的密码会被移走；OAuth 服务不接受粘贴的令牌；`no_auth` 表示公开服务、删除旧令牌）；在用时不能删除；HTTP/OAuth 服务保存和测试时按 MCP 协议获取工具清单（OAuth 先按需续期），取不到时 `tools_error` 说明原因；数据库测试在虚拟机里起一个一次性的连接器容器，用保存的密码登录并执行只读的 `SELECT 1`，能区分密码错误、库不存在、端口不通等原因（连接器镜像还没构建过时退回只测端口，并说明没验证密码） |
| `AuthorizeMCPService(id)` | OAuth 服务在默认浏览器里登录授权（最多等 5 分钟），授权保存到钥匙串，运行中的沙箱立即换上新令牌；应用每分钟检查一次在用的 OAuth 服务，令牌 10 分钟内到期就续期并下发，续期失败时提醒一次 |
| `DisconnectMCPService(id)` | 删除 OAuth 本机凭据并刷新引用它的运行沙箱，保留服务配置；当前不调用上游撤销端点 |
| `SetSandboxMCP(id, ids, filesystem)` | 选择沙箱可用的远程 MCP 与内置 filesystem；运行中立即下发 |
| `SetSandboxMCPPermissions(id, ids, filesystem, permissions)` | 保存每个服务的工具白名单（固定连接器和远程 HTTP/OAuth 服务），运行中立即刷新门卫；显式空数组拒绝所有工具；没有指定的服务保留原授权，新启用的远程服务默认只开放标注只读的工具 |
| `RefreshMCPTools(id)` | 重新获取远程服务的工具清单；新出现的工具在各沙箱里默认不开放 |
| `SecurityDiagnostics(id)` | 返回安全边界说明、门卫状态、MCP 授权、最近拒绝数与最近隔离检查；边界说明不是探针通过结果，最近隔离检查只保存在当前应用进程 |
| `ListAttacks()` / `SimulateAttacks(id)` | 7 个模拟攻击（偷环境变量里的 key、外传到攻击者域名、绕过代理直连、用外来 key 借白名单域名外传、读写宿主文件、DNS 外传、找数据库密码并直连数据库），各自对应一起真实事件；在沙箱里以 Agent 用户执行，宿主再核对输出中没有真实密钥；全部非破坏性 |
| `ExplainEvent(sandboxID, eventID)` / `SummarizeSandbox(id)` | AI 安全解说：只把事件元数据和允许的域名发给选定的供应商（桌面进程直接调用），提示词声明元数据是不可信数据；每条事件标明来源（“系统”下发配置等 / “沙箱内程序”），并标出用户运行内置模拟攻击期间的事件，避免把系统动作或演示说成 Agent 的行为；时间统一换成本机时间（与日志页一致），不让模型换算时区；结果按事件和解说供应商缓存，换供应商会重新生成；单次请求最长等 90 秒（推理模型常需十几秒以上，不沿用连接测试的 12 秒超时）；AI 不参与放行 |
| `ExplainerProvider()` / `SetExplainerProvider(id)` | AI 解说使用的供应商，默认取第一个已保存密钥的供应商 |

后台监视（`StartMonitor`，每 5 秒）：为每个运行中的沙箱订阅门卫 SSE（同一门卫运行内按 `Last-Event-ID` 续传，门卫重启后从头订阅），把事件追加写入 `~/Library/Application Support/AIofCage/audit/<沙箱ID>.jsonl`（0600），推送 `sandbox:event`；轮询门卫 `/usage` 并按“门卫运行 + 供应商”计算增量写回累计用量（应用重启也不会重复计数），推送 `sandbox:usage`；用一次 `docker stats` 采样所有运行中沙箱，推送 `sandbox:stats`。额度到 80% 与超额由门卫产生事件，界面弹出提醒。

复制项目时只复制普通文件、目录和符号链接本身（不跟随链接），超过 2 GB 或 20 万个文件会拒绝；拒绝复制家目录、根目录和工作区本身。

真实环境的端到端测试：`AIOFCAGE_E2E=1 go test -run TestEndToEnd ./internal/service/`（需要专用虚拟机和钥匙串中的 `sbx-builderday-key`），会创建、启动、进入、切换供应商、停止并删除一个临时沙箱。

界面自测：`frontend/src` 是静态文件，可以用任意静态服务器打开，并在加载 `main.js` 前定义假的 `window.go.service.App`，用无头浏览器截图检查各页面；地址里的 `#providers` 等锚点会直接打开对应页面。

## 沙箱运行时与 cagectl

`make cagectl` 先编译 Linux 门卫和连接器并把它们嵌入程序，再生成 `bin/cagectl`。它和桌面应用共用同一套运行时与数据目录，用于开发、自动检查和演示兜底：

```bash
make cagectl
./bin/cagectl env up                                   # 创建/启动专用虚拟机并核对共享边界
./bin/cagectl provider add --id xapi --name "七牛 xapi" --agent codex --protocol openai-responses \
    --base-url https://xapi.qiniu.io/v1 --model gpt-6.1-sol --keychain-service sbx-builderday-key
./bin/cagectl sandbox create --name demo --project ~/CageWorkspace/demo --bind codex=xapi --max-requests 200
./bin/cagectl sandbox start demo
./bin/cagectl sandbox exec demo -t -- codex            # 交互；非交互运行请关闭标准输入（codex exec 会等待管道输入）
./bin/cagectl sandbox events demo                      # 实时事件
./bin/cagectl mcp add --kind postgres --id shopdb --name 商城数据库 \
    --url 'postgresql://app@localhost:5432/shop?sslmode=disable' --token-stdin   # 密码从标准输入进钥匙串
./bin/cagectl mcp add --kind oauth --id notion --name Notion --url https://mcp.notion.com/mcp
./bin/cagectl mcp login notion                          # 打开浏览器登录授权
./bin/cagectl sandbox mcp demo shopdb                  # 运行中立即启动连接器并下发
./bin/cagectl sandbox stop demo
```

PostgreSQL 必须使用专用只读账号。连接器会拒绝超级用户、可切换到管理角色、可创建数据库/表或可写业务表的账号；不是仅检查工具名称或事务模式。仅接受单条 SELECT、只读 CTE 与允许的内置函数，拒绝 COPY、文件函数、程序执行、写入与事务切换。未知语法默认拒绝，复杂查询可能需要改写。密码仍由可信连接器持有，Agent 中的占位凭据只代表该沙箱获准调用的服务。

由数据库管理员在目标库中创建账号，例如下列 SQL（密码通过安全渠道设置，不要提交仓库）。只为需要访问的表授权，禁止直接把管理员账号用于 MCP：

```sql
CREATE ROLE cage_reader LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS;
GRANT CONNECT ON DATABASE shop TO cage_reader;
GRANT USAGE ON SCHEMA public TO cage_reader;
GRANT SELECT ON TABLE public.products, public.orders TO cage_reader;
ALTER ROLE cage_reader SET default_transaction_read_only = on;
```

账号若经 PUBLIC 或其他角色继承了写入/建表权限，也需要管理员先收回相应权限。连接器每次实际读取前重新检查有效权限；不得向该账号授权不可信的安全定义者函数或视图。SQL 策略测试在连接器镜像构建中执行；真实网关、数据库权限与越权回归可运行 `AIOFCAGE_E2E=1 go test ./internal/service -run '^TestEndToEndDatabaseMCP$' -count=1 -v`。

一个沙箱对应的 Docker 对象（都带标签 `io.aiofcage.sandbox=<ID>`）：

| 对象 | 名称 | 要点 |
|---|---|---|
| 内部网络 | `aiofcage-<ID>-in` | `--internal`，无默认路由 |
| 出网网络 | `aiofcage-<ID>-out` | 只有门卫和连接器接入，Agent 不在这里 |
| 门卫容器 | `aiofcage-<ID>-gate` | 只读根、无特权；管理端口只发布到宿主 `127.0.0.1` 的随机端口；管理 token 由门卫自己生成，写在只有门卫能读的 tmpfs，宿主用 `docker exec` 读取，不进 `docker inspect` |
| Agent 容器 | `aiofcage-<ID>-agent` | UID 1000、只读根、无特权、`--dns 127.0.0.1`；只挂载项目目录（与宿主同一路径）；`/etc/ssl/certs` 与 `/tmp` 为 tmpfs，每次启动写入门卫 CA |
| 连接器容器 | `aiofcage-<ID>-mcp-<服务ID>` | 每个启用的数据库 MCP 一个；标签 `io.aiofcage.role=connector`；UID 1000、只读根、无特权；只接入出网网络；启动配置（含密码）写在只有连接器能读的 tmpfs，读后即删 |
| home 数据卷 | `aiofcage-<ID>-home` | Agent 的配置与会话记录，跨重启保留 |

启动顺序：校验虚拟机 → 构建/复用镜像 → 网络与容器 → 启动门卫 → 启动连接器并投递密码（如有）→ 下发策略、密钥与连接器路由（从钥匙串读取）→ 启动 Agent → 写入 CA 与 Agent 配置（`~/.aiofcage/env` 里只有占位符）。任何一步失败都会停掉所有容器，不留下“Agent 在跑、门卫没配置”的状态。停止时先停 Agent，再停连接器，最后停门卫。重置沿用停止前的授权撤销顺序，销毁容器、网络和可选的 Agent home，然后复用同一启动顺序；沙箱元数据和项目目录始终保留，重建失败会返回错误而不删除配置。

## 各 Agent 的接入方式

| Agent | 镜像中的包 | 沙箱里的配置（只有占位符） | 供应商要求 |
|---|---|---|---|
| Codex | `@openai/codex@0.164.0` | `~/.codex/config.toml`：自定义 provider，`env_key = AIOFCAGE_CODEX_API_KEY`，`wire_api = "responses"`，项目目录设为受信任 | OpenAI Responses 接口（0.164 不再支持 Chat） |
| Claude Code | `@anthropic-ai/claude-code@2.1.309`（允许它的 postinstall：只是从平台可选依赖里复制原生二进制，不联网） | 环境变量 `ANTHROPIC_BASE_URL`、`ANTHROPIC_AUTH_TOKEN`（Bearer）或 `ANTHROPIC_API_KEY`（x-api-key）、模型、关闭遥测和自动更新；`~/.claude.json` 只合并“已完成引导、信任项目目录、已批准该占位符”三项 | Anthropic Messages 接口 |
| dsh | `@deepseek-ai/dsh@0.2.1-rc.2` | `DEEPSEEK_API_KEY`、`DSH_TELEMETRY_DISABLED=1` | 目前只支持 DeepSeek 官方接口 `https://api.deepseek.com/anthropic`（x-api-key）；改地址需要修改 dsh 的 profile YAML，暂未支持；模型在 dsh 里选择 |

镜像基于 Node 24（dsh 依赖 `import.meta.main`，Node 22.16 下会静默不执行）。`/tmp` 允许执行（dsh 的原生模块、编译器和测试会从 `/tmp` 运行程序）。多个供应商可以共用同一个接口地址（例如 Claude Code 和 dsh 都用 DeepSeek），门卫按请求里携带的占位符决定注入哪一把 key。

## MCP 接入

| 类型 | 做法 | 沙箱里看到的 |
|---|---|---|
| 远程 HTTP MCP（Bearer 令牌，例如 GitHub 官方 `https://api.githubcopilot.com/mcp/`） | 令牌存钥匙串 `aiofcage-mcp-<ID>`；启用后门卫为该服务地址放行 GET/POST/DELETE 并注入令牌 | Codex：`[mcp_servers.<ID>] url + bearer_token_env_var`；Claude Code：`~/.claude.json` 的 `mcpServers`（整体由 AIofCage 管理），请求头里只有占位符 |
| PostgreSQL 数据库（密码） | 连接串（不含密码）存在 `mcp.json`，密码存钥匙串 `aiofcage-mcp-<ID>`；沙箱启动时为它起一个可信连接器容器，运行 `@modelcontextprotocol/server-postgres@0.6.2`（每条查询都在只读事务里执行）；门卫的 MCP 入口 `8890` 校验占位符、只放行 `query` 工具、记录工具名（不记录 SQL 和结果），再用连接器令牌转发 | 两个 Agent 都配置为 `http://gatekeeper:8890/mcp/<ID>`，请求头里只有占位符；沙箱里既没有密码，也没有通往数据库的网络 |
| 远程 OAuth MCP（例如 Notion `https://mcp.notion.com/mcp`、Linear、Sentry） | 用户在 MCP 页点“登录授权”，应用在宿主完成 OAuth 并把授权（客户端 ID、访问令牌、刷新令牌）存进钥匙串 `aiofcage-mcp-<ID>`；门卫只拿到访问令牌，按与静态令牌相同的规则注入；应用在令牌到期前续期并重新下发 | 与静态令牌服务相同：URL + 占位符请求头。因为请求头已设置，Claude Code 不会自己发起 OAuth；登录过期时这个服务暂不开放，沙箱照常启动 |
| 内置 filesystem | 镜像内置 `@modelcontextprotocol/server-filesystem@2.0.0`，参数只有项目目录 | 两个 Agent 都配置为 stdio 服务，无需密钥 |

服务的 ID 同时是 Agent 配置里的服务名（GitHub 预设默认用 `github`）。界面里“测试连接”：HTTP 服务由桌面进程直接发送 MCP `initialize` 请求；数据库只检查端口能否连通，密码在沙箱启动时由连接器使用。

本机数据库填 `localhost` 即可：连接器在虚拟机里，会改用 `host.lima.internal` 访问 Mac 的本机端口。建议给 Agent 用只读数据库账号；即使账号有写权限，`server-postgres` 也只在只读事务里执行查询。

一次数据库调用的路径：Agent → `gatekeeper:8890/mcp/<ID>`（占位符）→ 门卫校验服务、方法和工具 → 连接器（连接器令牌，只有门卫和连接器知道）→ 每个 MCP 会话一个 `mcp-server-postgres` 进程 → 数据库。Claude Code 2.1.309 会先试探 `server/discover`，门卫按“方法不存在”回答且不记为拦截，它随后改用 `initialize`。

## 门卫本机开发

`make gatekeeper` 生成本机二进制；`make gatekeeper-linux` 生成沙箱里用的 Linux/arm64 二进制，并把 `docker/gatekeeper/Dockerfile` 一起放进 `build/gatekeeper/`：

```bash
make gatekeeper-linux
docker --context colima-aiofcage build -t aiofcage/gatekeeper:dev build/gatekeeper
```

管理 token 可以用 `--token-stdin` 从标准输入第一行读取（容器内使用，避免出现在 `docker inspect`），也可以在本机开发时用环境变量 `CAGE_ADMIN_TOKEN`。监听地址必须是具体接口 IP，默认分别为 `127.0.0.1:8899`（管理）、`127.0.0.1:8888`（代理）、`127.0.0.1:8890`（MCP）。

| 入口 | 行为 |
|---|---|
| 管理：无效、缺失或重复 Authorization | 401，不回显 token |
| 管理：带 Origin 或查询串 | 403 / 400 |
| `GET /health` | 未配置 503（`ready=false`）；配置后 200，返回 `run_id`、策略版本、最新事件序号 |
| `PUT /config` | 校验后原子替换策略、密钥、额度；策略版本必须递增（否则 409）；错误信息只含 ID，不含密钥或占位符 |
| `GET /events` | SSE；每条事件 `id: <run_id>:<seq>`；带 `Last-Event-ID` 断线续传；游标过旧或来自其他运行时先发 `audit.gap` |
| `GET /usage` | 请求数、拒绝数、额度计数、按密钥 ID 的请求与 token；不含任何密钥 |
| `GET /ca` | 本次运行的公有 CA 证书（私钥只在内存） |
| 代理 | 未配置时一律 503；配置后按 §6.2 的顺序授权，拒绝时返回 403/429 和 `X-Aiofcage-Reason` |
| MCP | 未配置时 503；Agent 退出时发出关闭会话后立即断开很常见：关闭会话照样转给连接器执行，记为正常；Agent 等不及回答就断开的调用记为 `client_closed`，不算连接器故障；关闭已结束的会话（404）也不算错误；`/mcp/<服务ID>` 须带该服务的占位符（否则 403，从不返回 401，避免 Agent 转去走 OAuth 登录）；只允许标准 MCP 方法，`tools/call` 只允许连接器目录里列出的工具（拒绝时返回 JSON-RPC 错误）；连接、工具调用和所有拦截写入事件 `mcp.call`（只记方法和工具名） |

`PUT /config` 的结构见 `internal/gatekeeper/config.go`：`policy`（规则）、`secrets`（ID、占位符、注入头 `Authorization` 或 `X-Api-Key`、前缀、真值）、`limits`（请求数/token 上限及基线）、`connectors`（服务 ID、占位符、连接器地址——必须是 `http://内网IP:端口`、连接器令牌、允许的工具）。

## 验证与接续开发

`make check` 执行带桌面构建标签的 Go vet/build；`make test` 执行后端竞态检查。当前测试验证：未配置时不能启动、不能通过管理配置启用代理，管理认证/Origin/重复认证检查，代理/CONNECT/MCP 拒绝，以及无效监听地址被拒绝。测试用 HTTP 记录器与内存会话，不读写用户文件、不启动容器、不调用真实外部服务。

前端语法可运行 `node --check frontend/src/main.js`，Node 仅用于可选开发检查，应用本身无需 Node。桌面需 macOS 工具链；独立门卫可用 `CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build ./cmd/gatekeeper` 编译到 Linux。没有 `.api` 及其生成类型，本工程目前不涉及 goctl 生成。

建议先实现和验证 runtime 的专用环境/拒绝绕过，再接门卫管理与事件，然后按目标文档处理模型和 MCP。角色建议为：边界负责人处理 runtime/gatekeeper/vault/connector；平台负责人处理 service/session/policy/audit/events/oauth/mcp；界面负责人处理 frontend 和桌面交互。实际成员姓名与完成情况尚未填写，不能把角色建议算成比赛分工证据。
