#!/usr/bin/env python3
"""NetBootX DHCP 探针：发一个真实的 DHCP DISCOVER，解析服务端 OFFER。

用法:
    sudo python3 scripts/dhcp-probe.py --port 16767 --mac aa:bb:cc:dd:ee:ff --arch 7

--arch 是 option 93 的值：
    0  = Legacy BIOS
    6  = IA32 UEFI
    7  = x86_64 UEFI
    9  = x86_64 UEFI (旧)
    11 = ARM64 UEFI
    16 = x86_64 HTTPBoot
    19 = ARM64 HTTPBoot
    27 = LoongArch64

不需要 root 就能测高位端口（如 16767），测 67 需要 root。
"""
import argparse
import random
import socket
import struct
import sys

MAGIC = b"\x63\x82\x53\x63"

def build_discover(mac: str, arch: int, hostname: str = "nbx-probe") -> bytes:
    """构造一个 DHCP DISCOVER 报文。"""
    macb = bytes(int(x, 16) for x in mac.split(":"))

    op = 1          # BOOTREQUEST
    htype = 1       # Ethernet
    hlen = 6
    hops = 0
    xid = random.randint(0, 0xFFFFFFFF)
    secs = 0
    flags = 0x8000  # broadcast flag
    ciaddr = "0.0.0.0"
    yiaddr = "0.0.0.0"
    siaddr = "0.0.0.0"
    giaddr = "0.0.0.0"

    pkt = struct.pack(
        "!BBBBIHH4s4s4s4s16s64s128s",
        op, htype, hlen, hops, xid, secs, flags,
        socket.inet_aton(ciaddr), socket.inet_aton(yiaddr),
        socket.inet_aton(siaddr), socket.inet_aton(giaddr),
        macb + b"\x00" * 10,   # chaddr (16 bytes)
        b"\x00" * 64,          # sname
        b"\x00" * 128,         # file
    )
    pkt += MAGIC

    def opt(code: int, data: bytes) -> bytes:
        return bytes([code, len(data)]) + data

    pkt += opt(53, b"\x01")                                   # DHCPDISCOVER
    pkt += opt(55, bytes([1, 3, 6, 12, 15, 28, 43, 60, 66, 67, 93, 97]))  # param request
    pkt += opt(60, b"PXEClient:Arch:%05d:UNDI:003000" % arch)  # vendor class
    pkt += opt(93, struct.pack("!H", arch))                    # client arch
    pkt += opt(97, b"\x00" + bytes(range(16)))                 # client UUID
    pkt += opt(12, hostname.encode())                          # hostname
    pkt += b"\xff"                                            # end

    # 填充到最小 300 字节（很多实现要求）
    if len(pkt) < 300:
        pkt += b"\x00" * (300 - len(pkt))
    return pkt


def parse_reply(data: bytes) -> dict:
    """解析 DHCP 回包。"""
    if len(data) < 240:
        return {"error": "报文太短"}
    (op, htype, hlen, hops, xid, secs, flags) = struct.unpack("!BBBBIHH", data[:12])
    yiaddr = socket.inet_ntoa(data[16:20])
    siaddr = socket.inet_ntoa(data[20:24])

    if data[236:240] != MAGIC:
        return {"error": "magic cookie 不对"}

    opts = {}
    i = 240
    while i < len(data):
        code = data[i]
        if code == 0:
            i += 1
            continue
        if code == 255:
            break
        if i + 1 >= len(data):
            break
        ln = data[i + 1]
        val = data[i + 2:i + 2 + ln]
        opts[code] = val
        i += 2 + ln

    msg_type = {1: "DISCOVER", 2: "OFFER", 3: "REQUEST", 4: "DECLINE",
                5: "ACK", 6: "NAK", 7: "RELEASE", 8: "INFORM"}.get(
        opts.get(53, b"\x00")[0], "?")

    def ip(o):
        v = opts.get(o)
        return socket.inet_ntoa(v) if v and len(v) == 4 else None

    def txt(o):
        v = opts.get(o)
        return v.decode("utf-8", "replace").rstrip("\x00") if v else None

    out = {
        "msg_type": msg_type,
        "op": op,
        "yiaddr": yiaddr,
        "siaddr": siaddr,
        "server_id": ip(54),
        "subnet_mask": ip(1),
        "router": ip(3),
        "dns": [socket.inet_ntoa(opts[6][j:j+4]) for j in range(0, len(opts.get(6, b"")), 4)],
        "domain": txt(15),
        "lease_time": struct.unpack("!I", opts[51])[0] if 51 in opts and len(opts[51]) == 4 else None,
        "tftp_server_name": txt(66),
        "bootfile_name": txt(67),
        "vendor_class": txt(60),
        "option43_len": len(opts.get(43, b"")),
        "all_option_codes": sorted(opts.keys()),
    }
    return out


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--host", default="127.0.0.1")
    ap.add_argument("--port", type=int, default=67)
    ap.add_argument("--mac", default="aa:bb:cc:dd:ee:ff")
    ap.add_argument("--arch", type=int, default=7)
    ap.add_argument("--timeout", type=float, default=5.0)
    args = ap.parse_args()

    pkt = build_discover(args.mac, args.arch)
    print(f"发送 DISCOVER: mac={args.mac} arch={args.arch} -> {args.host}:{args.port}")
    print(f"报文长度 {len(pkt)} 字节")

    s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
    s.setsockopt(socket.SOL_SOCKET, socket.SO_BROADCAST, 1)
    s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    try:
        s.bind(("0.0.0.0", 68))
    except OSError:
        # 68 被占用（本机有真 DHCP 客户端）时用随机端口
        s.bind(("0.0.0.0", 0))
    s.settimeout(args.timeout)

    s.sendto(pkt, (args.host, args.port))
    try:
        data, src = s.recvfrom(4096)
    except socket.timeout:
        print("\n❌ 超时：没收到回复")
        print("   检查：服务端是否启动？防火墙？端口对不对？")
        return 1

    print(f"\n收到来自 {src[0]}:{src[1]} 的回复，{len(data)} 字节")
    r = parse_reply(data)
    if "error" in r:
        print("❌ 解析失败:", r["error"])
        return 1

    print("\n" + "=" * 56)
    print(f"  报文类型      {r['msg_type']}")
    print(f"  分配 IP       {r['yiaddr']}")
    print(f"  next-server   {r['siaddr']}")
    print(f"  服务器标识    {r['server_id']}")
    print(f"  子网掩码      {r['subnet_mask']}")
    print(f"  网关          {r['router']}")
    print(f"  DNS           {', '.join(r['dns']) or '-'}")
    print(f"  域名          {r['domain']}")
    print(f"  租约          {r['lease_time']} 秒")
    print(f"  TFTP 服务器   {r['tftp_server_name']}")
    print(f"  引导文件      {r['bootfile_name']}")
    print(f"  厂商类        {r['vendor_class']}")
    print(f"  option 43     {r['option43_len']} 字节")
    print(f"  全部选项      {r['all_option_codes']}")
    print("=" * 56)

    # 断言
    ok = True
    if r["msg_type"] not in ("OFFER", "ACK"):
        print(f"⚠️  报文类型是 {r['msg_type']}，PXE 场景期望 OFFER 或 ACK")
        ok = False
    if not r["bootfile_name"]:
        print("❌ 没有下发引导文件（option 67）")
        ok = False
    if not r["server_id"]:
        print("❌ 没有服务器标识（option 54）")
        ok = False
    if args.arch != 0 and not r["tftp_server_name"] and r["siaddr"] == "0.0.0.0":
        print("⚠️  既没有 option 66 也没有 siaddr，客户机可能找不到 TFTP 服务器")

    print()
    if ok:
        print("✅ DHCP 应答看起来正常")
        return 0
    print("❌ DHCP 应答有问题")
    return 1


if __name__ == "__main__":
    sys.exit(main())
