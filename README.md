# NetBootX

> 增强版 PXE / UEFI HTTPBoot 服务器 —— 把 ISO 文件丢进去，客户机通过网络就能启动它。

**一句话**：你的 PC、笔记本、服务器、NAS、树莓派，装上 NetBootX 就变成一台 PXE 服务器，
同时给几十台机器网络启动、装系统。

> **状态**：v1.0.0 已完成并通过验收。
> 278 个测试全绿、真实 Alpine ISO 验证、真实 DHCP/TFTP 协议报文验证、真实浏览器 UI 验证。
> 详见 [docs/验收报告.md](docs/验收报告.md)。

---

## 它解决什么问题

传统装系统：刻盘 → 插 U 盘 → 一台台装。20 台机器要折腾一整天。

NetBootX：把 ISO 丢进目录 → 客户机开机按 F12 选网络启动 → 出现菜单 → 点一下 → 开装。
20 台机器同时开，你去喝咖啡。

---

## 核心特性

| 特性 | 说明 |
|---|---|
| **零配置** | ISO 丢进 `iso/` 目录就能用，自动识别是什么系统 |
| **不挂载 ISO** | 通过 HTTP 从 ISO 内部按字节偏移流式读取，不占磁盘、不用 loop 设备 |
| **多架构** | Legacy BIOS / IA32 UEFI / x86_64 UEFI / ARM64 UEFI / LoongArch64 UEFI |
| **HTTPBoot** | 支持 UEFI HTTP Boot，比 TFTP 快得多 |
| **多机并发** | 同时给几十台客户机提供启动服务 |
| **三种 DHCP 模式** | full（独立 DHCP）/ proxy（与现有 DHCP 共存）/ external（外部 DHCP 通告） |
| **Web 管理** | 浏览器操作，实时看到每台客户机的启动进度 |
| **跨平台** | macOS / Linux / Windows，单二进制，零依赖 |
| **无人值守** | 支持 Kickstart / Preseed / AutoYaST / AutoUnattend / Cloud-init 模板 |

---

## 快速开始

### 1. 准备

```bash
cd netbootx

# 下载 iPXE 引导文件（首次必需）
./scripts/fetch-boot.sh

# 编译
make build
```

### 2. 放 ISO

```bash
cp ~/Downloads/ubuntu-24.04.2-desktop-amd64.iso iso/
# 也可以软链接，不占额外空间
ln -s /Volumes/Data/isos/win11.iso iso/win11.iso
# 支持子目录分类
mkdir -p iso/linux iso/windows
```

### 3. 启动

```bash
# 需要 root 才能绑 67/69 端口
sudo ./bin/netbootx
```

浏览器会自动打开 `http://127.0.0.1:16080`：

1. 顶部选择**本机 IP**（就是客户机能访问到的那个网卡的 IP）
2. 选 **DHCP 模式**：
   - 局域网里**没有**其他 DHCP 服务器 → 选 `full`，填 IP 地址池
   - 局域网里**已有**路由器当 DHCP → 选 `proxy`（推荐，不影响现有网络）
3. 点绿色的 **▶ 开始** 按钮

### 4. 客户机启动

客户机开机 → 按 `F12` / `F11` / `Esc`（看主板）→ 选 **Network Boot / PXE** →
出现 NetBootX 菜单 → 选要装的系统 → 回车。

---

## DHCP 三种模式怎么选

| 模式 | 适用场景 | 风险 |
|---|---|---|
| **proxy**（默认） | 局域网已有 DHCP（路由器、公司网络） | 无。只应答 PXE 请求，不抢现有 DHCP 的活 |
| **full** | 隔离的测试网络，或没有 DHCP 的环境 | 会分配 IP。如果和现有 DHCP 冲突会导致网络混乱 |
| **external** | 已有企业级 DHCP，且你能改它的配置 | 无。NetBootX 只提供 TFTP/HTTP，需要你在 DHCP 里手动配 next-server |

> **不确定选哪个？选 proxy。** 它在 99% 的场景下都安全。

---

## 端口说明

| 端口 | 用途 | 需要 root |
|---|---|---|
| 67/UDP | DHCP | 是 |
| 69/UDP | TFTP（下发 iPXE 引导文件） | 是 |
| 16000/TCP | HTTP（下发 iPXE 脚本、内核、ISO 内容） | 否 |
| 16080/TCP | Web 管理界面 | 否 |

> 选 16000 而不是 80，是为了不和你机器上已有的 Web 服务冲突，
> 也为了能和 iVentoy（用 26000）装在同一台机器上互不干扰。

---

## 目录结构

```
netbootx/
├── bin/netbootx          编译产物
├── boot/                 iPXE 引导文件（脚本自动下载）
│   ├── x86_64/ipxe.efi   UEFI x86_64 客户机
│   ├── bios/undionly.kpxe Legacy BIOS 客户机
│   ├── arm64/ipxe.efi    ARM64 客户机
│   └── wimboot           Windows / WinPE 引导
├── iso/                  ★ 你的 ISO 放这里（支持子目录、软链接）
├── user/
│   └── autoinstall/      无人值守安装模板
├── data/settings.json    配置文件（界面上改的都会存这里）
├── log/                  运行日志
└── scripts/
    ├── fetch-boot.sh     下载引导文件
    └── install.sh        一键安装 + 开机自启
```

---

## 支持的 ISO

NetBootX 靠**特征文件**识别 ISO 类型，不是靠文件名。已覆盖：

**Linux**
- Ubuntu（Desktop / Server / Live Server）→ `casper/vmlinuz`
- Debian（Live / 安装盘）→ `live/vmlinuz`、`install.amd/vmlinuz`
- RHEL / CentOS / Rocky / AlmaLinux / openEuler / Kylin → `isolinux/vmlinuz`、`images/pxeboot/vmlinuz`
- Fedora → `images/pxeboot/vmlinuz`
- SUSE / openSUSE → `boot/x86_64/loader/linux`
- Arch Linux → `arch/boot/x86_64/vmlinuz`
- Alpine → `boot/vmlinuz-lts`
- 其他走 `/boot/vmlinuz` 或 `/isolinux/vmlinuz` 兜底

**Windows**
- Windows 7 / 8 / 10 / 11（含 Server）→ `bootmgr` + `sources/boot.wim`（wimboot 引导）
- WinPE → `sources/boot.wim` 但没有 `install.wim`

**其他**
- VMware ESXi → `vmxboot/efi/bootx64.efi`
- FreeBSD → `boot/loader`

> 放进去的 ISO 会显示「识别结果」，识别不出来的会标记为「未知」但仍然可以尝试启动。

---

## 无人值守安装

把模板放进 `user/autoinstall/`，在界面上给某个 ISO 挂上模板，启动时内核参数里会自动带上
指向模板的 URL。支持的格式：

| 发行版 | 模板类型 | 挂载参数 |
|---|---|---|
| RHEL / CentOS / Rocky | Kickstart | `inst.ks=` |
| Ubuntu / Debian | Preseed | `preseed/url=` |
| Ubuntu 20.04+ | Autoinstall (cloud-init) | `autoinstall ds=nocloud-net;s=` |
| SUSE | AutoYaST | `autoyast=` |
| Windows | AutoUnattend.xml | 注入到 WinPE 根目录 |

---

## 常见问题

**Q: 客户机 PXE 启动后卡在 TFTP timeout？**
A: 检查防火墙是否放行 67/69 UDP。macOS 上在「系统设置 → 网络 → 防火墙」放行。

**Q: 客户机拿到 IP 但没有引导菜单？**
A: 说明 DHCP 通了但 TFTP/HTTP 不通。检查 `boot/` 目录文件是否齐全（跑 `./scripts/fetch-boot.sh`）。

**Q: 局域网里有两个 DHCP 服务器？**
A: 把 NetBootX 切到 `proxy` 模式，它就不会分配 IP 了。

**Q: 能否和 iVentoy 装在同一台机器？**
A: 可以，只要端口不冲突。NetBootX 默认用 16000/16080，iVentoy 用 26000。
但 **DHCP 67 端口只有一个能占**，两者不能同时以 full/proxy 模式运行。

**Q: 支持 Secure Boot 吗？**
A: 支持，但需要签名的 iPXE。仓库里的 `boot/x86_64/shimx64.efi` 是 shim，
配合 `boot/x86_64/ipxe.efi` 可以走 Secure Boot 链路，需要额外配置 MOK。

**Q: ARM64 机器能用吗？**
A: 服务端支持 macOS ARM64 / Linux ARM64。客户端支持 ARM64 UEFI（鲲鹏、飞腾、树莓派 4/5）。

---

## 开发

```bash
make test        # 全部单元测试（278 个）
make test-verbose
make check       # vet + test + build
make run-nopriv  # 用高位端口跑，不需要 sudo（本机调试用）

./scripts/verify.sh        # 一键自检：目录/引导文件/编译/测试/连通性
./scripts/package.sh       # 打多平台发布包到 dist/
```

### 代码结构

| 包 | 职责 | 行数 |
|---|---|---|
| `internal/model` | 全系统共享数据结构 | ~290 |
| `internal/dhcp` | DHCP 服务器（full/proxy/external 三模式，手工编解码） | ~1,900 |
| `internal/tftp` | TFTP 服务器（RFC 2347/2348/2349/7440） | ~900 |
| `internal/iso` | ISO9660 + Joliet + Rock Ridge + El Torito + 系统识别 | ~2,400 |
| `internal/boot` | iPXE 脚本生成（kernel/wimboot/memdisk/sanboot） | ~900 |
| `internal/httpd` | HTTP 服务（Range 流式读 ISO 内部文件） | ~700 |
| `internal/store` | 配置持久化、客户机表、事件环形缓冲 | ~500 |
| `internal/pxe` | 编排器：把上面全部接起来 | ~800 |
| `internal/web` | Web 管理界面（REST + SSE + 内嵌前端） | ~1,100 |

**零外部依赖**：只用 Go 标准库，`go.mod` 没有 require。

### 文档

- [ARCHITECTURE.md](ARCHITECTURE.md) —— 模块划分与接口契约
- [docs/使用手册.md](docs/使用手册.md) —— 面向用户的完整教程
- [docs/iPXE脚本参考.md](docs/iPXE脚本参考.md) —— 生成的脚本长什么样、为什么
- [docs/验收报告.md](docs/验收报告.md) —— 测试结果与已知限制

---

## 许可证

MIT
