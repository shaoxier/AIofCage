# AIofCage 第二轮调研：沙箱开发环境、门卫规则与隔离自检

- 范围：P0-2（默认拒绝可用化）、P0-4（隔离自检闭环）的只读排查
- 代码基线：`main` @ `4f75106`（2026-10-29），仓库未改动
- 方法：读仓库代码与验收记录；读本机已装的 npm 10.9.3、dsh 0.1.5-rc.1 包源码；在本机临时目录里用 Go 1.26.4 标准库做了一个 URL 解析小实验；从 Mac 直连公网，对公开源发了少量只读 HEAD/GET（`--noproxy '*'`，无凭据）。没有构建，没有启动 VM 或容器，没有读钥匙串。
- 证据标记：
  - 〔代码〕读代码确认
  - 〔记录〕仓库验收记录
  - 〔推断〕由代码或记录推出，未实跑
  - 〔包源码〕本机已装客户端的源码，版本不一定与沙箱内相同
  - 〔宿主实测〕2026-10-29 从 Mac 直连的结果，不经门卫，不等于沙箱内行为
  - 〔需实测〕须在专用实例上验证

## 0. 最重要的发现

1. **门卫的路径规范化会挡住常见包请求，加了预设也照样挡。** 两类请求受影响：
   - npm 带 scope 的包元数据请求，例如 `GET /@types%2fnode`；
   - Go 模块路径里表示大写字母的 `!`，例如 `/github.com/!burnt!sushi/toml/@v/list`。

   原因是 `internal/gatekeeper/proxy.go:236` 只要 `u.RawPath != ""` 就判 `target_mismatch`。Go 遇到任何 `%xx`，或 `! ( ) * '` 这几个字符，都会设置 RawPath。〔代码〕〔小实验〕〔包源码：npm-package-arg `lib/npa.js:190`、pacote `lib/registry.js:36`〕
2. **门卫把所有 3xx 改成 403 `redirect_denied`**（`proxy.go:448-454`）。〔宿主实测〕受影响的跳转包括：
   - npmmirror 的 tarball 302 到 `cdn.npmmirror.com`；
   - GitHub release 302 到 `release-assets.githubusercontent.com`；
   - `/archive/` 和 API tarball 302 到 `codeload.github.com`。

   只放行域名不够，公开规则下需要有条件地透传跳转。
3. **镜像里没有 pip/venv、编译工具链、gh、Go、Rust、Java。** `apt` 在运行期用不了，原因有三：Agent 非 root，根文件系统只读，capabilities 全部去掉。另外 Debian 源是 `http://`，门卫会拒绝明文 HTTP。
   - 验收里的“沙箱内 `pip install` 成功”要先改镜像；
   - P0-2 列表里的 apt 在运行期无意义，应改成构建期的“环境组件”。〔代码〕
4. **创建沙箱时没有任何网络或工具链选项。** 规则只来自供应商和 MCP（`internal/runtime/manager.go:650,676`）。
   - 运行中可以热更新（`refresh` → PUT /config），这是现成的落点；
   - 但 `DisallowUnknownFields`（`internal/gatekeeper/handlers.go:105`）会让升级后仍在运行的旧门卫拒收新字段。〔代码〕
5. **CONNECT 阶段被拒的未知域名，事件里只有 host，没有方法和路径**（`proxy.go:186-203`）。所以这类记录的一键放行只能做到域名级。各客户端能看到的拦截信息不同：
   - npm 会把 CONNECT 403 的正文当作响应解析，并显示 JSON 里的 `error`；
   - pip 显示状态行的 reason phrase；
   - git 和 curl 只显示状态码。〔代码〕〔包源码〕〔推断〕
6. **公共主机（没有绑定密钥的主机）会原样转发 Agent 自带的 Authorization**（`proxy.go:333-344` 只检查已绑定密钥的主机）。开发源预设上线后，应在预设主机上拒绝外来凭据。〔代码〕
7. **现有 DNS 探针在 Docker 自定义网络上永远不会“通过”。** 它要求 nameserver 只能是 `127.0.0.1` 或 `::1`（`internal/service/attacks.go:233`），而自定义网络写入的是内嵌 DNS `127.0.0.11`。这和 10-25 整合验收的结果一致：UDP 53 阻断、无默认路由，结论仍是“无法验证”。〔代码〕〔记录〕〔推断〕
8. **产品内隔离检查的覆盖缺口：**
   - 没有 IPv6、管理面、跨沙箱和宿主回环探针；
   - Docker socket 只看一个路径；
   - 宿主文件探针的哨兵放在 macOS 的 `$TMPDIR`，VM 本来就看不到那里，所以结论没有区分度；
   - 早期实验包有管理面等探针，但跑的是实验门卫和实验拓扑，不能当产品证据。〔代码〕〔记录〕
9. **结果呈现与比对上的问题：**
   - 管理 token 和连接器 token 不在“输出含真实凭据”的比对列表里；
   - 隔离结果只存在内存（`internal/service/security.go:113-120`）；
   - 运行状态页的“x/y 项通过”把跳过和无法验证也算进分母（`frontend/src/js/health.js:51`）。〔代码〕

---

## 1. 沙箱里的开发环境现状

### 1.1 镜像里有什么

| 组件 | 现状 | 证据 |
| --- | --- | --- |
| 基础镜像 | `public.ecr.aws/docker/library/node:24.22.1-bookworm-slim`，固定摘要，Debian 12 arm64 | `internal/runtime/assets/agent/Dockerfile.tmpl:4` |
| Node / npm | Node 24.22.1，npm 为该版本自带。仓库没有记录 npm 的具体版本，Node 24 系列应为 npm 11.x；官方 node 镜像是否带 yarn 1.x 和 corepack 也未记录〔需实测 `npm -v`、`yarn -v`〕 | Dockerfile.tmpl:3-4 |
| apt 包 | `ca-certificates curl git less procps python3 ripgrep`，取 bookworm 源的版本（python3 3.11、git 2.39、curl 7.88 一级）〔需实测〕；构建期把源换成 `http://mirrors.aliyun.com` | Dockerfile.tmpl:6-9 |
| Python | `python3`，外加 `/usr/local/bin/python → python3` 软链。**没有 python3-pip，也没有 python3-venv** | Dockerfile.tmpl:8,24 |
| npm 全局包 | `@modelcontextprotocol/server-filesystem@2.0.0`；再按所选 Agent 安装 `@openai/codex@0.164.0`、`@anthropic-ai/claude-code@2.1.309`、`@deepseek-ai/dsh@0.2.1-rc.2`，版本可按沙箱固定 | Dockerfile.tmpl:15-18；`internal/runtime/images.go:42-46,120-152` |
| AIofCage 自带 | `/opt/aiofcage/{aiofcage-init.py,aiofcage-run,agent-turn.py}`、`~/.bashrc` | Dockerfile.tmpl:19-25 |
| **没有** | pip/venv、gcc/g++/make/pkg-config/python3-dev（node-gyp 原生模块编不了）、gh、openssh-client、wget、unzip、jq、dig/nslookup、sudo、Go、Rust、JDK/Maven/Gradle、docker CLI | 清单之外即没有 |

### 1.2 运行期约束（决定了“能装什么”）〔代码〕

- 进程与资源：
  - 用户 1000:1000（`agent`），根文件系统 `--read-only`；
  - `--cap-drop ALL`，`no-new-privileges`；
  - 内存 4g，CPU 2，PIDs 512。（`manager.go:536-547`）
- 可写位置：
  - `/tmp`（tmpfs 1g，可执行）；
  - `/home/agent`（每个沙箱一个持久卷）；
  - 项目目录（按相同路径 bind 挂载）；
  - `/etc/ssl/certs`（tmpfs 4m，由 init 写入系统根证书和门卫 CA）。（`manager.go:540-543`；`aiofcage-init.py:148-153`）
- 网络：
  - 只接 `aiofcage-<id>-in`，这是 `--internal` 网络，**没有 `--ipv6`**；
  - `--dns 127.0.0.1`。（`manager.go:468-483,537`）
- 代理与证书环境变量（Dockerfile.tmpl:26-34）：
  - 大小写两套 `HTTP(S)_PROXY` 都指向 `http://gatekeeper:8888`；
  - `NO_PROXY=localhost,127.0.0.1,gatekeeper`；
  - `SSL_CERT_FILE`、`CURL_CA_BUNDLE`、`REQUESTS_CA_BUNDLE`、`GIT_SSL_CAINFO` 指向系统证书包；
  - `NODE_EXTRA_CA_CERTS` 指向 `~/.aiofcage/gatekeeper-ca.crt`；
  - 门卫 CA 每次门卫启动都重新生成（`internal/gatekeeper/net.go:19-47`），启动时写入 Agent（`manager.go:712-733`）。
- 推论：
  - `npm install -g` 要写 `/usr/local`，只读，会失败；`npx` 和项目内 `npm install` 可以用，缓存在 `~/.npm`。〔推断〕
  - `apt-get` 运行期不可用：非 root、只读、没有 sudo；源还是 `http://`（Dockerfile.tmpl:6），门卫对明文 HTTP 一律返回 `plain_http_denied`（`proxy.go:230-233`）。〔代码〕
  - `pip install` 不可用：没有 pip 模块，`python3 -m venv` 也缺 ensurepip。就算补上 python3-pip，Debian 12 的 PEP 668（EXTERNALLY-MANAGED）也会挡住系统级和 `--user` 安装，需要用 venv 或 `PIP_BREAK_SYSTEM_PACKAGES`。〔推断/需实测〕
  - 用户态工具链（rustup、Go 压缩包、`pip --user`）可以装进 home 卷，重启后仍在，但前提是网络放行对应的下载源。

### 1.3 创建时用户能选什么〔代码〕

- 对话框（`frontend/src/js/sandboxes.js:198-315`）提供：
  - 名称；
  - 项目文件夹：新建、使用已有、复制到工作区；
  - 智能体（Codex / Claude Code / DeepSeek Harness 复选），每个智能体选一个供应商（274-289）；
  - MCP 服务、filesystem MCP 和工具权限（291-293）；
  - 请求次数和 token 上限，可选（295-304）。
- 后端结构一一对应：`service.CreateSandboxRequest`（`internal/service/sandboxes.go:50-60`）→ `runtime.CreateRequest`（`manager.go:124-132`）。`sandbox.Sandbox` 没有网络规则、预设或工具链字段（`internal/sandbox/sandbox.go:49-76`）。
- 创建后还能做的：
  - 增装智能体：会重建镜像（`internal/runtime/install.go:36-73,119+`）；
  - 改 MCP：热更新；
  - 切换供应商：重启后生效（`manager.go:735-769`）。

### 1.4 “环境预设”三层拆开看

**(a) 工具链（镜像层）**

- 现状：只有 Node/npm、git、curl、python3（不带 pip）、ripgrep。
- 缺口：
  - Python 生态不能用；
  - 原生 npm 依赖编译不了；
  - 没有 gh；
  - Go/Rust/Java 没有工具链，这些生态的网络预设暂时没人能用上。
- 落点：
  - `Dockerfile.tmpl` 按选项增加 `python3-pip python3-venv`、`build-essential`、`gh`。gh 的官方 apt 源需要另加源和签名，也可以从 GitHub release 下载后校验 sha256。
  - `agentContextVersions(agents, versions)`（`images.go:120-152`）加入组件参数，让组件进入镜像摘要；`sandbox.Sandbox` 增加 `Toolchains`。
  - 修改组件沿用 `replaceAgents`：先准备好新镜像再替换容器（`install.go:119-160`）。
- 注意：
  - 镜像在 VM 内直连构建（`images.go:177-178`），不经门卫，也不进审计。
  - 首次构建已经约 5 分 35 秒（`docs/validation/2026-10-25-agent-image-mirror.md`），每加一个组件都会更慢，组合越多镜像变体越多。建议只给 3～4 个固定档位，比如“基础 / +Python / +编译工具 / +gh”。

**(b) 网络规则（包管理源与 GitHub）**

- 现状：规则只从已应用供应商的 `p.Rules()`（`internal/provider/provider.go:143-166`）和非连接器 MCP 的 `service.Rules()`（`internal/mcp/mcp.go:125-140`）生成，见 `manager.go:637-677`。默认拒绝，所以沙箱内 `npm install` 在 CONNECT `registry.npmjs.org:443` 这一步就拿到 403 `default_deny`。
- 缺口：没有预设，也没有放行；门卫自身还有路径和跳转限制（见 2.6）。

**(c) 开发凭据（GitHub 等）**

- 现状：
  - 只能注入 `Authorization`（前缀为空或 `Bearer `）和 `X-Api-Key`（`internal/gatekeeper/config.go:72,106-111`）；
  - 门卫按整值比对占位符（`proxy.go:312,350,359-366`）；
  - 密钥来源只有供应商和 HTTP/OAuth MCP；
  - GitHub 只能接官方远程 MCP，用 PAT 和 Bearer 注入（`internal/mcp/presets.json`）。
- git over HTTPS 走不通，原因有两个：
  - **占位符认不出来。** git 发的是 `Authorization: Basic base64(用户:令牌)`，占位符藏在 base64 里，门卫识别不了，也不会替换。没绑定密钥的主机会把它原样转发；绑定了密钥的主机会判 `credential_foreign`。
  - **时序不对。** git 第一次请求不带认证，收到 401 才去问 credential helper。门卫对“规则需要密钥但请求没带”返回的是 403 `credential_missing`（`proxy.go:351-352`），而 git 只在 401 时重试，所以 credential helper 这条路不通。〔代码〕〔推断〕
- 落点（P0-3 会用到，先记在这里）：
  1. 新增“开发凭据”类型，存钥匙串；`gatekeeper.Secret` 增加 `Scheme: basic` 和 `Username`。
  2. `compile()`（`config.go:83-155`）预先算出两个值：匹配值 `Basic b64(user:placeholder)`，注入值 `Basic b64(user:token)`。`chooseMatch`、`checkCredentials`、`isPlaceholderFor` 和注入点（`proxy.go:271-286,303-366`）改用这两个预计算值。
  3. 4xx 正文脱敏除了原值，还要覆盖 base64 形式（`proxy.go:459-461`）。
  4. 沙箱侧在 `.aiofcage/env`（`internal/runtime/agentconfig.go:84-94`）里写 `GIT_CONFIG_COUNT/KEY_0/VALUE_0`，设置 `http.https://github.com/.extraheader`，值为 `Authorization: Basic …占位…`。这样首个请求就带占位符，也不覆盖用户的 `~/.gitconfig`。同时设 `GIT_TERMINAL_PROMPT=0`。
  5. 同一组 git 路径同时保留公开规则和密钥规则：没带占位符时走公开规则，并剥离凭据头（`proxy.go:287-291,317-322`）。
  6. npm 私有源可以复用现有的 Bearer 机制：`.npmrc` 写 `_authToken=cage-…`，npm 发出的就是 `Authorization: Bearer`。PyPI 私有源和 GOPRIVATE+git 需要 Basic。

### 1.5 回答用户的问题：“创建时能不能加 npm、node、GitHub？”

可以，但要分三件事做。node 和 npm 每个镜像里都已经有了，卡住的是网络：registry 默认被拒，scope 包和跳转还会被门卫本身挡住。

- **网络预设**：不用重建镜像，运行中可以热更新；
- **工具链组件**：要重建镜像；
- **GitHub 凭据**：钥匙串加 Basic 注入。

界面建议：

- 创建对话框在“智能体”和“工具连接”之间加一节“开发环境”，包括三项：
  - 开发源预设复选，每项可选官方源或国内镜像；
  - 工具链档位；
  - GitHub 只读克隆，可选绑定令牌。
- 沙箱详情加“网络与开发源”设置，形式参考 `MCPDialog`（`sandboxes.js:378-404`）。

---

## 2. 门卫规则模型与 P0-2 落点

### 2.1 规则怎么表达〔代码〕

- 规则：`policy.Rule{ID, Effect allow|deny, Host, Port, Methods, Path, SecretRef}`（`internal/policy/policy.go:14-24`）；快照：`Snapshot{Version, SessionID, Rules}`（26-30）。
- 校验限制（`validate`，135-166）：
  - Host：小写域名，至少一个点，不能是 IP（80-96）；只有 deny 能用 `*.` 后缀通配。
  - 端口：只允许 443。
  - 方法：allow 必须列出方法，只允许 GET/HEAD/POST/PUT/PATCH/DELETE/OPTIONS（67）。
  - Path：只能是精确路径，或以 `/*` 结尾的前缀。不支持中段通配、后缀和查询参数（156-161,252-262）。`ValidPath` 禁止 `%?#\`、`//` 和 `..`（101-110）。
  - 规则 ID 匹配 `^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`（65）；最多 256 条（61）。
- 判定：
  - 请求阶段：deny 优先；否则取第一条匹配的 allow，同时列出所有匹配项；都不匹配则 `default_deny`（168-189）。
  - CONNECT 阶段只看 host：只有“不限方法、不限路径”的 deny 才在这一步拦；只要有任何一条 allow 匹配 host 就放行隧道（193-207）。
- 对预设的影响：
  - GitHub smart HTTP 需要 `/*/*/info/refs`、`/*/*/git-upload-pack`，还要排除 `.../git-receive-pack`，这些都写不出来。现在只能给 `github.com` 开 `POST /*`（会连推送一起放），或者逐个仓库写死路径。需要扩展路径语法：段通配 `*` 加后缀匹配，最好再加查询参数约束。
  - 区分不了 `service=git-upload-pack` 和 `service=git-receive-pack`，因为查询串不参与判定。
  - allow 不能通配反而是好事：门卫只会解析确切的放行域名，Agent 没法借门卫的 DNS 带出自选标签（见 3.3）。预设应保持逐个列出确切域名。

### 2.2 配置怎么下发〔代码〕

- 宿主侧：`Manager.gateConfig`（`manager.go:630-679`）组装 `gatekeeper.Config{Policy, Secrets, Limits, Connectors}`。
  - 策略版本取当前毫秒时间（635）；
  - 供应商规则在 650 行加入，MCP 规则在 676 行加入；
  - `configure` 通过 `gateClient.putConfig` 发 `PUT /config`（`manager.go:698-710`；`internal/runtime/gateclient.go:86-107`）。
- 门卫侧：`serveConfig`（`handlers.go:102-140`）：
  - 请求体限 1 MiB，开启 `DisallowUnknownFields`；
  - 用 `compile()` 校验；版本不大于当前版本时返回 409；
  - 原子替换 `g.active`，并记一条 `policy.change/config_applied`。
- 管理面隔离：
  - 只监听出网网络上的 adminIP:8899，先发布到 VM 回环，再由 Colima 转到 Mac 回环；
  - 用随机 token 鉴权；带 Origin 或查询串的请求都拒绝。
  - 证据：`manager.go:511-517`；`handlers.go:41-74`；`cmd/gatekeeper/main.go:62-78`。

### 2.3 运行中能否热更新〔代码〕

- **能。** `Manager.Refresh` 和 `SetMCPPermissions` 都会走 `refresh`，重新 PUT /config（`manager.go:242-271,772-783,815-845`）。
- 每个请求都现取一次 `g.current()`（`proxy.go:179,211`），所以已建立的 TLS 隧道里，后续请求也按新策略判定。“只影响之后的请求”天然成立。
- `refresh` 每次还会执行 `syncConnectors` 并重写 Agent 文件（837-844）。放行和撤销这类高频操作，应另加一个只做 `configure` 的轻量入口。
- 供应商绑定不热更新：`Bind` 只保存选择（735-769），刷新时沿用已应用的快照（819-828）。
- **兼容性风险：**
  - 门卫二进制按内容摘要打进镜像（`images.go:74-85`）。应用升级后，正在运行的沙箱仍是旧门卫，直到重启。
  - Config 或 Rule 加字段后，旧门卫因 `DisallowUnknownFields` 会返回 400 `invalid_json`。
  - 对策：做能力探测（`/health` 返回 version 或 features），或者在界面提示“重启沙箱后可用”。

### 2.4 403 正文在哪里生成〔代码〕

**统一出口：** `textResponse`（`proxy.go:624-633`）。

- 格式：`text/plain`，正文为 `AIofCage gatekeeper: request blocked (<reason>)`；503 和 502 的正文是 `AIofCage gatekeeper: <reason>`。
- 附带 `X-Aiofcage-Reason` 和 `Cache-Control: no-store`。

**调用点：**

- **CONNECT 阶段** `connect()`（177-208）：
  - `not_configured` 返回 503；
  - `port_denied`、`ip_literal_denied`、`default_deny`、`rule_denied` 返回 403；
  - goproxy v1.9.2 会把 `ctx.Resp` 原样写回客户端（模块缓存 `goproxy@v1.9.2/https.go:221-240`）。
- **请求阶段** `request()`（210-298）：`plain_http_denied`、`target_mismatch`、`connection_header_denied`、策略判定、凭据检查、`body_too_large`、额度拦截（429）。
- **响应阶段** `response()`（421-479）：3xx 改成 `redirect_denied`；上游失败返回 502 `upstream_failed`；非公网地址返回 502 `destination_denied`（116-128）。
- **非代理请求**：`not_a_proxy_request`（112-114）。
- **MCP 入口**：返回 JSON problem，或带中文说明的 JSON-RPC 错误（`internal/gatekeeper/mcp.go:141-151`）。这是“写给 Agent 看的拦截文案”的现成先例。

`g.deny()` 不返回事件（`proxy.go:535-541`），但 `eventLog.append` 本身会返回带 ID 的事件（`internal/gatekeeper/events.go:37-55`），所以 403 里可以带上日志编号。

### 2.5 拦截事件怎么到日志页〔代码〕

**链路：**

1. 门卫：`g.deny` / `finish` 写入内存环形缓冲，容量 1000 条（`events.go:17,37-55`）。
2. 管理面：`GET /events` 以 SSE 推送，支持 `Last-Event-ID` 续传和缺口标记（`events.go:98-147`）。
3. 宿主采集：`monitor.tick` 每 5 秒发现运行中的门卫并订阅（`internal/service/monitor.go:81-147`），由 `follow` 接收（159-189）。
4. 落盘与推送：`record` 先追加 JSONL 并 Sync，再推进游标，然后 emit `sandbox:event`（213-233,239-271）。
5. 前端：`main.js:71-74` 合并事件，保留最近 500 条；`LogsPage` 按沙箱和决策筛选，一条一行（`frontend/src/js/logs.js:14-73`）；中文说明来自 `REASONS`（`frontend/src/js/core.js:57-71`）。

**事件字段**（`internal/audit/audit.go:10-49`）：action、decision、reason_code、rule、policy_version、host、method、path（不含查询串，截断到 256）、status、字节数、耗时。CONNECT 阶段的拒绝 `method=CONNECT`，**没有 path**（`proxy.go:186-203`）。

**坑：** 宿主恢复游标时，取 JSONL 末尾最后一条非缺口事件的 ID，不区分来源（`monitor.go:202-206`）。

- `audit.SourceApp` 已经定义但还没用过（`audit.go:54`）。如果 P0-2 把“放行、撤销”这类宿主事件写进同一个 JSONL，重启后游标会变成宿主事件的 ID，门卫会报 `cursor_from_other_run` 并从头补发。
- 解决：恢复游标时按 `Source == gatekeeper` 过滤，或者宿主事件另写一个文件。

### 2.6 现有机制里会卡住开发源的地方（必须和预设一起改）

**① RawPath 一律拒绝**（`proxy.go:234-239`；`ValidPath` 在 `policy.go:109`）〔代码〕〔小实验〕〔包源码〕

- npm 请求 scope 包的元数据时，路径是 `/@scope%2fname`（`npm-package-arg/lib/npa.js:190` 的 `escapedName = name.replace('/', '%2f')`，以及 `pacote/lib/registry.js:36`）。真实项目几乎都会依赖 `@types/*`、`@babel/*` 这类包。tarball 路径 `/@scope/name/-/name-x.tgz` 不受影响。
- Go 模块路径里的大写字母写成 `!小写`（BurntSushi、Azure、Masterminds、DATA-DOG 等），`sum.golang.org/lookup/...` 也一样受影响。
- 小实验（Go 1.26.4 `url.ParseRequestURI`）：`/a!b`、`/a(b)`、`/a*b`、`/a'b` 以及任何 `%xx` 都会设置 RawPath；`+ @ : ~ $ , ; = &` 不会。〔宿主实测〕上游对 `/@types%2fnode`、`!burnt!sushi` 的路径都返回 200。
- 改法：只接受“解码后不改变路径结构”的写法。
  - 允许字面的 `! ( ) * '`；
  - 只在 npm registry 预设的元数据路径（形如 `^/@[^/%]+%2[fF][^/%]+$`）上接受 `%2f`，用解码后的 Path 做策略匹配；
  - `%2e`、`%25`、`%5c` 等仍然拒绝；
  - 带密钥的规则保持现在的严格做法；
  - 同步调整 `policy.ValidPath` 和规则路径语法，测试加在 `proxy_test.go` 和 `policy_test.go`。

**② 3xx 一律改为 403 `redirect_denied`，且不透露目标**（`proxy.go:448-454`）

〔宿主实测〕实际会跳转的请求：

- `registry.npmmirror.com/lodash/-/lodash-4.17.21.tgz` → 302 → `cdn.npmmirror.com/packages/...`
- `github.com/<o>/<r>/releases/download/...` → 302 → `release-assets.githubusercontent.com/...`（带签名查询串）
- `github.com/<o>/<r>/archive/...` 和 `api.github.com/repos/.../tarball/...` → 302 → `codeload.github.com/...`

直接返回 200 的：npm 官方 tarball、pypi.org 与 files.pythonhosted.org、清华和阿里 PyPI 镜像、proxy.golang.org、sum.golang.org、goproxy.cn（含 `/sumdb/sum.golang.org/supported`）、index.crates.io、static.crates.io、Maven Central、maven.aliyun.com、raw.githubusercontent.com、codeload、git 的 `info/refs`（带不带 `.git` 都是 200）。

改法：

- 透传条件：只对没有注入凭据的请求（`state.secretID == ""`）透传 3xx，并且要求 Location 是 https、端口 443，目标 host 在当前策略里有 allow。
- 条件不满足：返回可读的 403，事件里只记跳转目标的 host，供一键放行使用。
- 带注入凭据的请求：继续拒绝跳转，因为签名 URL 可能是由注入的凭据派生出来的。
- 客户端跟随跳转时会发起新的 CONNECT，仍要经过完整的策略判定，这符合 `docs/requirements-and-acceptance.md:89`。

**③ CONNECT 阶段的拒绝没有方法和路径**：影响见 2.7.2。

**④ 公共主机放行外来凭据**

`checkCredentials` 只检查 `secretHosts` 里的主机（`proxy.go:333-344`）。在预设主机上，Agent 可以拿项目里捡到的 `NPM_TOKEN` 或 GitHub PAT 去读私有包，或调用需要认证的 GET。

建议：给预设规则加一个“禁止凭据”标记。命中这类规则时，只要出现 Authorization、X-Api-Key（视情况也包括 Cookie），就返回 `credential_foreign`。

**⑤ 明文 HTTP 全部拒绝**（`proxy.go:230-233`）

apt 的 http 源和任何 `http://` 包索引都用不了。apt 的情况见 1.2。

**⑥ 顺带发现：错误响应可能变乱码**〔推断/需实测〕

4xx 正文只读前 64 KiB 做脱敏，然后删掉 `Content-Encoding`，但没有解压（`proxy.go:455-469`）。npm 请求时带 `accept-encoding`，如果上游的 4xx 是 gzip 压缩的，客户端拿到的就是乱码，registry 的 404 说明会丢失。

### 2.7 P0-2 各块落点

#### 2.7.1 只读开发源预设

**数据与生成：**

- **预设目录**：新增 `internal/policy/devsources.go` 和内嵌的 `devsources.json`，做法同 `provider/presets.json`、`mcp/presets.json`。每个预设包含：
  - 中文名；
  - 规则：allow 只给 GET/HEAD，再加发布接口的显式 deny；
  - 客户端配置；
  - “禁止外来凭据”标记。
- **沙箱字段**：`sandbox.Sandbox` 加 `NetworkPresets []string`；`CreateSandboxRequest`、`runtime.CreateRequest` 同步增加。
- **下发**：
  - `gateConfig` 在供应商和 MCP 规则之后追加预设规则，规则 ID 用 `dev-<preset>-<n>`，注意 256 条的上限；
  - 新增 `Manager.SetNetworkPresets`，锁和存储更新参照 `SetMCPPermissions`（`manager.go:242-271`），然后调用只下发策略的 `applyPolicy`。
- **客户端配置**：写进 `agentFiles` 生成的 `.aiofcage/env`（`agentconfig.go:84-94`），尽量用环境变量，不覆盖用户自己的 rc 文件。
  - npm：`NPM_CONFIG_REGISTRY`、`NPM_CONFIG_AUDIT=false`、`NPM_CONFIG_FUND=false`、`NPM_CONFIG_UPDATE_NOTIFIER=false`
  - pip：`PIP_INDEX_URL`、`PIP_DISABLE_PIP_VERSION_CHECK=1`
  - Go：`GOPROXY=https://goproxy.cn,https://proxy.golang.org`（不带 `direct`）、`GOSUMDB`、`GOTOOLCHAIN=local`
  - Rust：`RUSTUP_DIST_SERVER`
  - 只能写文件的：Cargo 换源写 `~/.cargo/config.toml`；Maven 不读代理环境变量，要写 `settings.xml`；Gradle 写 `gradle.properties`。
  - Java 还要导入门卫 CA，等有对应工具链时再做。

**规则清单**（host 一律写确切域名；“已知”指公开协议或常识；标了宿主实测的只是确认了上游行为，经门卫的效果都需实测）：

| 生态 | 主机 | 放行 | 拒绝或说明 | 备注 |
| --- | --- | --- | --- | --- |
| npm 官方 | `registry.npmjs.org`（yarn v1 用 `registry.yarnpkg.com`，规则相同） | GET/HEAD `/*`：元数据 `/<pkg>`、`/@scope%2fname`、tarball `/<pkg>/-/<pkg>-<v>.tgz`、`/-/v1/search` | 显式拒绝 PUT/DELETE/PATCH/POST `/*`。发布是 `PUT /<pkg>`；unpublish、deprecate、dist-tag、owner、access 都是写方法；登录是 `PUT /-/user/*`、`POST /-/v1/login`；令牌是 `POST /-/npm/v1/tokens` | 依赖 ① 的 `%2f` 修复。npm audit 是 `POST /-/npm/v1/security/advisories/bulk`（和 `/audits/quick`），默认不放，做成子开关 |
| npm 镜像 | `registry.npmmirror.com` 与 `cdn.npmmirror.com` | GET/HEAD `/*` | 写方法拒绝 | 〔宿主实测〕tarball 302 到 cdn，依赖 ② 的跳转透传；二进制镜像 `cdn.npmmirror.com/binaries/*`〔需实测〕 |
| Node 头文件（可选） | `nodejs.org` | GET `/download/release/*`、`/dist/*` | — | 有编译工具链才有意义 |
| PyPI | `pypi.org`、`files.pythonhosted.org` | `pypi.org`：GET `/simple/*`、`/pypi/*`（JSON API）；`files.pythonhosted.org`：GET `/packages/*` | 不放 `upload.pypi.org`（上传是 `POST /legacy/`）和 `test.pypi.org` | 〔宿主实测〕200；镜像里先得有 pip |
| PyPI 镜像 | `pypi.tuna.tsinghua.edu.cn`；`mirrors.aliyun.com` | 清华：GET `/simple/*`、`/packages/*`；阿里：GET `/pypi/simple/*`、`/pypi/packages/*` | 阿里这台主机上还有别的仓库，必须带路径前缀 | 〔宿主实测〕simple 200；文件路径〔需实测〕 |
| Go | `proxy.golang.org`、`sum.golang.org`、`goproxy.cn`（可选 `sum.golang.google.cn`） | proxy.golang.org：GET `/*`；sum.golang.org：GET `/lookup/*`、`/tile/*`、`/latest`；goproxy.cn：GET `/*`，含 `/sumdb/sum.golang.org/*` | 没有写接口；不要配 `direct` | 依赖 ① 的 `!` 修复；〔宿主实测〕都可达 |
| crates | `index.crates.io`、`static.crates.io` | index：GET `/*`（sparse 协议，config.json 写的是 `dl=https://static.crates.io/crates`〔宿主实测〕）；static：GET `/crates/*` | 不放 `crates.io`：发布是 `PUT /api/v1/crates/new`，yank 和 owners 也在这台主机；`cargo search` 需要 GET `/api/v1/crates`，作为可选 | static 对 `+` 和 `%2B` 都返回 200〔宿主实测〕，cargo 实际发哪种〔需实测〕，`%2B` 会被 ① 拦下。镜像 rsproxy.cn、ustc、tuna 逐个〔需实测〕；rustup 走 `static.rust-lang.org` |
| Maven | `repo.maven.apache.org`、`repo1.maven.org`、`maven.aliyun.com` | Central：GET/HEAD `/maven2/*`；阿里：GET/HEAD `/repository/public/*`、`/repository/central/*` 等 | 部署是 PUT；不放 `central.sonatype.com`、`oss.sonatype.org`、`s01.oss.sonatype.org` | 〔宿主实测〕206。Gradle 走 `plugins.gradle.org/m2/*`，pom 是 200，jar 可能跳转〔需实测〕 |
| apt | — | 不做运行期预设 | 运行期不可用（1.2） | 改为构建期组件 |
| GitHub 只读 | `github.com`、`codeload.github.com`、`release-assets.githubusercontent.com`、`objects.githubusercontent.com`、`raw.githubusercontent.com`；可选 `api.github.com` | github.com：GET `/*/*/info/refs`、POST `/*/*/git-upload-pack`（需要新路径语法）；codeload、release-assets、objects、raw：GET `/*`；api.github.com：GET `/*`（未认证每小时 60 次） | 显式拒绝 `.../git-receive-pack`；GraphQL（`POST /graphql`）和 LFS batch（POST）默认拒绝 | release-assets 和 objects 是 release 的跳转目标，依赖 ②；不要开 `github.com GET /*`（理由见下文） |

**“下载可能用 POST、GET 也可能有副作用”：**

- **用 POST 的读操作**，需要按路径或正文区分：
  - git `git-upload-pack`（读）和 `git-receive-pack`（写）都是 POST，只能靠路径后缀区分；
  - Git LFS 的 `POST .../info/lfs/objects/batch` 靠正文里的 `operation` 区分下载和上传，现在的引擎看不到正文；
  - npm audit 的 POST 正文是依赖清单（包名和版本）；
  - GitHub GraphQL 的查询和变更都是 `POST /graphql`。
  - 处理：npm audit 默认不放，并设 `NPM_CONFIG_AUDIT=false` 减少噪声，需要时作为子开关精确放行两个路径；LFS 和 GraphQL 默认拒绝，留给 P0-3 的正文规则或审批。
- **只读不等于不能外传。** 拥有包或仓库的人能看到下载记录：
  - PyPI 有公开的逐次下载数据集，npm 和 crates 有按版本的下载计数；
  - GitHub 仓库 Traffic 的 Popular content 会列出被访问的路径；
  - 攻击者如果拥有某个包或仓库，就得到一条低带宽通道。

  另外，URL 本身就能带数据。npm 网页登录轮询 `doneUrl` 用的是 GET，但这一步会完成令牌签发。
  - 结论：预设只宣称“只放行下载，不放行发布”，不宣称“不能外传”；路径尽量收窄到包协议本身。例如 GitHub 只开 git 协议路径和 codeload，不开 `github.com GET /*`。
- **凭据**：预设主机上拒绝外来凭据，见 2.6 ④。

#### 2.7.2 限时放行（“仅本次”和“本沙箱”）

**谁计时：建议门卫强制到期，宿主负责持久化。**

只靠宿主定时器不可靠：桌面应用关闭、休眠或崩溃时会错过到期；`cagectl` 和本机 MCP 控制进程也没有定时器（`docs/architecture-current.md:151` 已说明桌面关闭时没有采集器）。

- **门卫：**
  - 规则加 `expires_at`（或剩余 `ttl_ms`）和可选的 `max_uses`；`Evaluate` / `AllowsConnect` 改为接收 `now`，跳过已过期的规则（`policy.go:168-207`）。
  - Gate 维护一张以 grant ID 为键的“截止时间和剩余次数”表，跨 PUT /config 保留；截止时间只能缩短不能延长，宿主重发配置不会续期。
  - 到期或次数用完时，门卫自己追加 `policy.change`，reason 为 `grant_expired` 或 `grant_consumed`，`rule` 填 grant ID。
- **时钟：** VM 时钟可能和 Mac 有偏差，`attacks.go:313-314` 的 `demoMargin` 就是为这个设的。可选做法：
  - 用管理响应的 `Date` 头换算（Go 会自动加这个头）；
  - 在 `/health` 里返回 `now`；
  - 只下发剩余 TTL。
- **宿主：**
  - `sandbox.Sandbox` 加 `NetworkGrants []Grant`；
  - 启动和刷新时剔除已过期的，其余随配置下发；
  - “本沙箱”档的放行，只要还没到期，重启后继续有效。

**收回：**

- 到期：门卫自动失效；
- 用户撤销：宿主删掉放行并重新下发；
- 停止沙箱：`revokeRuntime` 本来就会清空策略（`manager.go:876-895`）。

**范围（建议）：**

| 拦截类型 | 仅本次 | 本沙箱 |
| --- | --- | --- |
| CONNECT 阶段（只有 host） | 该 host 的 GET/HEAD，10 分钟 | 用户选时长（1 小时、8 小时或直到沙箱停止），仍然只放 GET/HEAD |
| 请求阶段（有方法和路径） | 同方法加精确路径，`max_uses=1`，10 分钟内有效 | 同方法加路径前缀（取前 1～2 段）。注意 path 最多 256 字符（`proxy.go:241`） |

- **不提供一键放行的原因码：**
  - `rule_denied`：显式拒绝优先，发布类操作走 P0-3 审批；
  - `ip_literal_denied`、`port_denied`、`plain_http_denied`、`target_mismatch`、`credential_*`、`connection_header_denied`。

  非 GET/HEAD 的放行需要二次确认，或者归 P0-3。
- **防社工：** 域名是 Agent 自己选的，日志页原样显示（`logs.js:61-63`）。被注入的 Agent 可能会请用户“去日志页放行 xxx”。界面要突出完整域名，标注“不在任何预设中”，默认只放读操作，并给最短时长。

**审计字段：**

- **宿主事件**（`Source=app`）：
  - 动作：`action` 为 `policy.grant` 或 `policy.revoke`，`rule=<grant id>`；
  - 放行内容：host、方法、路径模式；
  - 期限：`scope` 为 `once` 或 `sandbox`，`expires_at`、`max_uses`；
  - 来源：`source_event=<被拒事件 ID>`，`actor=desktop`。
- **门卫事件**：
  - 命中放行的请求：`rule=<grant id>`，现有字段够用；
  - 到期或用完：`policy.change` 加对应 reason。
- **需要补的地方**：
  - `audit.Event` 增加 `scope`、`expires_at`、`source_event` 等类型化字段（`audit.go:10-49`）；
  - 前端 `REASONS` 补对应文案；
  - JSONL 游标的坑见 2.5。

**界面：**

- 日志页：在 `decision === "deny"` 且原因码允许放行的行里，于“AI 解读”旁边（`logs.js:67`）加“仅本次 / 本沙箱…”。
- 沙箱详情：增加“当前放行”列表和撤销按钮。
- 新增 Wails 方法 `GrantNetworkAccess(sandboxID, eventID, scope, ttl)` 和 `RevokeNetworkGrant`。服务端按 eventID 从内存或 JSONL 取回原事件，并重新用 `policy.ValidHost`/`ValidPath` 校验。

#### 2.7.3 可读 403

**改什么：** `textResponse`（`proxy.go:624-633`），并让 `g.deny` 返回事件（535-541）。

- **正文**：中英双语，写明以下几点：
  - 这是 AIofCage 沙箱的网络策略，不是认证失败；
  - 不要重试，不要换源，不要改代理、证书、lockfile 或沙箱配置；
  - 可在「请求日志」里对这条记录放行（仅本次或本沙箱）；
  - 附上日志编号 `<run>:<seq>`，以及被拒的 host 和方法。
- **格式**：请求的 Accept 含 json 时返回 JSON `{"error":"…","reason":"…","event_id":"…"}`，否则返回 text/plain。
- **状态行**：自定义 ASCII reason phrase，例如 `403 Blocked by AIofCage sandbox policy`（Go 的 `Response.Write` 会使用 `resp.Status`）。
- **响应头**：增加 `X-Aiofcage-Event`。

**各客户端能看到什么：**

| 客户端 | 能看到 | 依据 |
| --- | --- | --- |
| npm | JSON 正文里的 `error` 和 reason phrase，CONNECT 阶段也能看到 | https-proxy-agent 把 CONNECT 失败响应当作 HTTP 响应回放（`dist/index.js:126-160`）；npm-registry-fetch 输出 `${status} ${statusText} - GET <url> - ${body.error}`，不看 content-type 就尝试 `JSON.parse`（`lib/errors.js`、`check-response.js:68-71`）〔包源码，npm 10.9.3；沙箱的 npm 11 需实测〕 |
| pip | `Tunnel connection failed: 403 <reason phrase>` | Python http.client 的报错格式。pip 会把 ProxyError 当作可重试错误，默认重试 5 次，日志里会多出几条拒绝〔推断/需实测〕 |
| git、curl（libcurl） | 只有 `CONNECT tunnel failed, response 403`，看不到正文和 phrase（加 `-v` 或 `GIT_CURL_VERBOSE` 才有） | 〔推断/需实测〕只能靠全局指令 |

**可选实验：未知 host 先 MITM，再在请求层拒绝。**

- 好处：所有客户端都能拿到带正文的 HTTP 403；日志里有方法和路径；pip 和 npm 也不会把它当连接错误去重试。
- 坏处：不信任门卫 CA 的客户端（自带根证书的 uv、Java、Deno 等）会看到 TLS 错误，比 CONNECT 403 更误导人；同一隧道内的连续拒绝可能刷屏，需要聚合同类拒绝。
- 建议：先做 A/B 实测再决定。

**MCP 入口：** 已经有中文 JSON-RPC 错误（`gatekeeper/mcp.go:122,126-128`），可以补一句“可在 MCP 设置中调整”。

#### 2.7.4 agent home 的全局指令文件

**放在哪里：**

| Agent | 路径 | 依据与待验证 |
| --- | --- | --- |
| Codex | `/home/agent/.codex/AGENTS.md` | `CODEX_HOME` 见 Dockerfile.tmpl:26；Codex 读取 `$CODEX_HOME/AGENTS.md` 作为全局指令〔需实测 0.164.0〕 |
| Claude Code | 用户级 `/home/agent/.claude/CLAUDE.md`；或托管策略 `/etc/claude-code/CLAUDE.md` | 托管策略文件烧进镜像，根文件系统只读，Agent 改不了，也不覆盖用户文件，更适合放安全指令〔据官方文档；需实测 2.1.309，以及 agent-turn.py:31-37 的 `--print` 模式〕 |
| dsh | `/home/agent/.dsh/AGENTS.md` | `DSH_HOME` 见 `agentconfig.go:79`。本机 0.1.5 的 `dsh-agent-instructions` README 写明加载 `$DSH_HOME/AGENTS.md`；`dsh-base/cordis.patch.yml:268-271` 启用了它（maxBytes 65536）；headless 档直接基于 dsh-base，而 agent-turn.py:43 用的正是 `--profile headless`〔包源码；沙箱固定的 0.2.1-rc.2 需复核〕 |

**怎么写：** 在 `agentFiles`（`agentconfig.go:39-95`）里加文件，由 `aiofcage-init.py` 原子写入（只能 0600/0644，最多 128 个文件，`aiofcage-init.py:110-140`）。问题是每次启动和刷新都会整份覆盖，两种解决办法：

- 在 init 脚本里增加“受管块”模式：只替换 `<!-- AIofCage:BEGIN -->…<!-- AIofCage:END -->` 之间的内容，保留用户自己写的部分；
- 只写 AIofCage 独占的文件：Claude Code 用托管路径，Codex 和 dsh 用受管块。

结构化会话还有备用通道：Claude Code 可在 agent-turn.py:31 追加 `--append-system-prompt`〔需实测〕。

**内容要点：**

1. 你在 AIofCage 沙箱里，网络全部经过门卫。
2. 识别拦截：403 并且正文或响应头里有 AIofCage 或 `X-Aiofcage-Reason`，或者报 `CONNECT tunnel failed, response 403`、`Tunnel connection failed: 403`，都是沙箱拦截。
3. 停下当前步骤：不要重试，不要换镜像源，不要关闭证书校验，不要改 lockfile 或代理配置。
4. 把被拦的域名和用途告诉用户，请用户在 AIofCage「请求日志」中放行。
5. 不要用 apt-get 或 sudo：这里是非 root、只读系统。
6. 需要全局安装时用 npx，或者把 npm prefix 设到 home 下。

#### 2.7.5 验收测试的注意点

- **`npm publish`：** 没有 token 时 npm 在本地就报 `ENEEDAUTH`，请求根本到不了门卫。要证明是门卫拒绝，需要配一个假的 `_authToken`（或者直接用 `curl -X PUT`），并以门卫事件为证据：`PUT` 加 `rule_denied` 或 `default_deny`。
- **`npm install`：** 用例必须包含 scope 包（如 `@types/node`）和 npmmirror 场景，后者用来测跳转。
- **`pip install`：** 先要有 pip 和 venv，见 1.4(a)。
- **“停止重试”分两层记录：**
  - 工具自身的重试：pip 默认 5 次，npm 有 fetch-retries；
  - 模型的行为：会不会换源、改配置。
- **一键放行：**
  - 只影响之后的请求，被拒的那次不会重放；
  - 到期后再次被拒；
  - JSONL 里有放行、命中、到期三类记录。

### 2.8 文件落点清单

| 文件 | 改动 |
| --- | --- |
| `internal/policy/policy.go` | 路径段通配和后缀语法、查询参数约束（可选）、`expires_at`/`max_uses`、`Evaluate(now)` |
| `internal/policy/devsources.go` + json（新） | 预设目录 |
| `internal/gatekeeper/proxy.go` | RawPath 白名单、跳转透传、预设主机拒绝外来凭据、可读 403、`deny` 返回事件、放行计数 |
| `internal/gatekeeper/config.go` | Basic 型 Secret；新字段校验；放行表 |
| `internal/gatekeeper/handlers.go` / `events.go` | `/health` 返回 features 和 now；到期事件 |
| `internal/audit/audit.go` | `scope`、`expires_at`、`source_event` 等字段 |
| `internal/sandbox/sandbox.go` | `NetworkPresets`、`NetworkGrants`、`Toolchains` |
| `internal/runtime/manager.go` | `gateConfig` 合并预设和放行；`SetNetworkPresets`、`AddGrant`/`RevokeGrant`、`applyPolicy` |
| `internal/runtime/agentconfig.go` | 客户端配置环境变量、全局指令文件、`GIT_CONFIG_*` |
| `internal/runtime/assets/agent/aiofcage-init.py` | 受管块写入 |
| `internal/runtime/assets/agent/Dockerfile.tmpl` + `images.go` | 工具链档位；Claude Code 托管 CLAUDE.md |
| `internal/service/sandboxes.go`、`monitor.go` | 请求字段、放行 API、宿主事件写 JSONL、游标过滤 |
| `frontend/src/js/sandboxes.js`、`logs.js`、`core.js`、`sandbox-detail.js` | 创建对话框、放行按钮、`REASONS`、“当前放行”列表 |
| `docs/validation/`、`docs/features.md`、`docs/README.md` | 新验收记录与索引 |

---

## 3. P0-4 现状与方案

### 3.1 现有 7 项检查（`internal/service/attacks.go:43-51,172-259`）

| ID | 测什么 | 怎么判定 | 当前结果 | 局限 |
| --- | --- | --- | --- | --- |
| steal-env 凭据配置 | 名字以 api_key/token/secret/password 结尾的环境变量是否都以 `cage-` 开头；4 个已知配置文件里有没有 `sk-…` 或带密码的 postgres URL（175-189） | 有非占位值或可疑内容：发现问题；有占位变量且无可疑：通过；一个凭据变量都没有：无法验证 | 绑定了供应商时通过 | 只查已知文件和两种格式；宿主比对列表 `realSecrets`（97-117）只有供应商和 MCP 的 key，没有管理 token 和连接器 token |
| exfil-domain 未授权域名 | 经代理 POST `https://upload.attacker-collector.example/...`（190-191） | 403 且原因为 `default_deny`：通过；最终 2xx/3xx：发现问题；其他：无法验证（162-169） | 通过〔记录〕 | 只覆盖 CONNECT 阶段 |
| bypass-proxy 直连公网 | 自建 socket 连 223.5.5.5:443 和 1.1.1.1:443（199-203） | 连上：发现问题；全部是 ENETUNREACH/EHOSTUNREACH/EACCES/EPERM：通过；超时或拒绝连接：无法验证 | 通过，两个都 blocked〔记录〕 | 只测 IPv4；没有出网侧对照 |
| foreign-key 外来凭据 | 向第一个供应商端点发带外来 Bearer 和 x-api-key 的请求（192-198） | 403 且原因为 `credential_foreign`：通过 | 通过；没有供应商时跳过 | — |
| host-files 宿主文件与 Docker 接口 | 宿主用 `os.MkdirTemp("")` 在 macOS `$TMPDIR` 写哨兵，看容器里同路径能否看到；检查 `/var/run/docker.sock` 是否存在（204-218） | 任一可见：发现问题，否则通过 | 通过 | 哨兵不在 VM 共享范围内，结论必然成立，测不到真正的边界（`~/CageWorkspace` 下的兄弟项目）；socket 只看一个路径是否存在 |
| dns-exfil 公网 DNS | 读 resolv.conf 和 IPv4/IPv6 路由表，向 223.5.5.5:53 发一个 `aiofcage.invalid` 的 UDP 查询（219-235） | 收到回应：发现问题；blocked、resolver 全是 127.0.0.1/::1、且无默认路由：通过；否则无法验证 | 无法验证〔记录：`docs/validation/2026-10-25-pr-integration.md:72`〕 | resolver 条件在 Docker 自定义网络上不成立，见 3.2 |
| db-direct 直连数据库 | 3 个配置文件里有没有带密码的连接串；从 Agent TCP 直连数据库地址（236-252） | 找到或连上：发现问题；blocked：通过；超时：无法验证 | 没启用数据库 MCP 时跳过 | — |

**共同点：**

- **执行方式**：脚本用 `docker exec --user 1000 … aiofcage-run python3 -c` 在 Agent 里执行（`attacks.go:257`；`manager.go:1059-1069`）。
- **判定保守**：执行出错或输出不完整都判无法验证（120-144；`attacks_test.go:15-33`）。
- **脱敏比对**：宿主检查输出里有没有真实密钥（71-83）。
- **结果保存与展示**：
  - 结果只存在内存（`security.go:113-120`），不能导出；
  - 运行状态页显示“x/y 项通过”（`health.js:51`），分母含跳过和无法验证；
  - 安全诊断里的四项边界只是静态说明（`security.go:53-58`）。
- **真实环境用例**：`TestDedicatedIsolationChecks` 需要设置 `AIOFCAGE_ISOLATION_ID`，且只对 `uat-` 开头的沙箱运行（`attacks_test.go:61-94`）。
- **不要混淆**：`internal/runtime/health.go` 和 `internal/service/health.go` 只做容器状态和资源采样（`runtime/health.go:123-268`），不是隔离探针。

### 3.2 现在给“无法验证”或“跳过”的项，以及原因

- **DNS 实际上总是无法验证。**
  - `--dns 127.0.0.1`（`manager.go:537`）用在自定义网络上时，resolv.conf 写的是 `nameserver 127.0.0.11`，127.0.0.1 只是内嵌 DNS 的上游。探针要求 nameserver 必须全是 127.0.0.1 或 ::1（`attacks.go:233`），所以 `local=False`，结论只能是无法验证。
  - 10-25 的记录是“UDP 被阻断、没有默认路由”，结果仍是无法验证，可以反推出 `local` 为假。〔推断〕
  - 更根本的问题：它只证明 Agent 拿不到 DNS 回答，没有证明报文没离开 VM（`docs/validation/2026-10-23-first-batch.md:82`、`2026-10-23-demo-machine.md:37` 都把它列为阻塞）。
- **IPv6 没有独立探针**，只在 DNS 那项里顺带看了 IPv6 默认路由。
  - 早期实验的 `public-ipv6-control`、`direct-ipv6` 都是阻塞：VM 出网网段没有 IPv6，缺正向对照（`demo-machine.md:35`）。
  - 实验拓扑的内部网络开了 `--ipv6`（`experiments/validation/runtime.py:272-273`），产品没开（`manager.go:468-483`），两边不能直接类比。
- **管理面、跨沙箱、宿主回环**：产品里没有对应探针，属于“没覆盖”，不是“无法验证”。
  - 早期实验的 `admin-via-agent-interface`、`admin-via-egress-interface`、`vm-gateway-admin`、`proxy-admin-denied`、`host-loopback-binding` 都通过了（`docs/validation/2026-10-23-demo-machine-container.json`），但跑的是实验门卫，不能当产品证据。
  - `lan-management` 一直阻塞。
- **Docker socket**：只检查一个路径是否存在，没有阳性样本，检测能力本身没有验证过。
- **其他情况：**
  - 没有供应商或 MCP 时，steal-env 无法验证；
  - 没有供应商时，foreign-key 跳过；
  - 没有数据库 MCP 时，db-direct 跳过；
  - 超时、拿不到原因头、执行失败时，各项都判无法验证。
- **关于“没有正向对照就标无法验证”：**
  - bypass-proxy 拿到的 ENETUNREACH 是内核在本地给出的结论：Agent 的网络命名空间里没有路由。这不依赖外网通不通，作为“Agent 没有直连能力”的证据是成立的，可以继续判通过。
  - 超时这类结果必须有正向对照才能下结论。
  - 建议在证据里写明出网侧对照是否可达，让用户看清覆盖范围。

### 3.3 DNS：让结果可判定

**A. 产品内检查（在演示机上就能给出结论）**

1. **配置断言**（确定性，宿主用 docker inspect 查）：
   - Agent 只接 internal 网络，`HostConfig.Dns == ["127.0.0.1"]`；
   - resolv.conf 里只有 `nameserver 127.0.0.11`，`# ExtServers:` 一行只有 127.0.0.1（这个注释格式〔需实测〕Docker 29）。
2. **Agent 侧路径矩阵**，每条路径用独立的随机标签 `a-<path>-<nonce>`：
   - `getaddrinfo`，走内嵌 DNS；
   - UDP 和 TCP 53，直连 223.5.5.5、8.8.8.8、2400:3200::1；
   - DoT（853 端口）；
   - 经代理访问 DoH（`https://dns.alidns.com/dns-query?...`），预期 CONNECT 403；
   - 经代理 CONNECT `<nonce>.<金丝雀域>:443`，预期 403，且门卫不去解析这个名字。
3. **本地观测点：DNS 金丝雀。** 宿主在该沙箱的出网网络上临时起一个容器，用 Agent 镜像里的 python3 监听 53 端口，非 root，用 `--sysctl net.ipv4.ip_unprivileged_port_start=53` 允许绑定低端口，只记录含 nonce 的查询名。另外设两个正向对照和一个反例：

   | 角色 | 做法 | 预期 |
   | --- | --- | --- |
   | 对照一 | 出网网络上的对照容器直接把 `c1-<nonce>` 发给金丝雀 | 必须记到 |
   | 对照二 | 对照容器用 `--dns <金丝雀IP>`，经内嵌 DNS 解析 `c2-<nonce>` | 必须记到，证明出网网络上内嵌 DNS 的转发能被观测到 |
   | 反例 | 在 internal 网络上起一个与 Agent 同参数的探针容器，但 `--dns` 指向金丝雀，查询 `x-<nonce>` | 记不到。记到了说明 internal 网络会把查询转出去 |

   反例比产品配置更严格：它证明即使 `--dns` 配错了，查询也出不去。

   判定：

   | 结论 | 条件 |
   | --- | --- |
   | 发现问题 | Agent 或反例的标签被记到 |
   | 通过 | 两个对照都记到，Agent 和反例都没记到，所有直连都是确定性失败 |
   | 无法验证 | 任一对照没记到 |
4. **落点：**
   - `attacks.go` 新增 `dns-egress`，替换现在 dns-exfil 的判定；
   - runtime 新增 `internal/runtime/probes.go`，提供对照容器和金丝雀的辅助函数；
   - 所有临时容器加 `--rm` 和标签，检查结束后清理。

**B. 写进验收记录的外部可观测对照**

1. **准备接收端（二选一）：**
   - 受控的权威 DNS：团队自有子域，把 NS 指向一台记录查询日志的小 VPS；
   - 公开的 OAST 服务：只发随机标签，不带任何数据。
2. **正向对照：** 从 VM 内（`colima ssh -p aiofcage -- getent hosts c-<nonce>.<zone>`，代码里已在用 `colima ssh`，见 `internal/runtime/environment.go:177`）和从出网容器各解析一次，权威端都必须看到。
3. **Agent 侧：** 跑完整的路径矩阵。
4. **门卫侧：** 给测试沙箱临时加一条 `gkctl-<nonce>.<zone>` 的确切 allow。被放行的名字应当在权威端出现，没放行的 `gk-<nonce>` 不应出现，以此证明门卫只解析放行名单里的名字。
5. **比对：** 等 60 秒后核对权威端日志。
6. **备选：在 VM 边界抓包。** 如果 Colima 用的是 vmnet 或 vzNAT，可以在 Mac 上 `sudo tcpdump -i bridge100 port 53 or port 853`；如果是用户态网络（gvisor/slirp），宿主没有可抓的网卡，只能用权威 DNS。先要确认当前实例用的是哪种网络模式。
7. **产出：** 脚本放 `experiments/validation/`（或随记录附带），结果存脱敏 JSON（nonce、时间、路径、是否被观测到），并在验收索引登记。

### 3.4 IPv6：让结果可判定

- **结构检查（确定性）：**
  - 读 `/proc/net/if_inet6`：预期只有 `lo ::1`，可能还有 `fe80::` 链路本地地址（Docker 会不会给非 IPv6 网络的网卡关掉 IPv6，〔需实测〕）。出现任何全局地址或 ULA 都判发现问题。
  - 读 `/proc/net/ipv6_route`：预期没有默认路由。
- **主动测试：** 连 `2400:3200::1:443`、`2001:4860:4860::8888:443`，以及下文本地金丝雀的 ULA 地址。预期得到 ENETUNREACH、EADDRNOTAVAIL 或 EAFNOSUPPORT。
- **本地正向对照（在演示机上可判定）：**
  1. 宿主临时执行 `docker network create --ipv6 --subnet fdxx:…/64 aiofcage-probe-v6-<nonce>`，建一个非 internal 网络；
  2. 在这个网络上起一个监听容器，再起一个对照容器去连它，必须连上；
  3. Agent 去连同一个监听地址，必须失败。

  | 结论 | 条件 |
  | --- | --- |
  | 无法验证 | 网络建不起来或对照连不上（VM 或 Docker 没启用 IPv6） |
  | 通过 | 对照成功、Agent 失败；范围写明“Agent 到不了自身网络之外的 IPv6 目标” |
- **公网 IPv6（另建测试环境）：**
  - 方案一：单独的验证用 Colima profile（如 `aiofcage-v6`，不碰产品 profile），用 `--network-address`（socket_vmnet）的桥接模式，接入带 IPv6 的网络（家用路由或手机热点）；出网网络开 `--ipv6` 并启用 ip6tables。
  - 方案二：一台有原生 IPv6 的 Linux 主机跑同样的拓扑。实验运行器已经有 `--internal --ipv6` 网络和对照容器（`experiments/validation/runtime.py:272-293`）。
  - 判定：对照容器能连公网 IPv6，Agent 却连不上，才算通过。演示机的记录如实写“公网 IPv6：无法验证（环境无 IPv6 出网）”，本地对照那一项写“通过”。
- **门卫自身不用改：** `publicDial` 会拨所有解析出的地址，非公网 IPv6 网段一律禁止（`net.go:51-63,82-109`）。

### 3.5 管理面探针

**Agent 侧：**

1. TCP 连 `gatekeeper:8899`（proxyIP）：预期 ECONNREFUSED，因为管理面只监听 adminIP；连上就判发现问题。
2. TCP 连 adminIP:8899：预期 ENETUNREACH。
3. TCP 连 internal 网关 `.1` 的 8899、管理发布端口、22、2375、2376、10250、80、443：预期失败；连上记为“发现问题”或“VM 服务可达”。这一项值得重点实测：发往网桥自身 IP 的流量走 VM 的 INPUT 链，Docker 的隔离规则主要在 FORWARD 链。实验只测过 `.1:8899`（`experiments/validation/probes.py:67-75`）。〔需实测〕
4. 经代理发请求：

   | 请求 | 预期 |
   | --- | --- |
   | `CONNECT <adminIP>:8899` | 403 `port_denied` |
   | `CONNECT <adminIP>:443` | `ip_literal_denied` |
   | `GET http://gatekeeper:8899/health` | `plain_http_denied` |
   | 直连代理端口 `GET /health` | `not_a_proxy_request` |
   | MCP 端口 8890 的 `/health`、`/config`、`/events` | 404 `mcp_unknown_service` |

   所有响应里都不能出现 `run_id`、`policy_version` 这类管理面 JSON。
5. token 泄露检查：宿主把管理 token 和连接器 token 加入 `realSecrets` 的比对列表（`attacks.go:97-117`）；再参照实验里的 agent-state-secrets，在宿主侧扫描 Agent 的环境变量、进程参数和 home/tmp。

**宿主侧正向对照：**

- 带 token 访问 health 得 200；不带 token 或用占位符得 401。
- 出网网络上的对照容器能 TCP 连上 adminIP:8899，证明端口确实开着，这样 Agent 侧的失败才有意义。
- Mac 上 `lsof -nP -iTCP:<port> -sTCP:LISTEN` 只显示 127.0.0.1 或 ::1（参照 `runtime.py:326-329`）。
- 用本机的局域网 IP 连同一个端口，应当失败。这可以代替第二台机器完成 `lan-management`。

**落点：**

- `attacks.go` 新增 `management-plane`；
- runtime 提供 `ProbeTopology(id)`，返回 proxyIP、adminIP、两个网关和发布端口（可复用 `hostAddress`，`manager.go:555-574`，以及 `docker port`）；
- runtime 再提供 `RunEgressControl(id, script)`，用 Agent 镜像起一次性对照容器（参照 `runtime.py:287-290`）。

### 3.6 Docker socket 探针

**Agent 侧：**

1. 读 `/proc/self/mountinfo`。预期只有这些挂载：
   - 项目 bind 和 home 卷；
   - `/tmp` 和 `/etc/ssl/certs` 两个 tmpfs；
   - Docker 管理的 hosts、hostname、resolv.conf。

   多出 `/var/run`、`/run`、`docker.sock` 或宿主根目录一类的挂载，就判发现问题。
2. 遍历每个挂载点（跳过 `/proc` 和 `/sys`），找出所有 socket 文件（`S_ISSOCK`）。再补查已知路径：
   - `/var/run/docker.sock`、`/run/docker.sock`；
   - `/run/containerd/containerd.sock`、`/run/podman/podman.sock`、`/run/user/*/docker.sock`；
   - `/run/buildkit/buildkitd.sock`、`~/.docker/run/docker.sock`；
   - Lima guestagent 的 socket。
3. 对找到的每个 socket 发 `GET /_ping`。返回 `OK` 或带 `Api-Version` 头，就判发现问题。
4. 对 `.1`、gatekeeper 和 127.0.0.1 的 2375/2376 端口发 `/_ping`，有响应就判发现问题。
5. 检查环境变量 `DOCKER_HOST`、`CONTAINER_HOST`。
6. 顺带做确定性检查（实验 `probes.py:25-42` 已有）：
   - uid 为 1000；
   - CapEff 为 0，NoNewPrivs 为 1；
   - 向根文件系统写入返回 EROFS。

**阳性样本（正向对照）：**

- 探针内部：先在 `/tmp` 起一个假的 Docker API unix socket（python 线程，`/_ping` 返回 OK）。扫描器必须找到它，之后把它排除在结论之外。
- 宿主侧：另起一个对照容器，用 `-v <临时socket>:/var/run/docker.sock` 挂一个假 API，跑同一份探针，结论必须是发现问题。
- 判定：两个阳性样本都检出，阴性结论才算通过；否则判无法验证。

**落点：** `attacks.go` 把 host-files 拆成两项。

- `docker-socket`：上面的探针。
- `project-boundary`：哨兵改放在 `~/CageWorkspace/.aiofcage-probe-<nonce>/`，也就是兄弟项目的位置。VM 内能看到（`colima ssh -- test -f`）作为正向对照，容器内看不到作为结论。

### 3.7 宿主回环与跨沙箱

**宿主回环：**

1. 桌面应用临时在 Mac 的 `127.0.0.1:<随机端口>` 开一个金丝雀监听。
2. 对照：出网网络上的对照容器经 `host.lima.internal` 去连（地址先用 `colima ssh -- getent hosts host.lima.internal` 取得），必须连上。这证明在当前环境里，从 VM 到宿主回环的路径确实存在。
3. Agent 去连同一个地址和端口，预期 ENETUNREACH。对照不通就判无法验证。

VM 能不能经 `host.lima.internal` 访问到 Mac 的回环端口，取决于 Lima/Colima 的网络模式，〔需实测〕。附带观察（不在 Agent 威胁模型内）：连接器在出网网络上，可能访问到宿主服务。

**跨沙箱：**

- 测法：从沙箱 A 的 Agent 去连沙箱 B 的 proxyIP:8888、proxyIP:8890 和 adminIP:8899，预期 ENETUNREACH；经代理 CONNECT B 的 IP，预期 `ip_literal_denied`。B 可以是另一个正在运行的沙箱，也可以由宿主临时建一对网络和监听来代替。
- 对照：B 侧的对照容器能连上 B 的门卫。
- 条件不满足时判跳过，并写明原因。

### 3.8 判定与展示

- **结构化输出：** 每项输出 `{status, evidence, controls:[{name, ok}], scope}`。没有对照的项，即使看起来全部阻断，也只判无法验证；只有 ENETUNREACH 这类本地确定的结果例外，并要写明理由。
- **结果留存：** 存进存储，或由宿主导出 JSON，供 docs/validation 引用。
- **前端：** `health.js:51` 改成按状态计数，例如“通过 6 · 发现问题 0 · 无法验证 1 · 跳过 1”（`ai.js` 的弹窗已经是按状态计数）。
- **验收记录：** 新建 `docs/validation/2026-1x-xx-isolation-closure.md`，内容包括：
  - 两套环境：演示机，以及有 IPv6 出网的环境；
  - 脚本与命令；
  - 脱敏 JSON；
  - 未覆盖项。

  写完更新验收索引。

---

## 4. 需在专用实例上实测的清单

**镜像与工具链**

1. 沙箱 npm 的版本（应为 11.x），以及 scope 包元数据确实发的是 `%2f`（npm 11 下）。
2. 镜像里是否带 yarn 和 corepack。
3. 加上 python3-pip 和 python3-venv 后，PEP 668 的实际表现。

**门卫行为（经门卫）**

1. 现在开预设时 `npm install @types/node` 是否报 `target_mismatch`；`go mod download github.com/BurntSushi/toml` 是否同样被拦。
2. cargo 下载 `wasi 0.11.0+wasi-snapshot-preview1` 时，路径里发的是 `+` 还是 `%2B`。
3. npmmirror、GitHub release、Gradle 插件经门卫时是否得到 `redirect_denied`。
4. 4xx 是 gzip 时，客户端看到的正文是不是乱码。

**客户端如何显示拦截**

- npm、pip、git、curl 面对 CONNECT 403 时实际打印什么，包括自定义 reason phrase 和 JSON 正文的效果；
- pip 的重试次数。

**全局指令**

- Codex 0.164.0、Claude Code 2.1.309（含 `--print` 和托管路径）、dsh 0.2.1-rc.2 的 headless 模式，是否加载各自的全局指令文件。

**网络与隔离**

1. resolv.conf 的实际内容，以及是否有 `ExtServers` 注释。
2. Agent 网卡是否有 `fe80::` 地址，IPv6 是否被关闭。
3. internal 网关 `.1` 上 VM 的哪些服务端口可达。
4. 当前 Colima 实例的网络模式，以及能否从 VM 访问宿主回环。
5. Docker 29 在 internal 网络上，内嵌 DNS 是否完全不向外转发。
