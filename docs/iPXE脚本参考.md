# iPXE 脚本参考

NetBootX 给每台客户机生成的启动脚本长什么样，以及为什么这么写。
界面上点 ISO 的「预览」就能看到实际生成的脚本。

---

## 一、主菜单脚本

客户机 PXE 启动 → 拿到 iPXE → iPXE 从 `http://server:16000/boot/menu.ipxe?mac=xx` 拉这个脚本。

```
#!ipxe
# NetBootX iPXE 启动脚本（由服务端自动生成，请勿手工编辑）
# 客户机: aa:bb:cc:dd:ee:ff / UEFI x86_64
# 服务端: http://192.168.123.10:16000
dhcp
set base http://192.168.123.10:16000
menu NetBootX 启动菜单 - aa:bb:cc:dd:ee:ff
item --gap --  Windows
item iso_def456 1) Windows 11 - win11.iso (5.4GB)
item --gap --  Linux
item iso_abc123 2) Ubuntu 24.04.2 LTS - ubuntu-24.04.2-desktop-amd64.iso (4.8GB)
choose --timeout 30000 --default iso_def456 target && goto ${target}
:iso_def456
kernel wimboot
initrd --name bootmgr  ${base}/iso/def456/bootmgr
initrd --name bcd      ${base}/iso/def456/boot/bcd
initrd --name boot.sdi ${base}/iso/def456/boot/boot.sdi
initrd --name boot.wim ${base}/iso/def456/sources/boot.wim
boot

:iso_abc123
kernel ${base}/iso/abc123/casper/vmlinuz boot=casper iso-scan/filename=http://192.168.123.10:16000/iso/abc123/linux/ubuntu-24.04.2-desktop-amd64.iso quiet splash ---
initrd ${base}/iso/abc123/casper/initrd
boot
```

### 逐行说明

| 行 | 作用 |
|---|---|
| `#!ipxe` | 脚本标识。**第一行必须是这个**，否则 iPXE 不认 |
| `dhcp` | 让 iPXE 自己再走一次 DHCP（拿到 IP，且能感知网络变化） |
| `set base ...` | 定义变量，后面用 `${base}` 引用 |
| `menu ...` | 定义菜单标题 |
| `item --gap -- Windows` | 分组标题（不可选） |
| `item iso_def456 1) Windows 11 ...` | 菜单项。格式：`item <label> <显示文字>` |
| `choose --timeout 30000 --default X target` | 弹出菜单，30 秒超时，选中的 label 存进 `${target}` |
| `&& goto ${target}` | 跳到对应 label |
| `:iso_abc123` | label 定义 |
| `kernel <url> <参数>` | 加载内核 |
| `initrd <url>` | 加载 initrd |
| `boot` | 开始启动 |

**注意**：`choose` 的 `--timeout` 单位是**毫秒**，30000 = 30 秒。

---

## 二、Linux 引导（kernel 方式）

```
:iso_abc123
kernel ${base}/iso/abc123/casper/vmlinuz boot=casper iso-scan/filename=${base}/iso/abc123/linux/ubuntu.iso quiet splash ---
initrd ${base}/iso/abc123/casper/initrd
boot
```

关键点：

1. **kernel 直接从 HTTP 拉**，不需要先下载完再启动——iPXE 支持流式加载。
2. **`iso-scan/filename=` 参数**告诉安装程序去哪个 URL 找完整的 ISO。
   这样内核只加载了 initrd，ISO 本体等真正需要时才流式读。
   这是「不挂载 ISO」的核心技巧。
3. `---` 后面的参数不会传给 initrd 里的 init。

### 各发行版的内核参数

| 发行版 | 关键参数 |
|---|---|
| Ubuntu Desktop | `boot=casper iso-scan/filename={iso} quiet splash ---` |
| Ubuntu Server | `boot=casper iso-scan/filename={iso} ---` |
| Debian Live | `boot=live components fetch={iso}` |
| RHEL/CentOS | `inst.stage2={iso} inst.repo={iso} ` |
| Fedora | `inst.stage2={iso} ` |
| openEuler | `inst.stage2={iso} ` |
| Kylin | `inst.stage2={iso} ` |
| SUSE | `install={iso}` |
| Arch | `archiso_http_srv={base}/iso/{id}/` |
| Alpine | （直接 kernel+initrd，无需 iso 参数） |

---

## 三、Windows 引导（wimboot 方式）

```
:iso_def456
kernel wimboot
initrd --name bootmgr  ${base}/iso/def456/bootmgr
initrd --name bcd      ${base}/iso/def456/boot/bcd
initrd --name boot.sdi ${base}/iso/def456/boot/boot.sdi
initrd --name boot.wim ${base}/iso/def456/sources/boot.wim
boot
```

**原理**：Windows 的引导链是 `bootmgr` → `bcd`（启动配置）→ `boot.sdi`（内存盘模板）
→ `boot.wim`（WinPE 镜像）。

`wimboot` 是一个特殊的内核，它把这 4 个文件「假装」成一块硬盘递给 Windows 引导程序，
于是 Windows 以为自己在从光盘启动，实际上所有文件都是 iPXE 从 HTTP 流式拉下来的。

这 4 个文件全部从 ISO 内部按偏移读出来，**ISO 本身从不需要完整下载**。

### WinPE 变体

WinPE 的 ISO 没有 `install.wim`，但引导链一样（只有 boot.wim）。NetBootX 会自动识别，
菜单里标记为 WinPE。

### 已知限制

- UEFI 模式启动某些 Windows 版本会花屏（iVentoy 也有这个问题，是显卡驱动加载时机问题）
- Windows 7 需要额外的 USB3/NVMe 驱动注入才能在新机器上装

---

## 四、VMware ESXi

ESXi 的引导结构比较特殊（`boot.cfg` + 一堆 `.b00` 模块文件）。
NetBootX 用 sanboot 方式：直接把整个 ISO 通过 HTTP 挂成一个块设备。

```
:iso_xyz
sanboot --no-describe ${base}/iso/xyz/esxi.iso
```

---

## 五、调试技巧

### 在 iPXE 里看日志

客户机启动出现 NetBootX 菜单后，按 `Ctrl+B` 进入 iPXE 命令行，然后：

```
ifstat            # 看网卡状态和 IP
route             # 看路由
ping 192.168.123.10
imgstat           # 看已加载的镜像
echo ${base}      # 看变量
```

### 手动重跑菜单

```
chain http://192.168.123.10:16000/boot/menu.ipxe?mac=aa:bb:cc:dd:ee:ff
```

### 看服务端收到了什么

界面底部日志面板，或者：

```bash
tail -f log/netbootx.log
```

### 用 curl 验证服务端

```bash
# 菜单脚本
curl 'http://192.168.123.10:16000/boot/menu.ipxe?mac=aa:bb:cc:dd:ee:ff'

# ISO 里的某个文件（前 100 字节）
curl -r 0-99 'http://192.168.123.10:16000/iso/abc123/casper/vmlinuz' | xxd | head

# 确认支持 Range
curl -I 'http://192.168.123.10:16000/iso/abc123/casper/vmlinuz'
# 应看到 Accept-Ranges: bytes
```

---

## 六、为什么不用 NFS / iSCSI / SMB

iVentoy 也支持这些，但 NetBootX 选择只用 HTTP，理由：

1. **HTTP 是唯一能穿透一切的网络协议**。防火墙默认放行 80/16000，NFS/iSCSI 经常被挡。
2. **HTTP Range 请求天生适合随机读**。ISO9660 是随机访问格式，HTTP Range 完美匹配。
3. **iPXE 原生支持 HTTP**，不需要额外驱动。
4. **可以用 CDN / 反向代理** 加速跨地域分发。

代价是：某些老系统（比如老版本 Windows PE）只认 SMB 挂载。这种情况 NetBootX 会回退到
wimboot 方式（把文件读出来喂给引导程序），而不是挂载整个 ISO。
