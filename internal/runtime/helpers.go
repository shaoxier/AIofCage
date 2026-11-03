package runtime

import (
	"context"
	"io"
)

// 宿主在 Agent 容器里运行命令有两种方式：
//   - Exec（ExecArgs）经过 aiofcage-run，先加载 ~/.aiofcage/env，再在项目目录里运行。只用于确实需要
//     这些设置的命令：智能体 CLI、终端、cagectl exec，以及检查 Agent 环境变量的那项模拟攻击。
//   - ExecHelper 运行应用自己的辅助命令，例如停止会话、写入附件、隔离检查，不加载 Agent 的任何设置。
//
// ~/.aiofcage/env 和项目目录都可以被 Agent 改写。辅助命令如果经过 aiofcage-run，Agent 可以让它先运行
// 别的命令或直接退出；python3 -c 还会先导入项目里的同名模块。这样“停止”可能什么也没结束，检查结果也可能是伪造的。

// ExecHelper 以 Agent 用户（与智能体相同的 1000:1000）直接运行宿主提供的命令：不经过 aiofcage-run；
// 工作目录是 /，而不是 Agent 可写的项目目录。PATH 等变量只来自容器配置，Agent 改不了。
// Python 脚本用 PythonHelper 构造命令。
func (m *Manager) ExecHelper(ctx context.Context, id string, stdin io.Reader, stdout, stderr io.Writer, command ...string) error {
	return m.docker.stream(ctx, stdin, stdout, stderr, helperArgs(id, command...)...)
}

// helperArgs 是 ExecHelper 的 docker 参数，供需要超时或错误摘要的调用直接交给 docker.run。
func helperArgs(id string, command ...string) []string {
	return append([]string{"exec", "--interactive", "--user", agentUser, "--workdir", "/", names(id).agent}, command...)
}

// PythonHelper 返回用镜像自带的 Python 运行脚本的命令。-I 隔离模式忽略 PYTHON* 变量和用户的
// site-packages（Agent 放在那里的 .pth 文件会在启动时执行），工作目录也不在模块搜索路径上。
func PythonHelper(script string, args ...string) []string {
	return append([]string{"/usr/bin/python3", "-I", "-c", script}, args...)
}

// endProcessesScript 只匹配随机会话标记，不输出环境内容，也不向其他会话的进程发送信号。
const endProcessesScript = `import os, pathlib, signal, sys, time
marker = ('AIOFCAGE_TERMINAL_ID=' + sys.argv[1]).encode()
for sig in (signal.SIGHUP, signal.SIGKILL):
    for path in pathlib.Path('/proc').glob('[0-9]*/environ'):
        try:
            pid = int(path.parent.name)
            if pid > 1 and marker in path.read_bytes().split(b'\0'):
                os.kill(pid, sig)
        except (OSError, ValueError):
            pass
    if sig == signal.SIGHUP:
        time.sleep(0.2)
`

// EndProcesses 结束沙箱里带有会话标记 AIOFCAGE_TERMINAL_ID=marker 的进程（子进程继承这个标记）：先发
// SIGHUP，0.2 秒后对剩下的发 SIGKILL。只结束宿主的 Docker 客户端不够，容器里的进程会继续运行。
func (m *Manager) EndProcesses(ctx context.Context, id, marker string) error {
	return m.ExecHelper(ctx, id, nil, io.Discard, io.Discard, PythonHelper(endProcessesScript, marker)...)
}
