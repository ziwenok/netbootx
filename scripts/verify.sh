#!/usr/bin/env bash
# NetBootX 自检脚本：验证安装是否完整、服务是否可用。
# 用法： ./scripts/verify.sh [服务端IP]
set -uo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

PASS=0; FAIL=0; WARN=0
ok()   { printf "  \033[32m✅ %s\033[0m\n" "$*"; PASS=$((PASS+1)); }
bad()  { printf "  \033[31m❌ %s\033[0m\n" "$*"; FAIL=$((FAIL+1)); }
warn() { printf "  \033[33m⚠️  %s\033[0m\n" "$*"; WARN=$((WARN+1)); }
hdr()  { printf "\n\033[1m%s\033[0m\n" "$*"; }

hdr "1. 目录结构"
for d in boot iso user data log; do
  if [ -d "$d" ]; then ok "目录 $d/ 存在"; else bad "目录 $d/ 缺失"; fi
done

hdr "2. 引导文件"
for f in bios/undionly.kpxe x86_64/ipxe.efi arm64/ipxe.efi wimboot; do
  if [ -s "boot/$f" ]; then
    ok "boot/$f  ($(du -h "boot/$f" | cut -f1))"
  else
    bad "boot/$f 缺失或为空 —— 运行 ./scripts/fetch-boot.sh"
  fi
done

hdr "3. 二进制"
if [ -x "bin/netbootx" ]; then
  ok "bin/netbootx 存在 ($(du -h bin/netbootx | cut -f1))"
  VER=$(./bin/netbootx -version 2>/dev/null || echo "无法执行")
  ok "版本: $VER"
else
  warn "bin/netbootx 不存在 —— 运行 make build"
fi

hdr "4. 源码编译"
if ./go build -o /dev/null . 2>/dev/null; then
  ok "整个项目编译通过"
else
  bad "编译失败 —— 运行 ./go build . 查看详情"
fi

hdr "5. 单元测试"
if OUT=$(./go test ./... -count=1 -timeout 180s 2>&1); then
  ok "全部测试通过"
else
  bad "测试失败"
  echo "$OUT" | grep -E '^(FAIL|---)' | head -10 | sed 's/^/     /'
fi

hdr "6. ISO 文件"
# 必须跟随软链接（用户常用 ln -s 把 ISO 链进来），所以不能用 -type f
ISOCOUNT=$(find -L iso -name '*.iso' 2>/dev/null | wc -l | tr -d ' ')
if [ "$ISOCOUNT" -gt 0 ]; then
  ok "找到 $ISOCOUNT 个 ISO 文件"
  find -L iso -name '*.iso' 2>/dev/null | head -5 | while read -r f; do
    if [ -L "$f" ]; then
      printf "     %s (%s, 软链接 → %s)\n" "$f" "$(du -hL "$f" | cut -f1)" "$(readlink "$f")"
    else
      printf "     %s (%s)\n" "$f" "$(du -h "$f" | cut -f1)"
    fi
  done
else
  warn "iso/ 目录里没有 ISO 文件 —— 放一个进去才能启动客户机"
fi

hdr "7. 服务连通性"
IP="${1:-}"
if [ -z "$IP" ]; then
  # 尝试从配置里读
  if [ -f data/settings.json ]; then
    IP=$(grep -o '"serverIP"[^,]*' data/settings.json 2>/dev/null | cut -d'"' -f4)
  fi
fi

if [ -n "$IP" ]; then
  WEBPORT=$(grep -o '"webPort"[^,]*' data/settings.json 2>/dev/null | grep -o '[0-9]*' || echo 16080)
  HTTPPORT=$(grep -o '"httpPort"[^,]*' data/settings.json 2>/dev/null | grep -o '[0-9]*' || echo 16000)

  if curl -sf --max-time 3 "http://$IP:$WEBPORT/api/status" >/dev/null 2>&1; then
    ok "Web 界面可达 http://$IP:$WEBPORT"
  else
    warn "Web 界面不可达（服务没启动？）http://$IP:$WEBPORT"
  fi
  if curl -sf --max-time 3 "http://$IP:$HTTPPORT/healthz" >/dev/null 2>&1; then
    ok "HTTP 文件服务可达 http://$IP:$HTTPPORT"
  else
    warn "HTTP 文件服务不可达 http://$IP:$HTTPPORT"
  fi
else
  warn "未配置服务端 IP，跳过连通性检查"
fi

hdr "8. 端口占用"
# 从配置里读实际端口，而不是硬编码默认值（用户可能改了端口）
CFG_WEB=$(grep -o '"webPort"[^,}]*' data/settings.json 2>/dev/null | grep -o '[0-9]*' | head -1)
CFG_HTTP=$(grep -o '"httpPort"[^,}]*' data/settings.json 2>/dev/null | grep -o '[0-9]*' | head -1)
CFG_TFTP=$(grep -o '"tftpPort"[^,}]*' data/settings.json 2>/dev/null | grep -o '[0-9]*' | head -1)
CFG_DHCP=$(grep -o '"dhcpPort"[^,}]*' data/settings.json 2>/dev/null | grep -o '[0-9]*' | head -1)
CFG_WEB=${CFG_WEB:-16080}; CFG_HTTP=${CFG_HTTP:-16000}
CFG_TFTP=${CFG_TFTP:-69}; CFG_DHCP=${CFG_DHCP:-67}

check_port() {
  local p="$1" proto="$2" name="$3" found proc
  if [ "$proto" = "tcp" ]; then
    found=$(lsof -nP -iTCP:"$p" -sTCP:LISTEN 2>/dev/null | tail -n +2 | head -1)
  else
    found=$(lsof -nP -iUDP:"$p" 2>/dev/null | tail -n +2 | head -1)
  fi
  if [ -n "$found" ]; then
    proc=$(printf '%s' "$found" | awk '{print $1}')
    ok "端口 $p/$proto ($name) 被 $proc 监听"
  else
    warn "端口 $p/$proto ($name) 空闲"
  fi
}
check_port "$CFG_DHCP" udp "DHCP"
check_port "$CFG_TFTP" udp "TFTP"
check_port "$CFG_HTTP" tcp "HTTP"
check_port "$CFG_WEB"  tcp "Web"

hdr "结果"
printf "  通过 %d  警告 %d  失败 %d\n\n" "$PASS" "$WARN" "$FAIL"
[ "$FAIL" -eq 0 ] && { printf "\033[32m全部检查通过 ✅\033[0m\n"; exit 0; }
printf "\033[31m有 %d 项检查失败 ❌\033[0m\n" "$FAIL"
exit 1
