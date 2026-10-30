# 首次使用链路排查：下载、教程、进度与停止

> 只读调研，2026-10-29，仓库 `main@4f75106`。没有修改仓库文件，没有提交、建分支或推送，没有启动、停止或删除 VM、容器和镜像，没有读取钥匙串。
>
> 标注说明：
> - **【代码】**：读代码确认，后附 `文件:行号`。
> - **【本机】**：在本机用只读命令观测到，包括 `colima version`、`colima start --help`、`limactl --version`、`docker --context colima-aiofcage images/ps/info/system df/buildx ls`、对二进制执行 `strings`、`ls` 缓存目录。
> - **【文档】**：仓库内的验收记录或说明。
> - **【推测】**：推断结论，或依赖外部工具的行为，需要实测确认。
> - **【外部】**：外部工具的通用行为，未在本仓库内验证。
>
> 步骤编号沿用界面上的 1–5（1 检查环境、2 配置供应商、3 创建沙箱、4 启动沙箱、5 开始对话）。代码中的 `t.step` 从 0 开始计数。
>
> 路径简写：
> - `internal/runtime/` 下：`tools.go`、`environment.go`、`images.go`、`manager.go`、`install.go`、`connectors.go`、`resources.go`、`dependencies.go`、`process_unix.go`、`agentconfig.go`。
> - `internal/service/` 下：`app.go`、`sandboxes.go`、`operations.go`、`onboarding.go`、`agent_updates.go`、`mcp.go`、`conversations.go`、`terminal.go`、`monitor.go`、`agent_control.go`。
> - 前端：`*.js` 在 `frontend/src/js/` 下，`main.js` 和 `main.css` 在 `frontend/src/` 下。
> - Dockerfile 在 `internal/runtime/assets/<gatekeeper|agent|connector>/` 下。

## 摘要：最重要的发现

1. **门卫镜像没有换源。** d1e4412 只把 Agent 镜像的 apt 源换成了阿里云。门卫镜像仍从 `deb.debian.org` 执行 `apt-get update`（`internal/runtime/assets/gatekeeper/Dockerfile:4`），而它是首次启动时第一个构建的镜像（`internal/runtime/manager.go:408`）。验收记录测得官方源 15 秒只收到约 151 KB（`docs/validation/2026-10-25-agent-image-mirror.md:17`）。【代码+文档】
2. **VM 磁盘镜像从 GitHub Releases 下载。** 文件约 317 MiB，本机缓存为 332,354,401 字节。应用剥离了全部代理变量（`internal/runtime/tools.go:85`），新建 VM 的硬超时是 20 分钟（`internal/runtime/environment.go:112`）。下载期间界面只显示“检查专用虚拟机”。colima 0.10.3 提供 `--disk-image`，可以改为“预下载、校验后再传入”。【代码+本机】
3. **退出应用不会取消正在进行的准备。** 操作使用 `context.Background()` 并设置 40 分钟超时（`internal/service/sandboxes.go:104`）。`OnShutdown` 只关闭终端和会话（`main.go:77`），子进程又运行在独立的进程组中（`internal/runtime/process_unix.go:15`），因此 colima 和 docker build 会在应用退出后继续运行，与 10-26 的实测现象一致。重新打开应用后，内存中的操作记录已经丢失，用户可以再次点击“启动”。【代码】
4. **进度只有阶段名，也没有停止按钮。** 一次首次启动只会发出约 8 条文字消息。外部命令的输出全部缓存到结束（`internal/runtime/tools.go:146-148`）。启动进行中点“停止”会被拒绝（`internal/service/operations.go:28-30`）。【代码】
5. **错误只保留 stderr 的最后一行**，按字节截断到 300（`internal/runtime/tools.go:123-130`）。用户看到的是“中文前缀 + 原始英文”，真正的原因（如 apt、npm 超时）在更早的输出行里，已被丢弃，也没有写入日志文件。【代码】
6. **侧栏和环境面板的状态过期。** `GetOverview` 只在首屏调用一次（`frontend/src/main.js:142-145`）。首次启动成功创建 VM 后，侧栏仍显示“运行环境未就绪”；装好依赖后仍显示缺失，直到重启应用。【代码】
7. **沙箱状态只在三种情况下刷新**：收到进度事件、切换页面、操作结束。监视器每 5 秒采样一次，但它发出的事件不会刷新列表（`frontend/src/main.js:86,89,140`）。多次并发 `reload()` 的结果没有先后保护，较早的请求可能后返回，把新状态覆盖成旧状态。【代码；覆盖时序为推测】
8. **第 2 步测试失败也能继续。** “继续”只要求密钥已保存（`frontend/src/js/onboarding.js:130`），404、405、429 也被当作成功（`internal/service/app.go:372-375`）。密钥错误要等首次构建完成、到第 5 步才暴露；改密钥前还必须先停止沙箱（`internal/service/app.go:245-256`）。【代码】
9. **npm 一律走 `registry.npmjs.org`。** 没有配置镜像，Agent 安装没有 lockfile，每一步都执行 `npm cache clean --force`。filesystem MCP 无论是否启用都会安装，实测耗时 218 秒。【代码+文档】
10. **教程从未在真实下载下验收过。** 界面验收使用的是模拟后端（`docs/onboarding.md:42`）；d1e4412 也写明“未新增界面构建进度、取消按钮”（`docs/validation/2026-10-25-agent-image-mirror.md:35`）。【文档】

## 本机环境快照（只读观测）

- colima 0.10.3（git `00f6c297`）和 lima 2.2.1，均为 arm64 版本，位于 `/opt/homebrew/bin`。colima 由 Homebrew 安装，只依赖 `lima`。【本机】
- docker 是 Docker Desktop 提供的 `/usr/local/bin/docker`（指向 `/Applications/Docker.app/...`），版本 29.8.1，附带 buildx v0.37.1；`--progress` 支持 `rawjson`。【本机】
- 专用 VM 中运行 Docker 29.5.2 和 BuildKit v0.30.0，使用 containerd 镜像存储；没有配置 registry-mirrors，daemon 也没有代理。【本机 `docker info` / `buildx ls`】
- colima 的磁盘镜像缓存在 `~/Library/Caches/colima/caches/b0992ab8…21a1c3`，大小 332,354,401 字节。文件名正好是镜像 URL 的 sha256，Lima 实例直接引用这个路径（`~/.colima/_lima/colima-aiofcage/lima.yaml` 中的 `images.location`）。【本机计算与查看】
- 专用 VM 已有 76 个镜像，共 26.86 GB，另有 14.74 GB 构建缓存；VM 磁盘是 40 GiB。Agent 镜像单个 0.58–2.64 GB，门卫镜像 159 MB，连接器镜像 371–399 MB。【本机 `docker system df/images`】
- `~/.docker/config.json` 中有 `credsStore: desktop`，以及 `ai` 插件挂在 `build` 上的 CLI 钩子。【本机，已脱敏查看】
- 本机 `go env GOPROXY` 为 `https://proxy.golang.org,direct`，`GOSUMDB` 为 `sum.golang.org`。【本机】

---

## 一、首次链路上的每一次网络下载

### 1.1 总表

| # | 阶段（触发点） | 下载内容 | 来源与配置位置 | 版本与校验 | 超时与重试 | 失败时用户看到什么 |
|---|---|---|---|---|---|---|
| 0a | 源码安装（README 第 1 步，队友路径） | Xcode CLT、Homebrew 安装脚本、`brew install go colima docker` 的 bottle | `README.md:17-27`；Homebrew 默认使用 raw.githubusercontent.com 上的安装脚本、formulae.brew.sh 的 API 和 ghcr.io 上的 bottle【外部】 | brew 按公式中的 sha256 校验【外部】 | brew 自带【外部】 | 在终端里报错，应用无从得知 |
| 0b | 源码安装（`make app`） | Go 模块（wails、goproxy、golang.org/x/*、git.sr.ht 等）；本地 Go 低于 1.26.4 时还会下载工具链 | `go.mod:3`（`go 1.26.4`）与 `go.mod:5-40`；`Makefile:10-11` 没有设置 GOPROXY；README 没有提到 GOPROXY，只有 `docs/development-plan.md:79` 提到 goproxy.cn | go.sum 加 sum.golang.org | Go 默认 | 终端报 `dial tcp … i/o timeout` |
| 1 | 第 1 步：依赖 | **应用不下载、也不安装**，只显示一条命令 | `internal/runtime/dependencies.go:35-51` 生成 `brew install colima docker [lima]`；检测到 Intel 版本时改为 `/opt/homebrew/bin/brew install …`（`dependencies.go:48-50`）；提示文案是“在终端中执行以下命令（需要先安装 Homebrew）：”（`frontend/src/js/onboarding.js:164`） | — | — | 对应行显示红字“尚未安装”，或“需要 ARM 原生工具：colima 是 Intel 版本……”（`tools.go:42-44`） |
| 2 | 第 4 步启动 → `Ensure` → `colima start`（新建 VM） | VM 磁盘镜像 `ubuntu-24.04-minimal-cloudimg-arm64-docker.raw.gz`（本机缓存 332,354,401 字节） | 地址 `https://github.com/abiosoft/colima-core/releases/download/v0.10.4/…` 写死在 colima 0.10.3 二进制中【本机 strings】；由 `environment.go:109-116` 触发 | colima 内置 sha512：`32242674b046b5057e60…dd2259240abbd`【本机 strings】；二进制里有 `saveResumeInfo`、`resumeInfoPath` 等续传符号，说明支持续传【本机，行为需实测】 | 应用侧 20 分钟（`environment.go:112`），没有重试；代理变量全部被剥离（`tools.go:85`） | 长时间停在“检查专用虚拟机”（`manager.go:396`）；失败时 toast 显示“启动专用虚拟机失败：colima start 失败：<stderr 末行 / 超时>”（`environment.go:121-122`、`tools.go:154-160`） |
| 3 | 第 4 步：门卫镜像构建（先于 Agent，`manager.go:408`） | `debian:bookworm-slim` 基础镜像；`apt-get update` 约 9 MB 软件索引加 `ca-certificates` | `public.ecr.aws/docker/library/debian:bookworm-slim@sha256:7ff6…`（`gatekeeper/Dockerfile:3`）；**apt 仍用默认的 `deb.debian.org`，没有换源**（`gatekeeper/Dockerfile:4`） | 基础镜像摘要固定；apt 按 Debian 签名校验 | 每次 `docker build` 30 分钟（`images.go:177`），应用侧没有重试 | 进度显示“正在构建镜像 aiofcage/gatekeeper:<16 位哈希>（首次需要几分钟）”（`images.go:175`）；失败时显示“构建镜像失败：docker build 失败：<末行>”（`images.go:178-179`） |
| 4a | 第 4 步：Agent 镜像的基础镜像 | `node:24.22.1-bookworm-slim` | ECR 加摘要 `55fb…`，这是多架构 index 摘要（`agent/Dockerfile.tmpl:4`；本机看到两个标签指向同一摘要） | 摘要固定 | 同第 3 行 | 同第 3 行 |
| 4b | 系统依赖 | ca-certificates、curl、git、less、procps、python3、ripgrep | 用 sed 把源换成 `http://mirrors.aliyun.com`（`agent/Dockerfile.tmpl:6-9`，来自 d1e4412） | 校验 Debian 签名，没有设置 trusted=yes | 应用未设置 apt 的重试和超时参数，依赖 apt 默认值【外部】 | 同第 3 行；换源后实测整步 33.6 秒（`…agent-image-mirror.md:18`） |
| 4c | filesystem MCP（**无论是否启用都会安装**） | `@modelcontextprotocol/server-filesystem@2.0.0` | 使用 npm 默认的 `registry.npmjs.org`。仓库里没有 `.npmrc`，也没有 `--registry` 或 `npm config`（`agent/Dockerfile.tmpl:15-16`；grep 无结果） | 顶层版本固定，**没有 lockfile**，传递依赖按 semver 范围浮动；使用 `--ignore-scripts`；装完执行 `npm cache clean --force` | npm 默认重试【外部】 | 实测 218.3 秒（`…agent-image-mirror.md:19`） |
| 4d | Agent CLI | Codex `@openai/codex@0.164.0`；Claude Code `@anthropic-ai/claude-code@2.1.309`，唯一允许运行 postinstall 的包；dsh `@deepseek-ai/dsh@0.2.1-rc.2` | npm 默认源（`images.go:42-46`、`agent/Dockerfile.tmpl:17-18`） | 同 4c。Claude Code 的原生二进制来自平台 optional 依赖（`images.go:37-39` 注释） | 同 4c | Codex 实测 63.2 秒；完整构建约 5 分 35 秒（同上文档） |
| 5 | 仅当启用连接器类 MCP（PostgreSQL 或演示案例）时：连接器镜像 | node 基础镜像（与 Agent 共用）；`npm ci` | `connector/Dockerfile.tmpl:4,7`；lockfile 是 `connector/postgres/package-lock.json`，含 117 条 integrity，resolved 全部指向 registry.npmjs.org | 镜像摘要加 lockfile 中的 sha512 | 跟随启动的 ctx | 显示“启动连接器「X」”或同样的构建失败；如果从未构建过，“测试连接”会返回“连接器镜像还没有构建”（`connectors.go:227-233`） |
| 6 | Python/pip | **没有**。python3 来自 apt，脚本只用标准库 | `agent/Dockerfile.tmpl:8`；`assets/agent/*.py` 的 import 全部是标准库 | — | — | — |
| 7 | dsh | 与 4d 相同，都来自 npm；没有其他下载来源 | `images.go:45` | — | — | — |
| 8 | 门卫和连接器二进制 | **不在运行时下载**，由 `make gatekeeper-linux` 和 `make connector-linux` 在本地编译后嵌入应用 | `Makefile:26-35`；`images.go:21-27` | 镜像标签取输入内容的摘要（`images.go:65-72`） | — | 没有嵌入时提示“门卫二进制没有打包进程序……”（`images.go:76-78`） |
| 9 | 第 2 步“测试连接”（不是下载，但同样受网络影响） | GET `<base>/models` 或 `/v1/models` | 宿主直连，`Proxy: nil`（`app.go:58`、`app.go:345-358`） | — | 客户端 12 秒，ctx 15 秒 | “无法连接到 X（超时/域名无法解析/证书校验失败…）”（`app.go:361-395`） |
| 10 | 设置页“检查更新”（不在教程链路上） | npm 的 latest 元数据 | `https://registry.npmjs.org/<pkg>/latest`（`agent_updates.go:79`） | 校验包名和版本 | 25 秒 | “检查失败，请稍后重试” |

运行期间 Agent 不会再下载东西。Claude Code 已关闭自动更新和非必要流量（`internal/runtime/agentconfig.go:71-74`），dsh 关闭了遥测（`agentconfig.go:81`），容器出网只能经过门卫。

### 1.2 VM 磁盘镜像：能否预下载或改用镜像源

- **来源固定在 colima 二进制中。** colima 0.10.3 内置一张镜像清单，按架构和运行时列出 URL 和 sha512，arm64 docker 对应 colima-core 的 `v0.10.4` 版本资产。清单中写死了 `BASE_URL=https://github.com/abiosoft/colima-core/releases/download`，colima 没有提供更换镜像源或基础 URL 的参数。【本机 strings、`colima start --help`】
- **`colima start --help` 中与此相关的参数**【本机】：
  - `-i, --disk-image string`：自定义磁盘镜像的本地文件路径。
  - `--force-disk-image`：加载不受支持的磁盘镜像。
  - `--downloader string`：可选 native 或 curl，默认 native。
  - 二进制中还有两段文案：“disk image must be downloaded from '%s', hash failure: %w”和“unable to validate disk image, but continuing as requested...”。由此推测 `--disk-image` 会按内置哈希校验文件，`--force-disk-image` 则跳过校验。【推测，需实测】
- **结论：可以改为“预下载、校验后传入 `--disk-image`”。** 文件可以放在国内对象存储或 CDN（例如七牛 Kodo），也可以来自离线包。完整性由 colima 内置的 sha512 加上应用自己的校验保证，因此镜像源本身不需要被信任。只需在新建分支（`environment.go:109-116`）增加参数，已有 VM 不受影响。
  - 风险：内置的镜像版本和哈希随 colima 版本变化，而 brew 升级 colima 不受应用控制。需要按 `colima version` 维护“版本 → URL、sha512”的映射，没有匹配时回退到默认下载；或者随应用打包固定版本的 colima 和 lima（与第二轮 P0-5 的安装包方向一致）。
  - 备选方案：直接把文件写到 `~/Library/Caches/colima/caches/<sha256(URL)>`。本机已验证缓存文件名规则，但这依赖 colima 的内部实现，比较脆弱。
- **代理**：`childEnvFor` 删除所有以 `_proxy` 结尾的变量以及 DOCKER_HOST 和 DOCKER_CONTEXT（`tools.go:80-88`）。所以即使用户从设置了代理的终端打开应用，colima 也会直连 GitHub。Go 的 net/http 和 curl 都不读取 macOS 的系统代理【外部】。剥离代理的本意是隔离：colima 会把宿主代理写进 VM 和 Docker 配置，二进制中有 `proxies.docker` 和 `http_proxy` 字样【本机 strings，推测】。
- **输出被吞掉**：colima 的原生下载器用 `schollz/progressbar/v3` 显示字节进度（二进制中有 `createProgressBar`、`OptionShowBytes`、`OptionSetPredictTime`）。但应用把 stdout 和 stderr 一直缓存到命令结束（`tools.go:146-148`），界面看不到任何进度。【本机+代码】
- **启动阶段可观测**：Lima 的 `~/.colima/_lima/colima-aiofcage/ha.stderr.log` 是 JSON 行日志，记录了“Waiting for the essential requirement 1 of 3: `ssh`”、“The final requirement 1 of 1 is satisfied”等阶段。【本机】
- **超时预算**：317 MiB 要在 20 分钟内下完，平均速度需要不低于约 270 KiB/s，否则会收到“colima start 失败：超时”。超时后整个进程组会被 SIGKILL（`process_unix.go:17-23`），之后能否续传需要实测。
- **小问题**：代码传的是 `--cpu`（`environment.go:114,119`），0.10.3 的帮助里写的是 `--cpus`。本机执行 `colima start --cpu 2 --help` 能正常显示帮助，说明 `--cpu` 是仍被接受的隐藏别名；将来版本删除它后，启动会直接报 unknown flag。【本机】

### 1.3 Docker 构建过程

- **构建顺序**：先构建门卫，再构建 Agent（`manager.go:408-412`）。门卫启动后，如果启用了连接器，再构建连接器（`connectors.go:91-97`）。
- **复用规则**：镜像标签等于 Dockerfile 与输入文件的内容摘要（`images.go:65-72,84,113,151`），同名镜像已存在就跳过（`images.go:157-159`）。已经成功的步骤能复用层缓存；失败的 RUN 步骤会整步重跑，而 npm 每次都清空缓存（`agent/Dockerfile.tmpl:16,18`），所以失败重试要从零下载。
- **没有做的事**：没有设置 `--progress`，没有用 `--network` 或 `--build-arg` 切换镜像源，没有隔离 `DOCKER_CONFIG`，也不检测 buildx（全仓 grep 无结果）。【代码】
- **buildx**：本机使用 Docker Desktop 的 CLI，带有 buildx。brew 的 `docker` 公式只安装 CLI，不包含 buildx【外部/推测】。没有 buildx 时，CLI 会打印“DEPRECATED: The legacy builder is deprecated…”并回退到旧构建器（本机 CLI 二进制中有这段文案【本机】）。旧构建器无法输出 rawjson 进度，也不支持 `RUN --mount=type=cache`。
- **用户 Docker 配置的干扰**：所有命令只指定了 `--context`，`~/.docker/config.json` 中的 `credsStore`、`proxies` 和 CLI 钩子都会生效。可能的后果包括：卸载 Docker Desktop 后凭据助手缺失导致拉取报错；`proxies` 把 `127.0.0.1:7890` 这类地址注入构建参数或 `docker create` 的环境变量；构建失败后 CLI 钩子的提示文字成为 stderr 末行，替代了真正的错误。本机配置中就有 `ai` 插件的 build 钩子；docker CLI 支持用 `DOCKER_CLI_HOOKS` 和 `DOCKER_CLI_HINTS` 关闭钩子和提示（本机二进制中有这两个变量名）。【推测，需实测】
- **磁盘只增不减**：代码中没有任何 prune 或 rmi，本机已累积 26.86 GB 镜像和 14.74 GB 构建缓存，而 VM 磁盘只有 40 GiB。【代码+本机】

### 1.4 失败时用户看到什么（统一机制）

- 所有外部命令都经过 `runner.run`：输出缓存到结束；失败时只保留 stderr 的最后一行，按字节截断到 300（`tools.go:123-130`，可能截断在一个 UTF-8 字符中间）；超时显示“超时”，取消显示“已取消”（`tools.go:154-160`）。完整输出被丢弃，也不写入日志文件。【代码】
- 前端统一显示成 toast：“「名称」启动失败：<后端错误>”（`frontend/src/js/sandboxes.js:34`）。红色 toast 不会自动消失，同一时间只显示一条，后来的 toast 会覆盖它（`main.js:43-46`）。教程卡片本身没有失败状态（`onboarding.js:177-178`）。
- 例如，门卫 apt 失败时，大致会显示“…构建镜像失败：docker build 失败：ERROR: failed to build: failed to solve: process "/bin/sh -c apt-get update && …" did not complete successfully: exit code: 100”【格式推测】。真正的原因，如 `Failed to fetch … Connection timed out` 或 `npm error code ETIMEDOUT`，在更早的输出行里，已被丢弃。
- 获取锁超时时直接返回 ctx 的错误，用户看到英文的 `context deadline exceeded`（`manager.go:94-100`、`internal/filelock/lock_unix.go`）。

### 1.5 git log 中的相关修复（最近 40 条及全量检索）

- `d1e4412`（10-26）：只把 Agent 镜像的 apt 换成阿里云。文档写明“未新增 npm 镜像、自动测速或下载源故障切换，也未新增界面构建进度、取消按钮”（`docs/validation/2026-10-25-agent-image-mirror.md:35`）。**门卫镜像没有一起改。**
- `8b3238c`：启动时补建 Docker context（`environment.go:194-237`）。
- `26a0d3d`：优先使用 ARM 原生依赖，识别 Intel 版本的 colima 和 limactl（`tools.go:30-72`）。
- `273de61`：加入教程。当时的验收使用模拟后端，没有执行真实镜像下载（`docs/onboarding.md:42`）。
- `8dea5cd`（10-25）：引入独立进程组和整组 SIGKILL（`process_unix.go:13-24`），只在 ctx 超时或取消时生效。
- `f356d4a`：改用 ECR Public，避开 Docker Hub。
- `0b97701`：把创建沙箱时的耗时说明改为 5–10 分钟。现在对话框只写“首次启动需要准备镜像，可在沙箱列表查看进度”（`sandboxes.js:307`），没有时长。
- **没有任何提交涉及**：npm 镜像、VM 镜像预下载、下载源回退、构建进度、取消、退出时的收尾。

---

## 二、中国大陆网络下的失败点、现有缓解与缺口

### 2.1 按可能性排序

| 排名 | 失败点 | 依据 | 现有缓解 | 缺口 |
|---|---|---|---|---|
| 1 | VM 磁盘镜像：GitHub Releases，约 317 MiB | GitHub 的 release 资产域名在大陆经常很慢或连接中断；代理被剥离（`tools.go:85`）；20 分钟硬超时（`environment.go:112`）；全程看不到进度 | colima 自带 sha512 校验和续传能力【本机，需实测】；重新点“启动”可能接着下载【推测】 | 镜像源回退：同一文件放到国内对象存储或 CDN，校验 sha512 后通过 `--disk-image` 传入；由应用自己下载，支持 Range 续传、显示字节进度、测速后切换；可选的“用代理下载运行环境”开关，只作用于应用自己的下载，不传给 colima 或 VM；支持离线导入 |
| 2 | 门卫镜像的 apt（`deb.debian.org`） | 首次启动的第一个构建步骤；同样要拉约 9 MB 索引；官方源实测 15 秒约 151 KB（`…agent-image-mirror.md:17`） | **没有**（d1e4412 只改了 Agent 镜像） | 照 Agent 镜像的做法 sed 换源，保留签名校验；或者干脆去掉 apt：把根证书内置进门卫二进制（如 Go 的 `x509roots/fallback`）后改用 `FROM scratch`，或从 node 镜像层拷贝证书 |
| 3 | npm（`registry.npmjs.org`） | filesystem MCP 耗时 218 秒、Codex 63 秒；Claude Code 和 Codex 的平台原生包更大；`npm cache clean` 让每次重试从零开始 | 顶层版本固定；成功的层可以复用 | 回退到 npmmirror（`--registry`）；为每个 Agent 版本生成 lockfile 并改用 `npm ci`，完整性由 lockfile 中的 sha512 保证，这样换镜像源也不降低安全性，还能得到确定的总包数；用 BuildKit 的 `--mount=type=cache` 保留 npm 缓存；安装后执行 `<cli> --version` 自检；filesystem MCP 改为启用时才安装，或预先打包 |
| 4 | 应用外的依赖：Homebrew 和 bottle；源码构建时的 Go 模块和工具链 | Homebrew 的安装脚本、API 和 ghcr.io bottle；proxy.golang.org 和 sum.golang.org | 没有（README 和教程都没提镜像） | 教程里给出可选的国内镜像说明（清华或中科大的 Homebrew 镜像、goproxy.cn）；检测 Homebrew 是否已安装；长期看打包 colima、lima 和 docker CLI（P0-5） |
| 5 | ECR Public 基础镜像（debian、node） | 走 CloudFront，一般能连上但不稳定 | 已避开 Docker Hub，摘要固定 | 备用 registry：按同一摘要拉取并校验后打上原标签；预拉取作为单独的一步并显示层进度；离线包加 `docker load` |
| 6 | 总时长超出整体超时 | VM 最长 20 分钟，每次构建最长 30 分钟，而整体只有 40 分钟（`sandboxes.go:104`） | 已完成的步骤可以复用 | 按“多久没有进展”而不是总时长判断超时；显示已用时间；超时后提示“再次启动会从已完成的部分继续” |
| 7 | 用户的 Docker 配置干扰构建 | config.json 中的 credsStore、proxies 和钩子会被继承（`tools.go:80-106` 只隔离了环境变量） | 无 | 子进程环境加上 `DOCKER_CLI_HOOKS=false` 和 `DOCKER_CLI_HINTS=false`；是否使用私有 `DOCKER_CONFIG` 需要团队决定，因为它会改变 context 的存放位置，与“所有 Docker 命令显式指定 context”的约定有关【推测】 |
| 8 | 磁盘空间 | VM 盘 40 GiB，镜像从不清理（本机已占 26.86 GB 加 14.74 GB） | 无 | 启动前检查宿主和 VM 的剩余空间；清理不再使用的 `aiofcage/*` 旧标签和构建缓存，只清理带 `io.aiofcage.managed=true` 标签的对象（`images.go:178`） |
| 9 | 模型接口连通（第 2 步、第 5 步） | 境外供应商在大陆直连会失败；宿主测试和门卫都不走代理（`app.go:58`、`internal/gatekeeper/proxy.go:101-102`） | 预设中有七牛、DeepSeek、Moonshot、智谱（`internal/provider/presets.json:7,46,55,64`） | 测试失败时阻止继续或要求二次确认；提示改用国内供应商 |

### 2.2 补缺建议（按收益和成本排序）

- **A. 当周就能做的小改动**
  - 门卫 Dockerfile 换源（1 行），或者去掉 apt。
  - npm 加 `--registry` 并在失败时回退；去掉 `npm cache clean`，改用构建缓存挂载。注意先确认 buildx 可用，否则旧构建器不认 `--mount`。
  - 在 Dockerfile 中加入 `codex --version`、`claude --version`、`dsh --version` 自检。
  - 子进程环境关闭 CLI 钩子和提示。
- **B. 预下载与校验**
  - 由应用下载 VM 镜像：按镜像源列表依次尝试，用 Range 续传，用 sha512 校验，最后传 `--disk-image`。
  - 通过 Docker Engine API 预拉取摘要固定的基础镜像，并显示层进度。
  - 为 npm 生成 lockfile。
- **C. 预构建与离线包**：在 CI 中构建门卫和 Agent 镜像，应用里内置每个镜像的期望摘要。导入时（`docker pull` 或 `docker load`）先核对摘要，再打上内容标签，`ensureImage` 就会直接命中（`images.go:157`）。离线包包括 VM 镜像、镜像 tar 和清单，清单签名或用 sha256 校验。与交接包的 A03 对应。
- **D. 首次启动前的预检（放在第 1 步）**：macOS 版本（vz 和 virtiofs 需要 13+，README 也写 13+，但代码只检查 darwin/arm64，见 `dependencies.go:15-19`）、buildx、宿主剩余空间、到各下载源的连通性和速度，并给出预计时间和流量。
- **E. 可读的错误和重试**
  - 按阶段和错误特征分类：DNS、超时、TLS、`no space left`、哈希不符、429 等，给出中文说明和下一步建议。
  - 在教程第 4 步和详情页提供“重试”与“查看完整日志”。完整日志先脱敏，再写到应用数据目录。
  - 网络瞬时错误自动退避重试一到两次。
- **F. 时间预期**：在欢迎页和第 1 步写明“首次约 10–30 分钟，需要下载约 X 百 MB”。X 要按实际测量填写，这里不给估计值。

---

## 三、教程链路的 bug 与边界情况

### 3.1 跨步骤问题（生命周期、刷新、并发）

**B1【高】退出应用不会取消准备，子进程成为孤儿**【代码；与 10-26 实测一致】
- 证据：`StartSandbox` 使用 `context.WithTimeout(context.Background(), 40*time.Minute)`（`sandboxes.go:104`），cancel 函数只在 defer 中调用；`operations` 只记录 label 和 progress 字符串（`operations.go:19`）；`OnShutdown` 只调用 `CloseAllTerminals`，后者只关闭会话和终端（`main.go:77`、`internal/service/terminal.go:240-258`）；子进程用 `Setpgid` 放进独立进程组，只有 ctx 取消或超时才会被杀（`process_unix.go:13-24`）。
- 现象：colima 在后台继续下载、创建和启动 VM，`docker build` 也继续运行，最后需要手动结束。
- 复现：新建一个 macOS 测试账户，得到全新的 `~/.colima`、缓存和应用数据。点“启动”，在“检查专用虚拟机”或“正在构建镜像”阶段按 ⌘Q，然后执行 `ps -axo pid,pgid,etime,command | grep -E 'colima start|limactl|docker( |-)build|buildx'`。可以用 Network Link Conditioner 限速。

**B2【高】重新打开应用后看不到上次的准备，可能重复启动**【推测，需实测】
- 证据：操作记录只在内存中（`operations.go:14-54`），应用重开后为空，界面显示“未启动过”，“启动”按钮可点。孤儿进程不持有应用的 flock：锁文件以 `O_CLOEXEC` 打开（`internal/filelock/lock_unix.go`），进程退出时锁就释放了。新实例的 `Ensure` 如果看到 VM 不存在或已停止，会再执行一次 `colima start aiofcage`（`environment.go:108-120`），与孤儿进程同时下载或启动同一个 profile。
- 复现：接 B1，在孤儿进程仍在运行时重新打开应用并点“启动”，观察两个 `colima start` 进程以及缓存目录中正在下载的文件。

**B3【中】两个沙箱同时首次启动，会并发创建同一个 VM**【代码确认锁的类型；colima 自身是否互斥需实测】
- 证据：`Start` 拿的是 `environmentMu.RLock` 加共享 flock（`manager.go:361-367`）；`Ensure` 拿的也是共享锁（`environment.go:84`）。两个启动都会看到 `vm == nil`，然后各自执行新建命令（`environment.go:109-116`）。同一个镜像也会被两次 `ensureImage` 并发构建（`images.go:157-180`）。
- 复现：在全新账户中创建两个沙箱，接连点两次“启动并打开”。

**B4【中】侧栏、环境面板和健康页的 VM 信息只在首屏刷新**【代码】
- 证据：`GetOverview` 只在挂载时调用一次（`main.js:142-145`），之后只有设置页的“停止运行环境”会刷新（`main.js:160-161`）。侧栏的“运行环境就绪/未就绪”（`main.js:174-177`）、沙箱页和设置页的 `EnvironmentPanel`（`sandboxes.js:116`、`settings.js:25`）、健康页的 VM 信息（`sandboxes.js:83`、`health.js:72`）都读这份快照。
- 现象：首次启动成功后，侧栏仍显示“运行环境未就绪”；装好依赖、点“重新检测”通过后，侧栏仍显示“Docker / Colima 缺失”。
- 复现：在 VM 不存在时打开应用，完成第 4 步，观察侧栏底部。

**B5【中】沙箱状态没有轮询**【代码】
- 证据：列表只在三种情况下刷新：收到 `sandbox:progress`（`main.js:89`）、切换页面（`main.js:140`）、`run()` 结束（`sandboxes.js:38`）。监视器每 5 秒采样健康状态（`internal/service/monitor.go:24,56-65`），但它发出的 `sandbox:stats` 只刷新用量汇总（`main.js:86`）。
- 现象：容器在应用之外被停止或崩溃，或者 VM 被停止时，界面仍显示“运行中”，直到切换页面。教程中的 `running` 也随之过期。
- 复现：在测试环境中，从终端执行 `docker --context colima-aiofcage stop aiofcage-<id>-agent`，界面不会更新。

**B6【中】并发的 `reload()` 没有先后保护，旧结果会覆盖新状态**【推测，时序问题】
- 证据：每条进度消息都会触发一次完整的 `reload()`，它并行调用 4 个接口（`main.js:48-60,89`），后返回的结果直接覆盖状态，没有序号或“只用最后一次”的保护。启动结束前会连续发出“准备网络与容器”“启动门卫”“向门卫下发策略与密钥”“启动 Agent 容器”“已启动”（`manager.go:413-460`），对应的 reload 时间上互相重叠。`ListSandboxes` 对每个沙箱执行两次 `docker inspect`，再读取操作状态（`app.go:463-466`）。如果较早的 reload 比 `run()` 收尾的那次更晚返回，界面就会停在 `operation=启动`。由于 `busy[s.id] ?? s.operation`（`sandboxes.js:84,112`），按钮会一直禁用，徽标一直显示“启动中…”，直到下一次刷新。
- 复现：在开发模式下给 `ListSandboxes` 加 1–3 秒的随机延迟，或者准备 10 个以上沙箱，然后启动一个已有镜像的沙箱。

**B7【中】等锁的操作超时后显示英文 `context deadline exceeded`**【代码】
- 证据：`SaveProvider` 的 ctx 是 30 秒（`app.go:183`），对使用该供应商的沙箱调用 `CaptureApplied`，后者调用 `m.lock`（`app.go:204-211`、`manager.go:806-813`）。启动期间锁最长被占用 40 分钟，`m.lock` 等到 ctx 结束后返回 `ctx.Err()`（`manager.go:94-100`）。
- 现象：第 4 步构建期间，用户回到供应商页编辑这个供应商，30 秒后对话框显示 `context deadline exceeded`。`BindProvider`（`sandboxes.go:186`）也有同样的问题，不过详情页在忙碌时会禁用下拉框。
- 复现：在首次构建期间编辑该沙箱绑定的供应商并保存。

**B8【低】启动期间点“停止运行环境”会阻塞，且无法取消**【代码+Go 语义】
- 证据：`SuspendEnvironment` 调用 `environmentMu.Lock()`，这一步不检查 ctx（`internal/runtime/resources.go:119-121`）。Go 的 RWMutex 写者优先：写者排队期间，新的 `RLock`（其他沙箱的启动）也会被阻塞。
- 复现：沙箱 A 正在构建时，在设置页点“停止运行环境”，然后启动沙箱 B，B 会卡住。

**B9【中】错误只有最后一行英文**：见 1.4。另外第 1 步中 VM 检查的详情也直接显示 `err.Error()`（`environment.go:258-259,267-268`）。【代码】

**B10【低】关闭窗口时没有确认**【推测】
- 证据：`main.go:64-79` 没有设置 `OnBeforeClose` 或 `HideWindowOnClose`。按 Wails 的默认行为，关闭主窗口就会退出应用，从而触发 B1。需要实测确认。

**并发与重复点击的现状**【代码】
- 同一个沙箱：`ops.begin` 会拒绝重复操作（`operations.go:22-37`），按钮在 busy 时也会禁用（`sandboxes.js:186`、`sandbox-detail.js:41-43`）。
- 不同沙箱：没有去重（B3）。
- “重启以应用”是先执行一次停止，再执行一次启动（`sandbox-detail.js:78-81`），两者之间会短暂可以点击。

### 3.2 第 1 步：检查环境

- **S1-1【中】VM 出错时不标红，也没有修复入口**【代码】
  - `onboarding.js:163` 的样式判断特意排除了 `c.id === "vm"`，所以 VM 的 `error` 状态显示为普通文字。同时“继续”被禁用（`onboarding.go:153-157`），却没有说明下一步该做什么。
  - 典型的错误有：“虚拟机共享目录不是唯一的 ~/CageWorkspace，拒绝继续”“同名 Docker context 没有指向专用虚拟机”（显示在“专用虚拟机”这一行下，见 `environment.go:233-234,264-270`）“colima list 失败：…”。
  - 复现：在测试账户中先建一个指向别处的同名 context `colima-aiofcage`，再启动 VM 并进入第 1 步。
- **S1-2【中】依赖装好后必须重启应用，但没有按钮，侧栏也不更新**【代码】
  - 当 `manager == nil` 时，后端会返回一个“重新打开应用”的检查项，并设置 `restart_required=true`（`onboarding.go:135-138`），但前端没有使用这个字段（全仓 grep 只在后端出现）。界面上没有“退出应用”按钮，侧栏仍显示缺失（B4）。
- **S1-3【中】依赖引导只有一条 brew 命令**【代码】
  - 只显示命令和“需要先安装 Homebrew”（`onboarding.js:164`），不检测 Homebrew 是否存在，没有复制按钮、链接或镜像说明。
  - 检测到 Intel 版本时，命令改为 `/opt/homebrew/bin/brew install …`（`dependencies.go:48-50`）。如果用户只装了 Intel 版 Homebrew，这个路径根本不存在，界面也没有说明如何安装 ARM 版 Homebrew。
  - brew 安装的 `docker` 不含 buildx（见 1.3）。
- **S1-4【中】预检缺项**【代码】
  - 不检查 macOS 版本（`dependencies.go:15-19`），而 vz 和 virtiofs 需要 13 及以上。
  - 不检查 buildx、磁盘空间和网络连通（grep 无结果）。
  - 欢迎页只提醒准备 API Key（`onboarding.js:140`），没有提到下载量和耗时。
- **S1-5【低】高亮的位置不对，显示的还是旧数据**【代码】
  - 第 1 步把用户带到设置页，高亮目标是页面最底部、默认折叠的 `EnvironmentPanel`（`onboarding.js:115`、`settings.js:25`、`sandboxes.js:360`）。
  - 这个面板显示的是首屏的 `overview`，可能与教程刚刚检查的结果不一致。
  - `GetOverview` 返回之前，设置页整页是空的（`settings.js:10`）。
- **S1-6【低】Lima 的 `Broken` 状态被当作“已创建，启动时自动启动”**【代码+推测】
  - 只要 `vm.Status != "Running"`，就一律归为 stopped（`environment.go:262-263`），而 stopped 被视为可以继续（`onboarding.go:153-154`）。
  - 新建过程中被 SIGKILL 后，实例可能残缺，启动会反复失败，应用也不提供“删除并重建专用虚拟机”（推测，需实测 `colima list` 返回的状态值）。
- **S1-7【低】检查时 VM 正在被创建**：检查上限 30 秒（`onboarding.go:140`），`colima list` 或 `colima ssh` 可能报错或超时，退回第 1 步时会看到 error 而无法继续。【推测】
- 另外需要在新机器上实测：没有任何实例时，`colima list --json` 的退出码是否为 0。如果非 0，新用户会卡在第 1 步（`environment.go:67-70,258-259`）。【需实测；10-26 队友看起来走通了，可能性低】

### 3.3 第 2 步：配置供应商

- **S2-1【中】测试失败也能继续**【代码】
  - `nextAllowed` 只要求 `provider?.key_saved`（`onboarding.js:130`），失败的测试结果只作为说明文字显示（`onboarding.js:173`）。
  - 结果：密钥错误或无法连通时，用户仍会进入第 4 步完成约 5 分半的构建，到第 5 步才发现对话失败。
  - 复现：填一个错误的 key，点测试显示“密钥无效”，“继续”仍然可以点。
- **S2-2【中】部分兼容服务实际上没有校验密钥**【代码+推测】
  - 404 和 405 被当作“服务可达”，429 被当作“限流”，两者都返回成功（`app.go:372-375`）。
  - Anthropic 协议的测试路径是 `<base>/v1/models`（`app.go:346-348`）。像 `https://api.deepseek.com/anthropic` 这样的地址，如果没有实现 models 接口，返回 404，测试就会显示成功（推测，需逐个实测返回码）。
- **S2-3【低】测试结果只保存在内存里**（`onboarding.js:13,79`），重新打开应用后丢失。
- **S2-4【中】在第 5 步发现密钥错误后，改正很别扭**【代码】
  - 已生效的沙箱还在引用这个钥匙串条目时，更新 key 会提示“请先停止沙箱「X」，再更新它正在使用的 API Key”（`app.go:245-256`）。
  - 停止沙箱后，教程会自动退回第 4 步并跳转页面（`onboarding-state.js:18`、`onboarding.js:47-49`），没有任何引导说明为什么。

### 3.4 第 3 步：创建沙箱

- **S3-1【中】“复制到工作区”同步执行，没有进度和取消，失败后留下残留**【代码】
  - 复制在 `CreateSandbox` 这个绑定调用里同步完成：先遍历整棵目录计算大小（`sandboxes.go:331-355`），再整体复制（`sandboxes.go:358-403`），上限是 2 GB 或 20 万个文件（`sandboxes.go:260-263`）。
  - 复制失败时不删除目标目录（`sandboxes.go:296-298`）。“新建空文件夹”模式在后续 `manager.Create` 失败时，也不清理已创建的目录（`sandboxes.go:73-82`）。
  - 对话框只显示“处理中…”，关闭按钮被禁用（`sandboxes.js:253`）。
  - 复现：复制一个包含不可读文件（`chmod 000`）的目录，`~/CageWorkspace` 里会留下 `xxx-2` 这样的半拷贝目录。
- **S3-2【低】“创建沙箱”按钮在 `GetOverview` 返回前被禁用**（`sandboxes.js:94,106`）。`GetOverview` 在 VM 运行时还会执行 `colima ssh` 做校验，最长约 30 秒（`app.go:103-105`）。教程高亮的是一个禁用的按钮，没有任何说明。【代码】
- **S3-3【低】教程中的对话框仍然可以勾选 MCP 和其他智能体**（`sandboxes.js:274-293`）。勾选后会额外构建连接器镜像或下载更多 npm 包，第 4 步会更慢；连接器还需要密码。【代码】

### 3.5 第 4 步：启动沙箱（首次构建）

- **S4-1【高】只显示阶段名，没有进度条和耗时**【代码】
  - 一次首次启动大约只发出 8 条进度文字，依次是：“检查专用虚拟机”、“正在构建镜像 aiofcage/gatekeeper:<哈希>（首次需要几分钟）”、“正在构建镜像 aiofcage/agent:<哈希>（…）”、“准备网络与容器”、“启动门卫”、“向门卫下发策略与密钥”、“启动 Agent 容器”、“已启动”（`manager.go:396-460`、`images.go:175`）。
  - 其中 VM 阶段最长可达 20 分钟，期间文字不变；Agent 构建约 5 分半，期间也不变。
  - `.progress` 只是一行警示色的小字（`frontend/src/main.css:156`），在教程卡片（`onboarding.js:178`）和详情页（`sandbox-detail.js:54`）各显示一遍。
- **S4-2【高】无法停止**【代码】
  - 界面上没有取消入口。启动进行中，后端会拒绝停止和删除操作，提示“「X」正在启动，请等它完成”（`operations.go:28-30`）。详见第四节。
- **S4-3【中】失败后没有失败状态，也没有重试入口**【代码】
  - 教程卡片仍显示“点击下方「启动」…”（`onboarding.js:177`），错误只在顶部 toast 中显示一行，而且可能被覆盖（1.4）。
  - 重启应用后看不到上次在哪一步失败、因为什么失败（操作记录不持久化）。
  - 复现：构建期间断网。
- **S4-4【低】可以离开页面，但离开后进度就看不到了**【代码】
  - 可以离开，操作继续在后台进行。但只要切换到详情页的其他标签，教程卡片就会变成“已保留教程进度……返回当前步骤”，不再显示进度（`onboarding.js:93,161`）。
  - 列表行只在沙箱列表页显示进度（`sandboxes.js:182`），其他页面没有全局的进行中提示。
  - 关闭窗口会退出应用（B10），进而触发 B1。
- **S4-5【低】“重启以应用”的文案和高亮都对不上**【代码】
  - 配置待生效时，文案写的是“请在下方重启以应用”，但按钮其实在页面头部（`sandbox-detail.js:44`）。
  - 此时沙箱处于运行中，高亮目标 `sandbox-start` 不存在（`onboarding.js:115`、`sandbox-detail.js:41-43`），什么都不会高亮。
- **S4-6【中】密钥在很晚才检查**【代码】
  - 钥匙串中的密钥直到下发门卫配置时才读取（`manager.go:637-641`），也就是 VM 创建和两次构建都完成之后。
  - 教程里第 2 步已经保证了密钥已保存，影响有限；但在教程之外，如果钥匙串条目被删除或无法读取，会在等待几十分钟后才失败。
- **S4-7【中】Agent 安装后没有自检**【代码+推测】
  - `npm install -g` 之后没有执行 `--version` 之类的检查（`agent/Dockerfile.tmpl:17-18`）。
  - 如果平台原生的 optional 依赖下载失败，被 npm 当作可选依赖跳过，镜像照样“构建成功”，问题要到第 5 步才暴露（推测，需实测 npm 的这一行为）。

### 3.6 第 5 步：开始对话

- **S5-1【低】会话组件检查把超时误报为“需要更新”**【代码+推测】
  - 检查用 5 秒超时执行 `docker exec test -f …`，任何错误都提示“沙箱需要更新会话组件，请停止后重新启动沙箱”（`internal/service/conversations.go:107-112`）。
  - 机器负载很高时（例如另一个沙箱正在构建），超时会被误报成需要更新。
- **S5-2【低】`docker inspect` 偶发失败会让教程倒退**【代码+推测】
  - 教程的 `running` 依赖 `docker inspect` 成功；inspect 一旦出错，状态就被当作“未创建”（`manager.go:326-331`）。
  - 这会让教程从第 5 步退回第 4 步并自动跳转（`onboarding-state.js:18`、`onboarding.js:47-49`），等下一次刷新正常后又跳回第 5 步。
- **S5-3【信息】打开失败时有诊断，但教程卡片里没有入口**
  - 打开智能体失败时，会自动运行 `DiagnoseConversation`，诊断结果显示在终端区（`frontend/src/js/terminal.js:133-137`），教程按钮恢复为“打开 X”，可以重试。
  - 教程卡片里没有指向诊断的入口，可以补一个。
- 智能体未就绪时的提示都是中文：“请先启动沙箱”“沙箱正在更新，请完成后再打开会话”“请先为该智能体选择供应商，并重新启动沙箱应用配置”（`conversations.go:81-103`）。

### 3.7 教程状态的保存与中途退出

- 现状与文档一致【代码】：
  - “退出教程”会保存为 paused；如果保存失败，也照样关闭，不会困住用户（`onboarding.js:59-63`）。
  - 直接关闭应用时状态保持 active，重新打开后自动回到当前步骤。
  - “重新学习”回到第 1 步，保留已有的供应商和沙箱（`onboarding.js:51-58`）。
  - 每次保存都会核对引用的资源是否还在（`onboarding.go:89-107`）。
- **S6-1【低】教程进度无效时会一直报错，没有重置入口**【代码】
  - 如果 `settings.json` 中的 `onboarding.v1` 损坏，或者来自不兼容的版本（如 step 超过 4），读取会一直失败（`onboarding.go:36-41,71-81`）。
  - 每个页面顶部都会显示“无法读取教程进度”，“重新读取”按钮无效（`onboarding.js:22,133`），也没有“重置教程”。
- **S6-2【低】进行中的操作不保存**
  - 应用重开后，第 4 步无法显示“上次准备在哪里中断，或因为什么失败”（与 B2 相关）。
- **S6-3【信息】存在自动跳转的副作用**
  - 自动跳转的 effect 依赖 `[active, step, sandbox_id, dataReady]`（`onboarding.js:47-49`）。只要 `dataReady` 从 false 变回 true（`reload` 失败一次就会这样，见 `main.js:56-58`），或者推导出的步骤变化，用户就会被拉回教程所在的页面。

---

## 四、可以做真实进度条和“停止”的长操作

规则沿用交接包的约定：进度只显示真实数据，总量未知时不显示百分比；取消要有明确的结束状态和清理结果；不按进程名批量结束用户的 Docker 或 Colima 进程。

### 4.1 逐项清单

| 长操作 | 入口（代码） | 现在怎么报告进度 | 可用的真实数据源 | 取消现状 | 从界面取消还缺什么 |
|---|---|---|---|---|---|
| 依赖检查 | `CheckOnboardingEnvironment`（`onboarding.go:126-144`） | 按钮显示“正在检测…”，最长约 30 秒，结果一次性返回 | 每完成一项就推送一项 | 有 30 秒上限，不可取消 | 不需要取消；可改为逐项显示 |
| 依赖安装 | 在应用外执行 `brew install`（`dependencies.go:45-51`） | 无 | 在应用内用 PTY 运行 brew，可以读到它的下载百分比（应用已有 creack/pty 和 xterm）；也可以打包二进制，免去安装 | — | 需要新增“在终端中安装”入口（已有打开 Terminal.app 的代码，见 `sandboxes.go:248-258`），或内置安装流程 |
| VM 镜像下载、创建与启动 | `Ensure`（`environment.go:82-125`），由 `startSnapshot` 调用（`manager.go:396-399`） | 一条“检查专用虚拟机”，最长 20 分钟 | ① 由应用自己下载：已知总字节数和速率，**确定**；② 逐行读取 colima stderr：可以拿到 logrus 的阶段行和 progressbar 的帧（字节、预计时间），需实测非 TTY 时是否输出；③ 监视 colima 缓存目录中正在下载的文件大小，文件名需实测；④ 读 Lima 的 `ha.stderr.log`，有“essential requirement N of 3”和“final requirement”，**可确定阶段** | ctx 超时或取消时 SIGKILL 整个进程组（`process_unix.go:14-24`），但 ctx 来自 Background（`sandboxes.go:104`），界面无法取消；Lima hostagent 和 colima 守护进程可能不在这个进程组里（推测） | 保存 cancel 函数；VM 阶段先发 SIGINT 或 SIGTERM，等待后再 SIGKILL；取消后检查实例状态，必要时提供“删除并重建专用虚拟机”，只针对 aiofcage，并二次确认 |
| 基础镜像拉取 | 隐含在 `docker build` 的 FROM 中 | 无 | Docker Engine API `POST /images/create`，经 `~/.colima/aiofcage/docker.sock` 返回 JSON 流，含每层的 `progressDetail.current/total`；或者 buildx rawjson 中的 statuses | 跟随构建 | 把预拉取拆成单独一步，关闭请求即可取消 |
| 镜像构建（门卫、Agent、连接器） | `ensureImage`（`images.go:156-182`） | 一条“正在构建镜像 <tag>” | `docker buildx build --progress=rawjson`（本机 buildx 0.37.1 支持）：vertex 名为 `[k/N] RUN …`，带开始、完成、cached 和 error；statuses 给出字节数；logs 中有 apt 的 `Get:`、`Fetched … kB` 和 npm 的输出。由此可以得到**按步骤确定**的进度、层下载字节和日志尾部。npm 步骤内部总量未知时，只显示耗时和最后一行日志。没有 buildx 时退回 `--progress=plain`，解析 `#n [k/N]` 和 `#n DONE 33.6s` | 同上，SIGKILL CLI 所在的进程组；BuildKit 是否随客户端断开而取消，需实测 | 先检测 buildx；改为逐行解析输出；取消后确认 daemon 中没有残留的构建 |
| Agent 增装与版本更新 | `AddSandboxAgents` 和 `UpgradeSandboxAgents`，最终调用 `replaceAgents`（`sandboxes.go:405-418`、`agent_updates.go:134-146`、`install.go:119-181`） | “准备智能体镜像，当前沙箱继续运行”“保存用量并更新容器，终端会话将结束”“智能体已更新”（`install.go:124,137,179`） | 同镜像构建；如果改用 lockfile 加 `npm ci`，总包数已知，可以按包计数（推测 `--loglevel http` 每个包输出一行） | ctx 40 分钟。运行时层面的取消语义已经具备：替换前取消会保留原环境，替换后失败会用 3 分钟走恢复流程（`install.go:151-176`）；验收记录写有“真实取消 2.64 秒后原环境完好”（`docs/validation/2026-10-25-final-delivery.md:43`） | 只差把 cancel 暴露给界面；在“已停止旧容器、正在替换”这一段，禁止取消或只允许“取消并恢复” |
| 项目复制导入 | `CreateSandbox` 调用 `prepareProject`（`sandboxes.go:282-299`），上限 2 GB 或 20 万个文件（`sandboxes.go:260-263`） | 对话框“处理中…” | `checkCopySize` 已经算出总字节数和文件数（`sandboxes.go:331-355`），复制时累计字节数即可，**确定** | 没有 ctx，不可取消，失败会留下半拷贝 | 改成带临时操作 ID 的长操作；在 WalkDir 回调和 io.Copy 中检查 ctx；取消或失败后删除目标目录 |
| 连接器构建与检查 | `syncConnectors`（`connectors.go:91-101`）；`CheckConnector`（`connectors.go:218-237`，90 秒，见 `mcp.go:214,291`） | 构建同上；检查没有进度（progress 回调传的是 nil，见 `connectors.go:234`） | `npm ci` 有 lockfile，总包数已知 | 跟随启动的 ctx，或 90 秒上限 | 同镜像构建 |
| 门卫就绪等待 | `connect`（`manager.go:578-612`，最多 50 次，每次间隔 200ms） | “启动门卫” | 已尝试次数和耗时 | 跟随 ctx | 优先级低 |
| 停止运行环境 | `SuspendEnvironment`（`resources.go:119-146`） | 按钮显示“处理中…” | colima stop 的输出 | 最长 2 分钟；等写锁时不可取消（B8） | 优先级低 |
| 重置运行环境 | `ResetSandbox`（`sandboxes.go:135-154`，40 分钟） | 同启动 | 同启动 | 同启动 | 同启动 |

### 4.2 取消现在走到哪一层

- **运行时层已经可以取消。** `runner.run` 使用 `exec.CommandContext` 加 `boundCommandLifetime`：独立进程组，取消时 SIGKILL 整组，WaitDelay 2 秒（`process_unix.go:13-24`），有对应单测（`internal/runtime/process_unix_test.go:12-18`）。等锁时也响应 ctx（`manager.go:94-100`、`internal/filelock/lock_unix.go`）。`limactl list` 用的是 `exec.CommandContext`，但没有设置进程组（`environment.go:148-150`），影响很小。
- **服务层在这里断开。** `StartSandbox`、`ResetSandbox`、`AddSandboxAgents`、`UpgradeSandboxAgents` 都从 `context.Background()` 创建 40 分钟超时，cancel 函数只用于 defer（`sandboxes.go:104-106,145-147,415-417`、`agent_updates.go:143-145`）。`operations` 不保存 cancel 函数（`operations.go:19`），也没有对应的绑定方法。
- **应用生命周期没有接上取消。** `OnStartup` 拿到了 Wails 的 ctx，只交给了监视器（`main.go:72-76`）；`OnShutdown` 不处理进行中的操作（`main.go:77`）。
- **参考实现**
  - 本机控制入口的 `run_task` 已经实现了可取消的 ctx 和 `cancel_task`（`internal/service/agent_control.go:182-183,403-414,478`）。
  - `cagectl` 用 `signal.NotifyContext` 支持 Ctrl+C 取消（`cmd/cagectl/main.go:76-77`）。

### 4.3 从界面发起取消还缺什么

**后端**

1. 所有长操作的 ctx 都派生自应用级的根 ctx（取自 OnStartup）。`OnShutdown` 时先取消根 ctx，并在有限时间内等待清理完成。
2. `operation` 增加以下字段：
   - `cancel` 函数。
   - `startedAt`。
   - 阶段序号与阶段总数。
   - `current` 与 `total`（含单位）。
   - 最近几行日志（脱敏后）。
3. 新增 `CancelSandboxOperation(id)` 绑定。取消返回 `context.Canceled` 时，统一映射为“已停止”，并附带清理结果，不显示成红色的失败。
4. `runner` 增加逐行读取输出的回调，同时保留末尾若干行，用于生成错误摘要；完整日志脱敏后写入应用数据目录。
5. 按阶段区分取消策略：
   - VM 阶段：先发温和的信号，再强制结束，最后检查实例状态。
   - 构建阶段：直接 SIGKILL，然后确认 BuildKit 中没有残留。
   - 复制阶段：检查 ctx，并删除目标目录。
   - Agent 替换：只允许“取消并恢复”。
6. 跨进程感知：
   - 把锁文件 fd 不带 `O_CLOEXEC` 地通过 `ExtraFiles` 传给 colima 子进程。这样子进程在应用退出后仍持有锁，新实例就能识别“上次准备仍在进行”，由用户选择等待或停止。只针对本应用自己启动的进程，不按进程名批量结束。
   - 也可以另外记录 pid 文件，并核对命令行。
7. 持久化每个沙箱最近一次操作的结果：在哪一步、什么原因、什么时间。
8. 让监视器每 5 秒的采样同时更新沙箱状态（B5）。

**前端**

1. 统一的进度组件：
   - 阶段列表。
   - 总量已知时显示确定的进度条，未知时只显示耗时和最后一行日志。
   - “停止”按钮和“查看日志”入口。
   - 在教程卡片、详情页和列表行中复用同一个组件。
2. 教程第 4 步增加失败状态，包括错误分类、“重试”按钮和帮助链接。
3. 关闭窗口时，如果仍有操作在进行，弹出确认（`OnBeforeClose`）。
4. 侧栏显示全局的进行中提示。环境快照在启动完成后、重新检测后都要刷新（B4）。
5. `reload()` 改为单飞，即同时只有一个请求，结束后再补一次；或者给每次请求编号，丢弃过期的结果（B6）。

### 4.4 取消语义的注意点

- **VM 创建阶段最复杂。**
  - 用 SIGKILL 结束 colima 时，Lima 的 hostagent 和 colima 守护进程可能已经脱离进程组（colima 二进制中有 `_GO_DAEMON`，推测存在自我守护化）。
  - 下载可能可以续传，但新建到一半的实例需要专门检测（`colima list` 加 `limactl list`）。
  - 是否删除重建要征求用户确认，而且只针对 aiofcage。
- **启动回滚已经部分具备。** 门卫启动之后的错误会调用 `stopContainers(context.Background(), n)` 进行清理（`manager.go:422-449`）；在构建阶段取消，不会留下需要清理的容器。
- **取消之后的重试要说明哪些工作可以复用**：已构建的层、已下载的镜像，以及 VM 镜像的续传。

---

## 附：需要实测的清单

1. colima 0.10.3 在非 TTY 下，stderr 是否输出 progressbar 的帧；下载中的文件叫什么；被 SIGKILL 后能否续传。
2. 传入同一 sha512 的文件给 `--disk-image`，能否省去下载、是否做校验；直接预置缓存路径是否被接受；没有实例时 `colima list --json` 的退出码。
3. 新建过程中被 SIGKILL 后，`colima list` 显示什么状态（是否为 Broken），再次执行 `colima start` 的表现；两个 `colima start aiofcage` 同时运行时会怎样。
4. 用 brew 安装的 docker CLI 是否带 buildx；旧构建器能否成功；`--progress=rawjson` 能否使用。
5. CLI 被结束后，BuildKit 是否立即取消；npm 和 apt 是否有残留进程。
6. 全新 VM 上，门卫镜像的 apt 在大陆网络中的耗时和失败率（这是修复后最先受益的一步）。
7. `~/.docker/config.json` 中的 `proxies`、`credsStore`（卸载 Docker Desktop 之后）和 CLI 钩子，分别对构建、`docker create` 和错误末行有什么影响。
8. Wails 关闭窗口时是否退出应用；`OnBeforeClose` 能否拦截。
9. 七牛、DeepSeek、Moonshot、智谱的兼容接口，访问 `/models` 或 `/anthropic/v1/models` 时的返回码，即测试连接能否真正校验密钥。
10. Codex 和 Claude Code 的平台原生 optional 依赖下载失败时，`npm install -g` 是报错还是静默跳过。
11. 多次切换版本后，40 GiB 磁盘还剩多少；清理旧标签和缓存的效果。

建议的复现环境：新建一个 macOS 测试账户（自带全新的 `~/.colima`、缓存、应用数据和钥匙串），配合 Network Link Conditioner 限速或断网，不影响主账户的专用 VM。
