package dhcp

import (
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/wupengcheng/netbootx/internal/model"
)

// 工作模式。
const (
	// ModeFull 是完整 DHCP 模式：本机分配 IP 地址。
	ModeFull = "full"
	// ModeProxy 是 proxyDHCP 模式：只回答 PXE 相关选项，不分配 IP。
	ModeProxy = "proxy"
	// ModeExternal 是外部 DHCP 模式：本机不监听 DHCP 端口，只提供引导文件查询。
	ModeExternal = "external"
)

// Config 是 DHCP 服务器的配置，由 pxe 编排器填充。
type Config struct {
	Mode       string   // "full" | "proxy" | "external"
	ServerIP   string   // 本机对客户机通告的 IP
	RangeStart string   // IP 池起始（full 模式必填）
	RangeEnd   string   // IP 池结束（full 模式必填）
	Netmask    string   // 子网掩码
	Gateway    string   // 网关
	DNS        []string // DNS 服务器
	Domain     string   // 域名
	LeaseTime  int      // 租约时间（秒）
	Port       int      // 监听端口，默认 67；测试时用高位端口
	// BindIP 仅为兼容旧配置保留：DHCP 必须监听 0.0.0.0（绑具体 IP 既收不到
	// 广播的 DISCOVER，也无法向 255.255.255.255 回包），Start 会忽略此字段。
	// 「对外通告哪个 IP」由 ServerIP 决定。
	BindIP      string
	BootFileFor func(arch model.Arch) string // 上层注入：该架构下发什么引导文件
	OnEvent     func(model.Event)            // 事件回调
	OnClient    func(c *model.Client)        // 每收到一个 DHCP 报文时回调
}

// normalize 校验并补全配置默认值。
func (c *Config) normalize() error {
	switch c.Mode {
	case "":
		c.Mode = ModeProxy
	case ModeFull, ModeProxy, ModeExternal:
	default:
		return fmt.Errorf("dhcp: 未知工作模式 %q（可选 full / proxy / external）", c.Mode)
	}
	if strings.TrimSpace(c.ServerIP) == "" {
		return fmt.Errorf("dhcp: ServerIP 不能为空")
	}
	ip := net.ParseIP(c.ServerIP)
	if ip == nil || ip.To4() == nil {
		return fmt.Errorf("dhcp: ServerIP %q 不是合法的 IPv4 地址", c.ServerIP)
	}
	c.ServerIP = ip.To4().String()

	if c.Port == 0 {
		c.Port = serverPort
	}
	if c.Port < 0 || c.Port > 65535 {
		return fmt.Errorf("dhcp: 端口 %d 非法", c.Port)
	}
	if c.LeaseTime <= 0 {
		c.LeaseTime = 3600
	}
	if c.Netmask == "" {
		c.Netmask = "255.255.255.0"
	}
	if _, err := parseMask(c.Netmask); err != nil {
		return err
	}
	if c.Gateway != "" {
		if g := net.ParseIP(c.Gateway); g == nil || g.To4() == nil {
			return fmt.Errorf("dhcp: 网关 %q 不是合法的 IPv4 地址", c.Gateway)
		}
	}
	for _, d := range c.DNS {
		if d == "" {
			continue
		}
		if x := net.ParseIP(d); x == nil || x.To4() == nil {
			return fmt.Errorf("dhcp: DNS %q 不是合法的 IPv4 地址", d)
		}
	}
	if c.BindIP != "" {
		if x := net.ParseIP(c.BindIP); x == nil || x.To4() == nil {
			return fmt.Errorf("dhcp: BindIP %q 不是合法的 IPv4 地址", c.BindIP)
		}
	}
	if c.Mode == ModeFull {
		if c.RangeStart == "" || c.RangeEnd == "" {
			return fmt.Errorf("dhcp: full 模式必须配置 IP 池（RangeStart / RangeEnd）")
		}
		s, err := parseIPv4(c.RangeStart)
		if err != nil {
			return fmt.Errorf("dhcp: RangeStart 非法: %w", err)
		}
		e, err := parseIPv4(c.RangeEnd)
		if err != nil {
			return fmt.Errorf("dhcp: RangeEnd 非法: %w", err)
		}
		sv, _ := ipToUint32(s)
		ev, _ := ipToUint32(e)
		if sv > ev {
			return fmt.Errorf("dhcp: IP 池起始 %v 大于结束 %v", s, e)
		}
		if srv, _ := ipToUint32(net.ParseIP(c.ServerIP)); srv >= sv && srv <= ev {
			return fmt.Errorf("dhcp: 服务器 IP %v 落在 IP 池 %v-%v 内，请调整 IP 池", c.ServerIP, s, e)
		}
		c.RangeStart = s.String()
		c.RangeEnd = e.String()
	}
	return nil
}

// parseIPv4 解析必须是 IPv4 的地址。
func parseIPv4(s string) (net.IP, error) {
	ip := net.ParseIP(strings.TrimSpace(s))
	if ip == nil || ip.To4() == nil {
		return nil, fmt.Errorf("%q 不是合法的 IPv4 地址", s)
	}
	return ip.To4(), nil
}

// parseMask 解析点分十进制子网掩码。
func parseMask(s string) (net.IPMask, error) {
	ip := net.ParseIP(strings.TrimSpace(s))
	if ip == nil || ip.To4() == nil {
		return nil, fmt.Errorf("dhcp: 子网掩码 %q 不是合法的 IPv4 地址", s)
	}
	m := net.IPMask(ip.To4())
	ones, bits := m.Size()
	if bits == 0 || ones == 0 {
		return nil, fmt.Errorf("dhcp: 子网掩码 %q 不连续，无法使用", s)
	}
	return m, nil
}

// leaseDuration 返回租约时长。
func (c *Config) leaseDuration() time.Duration {
	if c.LeaseTime <= 0 {
		return time.Hour
	}
	return time.Duration(c.LeaseTime) * time.Second
}
