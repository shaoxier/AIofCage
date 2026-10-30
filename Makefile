GO ?= go
WAILS_VERSION := v2.17.0
# 发布版本号以 Info.plist 为准：安装包文件名和应用里显示的版本都取它。
VERSION := $(shell /usr/libexec/PlistBuddy -c 'Print :CFBundleShortVersionString' packaging/macos/Info.plist)
DMG := bin/AiofCage-$(VERSION)-arm64.dmg
DESKTOP_LDFLAGS ?=

.PHONY: desktop app dmg icons gatekeeper gatekeeper-linux connector-linux cagectl dev check test clean

GATEKEEPER_LINUX := internal/runtime/assets/gatekeeper/gatekeeper-linux-arm64
CONNECTOR_LINUX := internal/runtime/assets/connector/connector-linux-arm64
APP := bin/Ai of Cage.app

desktop: gatekeeper-linux connector-linux
	$(GO) build -tags production -trimpath -ldflags "$(DESKTOP_LDFLAGS)" -o bin/AIofCage .

# macOS 应用包：可以在访达里双击打开，程序坞和自动化工具都按应用标识 io.aiofcage.desktop 识别它。
app: desktop
	rm -rf "$(APP)"
	mkdir -p "$(APP)/Contents/MacOS" "$(APP)/Contents/Resources"
	cp bin/AIofCage "$(APP)/Contents/MacOS/AIofCage"
	cp packaging/macos/Info.plist "$(APP)/Contents/Info.plist"
	cp packaging/macos/AppIcon.icns "$(APP)/Contents/Resources/AppIcon.icns"
	codesign --force --sign - "$(APP)"

# 安装包：用发布版本号重新构建应用，打成拖拽安装的 DMG，并写出 SHA256 校验文件。只有本机临时签名，
# 没有公证，首次打开的放行步骤见 README。
dmg:
	$(MAKE) app DESKTOP_LDFLAGS="-X github.com/shaoxier/AIofCage/internal/buildinfo.Version=$(VERSION)"
	scripts/package-dmg.sh "$(APP)" "$(DMG)"

# 修改 Logo SVG 后重新生成 PNG 和包含 16—1024 像素图像的 ICNS；日常打包直接使用已生成的图标。
icons:
	swift packaging/macos/generate-icon.swift frontend/src/assets/logo.svg packaging/macos

gatekeeper:
	$(GO) build -trimpath -o bin/gatekeeper ./cmd/gatekeeper

# 沙箱里运行的门卫：Linux/arm64 静态二进制，编译进桌面应用和 cagectl，在专用虚拟机里打包成镜像。
gatekeeper-linux:
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 $(GO) build -trimpath -ldflags "-s -w" -o $(GATEKEEPER_LINUX) ./cmd/gatekeeper

# 可信连接器：代持数据库密码等秘密，运行需要秘密的 stdio MCP 服务，只供门卫调用。
connector-linux:
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 $(GO) build -trimpath -ldflags "-s -w" -o $(CONNECTOR_LINUX) ./cmd/connector

# 命令行版本，与桌面应用共用运行时（开发、自动检查和演示兜底）。
cagectl: gatekeeper-linux connector-linux
	$(GO) build -trimpath -o bin/cagectl ./cmd/cagectl

dev:
	$(GO) run github.com/wailsapp/wails/v2/cmd/wails@$(WAILS_VERSION) dev

check:
	$(GO) vet -tags production ./...
	$(GO) build -tags production ./...

test:
	$(GO) test -race ./internal/...

clean:
	rm -f bin/AIofCage bin/gatekeeper bin/cagectl bin/AiofCage-*.dmg bin/AiofCage-*.dmg.sha256 $(GATEKEEPER_LINUX) $(CONNECTOR_LINUX)
	rm -rf "$(APP)"
