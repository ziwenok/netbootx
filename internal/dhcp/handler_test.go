package dhcp

import (
	"bytes"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/wupengcheng/netbootx/internal/model"
)

// testServer 构造一个用于单元测试的服务器（不启动监听）。
func testServer(t *testing.T, mode string) *Server {
	t.Helper()
	cfg := Config{
		Mode:       mode,
		ServerIP:   "192.168.123.1",
		RangeStart: "192.168.123.200",
		RangeEnd:   "192.168.123.210",
		Netmask:    "255.255.255.0",
		Gateway:    "192.168.123.1",
		DNS:        []string{"223.5.5.5", "114.114.114.114"},
		Domain:     "netbootx.lan",
		LeaseTime:  3600,
		Port:       16767,
		BindIP:     "127.0.0.1",
		BootFileFor: func(a model.Arch) string {
			switch a {
			case model.ArchBIOS:
				return "undionly.kpxe"
			case model.ArchUEFI32:
				return "ipxe32.efi"
			case model.ArchARM64:
				return "ipxe-arm64.efi"
			}
			return "ipxe.efi"
		},
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	return s
}

// discoverBytes 构造一个真实的 DHCP DISCOVER 报文（BOOTP 头 + cookie + options）。
func discoverBytes(t *testing.T, archOpt uint16, broadcast bool, giaddr net.IP, hostname string) []byte {
	t.Helper()
	p := &Packet{
		Op:     bootRequest,
		HType:  1,
		HLen:   6,
		XID:    0xcafe1234,
		CIAddr: net.IPv4zero.To4(),
		YIAddr: net.IPv4zero.To4(),
		SIAddr: net.IPv4zero.To4(),
		GIAddr: giaddr,
	}
	if broadcast {
		p.Flags = 0x8000
	}
	copy(p.CHAddr[:], testMAC)
	p.Options.SetUint8(OptMsgType, uint8(MsgDiscover))
	p.Options.Set(OptParamRequest, []byte{1, 3, 6, 15, 43, 60, 66, 67, 93, 97})
	p.Options.SetString(OptVendorClass, "PXEClient:Arch:00007:UNDI:003016")
	p.Options.SetUint16(OptClientArch, archOpt)
	p.Options.SetUint16(OptMaxMsgSize, 1500)
	if hostname != "" {
		p.Options.SetString(OptHostName, hostname)
	}
	raw, err := p.Encode()
	if err != nil {
		t.Fatalf("构造 DISCOVER 失败: %v", err)
	}
	return raw
}

// handleOne 直接调用 handler 处理一个报文，返回响应。
func handleOne(t *testing.T, s *Server, raw []byte, src *net.UDPAddr) (*Packet, *net.UDPAddr) {
	t.Helper()
	pkt, err := DecodePacket(raw)
	if err != nil {
		t.Fatalf("解析请求失败: %v", err)
	}
	rep := s.handlePacket(pkt, src)
	if rep == nil {
		return nil, nil
	}
	return rep.pkt, rep.dst
}

func TestFullModeDiscoverOffer(t *testing.T) {
	s := testServer(t, ModeFull)
	raw := discoverBytes(t, 7, false, nil, "client-1")
	rep, dst := handleOne(t, s, raw, &net.UDPAddr{IP: net.ParseIP("192.168.123.200"), Port: 68})
	if rep == nil {
		t.Fatal("DISCOVER 未得到响应")
	}
	if got, _ := rep.Options.Uint8(OptMsgType); MsgType(got) != MsgOffer {
		t.Errorf("消息类型 = %v，期望 OFFER", MsgType(got))
	}
	if rep.Op != bootReply {
		t.Errorf("op = %d，期望 %d", rep.Op, bootReply)
	}
	if rep.XID != 0xcafe1234 {
		t.Errorf("XID 未回显: %#x", rep.XID)
	}
	if rep.MAC() != "00:0c:29:aa:bb:cc" {
		t.Errorf("CHAddr 未回显: %q", rep.MAC())
	}
	// next-server 必须是本机 IP
	if !rep.SIAddr.Equal(net.ParseIP("192.168.123.1")) {
		t.Errorf("siaddr = %v，期望 192.168.123.1", rep.SIAddr)
	}
	// yiaddr 必须落在池内
	if !s.table.InPool(rep.YIAddr) {
		t.Errorf("yiaddr = %v，不在 IP 池内", rep.YIAddr)
	}
	// bootfile：file 字段 + option 67
	if rep.File != "ipxe.efi" {
		t.Errorf("file 字段 = %q，期望 ipxe.efi", rep.File)
	}
	if bf, _ := rep.Options.String(OptBootfileName); bf != "ipxe.efi" {
		t.Errorf("option 67 = %q，期望 ipxe.efi", bf)
	}
	// 66 = TFTP 服务器名
	if tf, _ := rep.Options.String(OptTFTPServerName); tf != "192.168.123.1" {
		t.Errorf("option 66 = %q", tf)
	}
	// 54 = 服务器标识
	if ids := rep.Options.IPs(OptServerID); len(ids) != 1 || !ids[0].Equal(net.ParseIP("192.168.123.1")) {
		t.Errorf("option 54 = %v", ids)
	}
	// 60 = PXEClient
	if vc, _ := rep.Options.String(OptVendorClass); vc != "PXEClient" {
		t.Errorf("option 60 = %q", vc)
	}
	// 51 租约时间
	if lt, ok := rep.Options.Uint32(OptLeaseTime); !ok || lt != 3600 {
		t.Errorf("option 51 = %d/%v", lt, ok)
	}
	// 1 / 3 / 6 / 15
	if m, _ := rep.Options.Get(OptSubnetMask); !bytes.Equal(m, []byte{255, 255, 255, 0}) {
		t.Errorf("option 1 = %v", m)
	}
	if r := rep.Options.IPs(OptRouter); len(r) != 1 || !r[0].Equal(net.ParseIP("192.168.123.1")) {
		t.Errorf("option 3 = %v", r)
	}
	if d := rep.Options.IPs(OptDNSServer); len(d) != 2 {
		t.Errorf("option 6 = %v", d)
	}
	if dom, _ := rep.Options.String(OptDomainName); dom != "netbootx.lan" {
		t.Errorf("option 15 = %q", dom)
	}
	// option 43 必须存在且能被解析
	vendor, ok := rep.Options.Get(OptVendorSpecific)
	if !ok || len(vendor) == 0 {
		t.Fatal("缺少 option 43")
	}
	pxe, err := DecodePXEVendor(vendor)
	if err != nil {
		t.Fatalf("option 43 解析失败: %v", err)
	}
	if pxe.DiscoveryControl != DefaultPXEDiscoveryControl {
		t.Errorf("PXE_DISCOVERY_CONTROL = %#x，期望 %#x（关闭广播/多播发现）",
			pxe.DiscoveryControl, DefaultPXEDiscoveryControl)
	}
	if pxe.MTFTP == nil || !pxe.MTFTP.IP.Equal(net.ParseIP("192.168.123.1")) {
		t.Errorf("MTFTP 提示 = %+v", pxe.MTFTP)
	}
	// 回包目的地：yiaddr 非 0 且未置广播位 → 单播到 yiaddr:68
	if !dst.IP.Equal(rep.YIAddr) || dst.Port != 68 {
		t.Errorf("回包目的地 = %v，期望 %v:68", dst, rep.YIAddr)
	}
	// 租约状态为 offered
	leases := s.Leases()
	if len(leases) != 1 || leases[0].State != LeaseStateOffered {
		t.Fatalf("租约 = %+v", leases)
	}
	if leases[0].Arch != model.ArchUEFI64 {
		t.Errorf("租约架构 = %v", leases[0].Arch)
	}
	if leases[0].Hostname != "client-1" {
		t.Errorf("租约主机名 = %q", leases[0].Hostname)
	}
}

func TestFullModeRequestAckAndLease(t *testing.T) {
	s := testServer(t, ModeFull)
	raw := discoverBytes(t, 0, false, nil, "")
	rep, _ := handleOne(t, s, raw, &net.UDPAddr{IP: net.ParseIP("192.168.123.200"), Port: 68})
	if rep == nil {
		t.Fatal("DISCOVER 无响应")
	}
	ip := rep.YIAddr

	// 构造 REQUEST（SELECTING）：带 option 50 和 54
	req := &Packet{
		Op: bootRequest, HType: 1, HLen: 6, XID: 0xcafe1234,
		CIAddr: net.IPv4zero.To4(), YIAddr: net.IPv4zero.To4(),
		SIAddr: net.IPv4zero.To4(), GIAddr: net.IPv4zero.To4(),
	}
	copy(req.CHAddr[:], testMAC)
	req.Options.SetUint8(OptMsgType, uint8(MsgRequest))
	req.Options.SetIP(OptRequestedIP, ip)
	req.Options.SetIP(OptServerID, net.ParseIP("192.168.123.1"))
	req.Options.SetUint16(OptClientArch, 0)
	rawReq, err := req.Encode()
	if err != nil {
		t.Fatal(err)
	}
	ack, _ := handleOne(t, s, rawReq, &net.UDPAddr{IP: net.ParseIP("192.168.123.200"), Port: 68})
	if ack == nil {
		t.Fatal("REQUEST 未得到响应")
	}
	if got, _ := ack.Options.Uint8(OptMsgType); MsgType(got) != MsgAck {
		t.Errorf("消息类型 = %v，期望 ACK", MsgType(got))
	}
	if !ack.YIAddr.Equal(ip) {
		t.Errorf("ACK yiaddr = %v，期望 %v", ack.YIAddr, ip)
	}
	// ACK 的 bootfile 按 BIOS 架构下发
	if ack.File != "undionly.kpxe" {
		t.Errorf("BIOS 客户机 bootfile = %q，期望 undionly.kpxe", ack.File)
	}
	leases := s.Leases()
	if len(leases) != 1 || leases[0].State != LeaseStateBound || leases[0].IP != ip.String() {
		t.Fatalf("租约 = %+v", leases)
	}

	// RELEASE 后租约消失
	rel := &Packet{Op: bootRequest, HType: 1, HLen: 6, XID: 0xcafe1234,
		CIAddr: ip.To4(), YIAddr: net.IPv4zero.To4(), SIAddr: net.IPv4zero.To4(), GIAddr: net.IPv4zero.To4()}
	copy(rel.CHAddr[:], testMAC)
	rel.Options.SetUint8(OptMsgType, uint8(MsgRelease))
	rawRel, _ := rel.Encode()
	if r, _ := handleOne(t, s, rawRel, nil); r != nil {
		t.Error("RELEASE 不应回包")
	}
	if len(s.Leases()) != 0 {
		t.Error("RELEASE 后租约应被删除")
	}
}

func TestFullModeRequestOtherServerIgnored(t *testing.T) {
	s := testServer(t, ModeFull)
	raw := discoverBytes(t, 0, false, nil, "")
	handleOne(t, s, raw, &net.UDPAddr{IP: net.ParseIP("192.168.123.200"), Port: 68})

	req := &Packet{Op: bootRequest, HType: 1, HLen: 6, XID: 1,
		CIAddr: net.IPv4zero.To4(), YIAddr: net.IPv4zero.To4(), SIAddr: net.IPv4zero.To4(), GIAddr: net.IPv4zero.To4()}
	copy(req.CHAddr[:], testMAC)
	req.Options.SetUint8(OptMsgType, uint8(MsgRequest))
	req.Options.SetIP(OptRequestedIP, net.ParseIP("192.168.123.200"))
	req.Options.SetIP(OptServerID, net.ParseIP("10.0.0.1")) // 别的服务器
	rawReq, _ := req.Encode()
	if rep, _ := handleOne(t, s, rawReq, nil); rep != nil {
		t.Error("选择了其他 DHCP 服务器时不应回包")
	}
	if len(s.Leases()) != 0 {
		t.Error("放弃分配后租约应被释放")
	}
}

func TestFullModeNakOutOfPool(t *testing.T) {
	s := testServer(t, ModeFull)
	req := &Packet{Op: bootRequest, HType: 1, HLen: 6, XID: 2,
		CIAddr: net.IPv4zero.To4(), YIAddr: net.IPv4zero.To4(), SIAddr: net.IPv4zero.To4(), GIAddr: net.IPv4zero.To4()}
	copy(req.CHAddr[:], testMAC)
	req.Options.SetUint8(OptMsgType, uint8(MsgRequest))
	req.Options.SetIP(OptRequestedIP, net.ParseIP("10.9.9.9"))
	raw, _ := req.Encode()
	rep, dst := handleOne(t, s, raw, &net.UDPAddr{IP: net.ParseIP("192.168.123.200"), Port: 68})
	if rep == nil {
		t.Fatal("池外请求应得到 NAK")
	}
	if got, _ := rep.Options.Uint8(OptMsgType); MsgType(got) != MsgNak {
		t.Errorf("消息类型 = %v，期望 NAK", MsgType(got))
	}
	if !isZeroIP(rep.YIAddr) {
		t.Errorf("NAK yiaddr 应为 0，实际 %v", rep.YIAddr)
	}
	if !dst.IP.Equal(net.IPv4bcast) {
		t.Errorf("NAK 应广播，实际发往 %v", dst)
	}
	if msg, _ := rep.Options.String(OptMessage); !strings.Contains(msg, "不在本机 IP 池内") {
		t.Errorf("NAK 消息 = %q", msg)
	}
}

func TestFullModeDeclineAndInform(t *testing.T) {
	s := testServer(t, ModeFull)
	raw := discoverBytes(t, 7, false, nil, "")
	rep, _ := handleOne(t, s, raw, &net.UDPAddr{IP: net.ParseIP("192.168.123.200"), Port: 68})
	ip := rep.YIAddr

	// DECLINE：报告地址冲突
	dec := &Packet{Op: bootRequest, HType: 1, HLen: 6, XID: 3,
		CIAddr: net.IPv4zero.To4(), YIAddr: net.IPv4zero.To4(), SIAddr: net.IPv4zero.To4(), GIAddr: net.IPv4zero.To4()}
	copy(dec.CHAddr[:], testMAC)
	dec.Options.SetUint8(OptMsgType, uint8(MsgDecline))
	dec.Options.SetIP(OptRequestedIP, ip)
	rawDec, _ := dec.Encode()
	if r, _ := handleOne(t, s, rawDec, nil); r != nil {
		t.Error("DECLINE 不应回包")
	}
	if len(s.Leases()) != 0 {
		t.Error("DECLINE 后该地址应被回收")
	}

	// INFORM：已有地址，只补参数，不回 bootfile
	inf := &Packet{Op: bootRequest, HType: 1, HLen: 6, XID: 4,
		CIAddr: net.ParseIP("192.168.123.55").To4(), YIAddr: net.IPv4zero.To4(),
		SIAddr: net.IPv4zero.To4(), GIAddr: net.IPv4zero.To4()}
	copy(inf.CHAddr[:], testMAC)
	inf.Options.SetUint8(OptMsgType, uint8(MsgInform))
	inf.Options.SetUint16(OptClientArch, 7)
	rawInf, _ := inf.Encode()
	ack, _ := handleOne(t, s, rawInf, nil)
	if ack == nil {
		t.Fatal("INFORM 应得到 ACK")
	}
	if got, _ := ack.Options.Uint8(OptMsgType); MsgType(got) != MsgAck {
		t.Errorf("INFORM 响应 = %v，期望 ACK", MsgType(got))
	}
	if !isZeroIP(ack.YIAddr) {
		t.Errorf("INFORM 的 yiaddr 应为 0，实际 %v", ack.YIAddr)
	}
	if _, ok := ack.Options.Get(OptLeaseTime); ok {
		t.Error("INFORM 不应下发租约时间")
	}
	if _, ok := ack.Options.Get(OptSubnetMask); !ok {
		t.Error("INFORM 应下发子网掩码")
	}
}

func TestProxyModeOfferHasNoLeaseOptions(t *testing.T) {
	s := testServer(t, ModeProxy)
	raw := discoverBytes(t, 7, false, nil, "")
	rep, _ := handleOne(t, s, raw, &net.UDPAddr{IP: net.ParseIP("192.168.123.200"), Port: 68})
	if rep == nil {
		t.Fatal("proxyDHCP 必须响应 DISCOVER")
	}
	if got, _ := rep.Options.Uint8(OptMsgType); MsgType(got) != MsgOffer {
		t.Errorf("proxy DISCOVER 的响应类型 = %v，期望 OFFER", MsgType(got))
	}
	if !isZeroIP(rep.YIAddr) {
		t.Errorf("proxy 模式 yiaddr 必须为 0，实际 %v", rep.YIAddr)
	}
	// 关键断言：不能带 option 1/3/6/51
	for _, code := range []byte{OptSubnetMask, OptRouter, OptDNSServer, OptLeaseTime, OptRenewalTime, OptRebindingTime, OptDomainName, OptBroadcastAddr} {
		if _, ok := rep.Options.Get(code); ok {
			t.Errorf("proxy 模式不应包含 option %d", code)
		}
	}
	// 必须带 54 / 60 / 43 / 66 / 67
	if ids := rep.Options.IPs(OptServerID); len(ids) != 1 || !ids[0].Equal(net.ParseIP("192.168.123.1")) {
		t.Errorf("option 54 = %v", ids)
	}
	if vc, _ := rep.Options.String(OptVendorClass); vc != "PXEClient" {
		t.Errorf("option 60 = %q", vc)
	}
	if _, ok := rep.Options.Get(OptVendorSpecific); !ok {
		t.Error("proxy 模式必须带 option 43")
	}
	if tf, _ := rep.Options.String(OptTFTPServerName); tf != "192.168.123.1" {
		t.Errorf("option 66 = %q", tf)
	}
	if bf, _ := rep.Options.String(OptBootfileName); bf != "ipxe.efi" {
		t.Errorf("option 67 = %q", bf)
	}
	if rep.File != "ipxe.efi" {
		t.Errorf("file 字段 = %q", rep.File)
	}
	if !rep.SIAddr.Equal(net.ParseIP("192.168.123.1")) {
		t.Errorf("siaddr = %v", rep.SIAddr)
	}
	// proxy 模式没有租约表
	if len(s.Leases()) != 0 {
		t.Errorf("proxy 模式不应有租约，实际 %d 条", len(s.Leases()))
	}
}

func TestProxyModeAcksRequest(t *testing.T) {
	s := testServer(t, ModeProxy)
	// PXE 客户端在 proxyDHCP 场景下会用 REQUEST 再次请求引导信息
	req := &Packet{Op: bootRequest, HType: 1, HLen: 6, XID: 5,
		CIAddr: net.IPv4zero.To4(), YIAddr: net.IPv4zero.To4(), SIAddr: net.IPv4zero.To4(), GIAddr: net.IPv4zero.To4()}
	copy(req.CHAddr[:], testMAC)
	req.Options.SetUint8(OptMsgType, uint8(MsgRequest))
	req.Options.SetIP(OptRequestedIP, net.ParseIP("192.168.123.77"))
	req.Options.SetString(OptVendorClass, "PXEClient:Arch:00000:UNDI:002001")
	req.Options.SetUint16(OptClientArch, 0)
	raw, _ := req.Encode()
	rep, _ := handleOne(t, s, raw, &net.UDPAddr{IP: net.ParseIP("192.168.123.200"), Port: 68})
	if rep == nil {
		t.Fatal("proxyDHCP 必须响应 REQUEST")
	}
	if got, _ := rep.Options.Uint8(OptMsgType); MsgType(got) != MsgAck {
		t.Errorf("proxy REQUEST 的响应类型 = %v，期望 ACK", MsgType(got))
	}
	if !isZeroIP(rep.YIAddr) {
		t.Errorf("proxy ACK 的 yiaddr 必须为 0，实际 %v", rep.YIAddr)
	}
	if rep.File != "undionly.kpxe" {
		t.Errorf("BIOS 客户机 bootfile = %q，期望 undionly.kpxe", rep.File)
	}
	if _, ok := rep.Options.Get(OptLeaseTime); ok {
		t.Error("proxy ACK 不应带 option 51")
	}
}

func TestProxyModeIgnoresRelease(t *testing.T) {
	s := testServer(t, ModeProxy)
	rel := &Packet{Op: bootRequest, HType: 1, HLen: 6, XID: 6,
		CIAddr: net.ParseIP("192.168.123.77").To4(), YIAddr: net.IPv4zero.To4(),
		SIAddr: net.IPv4zero.To4(), GIAddr: net.IPv4zero.To4()}
	copy(rel.CHAddr[:], testMAC)
	rel.Options.SetUint8(OptMsgType, uint8(MsgRelease))
	raw, _ := rel.Encode()
	if rep, _ := handleOne(t, s, raw, nil); rep != nil {
		t.Error("proxy 模式不应响应 RELEASE")
	}
}

func TestExternalModeNeverReplies(t *testing.T) {
	s := testServer(t, ModeExternal)
	raw := discoverBytes(t, 7, false, nil, "")
	if rep, _ := handleOne(t, s, raw, nil); rep != nil {
		t.Error("external 模式不应回包")
	}
	// 但引导文件查询必须可用
	if got := s.BootFile(model.ArchBIOS); got != "undionly.kpxe" {
		t.Errorf("BootFile(bios) = %q", got)
	}
	if got := s.BootFile(model.ArchUEFI64); got != "ipxe.efi" {
		t.Errorf("BootFile(uefi64) = %q", got)
	}
	if got := s.BootFile(model.ArchARM64); got != "ipxe-arm64.efi" {
		t.Errorf("BootFile(arm64) = %q", got)
	}
}

func TestBootFilePerArch(t *testing.T) {
	// 未注入 BootFileFor 时应使用内置默认值
	s, err := New(Config{Mode: ModeProxy, ServerIP: "10.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	cases := map[model.Arch]string{
		model.ArchBIOS:        "undionly.kpxe",
		model.ArchUEFI32:      "ipxe32.efi",
		model.ArchUEFI64:      "ipxe.efi",
		model.ArchUEFI64HTTP:  "ipxe.efi",
		model.ArchARM64:       "ipxe-arm64.efi",
		model.ArchARM64HTTP:   "ipxe-arm64.efi",
		model.ArchLoongArch64: "ipxe-loongarch64.efi",
	}
	for arch, want := range cases {
		if got := s.BootFile(arch); got != want {
			t.Errorf("BootFile(%v) = %q，期望 %q", arch, got, want)
		}
	}
	// 上层注入为空字符串时回落到默认值
	s2, _ := New(Config{Mode: ModeProxy, ServerIP: "10.0.0.1", BootFileFor: func(model.Arch) string { return "" }})
	if got := s2.BootFile(model.ArchBIOS); got != "undionly.kpxe" {
		t.Errorf("注入空值时应回落默认值，实际 %q", got)
	}
}

func TestReplyAddressSelection(t *testing.T) {
	s := testServer(t, ModeProxy)
	s.replyPort = 68
	s.relayPort = 67
	src := &net.UDPAddr{IP: net.ParseIP("192.168.123.200"), Port: 68}

	t.Run("中继代理优先", func(t *testing.T) {
		req := &Packet{GIAddr: net.ParseIP("10.0.0.254").To4(), Flags: 0x8000}
		dst := s.replyAddr(req, src, net.IPv4zero.To4(), false)
		if !dst.IP.Equal(net.ParseIP("10.0.0.254")) || dst.Port != 67 {
			t.Errorf("目的地 = %v，期望 10.0.0.254:67", dst)
		}
	})
	t.Run("广播标志", func(t *testing.T) {
		req := &Packet{GIAddr: net.IPv4zero.To4(), Flags: 0x8000}
		dst := s.replyAddr(req, src, net.ParseIP("192.168.123.200").To4(), false)
		if !dst.IP.Equal(net.IPv4bcast) || dst.Port != 68 {
			t.Errorf("目的地 = %v，期望广播:68", dst)
		}
	})
	t.Run("ciaddr 续租", func(t *testing.T) {
		req := &Packet{GIAddr: net.IPv4zero.To4(), CIAddr: net.ParseIP("192.168.123.150").To4()}
		dst := s.replyAddr(req, src, net.IPv4zero.To4(), false)
		if !dst.IP.Equal(net.ParseIP("192.168.123.150")) || dst.Port != 68 {
			t.Errorf("目的地 = %v", dst)
		}
	})
	t.Run("yiaddr 单播", func(t *testing.T) {
		req := &Packet{GIAddr: net.IPv4zero.To4()}
		dst := s.replyAddr(req, src, net.ParseIP("192.168.123.201").To4(), false)
		if !dst.IP.Equal(net.ParseIP("192.168.123.201")) {
			t.Errorf("目的地 = %v", dst)
		}
	})
	t.Run("都没有则广播", func(t *testing.T) {
		req := &Packet{GIAddr: net.IPv4zero.To4()}
		dst := s.replyAddr(req, nil, net.IPv4zero.To4(), false)
		if !dst.IP.Equal(net.IPv4bcast) {
			t.Errorf("目的地 = %v，期望广播", dst)
		}
	})
	t.Run("强制广播", func(t *testing.T) {
		req := &Packet{GIAddr: net.IPv4zero.To4(), CIAddr: net.ParseIP("192.168.123.150").To4()}
		dst := s.replyAddr(req, src, net.IPv4zero.To4(), true)
		if !dst.IP.Equal(net.IPv4bcast) {
			t.Errorf("目的地 = %v，期望广播", dst)
		}
	})
}

func TestPXEMenuOption43(t *testing.T) {
	s := testServer(t, ModeProxy)
	s.SetPXEMenu("请选择启动方式", []PXEMenuEntry{
		{Description: "从硬盘启动", BootItem: 0x0001},
		{Description: "NetBootX 引导", BootItem: 0x0007},
	})
	s.SetPXEBootItem(7)
	raw := discoverBytes(t, 7, false, nil, "")
	rep, _ := handleOne(t, s, raw, &net.UDPAddr{IP: net.ParseIP("192.168.123.200"), Port: 68})
	if rep == nil {
		t.Fatal("无响应")
	}
	vendor, ok := rep.Options.Get(OptVendorSpecific)
	if !ok {
		t.Fatal("缺少 option 43")
	}
	pxe, err := DecodePXEVendor(vendor)
	if err != nil {
		t.Fatal(err)
	}
	if pxe.MenuPrompt != "请选择启动方式" {
		t.Errorf("菜单提示 = %q", pxe.MenuPrompt)
	}
	if len(pxe.Menu) != 2 || pxe.Menu[1].BootItem != 7 {
		t.Errorf("菜单 = %+v", pxe.Menu)
	}
	if !pxe.HasBootItem || pxe.BootItem != 7 {
		t.Errorf("引导项 = %+v", pxe)
	}
	// 关闭菜单
	s.SetPXEMenu("", nil)
	s.SetPXEBootItem(0)
	rep, _ = handleOne(t, s, raw, &net.UDPAddr{IP: net.ParseIP("192.168.123.200"), Port: 68})
	vendor, _ = rep.Options.Get(OptVendorSpecific)
	pxe, _ = DecodePXEVendor(vendor)
	if len(pxe.Menu) != 0 || pxe.HasBootItem {
		t.Errorf("关闭后仍有菜单内容: %+v", pxe)
	}
}

func TestOnClientCallback(t *testing.T) {
	var got *model.Client
	s := testServer(t, ModeProxy)
	s.cfg.OnClient = func(c *model.Client) { got = c }
	raw := discoverBytes(t, 7, false, nil, "web-client")
	handleOne(t, s, raw, &net.UDPAddr{IP: net.ParseIP("192.168.123.200"), Port: 68})
	if got == nil {
		t.Fatal("OnClient 未被调用")
	}
	if got.MAC != "00:0c:29:aa:bb:cc" {
		t.Errorf("MAC = %q", got.MAC)
	}
	if got.Arch != model.ArchUEFI64 {
		t.Errorf("Arch = %v", got.Arch)
	}
	if got.Hostname != "web-client" {
		t.Errorf("Hostname = %q", got.Hostname)
	}
	if got.BootFile != "ipxe.efi" {
		t.Errorf("BootFile = %q", got.BootFile)
	}
	// proxy 模式不分配地址，IP 应为空
	if got.IP != "" {
		t.Errorf("proxy 模式 IP = %q，应为空", got.IP)
	}
	if got.State != model.StateDHCP {
		t.Errorf("State = %v", got.State)
	}
	if got.LastSeen.IsZero() {
		t.Error("LastSeen 未设置")
	}
}

// TestOnClientFullModeReportsAssignedIP 验证 full 模式下客户机列表能拿到分配的 IP。
func TestOnClientFullModeReportsAssignedIP(t *testing.T) {
	var got *model.Client
	s := testServer(t, ModeFull)
	s.cfg.OnClient = func(c *model.Client) { got = c }
	raw := discoverBytes(t, 7, false, nil, "")
	rep, _ := handleOne(t, s, raw, &net.UDPAddr{IP: net.ParseIP("192.168.123.200"), Port: 68})
	if rep == nil {
		t.Fatal("DISCOVER 无响应")
	}
	if got == nil {
		t.Fatal("OnClient 未被调用")
	}
	if got.IP != rep.YIAddr.String() {
		t.Errorf("OnClient 上报的 IP = %q，期望分配的 %v", got.IP, rep.YIAddr)
	}
	if !s.table.InPool(net.ParseIP(got.IP)) {
		t.Errorf("上报的 IP %q 不在池内", got.IP)
	}
}

func TestOnEventCallback(t *testing.T) {
	var events []model.Event
	s := testServer(t, ModeProxy)
	s.cfg.OnEvent = func(e model.Event) { events = append(events, e) }
	raw := discoverBytes(t, 7, false, nil, "")
	handleOne(t, s, raw, &net.UDPAddr{IP: net.ParseIP("192.168.123.200"), Port: 68})
	if len(events) == 0 {
		t.Fatal("OnEvent 未被调用")
	}
	if events[0].Kind != "dhcp" {
		t.Errorf("事件 Kind = %q", events[0].Kind)
	}
	if events[0].MAC != "00:0c:29:aa:bb:cc" {
		t.Errorf("事件 MAC = %q", events[0].MAC)
	}
}

func TestHandleUnknownMsgType(t *testing.T) {
	s := testServer(t, ModeProxy)
	p := newTestPacket(MsgType(42))
	raw, _ := p.Encode()
	if rep := s.handlePacket(mustDecode(t, raw), nil); rep != nil {
		t.Error("未知报文类型不应回包")
	}
	if s.Stats().Ignored == 0 {
		t.Error("Ignored 计数未增加")
	}
}

func mustDecode(t *testing.T, raw []byte) *Packet {
	t.Helper()
	p, err := DecodePacket(raw)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestStatsCounters(t *testing.T) {
	s := testServer(t, ModeFull)
	raw := discoverBytes(t, 7, false, nil, "")
	handleOne(t, s, raw, &net.UDPAddr{IP: net.ParseIP("192.168.123.200"), Port: 68})
	st := s.Stats()
	if st.Discover != 1 {
		t.Errorf("Discover = %d", st.Discover)
	}
	if st.Mode != ModeFull {
		t.Errorf("Mode = %q", st.Mode)
	}
	if st.PoolSize != 11 {
		t.Errorf("PoolSize = %d，期望 11", st.PoolSize)
	}
	if st.Leases != 1 {
		t.Errorf("Leases = %d，期望 1", st.Leases)
	}
	if st.Running {
		t.Error("未 Start 时 Running 应为 false")
	}
}

func TestExternalModeStartStop(t *testing.T) {
	s := testServer(t, ModeExternal)
	if err := s.Start(); err != nil {
		t.Fatalf("external 模式 Start 失败: %v", err)
	}
	if !s.Stats().Running {
		t.Error("Start 后 Running 应为 true")
	}
	if err := s.Stop(); err != nil {
		t.Fatalf("Stop 失败: %v", err)
	}
	if err := s.Stop(); err != nil {
		t.Fatalf("重复 Stop 应幂等: %v", err)
	}
	if s.Stats().Running {
		t.Error("Stop 后 Running 应为 false")
	}
}

// ---- 真实 UDP socket 端到端测试 ----

// testServerPort 优先返回 16767，被占用时回落到系统分配的空闲端口，
// 这样在共享开发环境里（已有 NetBootX 实例在跑）测试也不会失败。
func testServerPort(t *testing.T, preferred int) int {
	t.Helper()
	// 服务器监听的是 0.0.0.0，探测也必须用通配地址，否则会漏判冲突。
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: preferred})
	if err == nil {
		_ = conn.Close()
		return preferred
	}
	conn, err = net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
	if err != nil {
		t.Fatalf("无法分配测试端口: %v", err)
	}
	defer conn.Close()
	port := conn.LocalAddr().(*net.UDPAddr).Port
	t.Logf("端口 %d 已被占用，改用 %d", preferred, port)
	return port
}

// testClientPort 分配一个用于接收回包的客户端端口。
func testClientPort(t *testing.T, preferred int) int {
	t.Helper()
	return testServerPort(t, preferred)
}

// startTestServer 在 127.0.0.1 的高位端口上起一个真实的 DHCP 服务器，
// 并把回包端口指向测试客户端的高位端口（避免非 root 无法绑 68）。
func startTestServer(t *testing.T, mode string, clientPort int) (*Server, *net.UDPConn) {
	t.Helper()
	s := testServer(t, mode)
	s.cfg.Port = testServerPort(t, s.cfg.Port)
	s.replyPort = clientPort
	s.relayPort = clientPort

	// 客户端绑到 0.0.0.0:clientPort，这样发给 IP 池地址（127.0.0.x）的
	// 单播回包也能被收到。
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: clientPort})
	if err != nil {
		t.Fatalf("绑定客户端端口 %d 失败: %v", clientPort, err)
	}
	if err := s.Start(); err != nil {
		conn.Close()
		t.Fatalf("启动服务器失败: %v", err)
	}
	t.Cleanup(func() {
		_ = s.Stop()
		_ = conn.Close()
	})
	return s, conn
}

func TestEndToEndUDPDiscover(t *testing.T) {
	// proxy 模式：不分配 IP，回包按请求来源单播回来，路径最稳定。
	clientPort := testClientPort(t, 16768)
	s, conn := startTestServer(t, ModeProxy, clientPort)
	serverPort := s.cfg.Port

	raw := discoverBytes(t, 7, false, nil, "e2e-client")
	if _, err := conn.WriteToUDP(raw, &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: serverPort}); err != nil {
		t.Fatalf("发送 DISCOVER 失败: %v", err)
	}

	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 1500)
	n, src, err := conn.ReadFromUDP(buf)
	if err != nil {
		t.Fatalf("等待 OFFER 超时: %v", err)
	}
	// 回包必须来自服务器的监听端口
	if src.Port != serverPort {
		t.Errorf("响应来源端口 = %d，期望 %d", src.Port, serverPort)
	}
	rep, err := DecodePacket(buf[:n])
	if err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	if rep.Op != bootReply {
		t.Errorf("op = %d", rep.Op)
	}
	if got, _ := rep.Options.Uint8(OptMsgType); MsgType(got) != MsgOffer {
		t.Errorf("消息类型 = %v", MsgType(got))
	}
	if rep.MAC() != "00:0c:29:aa:bb:cc" {
		t.Errorf("CHAddr = %q", rep.MAC())
	}
	if rep.File != "ipxe.efi" {
		t.Errorf("bootfile = %q", rep.File)
	}
	if !rep.SIAddr.Equal(net.ParseIP("192.168.123.1")) {
		t.Errorf("next-server = %v", rep.SIAddr)
	}
	if !isZeroIP(rep.YIAddr) {
		t.Errorf("proxy 模式 yiaddr 应为 0，实际 %v", rep.YIAddr)
	}
}

func TestEndToEndUDPFullModeAllocation(t *testing.T) {
	// full 模式：IP 池用回环地址段，客户端 socket 绑 0.0.0.0 才能收到单播回包。
	s := testServer(t, ModeFull)
	if s.table != nil {
		s.table.Reset()
	}
	// 换成回环地址池
	tb, err := NewLeaseTable(net.IPv4(127, 0, 0, 100), net.IPv4(127, 0, 0, 110), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	s.table = tb

	// 通过中继代理（giaddr）单播回包：这样回包目标确定、可观测，
	// 也顺带覆盖 full 模式 + relay 的组合。
	s.cfg.Port = testServerPort(t, s.cfg.Port)
	serverPort := s.cfg.Port
	clientPort := testClientPort(t, 16769)
	s.replyPort = clientPort
	s.relayPort = clientPort
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: clientPort})
	if err != nil {
		t.Fatalf("绑定客户端端口失败: %v", err)
	}
	defer conn.Close()
	if err := s.Start(); err != nil {
		t.Fatalf("启动服务器失败: %v", err)
	}
	defer s.Stop()

	raw := discoverBytes(t, 7, false, net.ParseIP("127.0.0.1"), "full-e2e")
	if _, err := conn.WriteToUDP(raw, &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: serverPort}); err != nil {
		t.Fatalf("发送 DISCOVER 失败: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 1500)
	n, _, err := conn.ReadFromUDP(buf)
	if err != nil {
		t.Fatalf("等待 OFFER 超时: %v", err)
	}
	rep, err := DecodePacket(buf[:n])
	if err != nil {
		t.Fatal(err)
	}
	if !s.table.InPool(rep.YIAddr) {
		t.Errorf("分配的地址 %v 不在池内", rep.YIAddr)
	}
	if rep.File != "ipxe.efi" {
		t.Errorf("bootfile = %q", rep.File)
	}
	if lt, _ := rep.Options.Uint32(OptLeaseTime); lt != 3600 {
		t.Errorf("option 51 = %d", lt)
	}
	if _, ok := rep.Options.Get(OptSubnetMask); !ok {
		t.Error("full 模式 OFFER 必须带 option 1")
	}

	// 再发一次 REQUEST，应得到 ACK
	req := &Packet{Op: bootRequest, HType: 1, HLen: 6, XID: 0xcafe1234,
		CIAddr: net.IPv4zero.To4(), YIAddr: net.IPv4zero.To4(), SIAddr: net.IPv4zero.To4(),
		GIAddr: net.ParseIP("127.0.0.1").To4()}
	copy(req.CHAddr[:], testMAC)
	req.Options.SetUint8(OptMsgType, uint8(MsgRequest))
	req.Options.SetIP(OptRequestedIP, rep.YIAddr)
	req.Options.SetIP(OptServerID, net.ParseIP("192.168.123.1"))
	req.Options.SetUint16(OptClientArch, 7)
	rawReq, _ := req.Encode()
	if _, err := conn.WriteToUDP(rawReq, &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: serverPort}); err != nil {
		t.Fatalf("发送 REQUEST 失败: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	n, _, err = conn.ReadFromUDP(buf)
	if err != nil {
		t.Fatalf("等待 ACK 超时: %v", err)
	}
	ack, err := DecodePacket(buf[:n])
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := ack.Options.Uint8(OptMsgType); MsgType(got) != MsgAck {
		t.Errorf("响应类型 = %v，期望 ACK", MsgType(got))
	}
	if len(s.Leases()) != 1 || s.Leases()[0].State != LeaseStateBound {
		t.Errorf("租约 = %+v", s.Leases())
	}
}

func TestEndToEndUDPRelay(t *testing.T) {
	// giaddr 非 0 → 单播回中继代理端口
	s := testServer(t, ModeProxy)
	s.cfg.Port = testServerPort(t, s.cfg.Port)
	serverPort := s.cfg.Port
	relayPort := testClientPort(t, 16770)
	s.replyPort = 68
	s.relayPort = relayPort
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: relayPort})
	if err != nil {
		t.Fatalf("绑定中继端口失败: %v", err)
	}
	defer conn.Close()
	if err := s.Start(); err != nil {
		t.Fatalf("启动服务器失败: %v", err)
	}
	defer s.Stop()

	raw := discoverBytes(t, 7, false, net.ParseIP("127.0.0.1"), "")
	if _, err := conn.WriteToUDP(raw, &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: serverPort}); err != nil {
		t.Fatalf("发送报文失败: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 1500)
	n, _, err := conn.ReadFromUDP(buf)
	if err != nil {
		t.Fatalf("中继端口未收到响应: %v", err)
	}
	rep, err := DecodePacket(buf[:n])
	if err != nil {
		t.Fatal(err)
	}
	// 回包的 giaddr 必须原样回显
	if !rep.GIAddr.Equal(net.ParseIP("127.0.0.1")) {
		t.Errorf("回包 giaddr = %v，期望 127.0.0.1", rep.GIAddr)
	}
	if rep.File != "ipxe.efi" {
		t.Errorf("bootfile = %q", rep.File)
	}
}

func TestEndToEndBroadcastFlag(t *testing.T) {
	// 置了广播标志的 DISCOVER，回包应广播到 255.255.255.255:68。
	// 这里只验证服务器确实往广播地址发（用一个绑 0.0.0.0:replyPort 的 socket 收）。
	s := testServer(t, ModeProxy)
	s.cfg.Port = testServerPort(t, s.cfg.Port)
	serverPort := s.cfg.Port
	clientPort := testClientPort(t, 16771)
	s.replyPort = clientPort
	s.relayPort = clientPort
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: clientPort})
	if err != nil {
		t.Fatalf("绑定客户端端口失败: %v", err)
	}
	defer conn.Close()
	if err := s.Start(); err != nil {
		t.Fatalf("启动服务器失败: %v", err)
	}
	defer s.Stop()

	raw := discoverBytes(t, 0, true, nil, "")
	if _, err := conn.WriteToUDP(raw, &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: serverPort}); err != nil {
		t.Fatalf("发送报文失败: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 1500)
	n, _, err := conn.ReadFromUDP(buf)
	if err != nil {
		// 某些受限环境（容器 / 无广播路由）会丢弃 255.255.255.255 广播，
		// 此时退化为验证地址计算逻辑，不让测试因环境失败。
		t.Logf("未收到广播回包（环境限制），改为验证地址计算: %v", err)
		req := &Packet{GIAddr: net.IPv4zero.To4(), Flags: 0x8000}
		if dst := s.replyAddr(req, nil, net.IPv4zero.To4(), false); !dst.IP.Equal(net.IPv4bcast) || dst.Port != clientPort {
			t.Errorf("广播地址计算错误: %v", dst)
		}
		return
	}
	rep, err := DecodePacket(buf[:n])
	if err != nil {
		t.Fatal(err)
	}
	if rep.File != "undionly.kpxe" {
		t.Errorf("BIOS 客户机 bootfile = %q", rep.File)
	}
}

// TestEndToEndBroadcastReply 是「回包发不出去」这个 BUG 的回归测试。
//
// 场景完全模拟真实客户机：还没有 IP，DISCOVER 置广播标志，服务端必须把 OFFER
// 广播到 255.255.255.255:68。服务端监听 0.0.0.0，客户端也绑通配地址接收。
// 这条用例走的是真实 socket 发送路径（handleDatagram → conn.WriteToUDP），
// 而不是直接调 handlePacket，所以能抓到「绑定具体 IP 后无法广播」的问题。
func TestEndToEndBroadcastReply(t *testing.T) {
	s := testServer(t, ModeProxy)
	// 故意传一个具体 IP 作为 BindIP：包必须自己忽略它去绑 0.0.0.0。
	s.cfg.BindIP = "127.0.0.1"
	s.cfg.Port = testServerPort(t, 16767)
	serverPort := s.cfg.Port
	clientPort := testClientPort(t, 16773)
	s.replyPort = clientPort
	s.relayPort = clientPort

	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: clientPort})
	if err != nil {
		t.Fatalf("绑定客户端端口失败: %v", err)
	}
	defer conn.Close()

	if err := s.Start(); err != nil {
		t.Fatalf("启动服务器失败: %v", err)
	}
	defer s.Stop()

	// 确认监听在通配地址上（而不是 127.0.0.1）
	if la, ok := s.conn.LocalAddr().(*net.UDPAddr); ok {
		if !la.IP.IsUnspecified() {
			t.Errorf("监听地址 = %v，应为 0.0.0.0（绑定具体 IP 会导致无法广播回包）", la.IP)
		}
	} else {
		t.Fatal("无法获取监听地址")
	}

	// 置广播标志的 DISCOVER（客户机此时没有 IP）
	raw := discoverBytes(t, 7, true, nil, "bcast-client")
	if _, err := conn.WriteToUDP(raw, &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: serverPort}); err != nil {
		t.Fatalf("发送 DISCOVER 失败: %v", err)
	}

	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 1500)
	n, src, err := conn.ReadFromUDP(buf)
	if err != nil {
		t.Fatalf("未收到广播 OFFER（回包发送失败？）: %v", err)
	}
	if src.Port != serverPort {
		t.Errorf("响应来源端口 = %d，期望 %d", src.Port, serverPort)
	}
	rep, err := DecodePacket(buf[:n])
	if err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	if got, _ := rep.Options.Uint8(OptMsgType); MsgType(got) != MsgOffer {
		t.Errorf("消息类型 = %v，期望 OFFER", MsgType(got))
	}
	if rep.File != "ipxe.efi" {
		t.Errorf("bootfile = %q", rep.File)
	}
	// 发送统计必须增加（证明真的写出去了）
	if s.Stats().Sent == 0 {
		t.Error("Sent 统计为 0，说明回包没有真正发出")
	}
}

// TestStartIgnoresBindIP 验证 BindIP 不再影响监听地址。
func TestStartIgnoresBindIP(t *testing.T) {
	s := testServer(t, ModeProxy)
	s.cfg.BindIP = "127.0.0.1"
	s.cfg.Port = testServerPort(t, 16774)
	if err := s.Start(); err != nil {
		t.Fatalf("Start 失败: %v", err)
	}
	defer s.Stop()
	s.mu.RLock()
	conn := s.conn
	s.mu.RUnlock()
	la, ok := conn.LocalAddr().(*net.UDPAddr)
	if !ok {
		t.Fatal("无法获取监听地址")
	}
	if !la.IP.IsUnspecified() {
		t.Errorf("BindIP=127.0.0.1 时监听地址 = %v，应仍为 0.0.0.0", la.IP)
	}
}

func TestStartBindError(t *testing.T) {
	// 端口被占用时应返回可读错误
	blocker, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 16772})
	if err != nil {
		t.Fatalf("占用端口失败: %v", err)
	}
	defer blocker.Close()

	s := testServer(t, ModeProxy)
	s.cfg.Port = 16772
	if err := s.Start(); err == nil {
		s.Stop()
		t.Fatal("端口被占用时 Start 应返回错误")
	} else if !strings.Contains(err.Error(), "绑定") {
		t.Errorf("错误信息不够清晰: %v", err)
	}
}
