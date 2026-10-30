# NetBootX 架构说明（所有模块必须遵守）

NetBootX 是 iVentoy 的开源实现：一个 **增强版 PXE / HTTPBoot 服务器**。
把一个 ISO 文件丢进 `iso/` 目录，客户机通过网络（PXE 或 UEFI HTTPBoot）就能启动它，
支持多台机器同时启动、自动识别操作系统、自动安装。

服务端跨平台：macOS / Linux / Windows，单二进制，无运行时依赖。

---

## 一、总体数据流

```
                      ┌──────────────────── NetBootX 服务端 ────────────────────┐
                      │                                                          │
  客户机开机           │   ┌──────────┐   ┌──────────┐   ┌──────────┐            │
  ├─ DHCP DISCOVER ──►│──►│  dhcp    │   │  tftp    │   │  httpd   │            │
  │                   │   │ :67      │   │ :69      │   │ :16000   │            │
  │◄─ DHCP OFFER ─────│───│ 分配IP   │   │ 发引导文件│   │ 发脚本/ISO│            │
  │   (next-server,   │   │ 通告引导  │   │ iPXE/EFI │   │ Range支持 │            │
  │    bootfile)      │   └──────────┘   └──────────┘   └──────────┘            │
  │                   │         │              │              │                  │
  │◄─ TFTP RRQ ───────│─────────┼─────────────►│              │                  │
  │   得到 ipxe.efi   │         │              │              │                  │
  │                   │         │              │              │                  │
  │◄─ HTTP GET ───────│─────────┼──────────────┼─────────────►│                  │
  │   /boot/menu.ipxe │         │              │              │                  │
  │   按 MAC 生成菜单  │         │              │              │                  │
  │                   │         │              │              │                  │
  │◄─ HTTP GET kernel │─────────┼──────────────┼─────────────►│  从 ISO 里流式读   │
  │   /iso/{id}/path  │         │              │              │  不落地、不挂载     │
  └───────────────────┴─────────┴──────────────┴──────────────┴──────────────────┘
```

**核心设计**：ISO 文件**从不挂载、从不解压**。所有引导文件都通过 HTTP 从 ISO 内部
按字节偏移流式读取（ISO9660 是随机访问格式，天然支持 HTTP Range）。

---

## 二、目录结构与模块职责（写入范围严格划分）

```
netbootx/
├── go.mod                      module netbootx, go 1.27, 零外部依赖
├── ARCHITECTURE.md             本文件
├── main.go                     [Lead] 入口：命令行解析、组装、启动
├── internal/
│   ├── model/model.go          [Lead] 全系统共享数据结构（已完成，不要改）
│   ├── store/                  [Lead] 配置持久化、客户端表、事件环形缓冲
│   ├── pxe/                    [Lead] 编排器：把下面所有组件接起来
│   ├── dhcp/                   [netstack] DHCP 服务器（full/proxy/external）
│   ├── tftp/                   [netstack] TFTP 服务器（RFC1350 + 选项协商）
│   ├── iso/                    [isoprobe] ISO9660 解析 + 操作系统识别
│   ├── boot/                   [bootmenu] iPXE 脚本生成 + 内核命令行拼装
│   ├── httpd/                  [bootmenu] HTTP 文件服务器（Range + 动态路由）
│   └── web/                    [webui] Web 管理界面（API + 内嵌前端）
├── boot/                       [Lead] 内嵌引导文件（ipxe.efi / undionly.kpxe / wimboot）
├── iso/                        用户放 ISO 的地方
├── user/                       自动安装模板等用户文件
├── data/                       运行时数据
└── log/                        日志
```

**写入范围是硬边界**：只写自己负责的目录，绝不修改别人的文件。
需要改别人的接口 → 通过 send_message 告诉 Lead，由 Lead 协调。

---

## 三、模块接口契约（必须严格实现）

### 3.1 internal/dhcp

```go
package dhcp

type Config struct {
    Mode       string        // "full" | "proxy" | "external"
    ServerIP   string        // 本机对客户机通告的 IP
    RangeStart string
    RangeEnd   string
    Netmask    string
    Gateway    string
    DNS        []string
    Domain     string
    LeaseTime  int           // 秒
    Port       int           // 默认 67；测试时用高位端口
    BindIP     string        // 绑定的本机 IP，空表示 0.0.0.0
    BootFileFor func(arch model.Arch) string // 上层注入：该架构该下发什么引导文件
    OnEvent    func(model.Event)
    OnClient   func(c *model.Client)          // 每次收到 DHCP 报文时回调
}

func New(cfg Config) (*Server, error)
func (s *Server) Start() error   // 绑定端口 + 启动收包循环（非阻塞）
func (s *Server) Stop() error    // 幂等
func (s *Server) Leases() []Lease
func (s *Server) Stats() Stats

type Lease struct {
    MAC      string
    IP       string
    Arch     model.Arch
    Hostname string
    Expires  time.Time
    State    string // offered | bound
}
```

要求：
- 手工实现 DHCP 报文编解码（BOOTP 固定头 + options TLV），**不要引入外部库**。
- 支持 option 53 (msg type)、55 (param request list)、60 (vendor class)、61 (client id)、
  93 (client arch)、97 (client UUID)、12 (hostname)、57 (max msg size)、82 (relay agent)。
- 响应里必须包含：53、54 (server id)、51 (lease time)、1 (netmask)、3 (router)、6 (DNS)、
  15 (domain)、**66 (TFTP server name)**、**67 (bootfile name)**，以及 PXE 需要的
  43 (vendor specific: PXE discover/offer，含 MTFTP 提示)、60 = "PXEClient"。
- **proxy 模式**：不分配 IP（yiaddr=0，option 53 用 OFFER/ACK 但只回 PXE 相关选项，
  option 54 是本机 IP，不带 option 1/3/6/51）。必须同时响应 DISCOVER 和 REQUEST。
- **full 模式**：完整 DHCP，分配 IP，维护租约表，处理 REQUEST/RELEASE/DECLINE/INFORM。
- 必须处理 **DHCP Relay**（giaddr 非 0 时单播回 giaddr:67）。
- 广播回包要设置 SO_BROADCAST。
- 租约表并发安全（sync.RWMutex）。
- 端口被占用时返回清晰错误（提示需要 sudo 或端口冲突）。

### 3.2 internal/tftp

```go
package tftp

type Config struct {
    Root      string // 文件根目录
    Addr      string // ":69"
    BlockSize int    // 协商上限，默认 1468
    OnEvent   func(model.Event)
}

func New(cfg Config) (*Server, error)
func (s *Server) Start() error
func (s *Server) Stop() error
func (s *Server) Stats() Stats
```

要求：
- 只支持 RRQ（读），不支持写（WRQ 回 ERROR 2 access violation）。
- 支持 RFC 2347/2348/2349 选项协商：`blksize`、`tsize`、`timeout`。
- 支持 RFC 7440 `windowsize`（可选加分项）。
- 每个请求用新的临时端口（ephemeral）回包，主 socket 只收 RRQ。
- 路径穿越防护：`..` 必须拒绝。
- 文件不存在回 ERROR 1，权限不足回 ERROR 2。
- 超时重传（默认 3 次，每次 3 秒）。
- 记录每个传输的字节数/耗时到 Stats。

### 3.3 internal/iso

```go
package iso

// Probe 读一个 ISO 文件，识别它的类型并生成引导描述。
func Probe(path string) (*model.ISO, error)

// Scan 扫描目录下所有 *.iso（递归），并发探测。
func Scan(dir string) ([]model.ISO, error)
```

要求（这是整个项目技术含量最高的部分）：
- 纯 Go 实现 **ISO9660** 读取：Primary Volume Descriptor、目录记录、Joliet SVD (UCS-2)、
  Rock Ridge (NM 扩展) 文件名解码。不需要解压，只按需 seek 读扇区。
- 读 **El Torito Boot Catalog**（Boot Record Volume Descriptor, type 0），得到引导映像，
  用于判断是否可引导、是 BIOS 还是 UEFI 引导。
- **操作系统识别**（按优先级匹配 ISO 根目录特征文件/目录）：
  - Windows: `sources/boot.wim` + `bootmgr` → FamilyWindows；读 `sources/install.wim`
    或 `sources/install.esd` 存在性；识别 WinPE（`sources/boot.wim` 但无 install.wim）。
  - Linux: 找 `casper/vmlinuz`+`casper/initrd`（Ubuntu）、`live/vmlinuz`（Debian Live）、
    `isolinux/vmlinuz`+`isolinux/initrd.img`（CentOS/RHEL/openEuler/Kylin）、
    `images/pxeboot/vmlinuz`+`images/pxeboot/initrd.img`（RHEL/Fedora 安装盘）、
    `boot/vmlinuz`、`arch/boot/x86_64/vmlinuz`（Arch）、`boot/x86_64/loader/linux`（SUSE）、
    `boot/Image`+`boot/initrd`（arm64）。
  - ESXi: `vmxboot/efi/bootx64.efi` 或 `boot.cfg` + `tboot.b00` → FamilyESXi。
  - BSD: `boot/loader` + `etc/rc` → FamilyBSD。
  - 都不是 → FamilyOther。
- 填 `OSName`（如 "Ubuntu 24.04.2 LTS"、"Rocky Linux 9.4"），从 `.disk/info`、
  `README.diskdefines`、`etc/os-release`、`sources/boot.wim` 同级的 `sources/setup.exe` 等
  线索里提取，尽力而为，失败就用文件名。
- 填 `OSArch`：x86_64 / arm64（由是否存在 `boot/Image` 或 `arch/boot/aarch64` 判断）。
- 填 `BootSpec`：Method 为 `kernel` / `wimboot` / `sanboot`；
  Kernel / Initrd 用 **ISO 内绝对路径**（如 `/casper/vmlinuz`）；
  KernelArgs 给出发行版默认参数（如 Ubuntu Live 需要 `boot=casper iso-scan/filename=...`，
  Debian Live 需要 `boot=live`，RHEL 安装盘需要 `inst.stage2=` 等）。
- 必须容错：任何解析失败都不能 panic，返回带 `Error` 字段的 ISO 对象。
- 性能：Scan 要并发（errgroup 风格，但不用外部库），1 万个文件不能卡死。

### 3.4 internal/boot

```go
package boot

// Script 生成某台客户机的 iPXE 启动脚本。
func Script(c *model.Client, isos []model.ISO, s model.Settings, baseURL string) string

// Entry 生成启动菜单里的一个条目（给 Web UI 预览用）。
func Entry(c *model.Client, is model.ISO, s model.Settings, baseURL string) string
```

要求：
- 生成标准 iPXE 脚本（`#!ipxe` 开头）。
- 菜单要**按客户机架构过滤** ISO：x86_64 客户机不显示 arm64 镜像，反之亦然。
- 菜单项：序号 + 操作系统名 + 文件名 + 大小。
- 支持默认超时（`s.BootMenuTimeout`）自动启动默认项。
- 支持子菜单（按操作系统家族分组：Windows / Linux / WinPE / 其他）。
- 内核引导用 `kernel http://{server}:{port}/iso/{id}{path} {args}` + `initrd ...` + `boot`。
- Windows 用 wimboot：
  ```
  kernel wimboot
  initrd --name bootmgr http://.../iso/{id}/bootmgr
  initrd --name bcd     http://.../iso/{id}/boot/bcd
  initrd --name boot.sdi http://.../iso/{id}/boot/boot.sdi
  initrd --name boot.wim http://.../iso/{id}/sources/boot.wim
  boot
  ```
- 支持自动安装：如果 `is.Boot.AutoInstall` 非空，把模板 URL 追加进内核参数。
- 生成的脚本必须能被 iPXE 正确解析（注意转义、不要有裸空格、URL 编码）。
- 纯函数，不碰网络，方便单测。

### 3.5 internal/httpd

```go
package httpd

type Config struct {
    Addr       string
    ISODir     string
    UserDir    string
    ScriptFor  func(mac, isoID string) (string, error) // 注入：动态生成 iPXE 脚本
    ISOs       func() []model.ISO                     // 注入：当前 ISO 列表
    OnEvent    func(model.Event)
    OnClient   func(mac, ip, path string)
}

func New(cfg Config) (*Server, error)
func (s *Server) Start() error
func (s *Server) Stop() error
func (s *Server) Handler() http.Handler   // 方便单测
```

路由：
- `GET /boot/menu.ipxe`      → 按 `?mac=` 生成 iPXE 菜单（`Content-Type: text/plain`）
- `GET /boot/{file}`         → 发引导文件（ipxe.efi / undionly.kpxe / wimboot / 其它）
- `GET /iso/{id}/{path...}`  → **从 ISO 内部按偏移流式读取文件**，必须支持 Range
- `GET /user/{path...}`      → 自动安装模板等用户文件
- `GET /healthz`             → 健康检查

要求：
- `/iso/{id}/{path}` 是性能关键：不能把整个 ISO 读进内存。用 `io.SectionReader` 包住
  `*os.File` 做 seek + `http.ServeContent` 拿到 Range/ETag/If-Modified-Since 全套。
- 要缓存 ISO 的目录树（打开一次、复用 `*iso.Reader`），按 ISO 的 mtime 失效。
- 路径穿越防护。
- 访问日志写进 OnEvent（采样，别刷爆）。
- 支持 `--insecure` 之外的 CORS 不需要；但要有 `Accept-Ranges: bytes`。

### 3.6 internal/web

```go
package web

type Config struct {
    Addr     string
    Store    *store.Store
    PXE      Controller   // 见下
    Version  string
}

// Controller 是 web 层控制 PXE 服务的接口，由 pxe 包实现。
type Controller interface {
    Status() model.ServerStatus
    Start() error
    Stop() error
    Restart() error
    RescanISOs() (int, error)
    ISOs() []model.ISO
    Clients() []model.Client
    Events(limit int) []model.Event
    Settings() model.Settings
    SaveSettings(model.Settings) error
    Interfaces() []NetInterface
    DefaultISO() string
    SetDefaultISO(id string) error
}

func New(cfg Config) (*Server, error)
func (s *Server) Start() error
func (s *Server) Stop() error
func (s *Server) Handler() http.Handler
```

REST API（JSON，统一 `{"ok":bool,"data":...,"error":string}` 包装）：
- `GET  /api/status`
- `POST /api/start` / `POST /api/stop` / `POST /api/restart`
- `GET  /api/isos` / `POST /api/isos/rescan` / `POST /api/isos/upload`
- `GET  /api/clients` / `DELETE /api/clients`
- `GET  /api/events?limit=200`
- `GET  /api/settings` / `POST /api/settings`
- `GET  /api/interfaces`  → 本机网卡列表（名字 + IP + 掩码 + 是否 up）
- `GET  /api/preview/{isoID}?mac=` → 预览该 ISO 生成的 iPXE 脚本（调试神器）
- `GET  /api/version`

前端要求：**单页应用，零构建、零 CDN、内嵌进二进制**（`//go:embed`）。
- 原生 HTML + CSS + JS，不用框架。
- 中文界面，模仿 iVentoy 的布局：顶部工具栏（IP 选择 + IP 池 + 绿色启动/停止大按钮），
  左侧 ISO 列表，右侧客户机列表，底部日志面板。
- 深色/浅色都可，但要**好看**：有层次、有留白、字体清晰、状态用颜色区分。
- WebSocket 或 SSE 推送实时状态（客户机变化、日志）。用 SSE 更简单，选 SSE。
- 必须响应式，手机也能看。

---

## 四、编码约定

1. **Go 1.27，零外部依赖**（只用标准库）。测试可以用标准库 `testing`。
2. 注释和用户可见文案用**中文**；标识符用英文。
3. 每个包必须有 `doc.go` 或包注释说明用途。
4. 错误处理：用 `fmt.Errorf("xxx: %w", err)`，不要裸 `panic`（除了 main 里不可恢复的）。
5. 并发：`context.Context` 贯穿；Stop 必须幂等且不阻塞超过 2 秒。
6. 每个包必须有 `*_test.go`，覆盖核心逻辑。
7. 不要引入 `golang.org/x/...`，需要系统调用就用 `syscall`。
8. 文件名全小写，用下划线或短横线分隔。

---

## 五、验证方式

因为真实 PXE 启动需要独立物理机，本项目用三层验证：

1. **单元测试**：每个包的核心解析逻辑。
2. **协议级集成测试**（`internal/pxe/integration_test.go`）：
   - 起真实 UDP DHCP 服务器在 16767 端口，用真实 DHCP DISCOVER 报文打过去，断言 OFFER 里
     有正确的 next-server / bootfile / option 93 处理。
   - 起真实 TFTP 服务器，用 Go 写的 TFTP 客户端下载文件，校验内容一致。
   - 起真实 HTTP 服务器，用 Range 请求从 ISO 中间读一段，校验字节正确。
3. **真实 ISO 验证**：下载一个小的 Linux ISO（TinyCore/Alpine），跑 `iso.Scan` 断言识别正确。

`make test` 必须全绿。
