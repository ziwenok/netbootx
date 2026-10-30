package dhcp

import (
	"fmt"
	"net"
	"time"

	"github.com/wupengcheng/netbootx/internal/model"
)

// response 是一次待发送的回包：报文 + 目的地。
type response struct {
	pkt *Packet
	dst *net.UDPAddr
}

// pxeMenu 是可选的自定义 PXE 引导菜单（通过 SetPXEMenu 配置）。
type pxeMenu struct {
	prompt  string
	entries []PXEMenuEntry
}

// handlePacket 处理一个已解析的 DHCP 报文，返回需要发出的响应（nil 表示不回包）。
func (s *Server) handlePacket(req *Packet, src *net.UDPAddr) *response {
	mt := req.MsgType()
	mac := req.MAC()
	if mac == "" {
		s.stat.ignored.Add(1)
		return nil
	}
	arch := req.ClientArch()

	switch mt {
	case MsgDiscover:
		s.stat.discover.Add(1)
	case MsgRequest:
		s.stat.request.Add(1)
	case MsgRelease:
		s.stat.release.Add(1)
	case MsgDecline:
		s.stat.decline.Add(1)
	case MsgInform:
		s.stat.inform.Add(1)
	default:
		s.stat.ignored.Add(1)
		s.emit("warn", mac, "忽略未知 DHCP 报文类型 %d", uint8(mt))
		return nil
	}

	if s.cfg.Mode == ModeExternal {
		s.stat.ignored.Add(1)
		return nil
	}

	var rep *Packet
	var forceBroadcast bool
	switch s.cfg.Mode {
	case ModeFull:
		rep = s.handleFull(req, mt, mac, arch)
	case ModeProxy:
		rep = s.handleProxy(req, mt, mac, arch)
	}
	if rep == nil {
		s.notifyClient(req, arch, "", net.IPv4zero.To4())
		return nil
	}
	// DHCPNAK 必须广播：客户端可能还没有可用的地址（RFC 2131 §4.3.2）。
	if t, _ := rep.Options.Uint8(OptMsgType); MsgType(t) == MsgNak {
		forceBroadcast = true
	}

	bootFile := rep.File
	if bootFile == "" {
		bootFile, _ = rep.Options.String(OptBootfileName)
	}
	// 统计本机发出的应答类型。
	if t, ok := rep.Options.Uint8(OptMsgType); ok {
		switch MsgType(t) {
		case MsgOffer:
			s.stat.offer.Add(1)
		case MsgAck:
			s.stat.ack.Add(1)
		case MsgNak:
			s.stat.nak.Add(1)
		}
	}
	// 把「我们分配给它的地址」一起报给上层，客户机列表才能显示 IP。
	s.notifyClient(req, arch, bootFile, rep.YIAddr)

	dst := s.replyAddr(req, src, rep.YIAddr, forceBroadcast)
	return &response{pkt: rep, dst: dst}
}

// replyAddr 计算回包目的地，遵循 RFC 2131 §4.1 与任务约定：
//  1. giaddr 非 0 → 单播回中继代理的 67 端口；
//  2. 客户端置了 broadcast 标志 → 广播到 255.255.255.255:68；
//  3. ciaddr 非 0（续租）→ 单播到 ciaddr:68；
//  4. yiaddr 非 0 → 单播到 yiaddr:68；
//  5. 以上都不满足 → 广播（客户端此时还没有 IP）。
func (s *Server) replyAddr(req *Packet, src *net.UDPAddr, yiaddr net.IP, forceBroadcast bool) *net.UDPAddr {
	if !isZeroIP(req.GIAddr) {
		return &net.UDPAddr{IP: req.GIAddr, Port: s.relayPort}
	}
	bcast := &net.UDPAddr{IP: net.IPv4bcast, Port: s.replyPort}
	if forceBroadcast || req.Broadcast() {
		return bcast
	}
	if !isZeroIP(req.CIAddr) {
		return &net.UDPAddr{IP: req.CIAddr, Port: s.replyPort}
	}
	if !isZeroIP(yiaddr) {
		return &net.UDPAddr{IP: yiaddr, Port: s.replyPort}
	}
	if src != nil && !isZeroIP(src.IP) {
		return &net.UDPAddr{IP: src.IP, Port: s.replyPort}
	}
	return bcast
}

// newReply 按请求构造一个响应报文骨架。
func (s *Server) newReply(req *Packet) *Packet {
	rep := &Packet{
		Op:     bootReply,
		HType:  req.HType,
		HLen:   req.HLen,
		Hops:   req.Hops,
		XID:    req.XID,
		Secs:   0,
		Flags:  req.Flags,
		CIAddr: net.IPv4zero.To4(),
		YIAddr: net.IPv4zero.To4(),
		SIAddr: net.IPv4zero.To4(),
		GIAddr: req.GIAddr,
		CHAddr: req.CHAddr,
	}
	return rep
}

// setBootFile 同时填充 file 字段和 option 67（file 字段最长 127 字节）。
func setBootFile(rep *Packet, name string) {
	if name == "" {
		return
	}
	if len(name) <= 127 {
		rep.File = name
	}
	rep.Options.SetString(OptBootfileName, name)
}

// setServerOptions 填充所有模式共用的选项：54 + 66 + 60 + 43。
func (s *Server) setServerOptions(rep *Packet, req *Packet, arch model.Arch) {
	serverIP := net.ParseIP(s.cfg.ServerIP).To4()
	rep.Options.SetIP(OptServerID, serverIP)
	rep.Options.SetString(OptTFTPServerName, s.cfg.ServerIP)
	rep.Options.SetString(OptVendorClass, "PXEClient")
	if req.IsPXEClient() {
		rep.Options.Set(OptVendorSpecific, s.pxeVendorOptions(arch))
	}
	setBootFile(rep, s.BootFile(arch))
}

// pxeVendorOptions 构造 option 43 的内容。
//
// 默认的 PXE_DISCOVERY_CONTROL 关闭广播/多播引导服务器发现（见
// DefaultPXEDiscoveryControl），让 PXE ROM 直接用 option 67 里给的引导文件名，
// 不去 4011 端口找引导服务器 —— 本服务器没有实现 4011。
// 需要自定义时用 SetPXEDiscoveryControl 覆盖。
func (s *Server) pxeVendorOptions(arch model.Arch) []byte {
	s.mu.RLock()
	dc := s.pxeDiscoveryControl
	s.mu.RUnlock()
	if dc == 0 {
		dc = DefaultPXEDiscoveryControl
	}
	opt := PXEOptions{
		DiscoveryControl: dc,
		MTFTP: &MTFTPHint{
			IP:        net.ParseIP(s.cfg.ServerIP).To4(),
			BlockSize: 1456,
			Timeout:   3,
		},
	}
	s.mu.RLock()
	menu := s.pxeMenu
	bootItem := s.pxeBootItem
	s.mu.RUnlock()
	if bootItem != 0 {
		opt.HasBootItem = true
		opt.BootItem = bootItem
	}
	if menu != nil && len(menu.entries) > 0 {
		opt.MenuPrompt = menu.prompt
		opt.Menu = menu.entries
	}
	return EncodePXEVendor(opt)
}

// SetPXEMenu 设置 PXE 引导菜单（option 43 里的 PXE_BOOT_MENU 子选项）。
// 传入空列表可关闭菜单。
func (s *Server) SetPXEMenu(prompt string, entries []PXEMenuEntry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(entries) == 0 {
		s.pxeMenu = nil
		return
	}
	s.pxeMenu = &pxeMenu{prompt: prompt, entries: append([]PXEMenuEntry(nil), entries...)}
}

// SetPXEBootItem 设置 PXE 引导项号（option 43 里的 PXE_BOOT_ITEM 子选项），0 表示不设置。
func (s *Server) SetPXEBootItem(n uint16) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pxeBootItem = n
}

// SetPXEDiscoveryControl 覆盖 PXE_DISCOVERY_CONTROL（子选项 6）的值，
// 传 0 表示恢复默认值（DefaultPXEDiscoveryControl）。
func (s *Server) SetPXEDiscoveryControl(v byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pxeDiscoveryControl = v
}

// handleFull 处理 full 模式（完整 DHCP，分配 IP）下的报文。
func (s *Server) handleFull(req *Packet, mt MsgType, mac string, arch model.Arch) *Packet {
	switch mt {
	case MsgDiscover:
		return s.fullOffer(req, mac, arch)
	case MsgRequest:
		return s.fullRequest(req, mac, arch)
	case MsgDecline:
		s.fullDecline(req, mac)
		return nil
	case MsgRelease:
		s.fullRelease(req, mac)
		return nil
	case MsgInform:
		return s.fullInform(req, arch)
	}
	return nil
}

// fullOffer 响应 DISCOVER，分配一个 IP 并下发引导参数。
func (s *Server) fullOffer(req *Packet, mac string, arch model.Arch) *Packet {
	ip, err := s.table.Allocate(mac)
	if err != nil {
		s.stat.errs.Add(1)
		s.emit("error", mac, "分配 IP 失败: %v", err)
		return nil
	}
	hostname := req.Hostname()
	if err := s.table.Offered(mac, ip, arch, hostname); err != nil {
		s.stat.errs.Add(1)
		s.emit("error", mac, "登记租约失败: %v", err)
		return nil
	}
	rep := s.newReply(req)
	rep.YIAddr = ip.To4()
	rep.SIAddr = net.ParseIP(s.cfg.ServerIP).To4()
	s.fillLeaseOptions(rep, req, arch)
	rep.Options.SetUint8(OptMsgType, uint8(MsgOffer))
	s.emit("info", mac, "OFFER %v → %s（架构 %s，引导文件 %s）", ip, mac, arch, s.BootFile(arch))
	return rep
}

// fullRequest 处理 REQUEST（SELECTING / INIT-REBOOT / RENEWING / REBINDING）。
func (s *Server) fullRequest(req *Packet, mac string, arch model.Arch) *Packet {
	serverIP := net.ParseIP(s.cfg.ServerIP).To4()
	// 客户端选择了别的 DHCP 服务器：不回应，并释放我们发出的临时租约。
	if ids := req.Options.IPs(OptServerID); len(ids) > 0 && !ids[0].Equal(serverIP) {
		s.table.Release(mac)
		s.emit("info", mac, "客户端选择了其他 DHCP 服务器 %v，放弃本次分配", ids[0])
		return nil
	}
	requested := requestedIP(req)
	if requested == nil {
		s.stat.ignored.Add(1)
		s.emit("warn", mac, "REQUEST 既没有 option 50 也没有 ciaddr，忽略")
		return nil
	}
	if !s.table.InPool(requested) {
		return s.fullNak(req, mac, fmt.Sprintf("请求的地址 %v 不在本机 IP 池内", requested))
	}
	if l, ok := s.table.ByIP(requested); ok && l.MAC != mac {
		return s.fullNak(req, mac, fmt.Sprintf("地址 %v 已被 %s 占用", requested, l.MAC))
	}
	hostname := req.Hostname()
	if err := s.table.Bind(mac, requested, arch, hostname); err != nil {
		return s.fullNak(req, mac, fmt.Sprintf("绑定租约失败: %v", err))
	}
	rep := s.newReply(req)
	rep.YIAddr = requested.To4()
	rep.SIAddr = serverIP
	s.fillLeaseOptions(rep, req, arch)
	rep.Options.SetUint8(OptMsgType, uint8(MsgAck))
	s.emit("info", mac, "ACK %v → %s（架构 %s，引导文件 %s）", requested, mac, arch, s.BootFile(arch))
	return rep
}

// fullNak 回应 DHCPNAK（广播）。
func (s *Server) fullNak(req *Packet, mac, reason string) *Packet {
	rep := s.newReply(req)
	rep.YIAddr = net.IPv4zero.To4()
	rep.SIAddr = net.ParseIP(s.cfg.ServerIP).To4()
	rep.Options.SetUint8(OptMsgType, uint8(MsgNak))
	rep.Options.SetIP(OptServerID, net.ParseIP(s.cfg.ServerIP).To4())
	rep.Options.SetString(OptMessage, reason)
	rep.Options.SetString(OptVendorClass, "PXEClient")
	s.emit("warn", mac, "NAK %s：%s", mac, reason)
	return rep
}

// fullDecline 处理 DECLINE：客户端报告地址冲突，回收该地址。
func (s *Server) fullDecline(req *Packet, mac string) {
	ip := requestedIP(req)
	if ip == nil {
		return
	}
	if s.table.Delete(ip) {
		s.emit("warn", mac, "客户机报告地址冲突，回收 %v", ip)
	}
}

// fullRelease 处理 RELEASE：客户端主动释放租约。
func (s *Server) fullRelease(req *Packet, mac string) {
	if s.table.Release(mac) {
		s.emit("info", mac, "客户端释放了租约")
	}
}

// fullInform 处理 INFORM：客户端已有地址，只补发配置参数。
func (s *Server) fullInform(req *Packet, arch model.Arch) *Packet {
	rep := s.newReply(req)
	rep.YIAddr = net.IPv4zero.To4()
	rep.SIAddr = net.ParseIP(s.cfg.ServerIP).To4()
	s.setServerOptions(rep, req, arch)
	s.fillNetworkOptions(rep)
	rep.Options.SetUint8(OptMsgType, uint8(MsgAck))
	return rep
}

// fillLeaseOptions 填充 OFFER / ACK 需要的全部选项。
func (s *Server) fillLeaseOptions(rep *Packet, req *Packet, arch model.Arch) {
	lease := uint32(s.cfg.LeaseTime)
	if lease == 0 {
		lease = 3600
	}
	rep.Options.SetUint32(OptLeaseTime, lease)
	rep.Options.SetUint32(OptRenewalTime, lease/2)
	rep.Options.SetUint32(OptRebindingTime, lease*7/8)
	s.setServerOptions(rep, req, arch)
	s.fillNetworkOptions(rep)
}

// fillNetworkOptions 填充网络参数（option 1 / 3 / 6 / 15 / 28）。
func (s *Server) fillNetworkOptions(rep *Packet) {
	if m, err := parseMask(s.cfg.Netmask); err == nil {
		rep.Options.Set(OptSubnetMask, []byte(m))
		if b := broadcastOf(net.ParseIP(s.cfg.ServerIP).To4(), m); b != nil {
			rep.Options.SetIP(OptBroadcastAddr, b)
		}
	}
	if s.cfg.Gateway != "" {
		if g := net.ParseIP(s.cfg.Gateway).To4(); g != nil {
			rep.Options.SetIP(OptRouter, g)
		}
	}
	var dns []net.IP
	for _, d := range s.cfg.DNS {
		if ip := net.ParseIP(d); ip != nil && ip.To4() != nil {
			dns = append(dns, ip.To4())
		}
	}
	if len(dns) > 0 {
		rep.Options.SetIPs(OptDNSServer, dns)
	}
	if s.cfg.Domain != "" {
		rep.Options.SetString(OptDomainName, s.cfg.Domain)
	}
}

// handleProxy 处理 proxyDHCP 模式下的报文。
//
// proxyDHCP 的要点：
//   - 不分配地址：yiaddr 恒为 0，且不带 option 1/3/6/51/58/59；
//   - DISCOVER 回 OFFER，REQUEST 回 ACK（PXE 规范要求两者都响应）；
//   - 只带 54（本机 IP）+ 60（PXEClient）+ 43（PXE 厂商选项）+ 66/67（引导文件名）。
func (s *Server) handleProxy(req *Packet, mt MsgType, mac string, arch model.Arch) *Packet {
	switch mt {
	case MsgDiscover, MsgRequest:
	default:
		s.stat.ignored.Add(1)
		return nil
	}
	rep := s.newReply(req)
	rep.YIAddr = net.IPv4zero.To4()
	rep.SIAddr = net.ParseIP(s.cfg.ServerIP).To4()
	s.setServerOptions(rep, req, arch)
	// proxy 模式绝不能带租约/网络参数，否则会覆盖真正 DHCP 服务器下发的配置。
	rep.Options.Del(OptSubnetMask)
	rep.Options.Del(OptRouter)
	rep.Options.Del(OptDNSServer)
	rep.Options.Del(OptLeaseTime)
	rep.Options.Del(OptRenewalTime)
	rep.Options.Del(OptRebindingTime)
	rep.Options.Del(OptDomainName)
	rep.Options.Del(OptBroadcastAddr)
	if mt == MsgDiscover {
		rep.Options.SetUint8(OptMsgType, uint8(MsgOffer))
		s.emit("info", mac, "proxyDHCP OFFER（架构 %s，引导文件 %s，不分配 IP）", arch, s.BootFile(arch))
	} else {
		rep.Options.SetUint8(OptMsgType, uint8(MsgAck))
		s.emit("info", mac, "proxyDHCP ACK（架构 %s，引导文件 %s，不分配 IP）", arch, s.BootFile(arch))
	}
	return rep
}

// notifyClient 把本次交互汇报给上层（pxe 编排器据此更新客户机列表）。
//
// assignedIP 是本次响应里服务器分配/确认给客户机的地址（yiaddr）。
// 客户机自己上报的 ciaddr 优先（续租场景），其次才是我们分配的地址。
func (s *Server) notifyClient(req *Packet, arch model.Arch, bootFile string, assignedIP net.IP) {
	if s.cfg.OnClient == nil {
		return
	}
	now := time.Now()
	mac := req.MAC()
	ip := ""
	switch {
	case !isZeroIP(req.CIAddr):
		ip = req.CIAddr.String()
	case !isZeroIP(assignedIP):
		ip = assignedIP.String()
	case !isZeroIP(req.YIAddr):
		ip = req.YIAddr.String()
	}
	c := &model.Client{
		MAC:       mac,
		IP:        ip,
		Hostname:  req.Hostname(),
		Arch:      arch,
		UUID:      req.UUID(),
		BootFile:  bootFile,
		State:     model.StateDHCP,
		FirstSeen: now,
		LastSeen:  now,
		Note:      fmt.Sprintf("%s %s", s.cfg.Mode, req.MsgType()),
	}
	s.cfg.OnClient(c)
}

// requestedIP 返回 REQUEST 里客户端想要的地址（option 50 优先，其次 ciaddr）。
func requestedIP(req *Packet) net.IP {
	if ips := req.Options.IPs(OptRequestedIP); len(ips) > 0 && !isZeroIP(ips[0]) {
		return ips[0].To4()
	}
	if !isZeroIP(req.CIAddr) {
		return req.CIAddr.To4()
	}
	return nil
}

// isZeroIP 报告地址是否为空 / 全零。
func isZeroIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	v4 := ip.To4()
	if v4 == nil {
		return true
	}
	return v4[0] == 0 && v4[1] == 0 && v4[2] == 0 && v4[3] == 0
}

// broadcastOf 计算子网广播地址。
func broadcastOf(ip net.IP, mask net.IPMask) net.IP {
	if ip == nil || len(mask) != 4 {
		return nil
	}
	out := make(net.IP, 4)
	for i := 0; i < 4; i++ {
		out[i] = ip[i] | ^mask[i]
	}
	return out
}
