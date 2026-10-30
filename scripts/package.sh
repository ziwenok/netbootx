#!/usr/bin/env bash
# 打包成可分发压缩包：多平台二进制 + 引导文件 + 空目录结构 + 文档
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

VER=$(./bin/netbootx -version 2>/dev/null | awk '{print $2}' || echo "1.0.0")
DIST="dist"
rm -rf "$DIST"
mkdir -p "$DIST"

PLATFORMS=(
  "darwin  arm64  netbootx"
  "darwin  amd64  netbootx"
  "linux   amd64  netbootx"
  "linux   arm64  netbootx"
  "windows amd64  netbootx.exe"
  "windows arm64  netbootx.exe"
)

info() { printf "\033[36mℹ️  %s\033[0m\n" "$*"; }
ok()   { printf "\033[32m✅ %s\033[0m\n" "$*"; }

for entry in "${PLATFORMS[@]}"; do
  read -r goos goarch binname <<< "$entry"
  name="netbootx-${VER}-${goos}-${goarch}"
  out="${DIST}/${name}"
  info "构建 $name ..."

  if ! GOOS="$goos" GOARCH="$goarch" ./go build -trimpath -ldflags "-s -w" -o "$out/$binname" .; then
    printf "\033[33m⚠️  跳过 %s（构建失败）\033[0m\n" "$name"
    rm -rf "$out"
    continue
  fi

  cp -r boot "$out/"
  cp README.md ARCHITECTURE.md "$out/"
  cp -r docs "$out/" 2>/dev/null || true
  mkdir -p "$out"/iso "$out"/user/autoinstall "$out"/data "$out"/log
  cp user/autoinstall/example-* "$out/user/autoinstall/" 2>/dev/null || true
  touch "$out"/iso/.gitkeep "$out"/user/autoinstall/.gitkeep

  mkdir -p "$out/scripts"
  cp scripts/fetch-boot.sh scripts/verify.sh scripts/dhcp-probe.py "$out/scripts/" 2>/dev/null || true
  chmod +x "$out/scripts/"*.sh "$out/scripts/"*.py 2>/dev/null || true

  (cd "$DIST" && tar czf "${name}.tar.gz" "$name")
  if command -v zip >/dev/null 2>&1; then
    (cd "$DIST" && zip -qr "${name}.zip" "$name")
  fi
  rm -rf "$out"
  ok "$name  ($(du -h "$DIST/${name}.tar.gz" | cut -f1))"
done

echo
info "全部产物："
ls -lh "$DIST" | tail -n +2 | awk '{printf "  %-48s %s\n", $9, $5}'
echo
echo "分发说明："
echo "  1. 解压对应平台的压缩包"
echo "  2. 把 ISO 放进 iso/"
echo "  3. sudo ./netbootx"
echo "  4. 浏览器打开 http://127.0.0.1:16080"
