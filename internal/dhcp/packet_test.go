package dhcp

import (
	"bytes"
	"encoding/binary"
	"net"
	"testing"

	"github.com/wupengcheng/netbootx/internal/model"
)

// testMAC 是测试用的 MAC 地址。
var testMAC = net.HardwareAddr{0x00, 0x0c, 0x29, 0xaa, 0xbb, 0xcc}

// newTestPacket 构造一个带基础选项的请求报文。
func newTestPacket(mt MsgType) *Packet {
	p := &Packet{
		Op:     bootRequest,
		HType:  1,
		HLen:   6,
		XID:    0x12345678,
		Flags:  0x8000,
		CIAddr: net.IPv4zero.To4(),
		YIAddr: net.IPv4zero.To4(),
		SIAddr: net.IPv4zero.To4(),
		GIAddr: net.IPv4zero.To4(),
	}
	copy(p.CHAddr[:], testMAC)
	p.Options.SetUint8(OptMsgType, uint8(mt))
	return p
}

func TestPacketRoundTrip(t *testing.T) {
	p := newTestPacket(MsgDiscover)
	p.Hops = 1
	p.Secs = 12
	p.Options.Set(OptParamRequest, []byte{1, 3, 6, 15, 43, 60, 66, 67, 93, 97})
	p.Options.SetString(OptVendorClass, "PXEClient:Arch:00007:UNDI:003016")
	p.Options.Set(OptClientID, append([]byte{1}, testMAC...))
	p.Options.SetUint16(OptClientArch, 7)
	p.Options.SetString(OptHostName, "pxe-client-01")
	p.Options.SetUint16(OptMaxMsgSize, 1500)
	p.Options.Set(OptClientUUID, append([]byte{0}, bytes.Repeat([]byte{0xab}, 16)...))
	p.Options.Set(OptRelayAgent, []byte{0x01, 0x04, 0xde, 0xad, 0xbe, 0xef})

	raw, err := p.Encode()
	if err != nil {
		t.Fatalf("Encode 失败: %v", err)
	}
	if len(raw) < minPacketSize {
		t.Fatalf("编码结果长度 %d 小于最小报文长度 %d", len(raw), minPacketSize)
	}
	if got := binary.BigEndian.Uint32(raw[236:240]); got != magicCookie {
		t.Fatalf("magic cookie = %#x，期望 %#x", got, magicCookie)
	}

	got, err := DecodePacket(raw)
	if err != nil {
		t.Fatalf("DecodePacket 失败: %v", err)
	}
	if got.Op != bootRequest || got.HType != 1 || got.HLen != 6 || got.Hops != 1 {
		t.Errorf("头部字段不匹配: op=%d htype=%d hlen=%d hops=%d", got.Op, got.HType, got.HLen, got.Hops)
	}
	if got.XID != p.XID || got.Secs != 12 || got.Flags != 0x8000 {
		t.Errorf("xid/secs/flags 不匹配: %#x/%d/%#x", got.XID, got.Secs, got.Flags)
	}
	if got.MAC() != "00:0c:29:aa:bb:cc" {
		t.Errorf("MAC = %q", got.MAC())
	}
	if got.MsgType() != MsgDiscover {
		t.Errorf("MsgType = %v", got.MsgType())
	}
	if got.Hostname() != "pxe-client-01" {
		t.Errorf("Hostname = %q", got.Hostname())
	}
	if got.VendorClass() != "PXEClient:Arch:00007:UNDI:003016" {
		t.Errorf("VendorClass = %q", got.VendorClass())
	}
	if got.ClientArch() != model.ArchUEFI64 {
		t.Errorf("ClientArch = %v，期望 %v", got.ClientArch(), model.ArchUEFI64)
	}
	if !got.IsPXEClient() {
		t.Error("IsPXEClient = false，期望 true")
	}
	if !got.Broadcast() {
		t.Error("Broadcast = false，期望 true")
	}
	if v, ok := got.Options.Uint16(OptMaxMsgSize); !ok || v != 1500 {
		t.Errorf("option 57 = %d/%v", v, ok)
	}
	if v, ok := got.Options.Uint16(OptClientArch); !ok || v != 7 {
		t.Errorf("option 93 = %d/%v", v, ok)
	}
	if got.UUID() != "abababab-abab-abab-abab-abababababab" {
		t.Errorf("UUID = %q", got.UUID())
	}
	if len(got.RelayAgent()) != 6 {
		t.Errorf("option 82 长度 = %d，期望 6", len(got.RelayAgent()))
	}
	if len(got.ParamRequestList()) != 10 {
		t.Errorf("option 55 长度 = %d，期望 10", len(got.ParamRequestList()))
	}
	if len(got.Options.All()) == 0 || got.Options.All()[0].Code != OptMsgType {
		t.Errorf("选项顺序未保持")
	}
}

func TestDecodePacketErrors(t *testing.T) {
	valid, err := newTestPacket(MsgDiscover).Encode()
	if err != nil {
		t.Fatal(err)
	}

	t.Run("报文过短", func(t *testing.T) {
		if _, err := DecodePacket(valid[:100]); err == nil {
			t.Fatal("期望报错，实际通过")
		}
	})
	t.Run("magic cookie 错误", func(t *testing.T) {
		bad := append([]byte(nil), valid...)
		binary.BigEndian.PutUint32(bad[236:240], 0xdeadbeef)
		if _, err := DecodePacket(bad); err == nil {
			t.Fatal("期望报错，实际通过")
		}
	})
	t.Run("hlen 非法", func(t *testing.T) {
		bad := append([]byte(nil), valid...)
		bad[2] = 20
		if _, err := DecodePacket(bad); err == nil {
			t.Fatal("期望报错，实际通过")
		}
	})
	t.Run("选项长度越界", func(t *testing.T) {
		bad := append([]byte(nil), valid[:minPacketLen]...)
		bad = append(bad, 0x63, 200, 0x01)
		if _, err := DecodePacket(bad); err == nil {
			t.Fatal("期望报错，实际通过")
		}
	})
	t.Run("选项缺少长度字节", func(t *testing.T) {
		bad := append([]byte(nil), valid[:minPacketLen]...)
		bad = append(bad, 0x63)
		if _, err := DecodePacket(bad); err == nil {
			t.Fatal("期望报错，实际通过")
		}
	})
	t.Run("pad 被跳过", func(t *testing.T) {
		bad := append([]byte(nil), valid[:minPacketLen]...)
		bad = append(bad, OptPad, OptPad, OptMsgType, 1, 1, OptEnd)
		p, err := DecodePacket(bad)
		if err != nil {
			t.Fatalf("不应报错: %v", err)
		}
		if p.MsgType() != MsgDiscover {
			t.Errorf("MsgType = %v", p.MsgType())
		}
	})
}

func TestDecodeOptionOverload(t *testing.T) {
	// 手工构造：option 52 = 1 表示 file 字段承载选项，option 82 只出现在 file 字段里。
	raw := make([]byte, minPacketSize)
	raw[0] = bootRequest
	raw[1] = 1
	raw[2] = 6
	copy(raw[28:44], testMAC)
	binary.BigEndian.PutUint32(raw[236:240], magicCookie)
	opts := []byte{OptOptionOverload, 1, 1, OptEnd}
	copy(raw[minPacketLen:], opts)
	// file 字段：82 = 01 02 03 04
	copy(raw[108:], []byte{OptRelayAgent, 4, 1, 2, 3, 4, OptEnd})

	got, err := DecodePacket(raw)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if v := got.RelayAgent(); !bytes.Equal(v, []byte{1, 2, 3, 4}) {
		t.Errorf("重载区选项 82 = %v", v)
	}
	if got.File != "" {
		t.Errorf("重载时 File 字段应为空，实际 %q", got.File)
	}
}

func TestEncodeFieldTooLong(t *testing.T) {
	p := newTestPacket(MsgOffer)
	p.SName = string(bytes.Repeat([]byte("a"), 64))
	if _, err := p.Encode(); err == nil {
		t.Error("sname 超长应报错")
	}
	p.SName = ""
	p.File = string(bytes.Repeat([]byte("b"), 128))
	if _, err := p.Encode(); err == nil {
		t.Error("file 超长应报错")
	}
}

func TestOptionsHelpers(t *testing.T) {
	o := NewOptions()
	o.SetUint8(OptMsgType, 1)
	o.SetUint8(OptMsgType, 2) // 覆盖
	if v, _ := o.Uint8(OptMsgType); v != 2 {
		t.Errorf("覆盖失败: %d", v)
	}
	o.SetIP(OptServerID, net.ParseIP("192.168.1.1"))
	if ips := o.IPs(OptServerID); len(ips) != 1 || ips[0].String() != "192.168.1.1" {
		t.Errorf("SetIP/IPs 不匹配: %v", ips)
	}
	o.SetIPs(OptDNSServer, []net.IP{net.ParseIP("8.8.8.8"), net.ParseIP("1.1.1.1")})
	if ips := o.IPs(OptDNSServer); len(ips) != 2 || ips[1].String() != "1.1.1.1" {
		t.Errorf("SetIPs 结果 = %v", ips)
	}
	o.SetString(OptBootfileName, "ipxe.efi")
	if s, _ := o.String(OptBootfileName); s != "ipxe.efi" {
		t.Errorf("String = %q", s)
	}
	o.Del(OptBootfileName)
	if o.Has(OptBootfileName) {
		t.Error("Del 未生效")
	}
	// msgtype / serverid / dns（bootfile 已被 Del）
	if o.Len() != 3 {
		t.Errorf("选项个数 = %d，期望 3", o.Len())
	}
	if o.All() == nil {
		t.Error("All 不应为 nil")
	}
	// RFC 3396：重复选项拼接
	o2 := NewOptions()
	o2.addRaw(OptClientArch, []byte{0})
	o2.addRaw(OptClientArch, []byte{7})
	if d, _ := o2.Get(OptClientArch); !bytes.Equal(d, []byte{0, 7}) {
		t.Errorf("重复选项未拼接: %v", d)
	}
	if _, ok := o.Uint16(OptClientArch); ok {
		t.Error("不存在的选项不应返回值")
	}
	if _, ok := o.Uint32(OptLeaseTime); ok {
		t.Error("不存在的 Uint32 不应返回值")
	}
	var nilOpts *Options
	if nilOpts.Len() != 0 || nilOpts.All() != nil || nilOpts.Has(1) {
		t.Error("nil Options 的只读方法应安全")
	}
	nilOpts.Set(1, nil) // 不应 panic
	nilOpts.Del(1)
}

func TestClientArchBranches(t *testing.T) {
	cases := []struct {
		name   string
		opt93  *uint16
		vendor string
		want   model.Arch
	}{
		{"BIOS 00000", u16(0), "PXEClient:Arch:00000:UNDI:002001", model.ArchBIOS},
		{"IA32 UEFI 00006", u16(6), "PXEClient:Arch:00006:UNDI:003000", model.ArchUEFI32},
		{"x86_64 UEFI 00007", u16(7), "PXEClient:Arch:00007:UNDI:003016", model.ArchUEFI64},
		{"x86_64 UEFI 旧编号 9", u16(9), "", model.ArchUEFI64},
		{"ARM64 UEFI 00011", u16(11), "PXEClient:Arch:00011:UNDI:003000", model.ArchARM64},
		{"x86_64 HTTPBoot 00016", u16(16), "HTTPClient:Arch:00016:UNDI:003000", model.ArchUEFI64HTTP},
		{"ARM64 HTTPBoot 00019", u16(19), "HTTPClient:Arch:00019:UNDI:003000", model.ArchARM64HTTP},
		{"LoongArch64 00027", u16(27), "", model.ArchLoongArch64},
		{"只有 vendor class", nil, "PXEClient:Arch:00007:UNDI:003016", model.ArchUEFI64},
		{"vendor class 里是旧编号 9", nil, "PXEClient:Arch:00009:UNDI:002001", model.ArchUEFI64},
		{"HTTPClient 无 Arch", nil, "HTTPClient", model.ArchUEFI64HTTP},
		{"PXEClient 无 Arch", nil, "PXEClient", model.ArchBIOS},
		{"完全没有线索", nil, "", model.ArchUnknown},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := newTestPacket(MsgDiscover)
			if c.opt93 != nil {
				p.Options.SetUint16(OptClientArch, *c.opt93)
			}
			if c.vendor != "" {
				p.Options.SetString(OptVendorClass, c.vendor)
			}
			if got := p.ClientArch(); got != c.want {
				t.Errorf("ClientArch() = %v，期望 %v", got, c.want)
			}
		})
	}
}

func u16(v uint16) *uint16 { return &v }

func TestMsgTypeString(t *testing.T) {
	cases := map[MsgType]string{
		MsgDiscover: "DISCOVER",
		MsgOffer:    "OFFER",
		MsgRequest:  "REQUEST",
		MsgDecline:  "DECLINE",
		MsgAck:      "ACK",
		MsgNak:      "NAK",
		MsgRelease:  "RELEASE",
		MsgInform:   "INFORM",
		99:          "UNKNOWN(99)",
	}
	for mt, want := range cases {
		if got := mt.String(); got != want {
			t.Errorf("%d.String() = %q，期望 %q", uint8(mt), got, want)
		}
	}
}
