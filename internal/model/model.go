// Package model 定义 NetBootX 全系统共享的数据结构。
// 所有子模块（dhcp / tftp / httpd / iso / boot / web）都只依赖本包的类型，
// 避免模块之间互相 import。
package model

import (
	"strings"
	"time"
)

// Arch 是客户机的固件/CPU 架构，由 DHCP option 93（Client System Architecture）推导。
type Arch string

const (
	ArchBIOS        Arch = "bios"        // x86 Legacy BIOS (PXEClient:Arch:00000)
	ArchUEFI32      Arch = "uefi32"      // IA32 UEFI        (00006)
	ArchUEFI64      Arch = "uefi64"      // x86_64 UEFI      (00007)
	ArchUEFI64HTTP  Arch = "uefi64http"  // x86_64 UEFI HTTPBoot (00016)
	ArchARM64       Arch = "arm64"       // ARM64 UEFI       (00011)
	ArchARM64HTTP   Arch = "arm64http"   // ARM64 UEFI HTTPBoot (00019)
	ArchLoongArch64 Arch = "loongarch64" // LoongArch64 UEFI (00027)
	ArchUnknown     Arch = "unknown"
)

// IsUEFI 报告该架构是否为 UEFI 固件。
func (a Arch) IsUEFI() bool {
	switch a {
	case ArchUEFI32, ArchUEFI64, ArchUEFI64HTTP, ArchARM64, ArchARM64HTTP, ArchLoongArch64:
		return true
	}
	return false
}

// IsHTTPBoot 报告该架构是否走 UEFI HTTPBoot（不需要 TFTP）。
func (a Arch) IsHTTPBoot() bool {
	return a == ArchUEFI64HTTP || a == ArchARM64HTTP
}

// String 返回给界面显示的中文名。
func (a Arch) String() string {
	switch a {
	case ArchBIOS:
		return "Legacy BIOS (x86)"
	case ArchUEFI32:
		return "UEFI IA32"
	case ArchUEFI64:
		return "UEFI x86_64"
	case ArchUEFI64HTTP:
		return "UEFI x86_64 HTTPBoot"
	case ArchARM64:
		return "UEFI ARM64"
	case ArchARM64HTTP:
		return "UEFI ARM64 HTTPBoot"
	case ArchLoongArch64:
		return "UEFI LoongArch64"
	}
	return "未知架构"
}

// ArchFromOption93 把 DHCP option 93 的原始值映射成 Arch。
func ArchFromOption93(v uint16) Arch {
	switch v {
	case 0:
		return ArchBIOS
	case 6:
		return ArchUEFI32
	case 7:
		return ArchUEFI64
	case 16:
		return ArchUEFI64HTTP
	case 11:
		return ArchARM64
	case 19:
		return ArchARM64HTTP
	case 27:
		return ArchLoongArch64
	}
	return ArchUnknown
}

// ClientState 是客户机的启动阶段，用于界面进度条。
type ClientState string

const (
	StateDHCP   ClientState = "dhcp"   // 已发 DHCP，等待引导
	StateTFTP   ClientState = "tftp"   // 正在下载引导文件
	StateMenu   ClientState = "menu"   // 已进入启动菜单
	StateBooting ClientState = "booting" // 已选择 ISO，正在加载内核
	StateInstall ClientState = "install" // 安装中（自动安装脚本已下发）
	StateFailed  ClientState = "failed"
)

// Client 是一台通过 PXE 启动的客户机。
type Client struct {
	MAC       string      `json:"mac"`
	IP        string      `json:"ip"`
	Hostname  string      `json:"hostname"`
	Arch      Arch        `json:"arch"`
	Vendor    string      `json:"vendor"`   // 网卡厂商（由 MAC OUI 推导）
	Product   string      `json:"product"`  // SMBIOS 产品名（iPXE 上报）
	Serial    string      `json:"serial"`   // SMBIOS 序列号
	UUID      string      `json:"uuid"`     // SMBIOS UUID
	State     ClientState `json:"state"`
	Selected  string      `json:"selected"` // 选中的 ISO ID
	BootFile  string      `json:"bootFile"` // 下发的引导文件名
	BootCount int         `json:"bootCount"`
	FirstSeen time.Time   `json:"firstSeen"`
	LastSeen  time.Time   `json:"lastSeen"`
	Note      string      `json:"note"`
}

// ISOFamily 是 ISO 的大类，决定引导方式和自动安装模板。
type ISOFamily string

const (
	FamilyWindows ISOFamily = "windows" // Windows 安装盘（bootmgr + install.wim）
	FamilyWinPE   ISOFamily = "winpe"   // WinPE
	FamilyLinux   ISOFamily = "linux"   // 通用 Linux Live / 安装盘
	FamilyESXi    ISOFamily = "esxi"    // VMware ESXi
	FamilyBSD     ISOFamily = "bsd"     // FreeBSD 等
	FamilyOther   ISOFamily = "other"
)

// ISO 是一个可网络引导的光盘镜像。
type ISO struct {
	ID       string    `json:"id"`       // 稳定的短 ID（由相对路径 hash 得到）
	Path     string    `json:"path"`     // 绝对路径
	RelPath  string    `json:"relPath"`  // 相对 iso 根目录的路径
	Name     string    `json:"name"`     // 文件名
	Size     int64     `json:"size"`
	ModTime  time.Time `json:"modTime"`
	Family   ISOFamily `json:"family"`
	OSName   string    `json:"osName"`   // 如 "Ubuntu 24.04 LTS"
	OSArch   Arch      `json:"osArch"`   // 镜像支持的架构
	Bootable bool      `json:"bootable"`
	Error    string    `json:"error,omitempty"`
	// Boot 描述如何从这个 ISO 引导。
	Boot BootSpec `json:"boot"`
}

// BootSpec 是「怎么把这个 ISO 跑起来」的完整描述。
// 由 iso 包探测生成，由 boot 包消费生成 iPXE 脚本。
type BootSpec struct {
	// Method 决定 iPXE 的引导命令序列。
	//   kernel   —— Linux：直接 http 拉 kernel + initrd，再拼 cmdline
	//   wimboot  —— Windows/WinPE：wimboot 从 ISO 里取 bootmgr/bcd/boot.sdi/boot.wim
	//   memdisk  —— 小镜像整体载入内存
	//   sanboot  —— 直接 http 挂载 ISO（部分 Linux Live 支持）
	Method string `json:"method"`
	// Kernel / Initrd 是 ISO 内的路径（kernel 方式）。
	Kernel string `json:"kernel,omitempty"`
	Initrd []string `json:"initrd,omitempty"`
	// KernelArgs 是发行版相关的默认内核参数模板，可含 {iso} {server} 等占位符。
	KernelArgs []string `json:"kernelArgs,omitempty"`
	// WIM 是 wimboot 方式需要的文件（ISO 内路径）。
	BootMGR string `json:"bootmgr,omitempty"`
	BCD     string `json:"bcd,omitempty"`
	BootSDI string `json:"bootsdi,omitempty"`
	BootWIM string `json:"bootwim,omitempty"`
	// AutoInstall 是自动安装的元信息（可空）。
	AutoInstall *AutoInstall `json:"autoInstall,omitempty"`
}

// AutoInstall 描述一个发行版的无人值守安装方式。
type AutoInstall struct {
	Kind string `json:"kind"` // kickstart | preseed | autoyast | autounattend | cloudinit | autoinstall
	// Template 是模板文件名（相对 user/autoinstall/）。
	Template string `json:"template,omitempty"`
	// KernelArgs 是追加到内核命令行的自动安装参数（含占位符）。
	KernelArgs []string `json:"kernelArgs,omitempty"`
}

// Settings 是持久化的服务端配置。
type Settings struct {
	// 网络
	ServerIP   string `json:"serverIP"`   // PXE 服务对外通告的 IP
	Interface  string `json:"interface"`  // 绑定的网卡名（空=自动）
	RangeStart string `json:"rangeStart"`
	RangeEnd   string `json:"rangeEnd"`
	Netmask    string `json:"netmask"`
	Gateway    string `json:"gateway"`
	DNS        []string `json:"dns"`
	Domain     string `json:"domain"`
	LeaseTime  int      `json:"leaseTime"` // 秒

	// DHCP 工作模式
	//   full    —— 本机就是唯一 DHCP 服务器
	//   proxy   —— 局域已有 DHCP，本机只做 proxyDHCP（PXE 专用）
	//   external—— 完全由外部 DHCP 通告 next-server，本机只提供 TFTP/HTTP
	DHCPMode string `json:"dhcpMode"`
	DHCPPort int    `json:"dhcpPort"` // 默认 67

	// 服务端口
	HTTPPort int `json:"httpPort"` // 默认 16000（iVentoy 用 26000，避开它）
	TFTPPort int `json:"tftpPort"` // 默认 69
	WebPort  int `json:"webPort"`  // 默认 16080，UI

	// 路径
	ISODir  string `json:"isoDir"`
	UserDir string `json:"userDir"`
	DataDir string `json:"dataDir"`
	LogDir  string `json:"logDir"`

	// 引导
	BootMenuTimeout int    `json:"bootMenuTimeout"` // 秒
	DefaultISO      string `json:"defaultISO"`
	TFTPBlockSize   int    `json:"tftpBlockSize"`
	SecureBoot      bool   `json:"secureBoot"`
	AutoStart       bool   `json:"autoStart"`
	Language        string `json:"language"`
}

// DefaultSettings 返回一套开箱可用的默认配置。
func DefaultSettings() Settings {
	return Settings{
		RangeStart:      "192.168.123.200",
		RangeEnd:        "192.168.123.250",
		Netmask:         "255.255.255.0",
		Gateway:         "192.168.123.1",
		DNS:             []string{"223.5.5.5", "114.114.114.114"},
		Domain:          "netbootx.lan",
		LeaseTime:       3600,
		DHCPMode:        "proxy",
		DHCPPort:        67,
		HTTPPort:        16000,
		TFTPPort:        69,
		WebPort:         16080,
		ISODir:          "iso",
		UserDir:         "user",
		DataDir:         "data",
		LogDir:          "log",
		BootMenuTimeout: 30,
		TFTPBlockSize:   1468,
		Language:        "zh-CN",
	}
}

// Event 是一条运行事件，用于界面的日志面板。
type Event struct {
	Time  time.Time `json:"time"`
	Level string    `json:"level"` // info | warn | error
	MAC   string    `json:"mac,omitempty"`
	Kind  string    `json:"kind"`  // dhcp | tftp | http | boot | system
	Msg   string    `json:"msg"`
}

// ServerStatus 是推送给 Web UI 的实时状态。
type ServerStatus struct {
	Running     bool      `json:"running"`
	ServerIP    string    `json:"serverIP"`
	DHCPMode    string    `json:"dhcpMode"`
	StartedAt   time.Time `json:"startedAt"`
	UptimeSec   int64     `json:"uptimeSec"`
	ClientCount int       `json:"clientCount"`
	ISOCount    int       `json:"isoCount"`
	HTTPPort    int       `json:"httpPort"`
	TFTPPort    int       `json:"tftpPort"`
	WebPort     int       `json:"webPort"`
	Version     string    `json:"version"`
	Hostname    string    `json:"hostname"`
	OS          string    `json:"os"`
	Arch        string    `json:"arch"`
	BootFileBIOS string   `json:"bootFileBIOS"`
	BootFileUEFI string   `json:"bootFileUEFI"`
}

// NormalizeMAC 把各种写法统一成小写冒号分隔。
func NormalizeMAC(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.ReplaceAll(s, "-", ":")
	var parts []string
	for _, p := range strings.Split(s, ":") {
		p = strings.TrimLeft(p, "0")
		if p == "" {
			p = "0"
		}
		parts = append(parts, p)
	}
	if len(parts) != 6 {
		return strings.ToLower(strings.TrimSpace(s))
	}
	// 重新补零到两位
	for i, p := range parts {
		if len(p) == 1 {
			parts[i] = "0" + p
		}
	}
	return strings.Join(parts, ":")
}
