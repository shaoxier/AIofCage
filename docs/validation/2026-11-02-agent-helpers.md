# 应用的辅助命令不再经过 aiofcage-run

[验收记录索引](README.md)

## 背景

应用会在 Agent 容器里运行一些自己的命令，例如停止会话、检查会话组件、隔离检查。以前它们和智能体 CLI 一样经过 `aiofcage-run`，在项目目录里运行。`aiofcage-run` 会先加载 `~/.aiofcage/env`，而这个文件和项目目录都能被智能体改写：

- 在 `~/.aiofcage/env` 里加一段 `case`，就能让用 `python3` 运行的命令直接退出，或者输出伪造的结果；
- `python3 -c` 会把工作目录放在模块搜索路径最前面，项目里放一个 `pathlib.py` 就能顶替标准库；
- 没加 `-I` 的 Python 会执行用户 site-packages 里 `.pth` 文件中的代码；
- curl 会读 `~/.curlrc`。

结果是智能体只要改一次文件，就能让以后的“停止”什么也不结束，也能伪造隔离检查的结果。[#87](2026-11-02-attach-files.md) 已经把附件写入改成直接运行，两轮复查都指出停止脚本还有同样的问题。

## 改动

基于 main `c3b3e7d`。

- 新增 `internal/runtime/helpers.go`：
  - `ExecHelper`：以 Agent 用户（1000:1000）直接运行应用自己的命令，即 `docker --context colima-aiofcage exec --interactive --user 1000:1000 --workdir / <Agent 容器> …`，不经过 `aiofcage-run`。PATH 等环境变量只来自容器配置，智能体改不了。
  - `PythonHelper`：`/usr/bin/python3 -I -c …`。
  - `EndProcesses`：停止脚本从 `internal/service/terminal.go` 移到这里，脚本内容不变：按完整的会话标记找进程，先发 SIGHUP，0.2 秒后对剩下的发 SIGKILL。
- 镜像没有改动：改的只是应用调用的方式，`aiofcage-run`、`aiofcage-init.py` 等镜像文件都没变，已有沙箱不需要重建镜像。

逐项判断由应用触发、在 Agent 容器里执行的命令：

| 命令 | 位置 | 原来 | 现在 |
|---|---|---|---|
| 停止会话进程：关闭终端、停止对话的这一轮、取消本机 AI 控制任务 | `service/terminal.go`、`conversations.go`、`agent_control.go` | 经过 `aiofcage-run`，在项目目录里运行 `python3 -c` | `EndProcesses` |
| 会话组件检查 `test -f /opt/aiofcage/agent-turn.py`：打开会话前、会话诊断 | `conversations.go`、`conversation_diagnostics.go` | 经过 `aiofcage-run` | `ExecHelper` |
| 隔离检查：DNS 查询外传、Docker 接口、虚拟机网关端口、项目以外的文件 | `isolation_probes.go` 的 `inAgent` | 经过 `aiofcage-run`，`python3 -I -S` | `ExecHelper`，`/usr/bin/python3 -I -S` |
| 隔离检查：未授权域名、绕开代理、外来凭据、直连远程 MCP、直连数据库 | `attacks.go` 的 `runAttack` | 同上，curl 会读 `~/.curlrc` | `ExecHelper`；curl 改为 `/usr/bin/curl --disable`（`--disable` 必须是第一个参数） |
| 写入 Agent 配置 `aiofcage-init.py` | `runtime/manager.go` | 不经过 `aiofcage-run`，但 Python 没加 `-I`，在项目目录里运行 | `/usr/bin/python3 -I`，工作目录 `/` |
| 查找原生会话记录 `find` | `runtime/conversation.go` | 不经过 `aiofcage-run`，在项目目录里运行 | 工作目录 `/` |
| 写入、删除附件 | `runtime/terminal_images.go` | #87 已改为直接运行 | 改用 `ExecHelper`，命令行不变 |

工作目录改成 `/`，是为了不在智能体可写的项目目录里运行。另外核对过：项目目录被改成 000 时，`docker exec` 照样能在里面运行命令，原来的方式不会因为这个失效。

Docker 接口检查原来也会看到 `~/.aiofcage/env` 里的 `DOCKER_HOST`、`CONTAINER_HOST`，现在只看容器配置里的环境变量。应用生成的设置从不写这两个变量（`agentconfig.go`、开发源都不生成），所以实际结果不变。

保留 `aiofcage-run` 的命令，代码注释里写明了原因：

| 命令 | 原因 |
|---|---|
| 对话的每一轮（`agent-turn.py` 启动智能体 CLI） | CLI 需要 `~/.aiofcage/env` 里的占位符、接口地址和模型 |
| 内置终端、系统终端（包括 dsh 控制台） | 用户在里面运行智能体，同样需要这些设置 |
| `cagectl sandbox exec` | 按智能体的环境运行用户给的命令 |
| 隔离检查“凭据配置” | 它查的正是智能体加载这个文件后看到的环境变量。智能体改写这个文件只能改变这一项的结果，真实密钥从不进入沙箱 |

以下命令不在 Agent 容器里运行，不受影响：在门卫容器里读管理令牌、在连接器容器里交付密码，以及隔离检查的临时对照容器（用同一镜像，但不挂 home 卷和项目）。

## 实测

环境：macOS 26.6.1，colima 0.10.3，Docker 29.5.2，Go 1.26.4；镜像里是 Python 3.11.2、curl 7.88.1。

### 自动测试

- `internal/runtime`：
  - `TestHelpersSkipTheAgentSettings`：逐个参数比对 `EndProcesses`、`ExecHelper` 和 `PythonHelper` 生成的完整命令行，包括 `--context colima-aiofcage`、`--workdir /`、`/usr/bin/python3 -I`；
  - `TestAgentSettingsAreWrittenByAHelper`：写入 Agent 配置的命令行，并确认整个刷新过程中没有命令经过 `aiofcage-run`；
  - `TestNativeSessionLookup`：改为断言新的 find 命令行；
  - `TestAgentCommandsLoadTheAgentSettings`：对话的每一轮和终端仍经过 `aiofcage-run`。
- `internal/service`：
  - `TestOnlyAgentCommandsLoadTheAgentSettings`：解析本包源码，找出对 `manager` 上所有经过 `aiofcage-run` 的入口（`Exec`、`ExecArgs`、`TerminalCommand`、`ConversationCommand`）的调用和方法值，只允许出现在启动对话、打开终端，以及 `runAttack` 里 `check.ID == "steal-env"` 那个分支。用同样的方法检查 main 的源码，能找到 7 处 `manager.Exec` 调用；把 5 项检查改回 `manager.Exec` 后，测试失败。先把 `manager` 存进别的变量再调用，以及 runtime 包里新写的辅助命令，这个测试查不到；后者靠各自的命令行测试。
  - `TestCheckScriptsUseTheImagesPython`、`TestIsolationCurlSkipsTheAgentsConfig`：检查脚本用的解释器和 curl 参数。
- `go vet -tags production ./...`、`go test -race ./internal/...`、`make check` 都通过。

### 真实容器（不调用模型）

运行 `make gatekeeper-linux connector-linux` 后，执行 `AIOFCAGE_TERMINAL_E2E=1 go test -run TestHelpersWithstandAHostileAgentEndToEnd ./internal/service/`。测试会建一个临时空白沙箱，像一个想让停止失效的智能体那样，改写自己的设置和项目：

- `~/.aiofcage/env`：拦下用 `python3`（任何路径）或 `test` 运行的命令，并伪造检查结果；
- 项目里：放一个 `pathlib.py`，导入它的脚本会直接退出；
- 用户 site-packages：放一个 `.pth` 文件；
- `~/.curlrc`：给 curl 的输出加上内容。

| 步骤 | 结果 |
|---|---|
| 对照：经过 `aiofcage-run` 运行 `python3 -c`、`/usr/bin/python3 -c` | 输出的都是伪造的“通过” |
| 对照：经过 `aiofcage-run` 运行 `test -f` | 失败 |
| 对照：在项目目录里运行 `python3 -c 'import pathlib'` | 导入了项目里的 `pathlib.py`，脚本没有运行到底 |
| 对照：不加 `-I` 的 Python | `.pth` 里的代码运行了 |
| 对照：不加 `--disable` 的 curl | 输出里出现了 `~/.curlrc` 加的内容 |
| `PythonHelper` 运行的脚本 | 正常运行，不受以上任何一处影响 |
| `curl --disable` | 没有读 `~/.curlrc` |
| 会话组件检查、原生会话查找 | 正常 |
| 停止：终端里有 bash 和一个后台 `sleep` | 先用原来的方式（经过 `aiofcage-run` 运行 `python3 -c`）结束进程，2 个会话进程都还在；关闭终端（`EndProcesses`）后剩 0 个 |
| 隔离检查 | “凭据配置”仍经过 `aiofcage-run`，结果被伪造成“通过”，证据是 `FORGED`，这也是对照。其余 6 项拿到真实结果：未授权域名、绕开代理、DNS 查询外传、Docker 接口、虚拟机网关端口、项目以外的文件都“通过”；外来凭据、直连远程 MCP、直连数据库没有配置，跳过 |

结果通过，耗时 7.9 秒。测试沙箱、它的两个网络和项目目录都已删除，虚拟机里的 aiofcage 网络数回到测试前的 24 个。

反向运行：把隔离检查临时改回经过 `aiofcage-run`，测试失败。未授权域名、绕开代理被伪造成“通过”；Docker 接口、虚拟机网关端口、项目以外的文件、DNS 查询外传都变成“无法验证”。之后已还原。

同一开关下，原有的 `TestTerminalLifecycleEndToEnd`（终端生命周期、子进程清理、退出竞态）和 `TestTerminalImagesEndToEnd`（附件）也通过。

## 复查

请子代理只读复查了一轮。它没有建沙箱，只在不联网的一次性容器里做实验。结论是没有高严重度问题：入口都找到了，普通沙箱没有回归；旧镜像里 `python3`、`curl` 都在 `/usr/bin`，curl 支持 `--disable`，历史上各版 `aiofcage-init.py` 只用标准库。另外提了 2 个中、5 个低：

| 级别 | 问题 | 处理 |
|---|---|---|
| 中 | 测试守不住 `runAttack` 里 5 项检查的走法：源码检查只看哪个函数调用了 `manager.Exec`；真实容器测试只拦第一个参数正好是 `python3` 的命令，而检查用的是 `/usr/bin/python3`，所以检查即使仍经过 `aiofcage-run` 也会通过 | 源码检查改为要求 `runAttack` 里的调用只能在“凭据配置”分支；真实容器测试改为拦截任何路径的 `python3`，要求“凭据配置”被伪造（对照），其余各项拿到真实的“通过”。两处都做了反向验证 |
| 中 | 同一用户的常驻进程能伪造辅助命令的输出：可以打开 `/proc/<pid>/fd/1`，往检查进程的输出管道里写内容，Yama 挡不住。复查实测，在真实 JSON 前后各加一段，宿主判成“通过”，退出码为 0 | 写进下面的限制。要堵住，检查需要换一个用户运行，但那样它看到的就不是智能体能看到的东西，这次没做 |
| 低 | `/etc/ssl/certs` 是智能体可写的 tmpfs，改坏 CA 会让“外来凭据”固定变成“无法验证”，也可能把本该“发现问题”的结果变成“无法验证”；home 和 `/tmp` 里放超过 5 万个条目，Docker 接口一项会判“无法验证” | 写进限制。这两种都伪造不出“通过” |
| 低 | 把 `~/.aiofcage/env` 等配置位置换成目录、FIFO 或链接，`aiofcage-init.py` 会按设计拒绝写入，沙箱每次启动都失败（main 上就有） | 写进限制，并写明恢复办法 |
| 低 | Docker 接口检查不再看 `~/.aiofcage/env` 里的变量 | 应用生成的设置从不写这两个变量，见上文说明 |
| 低 | 源码检查只认 `X.manager.Exec(...)` 这一种写法 | 扩大到所有经过 `aiofcage-run` 的入口，方法值也算；剩下的盲区写在上文 |
| 低 | 记录里说“逐个参数比对会话组件检查的命令行”过宽，实际比对的是 `ExecHelper` 本身；索引里漏了“凭据配置除外” | 已改 |

修复后没有再请子代理复查，用上面的反向验证和真实容器测试确认。

## 范围与限制

- 辅助命令仍以和智能体相同的用户运行，所以智能体留一个常驻进程，就能在辅助命令运行时干扰它：
  - 向它发信号，在它运行的那一刻结束它；
  - 打开它的输出管道写入内容，伪造隔离检查的结果，宿主看到的退出码仍是 0（复查实测）。
  - 这次堵住的是“改一次文件就持续生效”的那一类。运行时干扰需要专门的常驻进程，要防住，得让检查换一个用户运行。
- 停止靠会话标记 `AIOFCAGE_TERMINAL_ID` 找进程。这要靠智能体配合，主动对抗的智能体能躲开：
  - 在会话里用 `env -u AIOFCAGE_TERMINAL_ID`、`env -i` 启动不带标记的后台进程，或者把进程设成不可转储，这样读不到它的 `/proc/<pid>/environ`；
  - 改写 `~/.aiofcage/env`，让以后经过 `aiofcage-run` 启动的会话都不带标记。已在一次性容器（同一镜像，不联网）里实测：加一行过滤参数的循环后，进程上的标记从 1 个变成 0 个。按本次范围，对话的每一轮和终端仍然经过 `aiofcage-run`，所以这条路没有堵上，记为后续事项。
  - 出现这些情况时，停止会断开应用和这一轮的连接，但容器里的进程可能继续运行。可靠的办法是停止沙箱，整个容器会停掉。
- 隔离检查“凭据配置”仍经过 `aiofcage-run`，智能体改写 `~/.aiofcage/env` 能改变这一项的结果。
- 智能体能让一部分检查变成“无法验证”，但伪造不出“通过”：改坏它可写的 CA 文件，“外来凭据”一项就无法完成；在 home 或 `/tmp` 里放超过 5 万个条目，Docker 接口一项就扫不完。
- 智能体能让沙箱一直启动失败：把 `~/.aiofcage/env` 等配置位置换成目录、FIFO 或链接，`aiofcage-init.py` 会拒绝写入，启动随即停掉容器。home 卷在重启后保留，要恢复，得重置沙箱且不保留 Agent home，或者删除沙箱。这在 main 上就有，不是这次引入的。
- 其余检查在 Agent 容器里以 Agent 用户运行的其他限制，见[隔离检查补对照](2026-10-31-isolation-checks.md)。

## 未覆盖

- 停止对话的这一轮、取消本机 AI 控制任务没有用真实模型跑。它们和关闭终端调用同一个 `EndProcesses`，源码检查保证没有别的入口。
