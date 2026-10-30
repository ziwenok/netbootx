package dhcp

import (
	"encoding/binary"
	"fmt"
	"net"
)

// option 43 内部的 PXE 子选项编号（PXE 2.1 规范）。
const (
	PXEOptMTFTPIP          byte = 1  // MTFTP 服务器 IP
	PXEOptMTFTPClientPort  byte = 2  // MTFTP 客户端端口
	PXEOptMTFTPServerPort  byte = 3  // MTFTP 服务器端口
	PXEOptMTFTPBlockSize   byte = 4  // MTFTP 块大小
	PXEOptMTFTPTimeout     byte = 5  // MTFTP 超时
	PXEOptDiscoveryControl byte = 6  // 引导服务器发现方式
	PXEOptMulticastAddr    byte = 7  // 多播地址
	PXEOptBootServers      byte = 8  // 引导服务器列表
	PXEOptBootMenu         byte = 9  // PXE 引导菜单
	PXEOptMenuPrompt       byte = 10 // 菜单提示语
	PXEOptBootItem         byte = 71 // PXE 引导项
)

// PXE_DISCOVERY_CONTROL（子选项 6）的位定义。
//
// 注意：PXE 2.1 规范里这 4 个位都是「禁止 / 限定」语义，置位代表限制而不是启用：
//
//	bit0 0x01 —— 禁止广播方式发现引导服务器
//	bit1 0x02 —— 禁止多播方式发现引导服务器
//	bit2 0x04 —— 只接受 PXE 引导服务器列表（子选项 8）里列出的服务器
//	bit3 0x08 —— 只接受 PXE 引导菜单（子选项 9）里列出的引导项
//
// 客户端在没有被限制的情况下会去 4011 端口做「引导服务器发现」；本服务器没有
// 实现 4011，所以默认把广播/多播发现都关掉（见 DefaultPXEDiscoveryControl），
// 让客户端直接用我们下发的 option 67 / file 字段引导。
const (
	PXEDiscoveryDisableBroadcast  byte = 0x01
	PXEDiscoveryDisableMulticast  byte = 0x02
	PXEDiscoveryUseBootServerList byte = 0x04
	PXEDiscoveryUseBootMenu       byte = 0x08
)

// DefaultPXEDiscoveryControl 是默认的 PXE 发现控制值：
// 关闭广播和多播发现，让 PXE ROM 直接使用 DHCP 报文里给的引导文件名，
// 不去 4011 端口找引导服务器（我们没有 4011 服务）。
const DefaultPXEDiscoveryControl = PXEDiscoveryDisableBroadcast | PXEDiscoveryDisableMulticast

// PXEMenuEntry 是 PXE 引导菜单里的一项。
type PXEMenuEntry struct {
	Description string
	BootItem    uint16
}

// MTFTPHint 是 option 43 里的 MTFTP 提示信息（可空）。
type MTFTPHint struct {
	IP         net.IP
	ClientPort uint16
	ServerPort uint16
	BlockSize  uint16
	Timeout    uint8
}

// PXEOptions 是 option 43 里承载的 PXE 厂商选项。
type PXEOptions struct {
	// DiscoveryControl 是引导服务器发现方式位图，0 表示由编码器按内容自动推导。
	DiscoveryControl byte
	// HasBootItem / BootItem 对应 PXE_BOOT_ITEM 子选项。
	HasBootItem bool
	BootItem    uint16
	// MenuPrompt / Menu 对应 PXE_BOOT_MENU 子选项。
	MenuPrompt string
	Menu       []PXEMenuEntry
	// MTFTP 是可选的多播传输提示。
	MTFTP *MTFTPHint
}

// EncodePXEVendor 把 PXE 选项编码成 option 43 的数据部分。
func EncodePXEVendor(o PXEOptions) []byte {
	var buf []byte
	add := func(code byte, data []byte) {
		buf = append(buf, code, byte(len(data)))
		buf = append(buf, data...)
	}

	dc := o.DiscoveryControl
	if len(o.Menu) > 0 {
		// 有引导菜单时告诉客户端「只认菜单里的引导项」。
		dc |= PXEDiscoveryUseBootMenu
	}
	add(PXEOptDiscoveryControl, []byte{dc})

	if o.HasBootItem {
		b := make([]byte, 2)
		binary.BigEndian.PutUint16(b, o.BootItem)
		add(PXEOptBootItem, b)
	}
	if len(o.Menu) > 0 {
		add(PXEOptBootMenu, encodePXEBootMenu(o.MenuPrompt, o.Menu))
	}
	if m := o.MTFTP; m != nil {
		if ip := ip4(m.IP); ip != nil {
			add(PXEOptMTFTPIP, ip)
		}
		if m.ClientPort != 0 {
			b := make([]byte, 2)
			binary.BigEndian.PutUint16(b, m.ClientPort)
			add(PXEOptMTFTPClientPort, b)
		}
		if m.ServerPort != 0 {
			b := make([]byte, 2)
			binary.BigEndian.PutUint16(b, m.ServerPort)
			add(PXEOptMTFTPServerPort, b)
		}
		if m.BlockSize != 0 {
			b := make([]byte, 2)
			binary.BigEndian.PutUint16(b, m.BlockSize)
			add(PXEOptMTFTPBlockSize, b)
		}
		if m.Timeout != 0 {
			add(PXEOptMTFTPTimeout, []byte{m.Timeout})
		}
	}
	return buf
}

// encodePXEBootMenu 按 PXE 2.1 规范编码 PXE_BOOT_MENU 子选项。
//
// 结构：菜单类型(1 字节，0=简单菜单) + 提示语(NUL 结尾) +
// 若干菜单项(项类型 2 字节大端，0=有引导项、1=列表结束；有引导项时后跟
// 引导项号 2 字节 + 描述(NUL 结尾))。
func encodePXEBootMenu(prompt string, entries []PXEMenuEntry) []byte {
	if prompt == "" {
		prompt = "NetBootX"
	}
	buf := []byte{0} // 菜单类型：0 = 简单菜单
	buf = append(buf, prompt...)
	buf = append(buf, 0)
	for _, e := range entries {
		buf = append(buf, 0, 0) // 项类型 0 = 有引导项
		buf = append(buf, byte(e.BootItem>>8), byte(e.BootItem))
		d := e.Description
		if d == "" {
			d = fmt.Sprintf("Boot Item %d", e.BootItem)
		}
		buf = append(buf, d...)
		buf = append(buf, 0)
	}
	buf = append(buf, 0, 1) // 项类型 1 = 列表结束
	return buf
}

// DecodePXEVendor 解析 option 43 的 PXE 子选项。
func DecodePXEVendor(b []byte) (PXEOptions, error) {
	var o PXEOptions
	for i := 0; i < len(b); {
		code := b[i]
		if code == 0 { // padding
			i++
			continue
		}
		if code == 255 {
			break
		}
		if i+1 >= len(b) {
			return o, fmt.Errorf("dhcp: PXE 子选项 0x%02x 缺少长度字节", code)
		}
		l := int(b[i+1])
		if i+2+l > len(b) {
			return o, fmt.Errorf("dhcp: PXE 子选项 0x%02x 长度 %d 越界", code, l)
		}
		data := b[i+2 : i+2+l]
		switch code {
		case PXEOptDiscoveryControl:
			if len(data) > 0 {
				o.DiscoveryControl = data[0]
			}
		case PXEOptBootItem:
			if len(data) >= 2 {
				o.HasBootItem = true
				o.BootItem = binary.BigEndian.Uint16(data)
			}
		case PXEOptBootMenu:
			prompt, entries, err := decodePXEBootMenu(data)
			if err != nil {
				return o, err
			}
			o.MenuPrompt = prompt
			o.Menu = entries
		case PXEOptMTFTPIP:
			if len(data) >= 4 {
				if o.MTFTP == nil {
					o.MTFTP = &MTFTPHint{}
				}
				o.MTFTP.IP = net.IP(append([]byte(nil), data[:4]...))
			}
		case PXEOptMTFTPClientPort:
			if len(data) >= 2 {
				if o.MTFTP == nil {
					o.MTFTP = &MTFTPHint{}
				}
				o.MTFTP.ClientPort = binary.BigEndian.Uint16(data)
			}
		case PXEOptMTFTPServerPort:
			if len(data) >= 2 {
				if o.MTFTP == nil {
					o.MTFTP = &MTFTPHint{}
				}
				o.MTFTP.ServerPort = binary.BigEndian.Uint16(data)
			}
		case PXEOptMTFTPBlockSize:
			if len(data) >= 2 {
				if o.MTFTP == nil {
					o.MTFTP = &MTFTPHint{}
				}
				o.MTFTP.BlockSize = binary.BigEndian.Uint16(data)
			}
		case PXEOptMTFTPTimeout:
			if len(data) >= 1 {
				if o.MTFTP == nil {
					o.MTFTP = &MTFTPHint{}
				}
				o.MTFTP.Timeout = data[0]
			}
		}
		i += 2 + l
	}
	return o, nil
}

// decodePXEBootMenu 解析 PXE_BOOT_MENU 子选项。
func decodePXEBootMenu(b []byte) (string, []PXEMenuEntry, error) {
	if len(b) == 0 {
		return "", nil, fmt.Errorf("dhcp: PXE 引导菜单为空")
	}
	pos := 1 // 跳过菜单类型
	end := indexByte(b, 0, pos)
	if end < 0 {
		return "", nil, fmt.Errorf("dhcp: PXE 引导菜单缺少提示语结束符")
	}
	prompt := string(b[pos:end])
	pos = end + 1
	var entries []PXEMenuEntry
	for pos+1 < len(b) {
		typ := binary.BigEndian.Uint16(b[pos : pos+2])
		pos += 2
		if typ != 0 {
			break
		}
		if pos+2 > len(b) {
			return prompt, entries, fmt.Errorf("dhcp: PXE 引导菜单项缺少引导项编号")
		}
		item := binary.BigEndian.Uint16(b[pos : pos+2])
		pos += 2
		e := indexByte(b, 0, pos)
		if e < 0 {
			e = len(b)
		}
		entries = append(entries, PXEMenuEntry{Description: string(b[pos:e]), BootItem: item})
		pos = e + 1
		if pos > len(b) {
			break
		}
	}
	return prompt, entries, nil
}

func indexByte(b []byte, c byte, from int) int {
	for i := from; i < len(b); i++ {
		if b[i] == c {
			return i
		}
	}
	return -1
}
