#!/usr/bin/env bash
# NetBootX 一键安装脚本（Linux / macOS）
#
# 做三件事：
#   1. 编译（如果还没有二进制）
#   2. 生成目录结构和默认配置
#   3. 安装 systemd / launchd 开机自启（可选）
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

info()  { printf "\033[36mℹ️  %s\033[0m\n" "$*"; }
ok()    { printf "\033[32m✅ %s\033[0m\n" "$*"; }
warn()  { printf "\033[33m⚠️  %s\033[0m\n" "$*"; }
die()   { printf "\033[31m❌ %s\033[0m\n" "$*" >&2; exit 1; }

# ---- 1. 检查环境 ----
info "检查环境…"
OS="$(uname -s)"
ARCH="$(uname -m)"
info "系统: $OS / $ARCH"

if [ ! -x "./go" ]; then
  die "找不到 ./go 包装脚本。请先确认工具链目录 .tools/go 存在。"
fi
if [ ! -x "./.tools/go/bin/go" ]; then
  die "Go 工具链缺失：.tools/go/bin/go 不存在。
  请下载 Go 1.27+ 解压到 .tools/go/，或修改 ./go 包装脚本指向系统 Go。"
fi

# ---- 2. 编译 ----
if [ ! -x "bin/netbootx" ]; then
  info "编译中…"
  make build
else
  info "已有二进制，跳过编译（要重编请 make clean && make build）"
fi

# ---- 3. 目录结构 ----
info "创建目录结构…"
mkdir -p iso user/autoinstall data log
[ -f iso/.gitkeep ] || touch iso/.gitkeep
[ -f user/autoinstall/.gitkeep ] || touch user/autoinstall/.gitkeep

# ---- 4. 引导文件自检 ----
MISSING=0
for f in bios/undionly.kpxe x86_64/ipxe.efi arm64/ipxe.efi wimboot; do
  if [ ! -s "boot/$f" ]; then
    warn "引导文件缺失: boot/$f"
    MISSING=1
  fi
done
if [ "$MISSING" = "1" ]; then
  warn "引导文件不全会导致客户机无法启动。可运行 scripts/fetch-boot.sh 重新下载。"
fi

# ---- 5. 服务注册 ----
if [ "$OS" = "Linux" ] && command -v systemctl >/dev/null 2>&1; then
  info "生成 systemd 服务文件（不自动启用）"
  cat > netbootx.service <<EOF
[Unit]
Description=NetBootX PXE/HTTPBoot 服务器
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
WorkingDirectory=$ROOT
ExecStart=$ROOT/bin/netbootx -root $ROOT -autostart -no-browser
Restart=on-failure
RestartSec=3
# DHCP(67)/TFTP(69) 是特权端口
AmbientCapabilities=CAP_NET_BIND_SERVICE CAP_NET_BROADCAST CAP_NET_RAW
CapabilityBoundingSet=CAP_NET_BIND_SERVICE CAP_NET_BROADCAST CAP_NET_RAW

[Install]
WantedBy=multi-user.target
EOF
  ok "已生成 $ROOT/netbootx.service"
  info "安装开机自启:  sudo cp $ROOT/netbootx.service /etc/systemd/system/ && sudo systemctl enable --now netbootx"
fi

if [ "$OS" = "Darwin" ]; then
  info "生成 launchd 服务文件（不自动加载）"
  PLIST="$HOME/Library/LaunchAgents/com.netbootx.server.plist"
  cat > "$ROOT/com.netbootx.server.plist" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>com.netbootx.server</string>
  <key>ProgramArguments</key>
  <array>
    <string>$ROOT/bin/netbootx</string>
    <string>-root</string><string>$ROOT</string>
    <string>-autostart</string><string>-no-browser</string>
  </array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>StandardOutPath</key><string>$ROOT/log/netbootx.log</string>
  <key>StandardErrorPath</key><string>$ROOT/log/netbootx.err.log</string>
</dict>
</plist>
EOF
  ok "已生成 $ROOT/com.netbootx.server.plist"
  info "安装开机自启:  cp $ROOT/com.netbootx.server.plist $PLIST && launchctl load $PLIST"
  warn "macOS 上绑定 67/69 端口需要 root，launchd 默认用户身份运行会失败。"
  warn "如需开机自启，请把 plist 放到 /Library/LaunchDaemons/ 并用 sudo launchctl load。"
fi

ok "安装完成！"
echo
echo "  启动方式一（推荐，有图形界面）:"
echo "     sudo ./bin/netbootx"
echo "     然后浏览器打开 http://127.0.0.1:16080"
echo
echo "  启动方式二（命令行直启）:"
echo "     sudo ./bin/netbootx -autostart -no-browser"
echo
echo "  把 ISO 文件放进: $ROOT/iso/"
echo
