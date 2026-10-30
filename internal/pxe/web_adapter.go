package pxe

import (
	"net"
	"github.com/wupengcheng/netbootx/internal/web"
)

// webController 把 *Server 适配成 web.Controller。
//
// 为什么需要这一层：web 包定义了它自己需要的 NetInterface 类型（字段更贴合前端），
// 而 pxe 包内的 NetInterface 是给命令行和自检用的。两者字段不同，无法直接复用，
// 所以在边界上做一次转换，而不是让 web 包反向依赖 pxe 的类型。
//
// 同时补齐两个可选扩展：
//   - ClearClients() error  → 让界面的「清空客户机」按钮真正生效
//   - Preview(isoID, mac)   → 让界面预览到的脚本和实际下发完全一致
type webController struct {
	*Server
}

// Interfaces 返回本机网卡列表（web 包的类型）。
func (c webController) Interfaces() []web.NetInterface {
	src := Interfaces()
	out := make([]web.NetInterface, 0, len(src))
	for _, n := range src {
		ip, mask := "", n.Netmask
		if len(n.IPs) > 0 {
			ip = n.IPs[0]
		}
		ni := web.NetInterface{
			Name: n.Name,
			IP:   ip,
			Mask: mask,
			Up:   n.Up,
			// 回环网卡标记为不可选（前端据此置灰）
			Default: !n.Loopback && ip != "",
		}
		out = append(out, ni)
	}
	return out
}

// ClearClients 实现 web.ClientClearer，让 DELETE /api/clients 真正清空客户机表。
func (c webController) ClearClients() error {
	c.Server.ClearClients()
	return nil
}

// Preview 实现 web.Previewer：返回与真实下发一致的 iPXE 脚本。
func (c webController) Preview(isoID, mac string) (string, error) {
	if mac == "" {
		mac = "00:00:00:00:00:00"
	}
	return c.Server.ScriptFor(mac, isoID)
}

// asController 是内部便捷函数，把 *Server 包成 web.Controller。
func asController(s *Server) web.Controller { return webController{s} }

// 编译期断言：webController 必须满足 web.Controller 及其可选扩展。
var (
	_ web.Controller   = webController{}
	_ web.ClientClearer = webController{}
	_ web.Previewer    = webController{}
)

// hostIP 返回某网卡名对应的第一个 IPv4 地址，供界面默认值使用。
func hostIP(name string) string {
	ip, err := LocalIPForInterface(name)
	if err != nil {
		return ""
	}
	return ip
}

// ensure net 包被使用（Default 字段计算里用到 IP 比较时可能引入）
var _ = net.ParseIP
