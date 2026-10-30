package dhcp

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/wupengcheng/netbootx/internal/model"
)

// 协议常量。
const (
	headerLen     = 236        // BOOTP 固定头长度
	magicCookie   = 0x63825363 // DHCP magic cookie（0x63 0x82 0x53 0x63）
	minPacketLen  = headerLen + 4
	minPacketSize = 300 // RFC 1542 要求的最小 BOOTP 报文长度

	bootRequest = 1 // 客户端 → 服务器
	bootReply   = 2 // 服务器 → 客户端

	clientPort = 68
	serverPort = 67
)

// DHCP 选项号（RFC 2132 及 PXE 扩展）。
const (
	OptPad            byte = 0
	OptSubnetMask     byte = 1
	OptRouter         byte = 3
	OptDNSServer      byte = 6
	OptHostName       byte = 12
	OptDomainName     byte = 15
	OptBroadcastAddr  byte = 28
	OptVendorSpecific byte = 43
	OptRequestedIP    byte = 50
	OptLeaseTime      byte = 51
	OptOptionOverload byte = 52
	OptMsgType        byte = 53
	OptServerID       byte = 54
	OptParamRequest   byte = 55
	OptMessage        byte = 56
	OptMaxMsgSize     byte = 57
	OptRenewalTime    byte = 58
	OptRebindingTime  byte = 59
	OptVendorClass    byte = 60
	OptClientID       byte = 61
	OptTFTPServerName byte = 66
	OptBootfileName   byte = 67
	OptRelayAgent     byte = 82
	OptClientArch     byte = 93
	OptClientUUID     byte = 97
	OptEnd            byte = 255
)

// MsgType 是 DHCP 报文类型（option 53）。
type MsgType uint8

// DHCP 报文类型。
const (
	MsgDiscover MsgType = 1
	MsgOffer    MsgType = 2
	MsgRequest  MsgType = 3
	MsgDecline  MsgType = 4
	MsgAck      MsgType = 5
	MsgNak      MsgType = 6
	MsgRelease  MsgType = 7
	MsgInform   MsgType = 8
)

// String 返回报文类型的中文名。
func (t MsgType) String() string {
	switch t {
	case MsgDiscover:
		return "DISCOVER"
	case MsgOffer:
		return "OFFER"
	case MsgRequest:
		return "REQUEST"
	case MsgDecline:
		return "DECLINE"
	case MsgAck:
		return "ACK"
	case MsgNak:
		return "NAK"
	case MsgRelease:
		return "RELEASE"
	case MsgInform:
		return "INFORM"
	}
	return fmt.Sprintf("UNKNOWN(%d)", uint8(t))
}

// Option 是一个 DHCP 选项（TLV 中的 TLV）。
type Option struct {
	Code byte
	Data []byte
}

// Options 是一组有序的 DHCP 选项。
type Options struct {
	list []Option
}

// NewOptions 创建一个空的选项集合。
func NewOptions() *Options { return &Options{} }

// Len 返回选项个数。
func (o *Options) Len() int {
	if o == nil {
		return 0
	}
	return len(o.list)
}

// All 返回全部选项的副本。
func (o *Options) All() []Option {
	if o == nil {
		return nil
	}
	out := make([]Option, len(o.list))
	copy(out, o.list)
	return out
}

// Get 查找某个选项。
func (o *Options) Get(code byte) ([]byte, bool) {
	if o == nil {
		return nil, false
	}
	for i := range o.list {
		if o.list[i].Code == code {
			return o.list[i].Data, true
		}
	}
	return nil, false
}

// Has 报告某个选项是否存在。
func (o *Options) Has(code byte) bool {
	_, ok := o.Get(code)
	return ok
}

// Del 删除某个选项。
func (o *Options) Del(code byte) {
	if o == nil {
		return
	}
	out := o.list[:0]
	for _, it := range o.list {
		if it.Code != code {
			out = append(out, it)
		}
	}
	o.list = out
}

// Set 设置选项（已存在则覆盖，保持原有顺序）。
func (o *Options) Set(code byte, data []byte) {
	if o == nil {
		return
	}
	d := append([]byte(nil), data...)
	for i := range o.list {
		if o.list[i].Code == code {
			o.list[i].Data = d
			return
		}
	}
	o.list = append(o.list, Option{Code: code, Data: d})
}

// addRaw 追加选项数据；同一选项重复出现时按 RFC 3396 拼接。
func (o *Options) addRaw(code byte, data []byte) {
	for i := range o.list {
		if o.list[i].Code == code {
			o.list[i].Data = append(o.list[i].Data, data...)
			return
		}
	}
	o.list = append(o.list, Option{Code: code, Data: append([]byte(nil), data...)})
}

// SetUint8 设置单字节选项。
func (o *Options) SetUint8(code byte, v uint8) { o.Set(code, []byte{v}) }

// SetUint16 设置双字节大端选项。
func (o *Options) SetUint16(code byte, v uint16) {
	b := make([]byte, 2)
	binary.BigEndian.PutUint16(b, v)
	o.Set(code, b)
}

// SetUint32 设置四字节大端选项。
func (o *Options) SetUint32(code byte, v uint32) {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, v)
	o.Set(code, b)
}

// SetIP 设置 IPv4 选项。
func (o *Options) SetIP(code byte, ip net.IP) { o.Set(code, ip4(ip)) }

// SetIPs 设置多个 IPv4 选项（如 DNS）。
func (o *Options) SetIPs(code byte, ips []net.IP) {
	buf := make([]byte, 0, 4*len(ips))
	for _, ip := range ips {
		buf = append(buf, ip4(ip)...)
	}
	o.Set(code, buf)
}

// SetString 设置字符串选项。
func (o *Options) SetString(code byte, s string) { o.Set(code, []byte(s)) }

// Uint8 读取单字节选项。
func (o *Options) Uint8(code byte) (uint8, bool) {
	d, ok := o.Get(code)
	if !ok || len(d) < 1 {
		return 0, false
	}
	return d[0], true
}

// Uint16 读取双字节大端选项。
func (o *Options) Uint16(code byte) (uint16, bool) {
	d, ok := o.Get(code)
	if !ok || len(d) < 2 {
		return 0, false
	}
	return binary.BigEndian.Uint16(d), true
}

// Uint32 读取四字节大端选项。
func (o *Options) Uint32(code byte) (uint32, bool) {
	d, ok := o.Get(code)
	if !ok || len(d) < 4 {
		return 0, false
	}
	return binary.BigEndian.Uint32(d), true
}

// String 读取字符串选项（去掉尾部 NUL）。
func (o *Options) String(code byte) (string, bool) {
	d, ok := o.Get(code)
	if !ok {
		return "", false
	}
	return string(bytes.TrimRight(d, "\x00")), true
}

// IPs 读取 IPv4 列表选项。
func (o *Options) IPs(code byte) []net.IP {
	d, ok := o.Get(code)
	if !ok {
		return nil
	}
	var out []net.IP
	for i := 0; i+4 <= len(d); i += 4 {
		out = append(out, net.IP(append([]byte(nil), d[i:i+4]...)))
	}
	return out
}

// decode 解析一段 options TLV。
func (o *Options) decode(b []byte) error {
	for i := 0; i < len(b); {
		code := b[i]
		if code == OptPad {
			i++
			continue
		}
		if code == OptEnd {
			return nil
		}
		if i+1 >= len(b) {
			return fmt.Errorf("dhcp: 选项 0x%02x 缺少长度字节", code)
		}
		l := int(b[i+1])
		if i+2+l > len(b) {
			return fmt.Errorf("dhcp: 选项 0x%02x 声明长度 %d 超出报文剩余长度 %d", code, l, len(b)-i-2)
		}
		o.addRaw(code, b[i+2:i+2+l])
		i += 2 + l
	}
	return nil
}

// Packet 是一个 DHCP 报文。
type Packet struct {
	Op      byte
	HType   byte
	HLen    byte
	Hops    byte
	XID     uint32
	Secs    uint16
	Flags   uint16
	CIAddr  net.IP // client IP
	YIAddr  net.IP // your IP
	SIAddr  net.IP // next server
	GIAddr  net.IP // relay agent
	CHAddr  [16]byte
	SName   string
	File    string
	Options Options
}

// cString 把定长字段按 C 字符串处理。
func cString(b []byte) string {
	if i := bytes.IndexByte(b, 0); i >= 0 {
		b = b[:i]
	}
	return string(b)
}

// ip4 返回 4 字节形式的 IPv4 地址，非法时返回 0.0.0.0。
func ip4(ip net.IP) []byte {
	if ip == nil {
		return []byte{0, 0, 0, 0}
	}
	if v4 := ip.To4(); v4 != nil {
		return append([]byte(nil), v4...)
	}
	return []byte{0, 0, 0, 0}
}

// DecodePacket 解析一个 DHCP 报文。
func DecodePacket(b []byte) (*Packet, error) {
	if len(b) < minPacketLen {
		return nil, fmt.Errorf("dhcp: 报文长度 %d 字节，至少需要 %d 字节", len(b), minPacketLen)
	}
	if binary.BigEndian.Uint32(b[headerLen:minPacketLen]) != magicCookie {
		return nil, fmt.Errorf("dhcp: magic cookie 不是 0x63825363")
	}
	p := &Packet{
		Op:     b[0],
		HType:  b[1],
		HLen:   b[2],
		Hops:   b[3],
		XID:    binary.BigEndian.Uint32(b[4:8]),
		Secs:   binary.BigEndian.Uint16(b[8:10]),
		Flags:  binary.BigEndian.Uint16(b[10:12]),
		CIAddr: net.IP(append([]byte(nil), b[12:16]...)),
		YIAddr: net.IP(append([]byte(nil), b[16:20]...)),
		SIAddr: net.IP(append([]byte(nil), b[20:24]...)),
		GIAddr: net.IP(append([]byte(nil), b[24:28]...)),
	}
	if p.HLen > 16 {
		return nil, fmt.Errorf("dhcp: 硬件地址长度 %d 非法（最大 16）", p.HLen)
	}
	copy(p.CHAddr[:], b[28:44])
	rawSName := b[44:108]
	rawFile := b[108:236]
	if err := p.Options.decode(b[minPacketLen:]); err != nil {
		return nil, err
	}
	// option 52：sname / file 字段被重载为 options。
	overload := byte(0)
	if d, ok := p.Options.Get(OptOptionOverload); ok && len(d) > 0 {
		overload = d[0]
	}
	if overload&1 != 0 {
		if err := p.Options.decode(rawFile); err != nil {
			return nil, err
		}
	} else {
		p.File = cString(rawFile)
	}
	if overload&2 != 0 {
		if err := p.Options.decode(rawSName); err != nil {
			return nil, err
		}
	} else {
		p.SName = cString(rawSName)
	}
	return p, nil
}

// Encode 把报文编码成字节流。
func (p *Packet) Encode() ([]byte, error) {
	if len(p.SName) > 63 {
		return nil, fmt.Errorf("dhcp: sname 字段最长 63 字节，实际 %d", len(p.SName))
	}
	if len(p.File) > 127 {
		return nil, fmt.Errorf("dhcp: file 字段最长 127 字节，实际 %d", len(p.File))
	}
	buf := make([]byte, minPacketLen)
	buf[0] = p.Op
	buf[1] = p.HType
	buf[2] = p.HLen
	buf[3] = p.Hops
	binary.BigEndian.PutUint32(buf[4:8], p.XID)
	binary.BigEndian.PutUint16(buf[8:10], p.Secs)
	binary.BigEndian.PutUint16(buf[10:12], p.Flags)
	copy(buf[12:16], ip4(p.CIAddr))
	copy(buf[16:20], ip4(p.YIAddr))
	copy(buf[20:24], ip4(p.SIAddr))
	copy(buf[24:28], ip4(p.GIAddr))
	copy(buf[28:44], p.CHAddr[:])
	copy(buf[44:108], p.SName)
	copy(buf[108:236], p.File)
	binary.BigEndian.PutUint32(buf[236:240], magicCookie)

	for _, o := range p.Options.list {
		switch o.Code {
		case OptPad:
			buf = append(buf, 0)
			continue
		case OptEnd:
			continue
		}
		if len(o.Data) > 255 {
			return nil, fmt.Errorf("dhcp: 选项 0x%02x 数据长度 %d 超过 255", o.Code, len(o.Data))
		}
		buf = append(buf, o.Code, byte(len(o.Data)))
		buf = append(buf, o.Data...)
	}
	buf = append(buf, OptEnd)
	for len(buf) < minPacketSize {
		buf = append(buf, 0)
	}
	return buf, nil
}

// MsgType 返回报文类型，未携带 option 53 时返回 0。
func (p *Packet) MsgType() MsgType {
	if v, ok := p.Options.Uint8(OptMsgType); ok {
		return MsgType(v)
	}
	return 0
}

// MAC 返回客户端的 MAC 地址（小写冒号分隔）。
func (p *Packet) MAC() string {
	if p.HLen == 0 || p.HLen > 16 {
		return ""
	}
	hw := net.HardwareAddr(p.CHAddr[:p.HLen])
	return model.NormalizeMAC(hw.String())
}

// Hostname 返回客户端上报的主机名（option 12）。
func (p *Packet) Hostname() string {
	s, _ := p.Options.String(OptHostName)
	return strings.TrimSpace(s)
}

// UUID 返回客户端上报的 SMBIOS UUID（option 97）。
func (p *Packet) UUID() string {
	d, ok := p.Options.Get(OptClientUUID)
	if !ok || len(d) == 0 {
		return ""
	}
	if len(d) >= 17 && d[0] == 0 {
		d = d[1:]
	}
	if len(d) != 16 {
		return ""
	}
	return fmt.Sprintf("%x-%x-%x-%x-%x", d[0:4], d[4:6], d[6:8], d[8:10], d[10:16])
}

// VendorClass 返回客户端厂商类别（option 60）。
func (p *Packet) VendorClass() string {
	s, _ := p.Options.String(OptVendorClass)
	return strings.TrimSpace(s)
}

// IsPXEClient 报告这是否是一个 PXE / HTTPBoot 客户机。
func (p *Packet) IsPXEClient() bool {
	u := strings.ToUpper(p.VendorClass())
	if strings.HasPrefix(u, "PXECLIENT") || strings.HasPrefix(u, "HTTPCLIENT") {
		return true
	}
	return p.Options.Has(OptClientArch)
}

// Broadcast 报告客户端是否要求广播回包。
func (p *Packet) Broadcast() bool { return p.Flags&0x8000 != 0 }

// ClientArch 由 option 93（或 option 60 里的 Arch 字段）推导客户机架构。
func (p *Packet) ClientArch() model.Arch {
	if d, ok := p.Options.Get(OptClientArch); ok && len(d) >= 2 {
		if a := archFromOption93(binary.BigEndian.Uint16(d[:2])); a != model.ArchUnknown {
			return a
		}
	}
	return archFromVendorClass(p.VendorClass())
}

// archFromOption93 在 model.ArchFromOption93 的基础上补上旧编号 9。
//
// model 包只映射了 0/6/7/11/16/19/27；部分老 UEFI 固件上报 9 表示
// 「x86_64 UEFI（旧编号）」，这里把它归到 ArchUEFI64，避免误判成 unknown。
func archFromOption93(v uint16) model.Arch {
	if a := model.ArchFromOption93(v); a != model.ArchUnknown {
		return a
	}
	if v == 9 {
		return model.ArchUEFI64
	}
	return model.ArchUnknown
}

// archFromVendorClass 从 "PXEClient:Arch:00007:UNDI:003016" 这类字符串里解析架构。
func archFromVendorClass(vc string) model.Arch {
	u := strings.ToUpper(strings.TrimSpace(vc))
	if u == "" {
		return model.ArchUnknown
	}
	if i := strings.Index(u, "ARCH:"); i >= 0 {
		rest := u[i+len("ARCH:"):]
		if j := strings.IndexByte(rest, ':'); j >= 0 {
			rest = rest[:j]
		}
		rest = strings.TrimSpace(rest)
		if n, err := strconv.Atoi(rest); err == nil {
			if a := archFromOption93(uint16(n)); a != model.ArchUnknown {
				return a
			}
		}
	}
	if strings.HasPrefix(u, "HTTPCLIENT") {
		// 未带 Arch 字段的 HTTPBoot 客户端，默认按 x86_64 处理。
		return model.ArchUEFI64HTTP
	}
	if strings.HasPrefix(u, "PXECLIENT") {
		// PXE 规范里不带 Arch 字段的老客户端视为 Legacy BIOS。
		return model.ArchBIOS
	}
	return model.ArchUnknown
}

// ParamRequestList 返回客户端请求的参数列表（option 55）。
func (p *Packet) ParamRequestList() []byte {
	d, _ := p.Options.Get(OptParamRequest)
	return d
}

// RelayAgent 返回中继代理信息（option 82），不存在时返回 nil。
func (p *Packet) RelayAgent() []byte {
	d, _ := p.Options.Get(OptRelayAgent)
	return d
}
