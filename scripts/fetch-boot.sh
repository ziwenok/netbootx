#!/usr/bin/env bash
# 下载 iPXE 引导文件到 boot/ 目录。
# 这些文件是客户机 PXE 启动时真正执行的东西，必须齐全。
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT/boot"

mkdir -p x86_64 bios arm64 i386 loong64

dl() {
  local url="$1" out="$2"
  printf "  %-34s " "$out"
  local code
  code=$(curl -sL --max-time 120 -o "$out" -w "%{http_code}" "$url" || echo "ERR")
  local sz
  sz=$(stat -f%z "$out" 2>/dev/null || stat -c%s "$out" 2>/dev/null || echo 0)
  if [ "$code" = "200" ] && [ "$sz" -gt 10000 ]; then
    printf "\033[32m✅ %s 字节\033[0m\n" "$sz"
  else
    printf "\033[31m❌ HTTP %s, %s 字节\033[0m\n" "$code" "$sz"
    rm -f "$out"
    return 1
  fi
}

echo "下载 iPXE 引导文件…"
BASE=https://boot.ipxe.org
dl "$BASE/x86_64-efi/ipxe.efi"        x86_64/ipxe.efi
dl "$BASE/x86_64-efi/snponly.efi"     x86_64/snponly.efi
dl "$BASE/i386-pcbios/undionly.kpxe"  bios/undionly.kpxe
dl "$BASE/i386-pcbios/ipxe.pxe"       bios/ipxe.pxe
dl "$BASE/arm64-efi/ipxe.efi"         arm64/ipxe.efi
dl "$BASE/i386-efi/ipxe.efi"          i386/ipxe.efi
dl "$BASE/loong64-efi/ipxe.efi"       loong64/ipxe.efi
dl "$BASE/shimx64.efi"                x86_64/shimx64.efi

echo "下载 wimboot（Windows 引导必需）…"
dl "https://github.com/ipxe/wimboot/releases/latest/download/wimboot" wimboot

echo
echo "完成。文件清单："
find . -type f -size +10k | sort | while read -r f; do
  printf "  %-32s %10s 字节\n" "$f" "$(stat -f%z "$f" 2>/dev/null || stat -c%s "$f")"
done
