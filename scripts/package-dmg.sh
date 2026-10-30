#!/bin/sh
# 把 make app 生成的 Ai of Cage.app 打成拖拽安装的 DMG，并写出 SHA256 校验文件。
# 用法：scripts/package-dmg.sh "bin/Ai of Cage.app" bin/AiofCage-0.1.0-arm64.dmg
set -eu

app=$1
dmg=$2
if [ ! -d "$app" ]; then
	echo "找不到 ${app}，请先运行 make app" >&2
	exit 1
fi
# 安装包里的应用必须带有效签名；签名坏了，系统会提示“已损坏”而不是“无法验证开发者”。
codesign --verify --deep --strict "$app"

staging=$(mktemp -d "${TMPDIR:-/tmp}/aiofcage-dmg.XXXXXX")
mounted=""
cleanup() {
	if [ -n "$mounted" ]; then
		hdiutil detach -quiet "$mounted" 2>/dev/null || true
		rmdir "$mounted" 2>/dev/null || true
	fi
	rm -rf "$staging"
}
# 被中断时清理后退出，不要接着往下执行。
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

ditto "$app" "$staging/$(basename "$app")"
ln -s /Applications "$staging/Applications"
rm -f "$dmg" "$dmg.sha256"
hdiutil create -quiet -volname "Ai of Cage" -srcfolder "$staging" -fs HFS+ -format UDZO -imagekey zlib-level=9 "$dmg"

# 挂载检查：镜像完整，里面的应用签名仍然有效。
hdiutil verify -quiet "$dmg"
mounted=$(mktemp -d "${TMPDIR:-/tmp}/aiofcage-mount.XXXXXX")
hdiutil attach -quiet -nobrowse -readonly -mountpoint "$mounted" "$dmg"
codesign --verify --deep --strict "$mounted/$(basename "$app")"
hdiutil detach -quiet "$mounted"
rmdir "$mounted"
mounted=""

(cd "$(dirname "$dmg")" && shasum -a 256 "$(basename "$dmg")" >"$(basename "$dmg").sha256")
echo "已生成 ${dmg}"
cat "$dmg.sha256"
