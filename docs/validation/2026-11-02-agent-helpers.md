# 应用的辅助命令不再经过 aiofcage-run

[验收记录索引](README.md)

## 背景

应用会在 Agent 容器里运行一些自己的命令，例如停止会话、检查会话组件、隔离检查。以前它们和智能体 CLI 一样经过 `aiofcage-run`，在项目目录里运行。`aiofcage-run` 会先加载 `~/.aiofcage/env`，而这个文件和项目目录都能被智能体改写：

- 在 `~/.aiofcage/env` 里加一段 `case`，就能让以 `python3` 开头的命令直接退出，或者输出伪造的结果；
- `python3 -c` 会把工作目录放在模块搜索路径最前面，项目里放一个 `pathlib.py` 就能顶替标准库；
- 没加 `-I` 的 Python 会执行用户 site-packages 里 `.pth` 文件中的代码；
- curl 会读 `~/.curlrc`。

结果是智能体可以让“停止”什么也不结束，也可以伪造隔离检查的结果。[#87](2026-11-02-attach-files.md) 已经把附件写入改成直接运行，两轮复查都指出停止脚本还有同样的问题。

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
  - `TestHelpersSkipTheAgentSettings`：逐个参数比对停止、会话组件检查和 Python 辅助命令的完整命令行，包括 `--context colima-aiofcage`、`--workdir /`、`/usr/bin/python3 -I`；
  - `TestAgentSettingsAreWrittenByAHelper`：写入 Agent 配置的命令行，并确认整个刷新过程中没有命令经过 `aiofcage-run`；
  - `TestNativeSessionLookup`：改为断言新的 find 命令行；
  - `TestAgentCommandsLoadTheAgentSettings`：对话的每一轮和终端仍经过 `aiofcage-run`。
- `internal/service`：
  - `TestOnlyTheEnvironmentCheckLoadsTheAgentSettings`：解析本包源码，经过 `aiofcage-run` 的 `manager.Exec` 只能剩 `runAttack` 一处（“凭据配置”）。以后新加的辅助命令用错入口，这个测试会失败。用同样的方法检查 main 的源码，能找到 7 处调用；
  - `TestCheckScriptsUseTheImagesPython`、`TestIsolationCurlSkipsTheAgentsConfig`：检查脚本用的解释器和 curl 参数。
- `go vet -tags production ./...`、`go test -race ./internal/...`、`make check` 都通过。

### 真实容器（不调用模型）

运行 `make gatekeeper-linux connector-linux` 后，执行 `AIOFCAGE_TERMINAL_E2E=1 go test -run TestHelpersWithstandAHostileAgentEndToEnd ./internal/service/`。测试会建一个临时空白沙箱，像一个想让停止失效的智能体那样，改写自己的设置和项目：

- `~/.aiofcage/env`：拦下以 `python3`、`test` 开头的命令，并伪造检查结果；
- 项目里：放一个 `pathlib.py`，导入它的脚本会直接退出；
- 用户 site-packages：放一个 `.pth` 文件；
- `~/.curlrc`：给 curl 的输出加上内容。

| 步骤 | 结果 |
|---|---|
| 对照：经过 `aiofcage-run` 运行 `python3 -c` | 输出的是伪造的“通过” |
| 对照：经过 `aiofcage-run` 运行 `test -f` | 失败 |
| 对照：在项目目录里运行 `python3 -c 'import pathlib'` | 导入了项目里的 `pathlib.py`，脚本没有运行到底 |
| 对照：不加 `-I` 的 Python | `.pth` 里的代码运行了 |
| 对照：不加 `--disable` 的 curl | 输出里出现了 `~/.curlrc` 加的内容 |
| `PythonHelper` 运行的脚本 | 正常运行，不受以上任何一处影响 |
| `curl --disable` | 没有读 `~/.curlrc` |
| 会话组件检查、原生会话查找 | 正常 |
| 停止：终端里有 bash 和一个后台 `sleep` | 先用原来的方式（经过 `aiofcage-run` 运行 `python3 -c`）结束进程，2 个会话进程都还在；关闭终端（`EndProcesses`）后剩 0 个 |
| 隔离检查 | 在 Agent 容器里跑的 7 项都是真实结果，没有伪造：未授权域名、绕开代理、DNS 查询外传、Docker 接口、虚拟机网关端口、项目以外的文件都“通过”；“凭据配置”判“无法验证”，因为空白沙箱没有凭据变量，和以前一样；外来凭据、直连远程 MCP、直连数据库没有配置，跳过 |

结果通过，耗时 7.9 秒。测试沙箱、它的两个网络和项目目录都已删除，虚拟机里的 aiofcage 网络数回到测试前的 24 个。

同一开关下，原有的 `TestTerminalLifecycleEndToEnd`（终端生命周期、子进程清理、退出竞态）和 `TestTerminalImagesEndToEnd`（附件）也通过。

## 范围与限制

- 辅助命令仍以和智能体相同的用户运行。智能体自己的进程可以向它发信号，在它运行的那一刻结束它；这需要智能体专门留一个后台进程盯着。
- 停止靠会话标记 `AIOFCAGE_TERMINAL_ID` 找进程。这要靠智能体配合，主动对抗的智能体能躲开：
  - 在会话里用 `env -u AIOFCAGE_TERMINAL_ID`、`env -i` 启动不带标记的后台进程，或者把进程设成不可转储，这样读不到它的 `/proc/<pid>/environ`；
  - 改写 `~/.aiofcage/env`，让以后经过 `aiofcage-run` 启动的会话都不带标记。已在一次性容器（同一镜像，不联网）里实测：加一行过滤参数的循环后，进程上的标记从 1 个变成 0 个。按本次范围，对话的每一轮和终端仍然经过 `aiofcage-run`，所以这条路没有堵上，记为后续事项。
  - 出现这些情况时，停止会断开应用和这一轮的连接，但容器里的进程可能继续运行。可靠的办法是停止沙箱，整个容器会停掉。
- 隔离检查“凭据配置”仍经过 `aiofcage-run`，智能体改写 `~/.aiofcage/env` 能改变这一项的结果。
- 其余检查不再受这些设置文件影响，但仍在 Agent 容器里以 Agent 用户运行，已被控制的智能体在运行时仍能干扰，见[隔离检查补对照](2026-10-31-isolation-checks.md)的限制。

## 未覆盖

- 停止对话的这一轮、取消本机 AI 控制任务没有用真实模型跑。它们和关闭终端调用同一个 `EndProcesses`，源码检查保证没有别的入口。
