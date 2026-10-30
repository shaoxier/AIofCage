# Ai of Cage

Ai of Cage，寓意“笼中 AI”，是结合独立沙箱的桌面智能体工作区：为 Codex、Claude Code 和 DeepSeek Harness 配置供应商，在沙箱中工作，并查看用量、费用估算和运行状态。

Qiniu Builder Day 2026 · 赛道一参赛项目，当前面向 macOS Apple Silicon。

## 演示视频

[观看 Ai of Cage 产品演示](docs/demo/demo.mp4)（约 4 分 26 秒，MP4，24 MiB）。

## 下载安装

支持 **macOS 13+、Apple Silicon（M 系列芯片）**。安装包发布在 [Releases](https://github.com/shaoxier/AIofCage/releases) 页面；还没有可下载的版本时，请按下一节从源码安装，或在源码目录运行 `make dmg` 自己打包。

**1. 安装运行环境**

沙箱运行在专用虚拟机里，需要先用 [Homebrew](https://brew.sh/) 安装 Colima 和 Docker CLI，应用的新手教程也会检查：

```bash
brew install colima docker
```

**2. 下载并核对**

下载 `AiofCage-版本号-arm64.dmg` 和同名的 `.sha256` 文件，放在同一个文件夹，在终端里核对（把版本号换成实际下载的）：

```bash
cd ~/Downloads && shasum -a 256 -c AiofCage-0.1.0-arm64.dmg.sha256
```

显示 `OK` 再继续。

**3. 安装**

双击打开 DMG，把 Ai of Cage 拖进「应用程序」文件夹。

**4. 第一次打开**

安装包没有经过 Apple 公证，第一次双击时系统会提示无法验证它是否包含恶意软件：

1. 点「完成」关掉提示。
2. 打开「系统设置 → 隐私与安全性」，向下滚到“安全性”，会看到 Ai of Cage 被阻止的提示。
3. 点「仍要打开」，在弹出的确认框里再点一次「仍要打开」，用触控 ID 或开机密码确认。

这个按钮在系统拦下过一次之后才出现，大约一小时后消失；找不到时，再双击一次应用，马上回到这个页面。放行之后就可以正常双击打开。macOS 15 起，按住 Control 点「打开」的老办法已经不管用了。首次启动沙箱时会自动下载虚拟机镜像和所选智能体，请保持联网。

## 从源码安装

支持 **macOS 13+、Apple Silicon（M 系列芯片）**。

**1. 安装依赖**

需要先安装 [Homebrew](https://brew.sh/) 和 Xcode Command Line Tools：

```bash
xcode-select --install
```

通过 Homebrew 安装 Go（1.26.4+）、Colima 和 Docker CLI：

```bash
brew install go colima docker
```

已安装的依赖可跳过。

**2. 获取源码并启动**

```bash
git clone https://github.com/shaoxier/AIofCage.git
cd AIofCage
make app
open "bin/Ai of Cage.app"
```

也可以将生成的 `bin/Ai of Cage.app` 拖入「应用程序」文件夹，之后双击打开。

首次启动沙箱时会自动下载运行环境和所选智能体，请保持联网。

## 开始使用

首次启动会显示使用教程，按步骤完成环境检查、供应商配置、沙箱创建与首次对话。以后也可从左侧「使用教程」继续或重新学习，已有配置会保留。详见[新手教程](docs/onboarding.md)。

1. 在「模型供应商」中添加智能体的接口、模型和 API Key，并测试连接。
2. 点击「创建沙箱」，选择或新建项目，勾选智能体并绑定供应商。
3. 启动沙箱，在「终端」页新建智能体会话并输入任务；在「用量」「运行状态」「日志」中查看消耗和运行情况。

供应商更改在重启沙箱后生效。配置、会话恢复和环境重置见[使用指南](docs/user-guide.md)。

## 主要功能

- [x] 首次启动引导与常驻教程入口，支持进度恢复和复用已有配置。
- [x] 多供应商管理，每个沙箱独立选择智能体与供应商。
- [x] 沙箱启停、智能体会话、图片附件与会话恢复。
- [x] 用量明细、费用估算、请求额度和资源监控。
- [x] MCP 工具连接、权限管理、安全诊断与脱敏日志。
- [x] 业务案例与本机 MCP 控制，支持批量任务和结果对比。

完整实现范围与未支持项见[功能清单](docs/features.md)。费用为估算值；桌面会话切换供应商需手动操作。

智能体可以修改或删除项目文件，请保留版本与备份。API Key 保存在 macOS 钥匙串；沙箱不提供逐命令审批，安全机制与限制见[当前架构](docs/architecture-current.md#三条隔离边界)。

## 文档

| 想了解什么 | 入口 |
| --- | --- |
| 首次配置、教程进度与重新学习 | [新手教程](docs/onboarding.md) |
| 日常操作、配置与恢复 | [使用指南](docs/user-guide.md) |
| 构建、测试与贡献 | [开发指南](docs/development.md) |
| 产品规格、架构、验收与项目资料 | [文档目录](docs/README.md) |

## 致谢

界面流程参考 [CC Switch](https://github.com/farion1231/cc-switch) 和 Docker Desktop；代理注入设计参考 [mattolson/agent-sandbox](https://github.com/mattolson/agent-sandbox) 与 [Docker Sandboxes](https://docs.docker.com/ai/sandboxes/)。第三方前端库来源和许可证见 [vendor 说明](frontend/src/vendor/README.md)。
