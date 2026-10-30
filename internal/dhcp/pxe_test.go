package dhcp

import (
	"bytes"
	"net"
	"testing"
)

func TestPXEVendorRoundTrip(t *testing.T) {
	in := PXEOptions{
		DiscoveryControl: DefaultPXEDiscoveryControl,
		HasBootItem:      true,
		BootItem:         0x0007,
		MenuPrompt:       "NetBootX 网络引导",
		Menu: []PXEMenuEntry{
			{Description: "从硬盘启动", BootItem: 0x0001},
			{Description: "NetBootX 菜单", BootItem: 0x0007},
		},
		MTFTP: &MTFTPHint{
			IP:        net.ParseIP("192.168.1.1").To4(),
			BlockSize: 1456,
			Timeout:   3,
		},
	}
	raw := EncodePXEVendor(in)
	if len(raw) == 0 {
		t.Fatal("编码结果为空")
	}
	got, err := DecodePXEVendor(raw)
	if err != nil {
		t.Fatalf("解码失败: %v", err)
	}
	if got.DiscoveryControl&DefaultPXEDiscoveryControl != DefaultPXEDiscoveryControl {
		t.Errorf("discovery control = %#x，应包含默认位 %#x", got.DiscoveryControl, DefaultPXEDiscoveryControl)
	}
	if !got.HasBootItem || got.BootItem != 7 {
		t.Errorf("boot item = %v/%d", got.HasBootItem, got.BootItem)
	}
	if got.MenuPrompt != "NetBootX 网络引导" {
		t.Errorf("菜单提示语 = %q", got.MenuPrompt)
	}
	if len(got.Menu) != 2 || got.Menu[1].Description != "NetBootX 菜单" || got.Menu[1].BootItem != 7 {
		t.Errorf("菜单项 = %+v", got.Menu)
	}
	if got.MTFTP == nil {
		t.Fatal("MTFTP 提示丢失")
	}
	if !got.MTFTP.IP.Equal(net.ParseIP("192.168.1.1")) {
		t.Errorf("MTFTP IP = %v", got.MTFTP.IP)
	}
	if got.MTFTP.BlockSize != 1456 || got.MTFTP.Timeout != 3 {
		t.Errorf("MTFTP 参数 = %+v", got.MTFTP)
	}
}

func TestEncodePXEVendorDiscoveryControl(t *testing.T) {
	// 调用方显式给 0 时，编码器不擅自填值（默认值由 Server 层负责）。
	raw := EncodePXEVendor(PXEOptions{})
	got, err := DecodePXEVendor(raw)
	if err != nil {
		t.Fatal(err)
	}
	if got.DiscoveryControl != 0 {
		t.Errorf("discovery control = %#x，期望 0", got.DiscoveryControl)
	}

	// 有菜单时自动置上「只认引导菜单」位。
	raw = EncodePXEVendor(PXEOptions{
		DiscoveryControl: DefaultPXEDiscoveryControl,
		Menu:             []PXEMenuEntry{{Description: "x", BootItem: 1}},
	})
	got, err = DecodePXEVendor(raw)
	if err != nil {
		t.Fatal(err)
	}
	if got.DiscoveryControl&PXEDiscoveryUseBootMenu == 0 {
		t.Errorf("有菜单时 discovery control 应含 UseBootMenu 位，实际 %#x", got.DiscoveryControl)
	}
	// 默认值必须同时关闭广播与多播发现
	if DefaultPXEDiscoveryControl&PXEDiscoveryDisableBroadcast == 0 ||
		DefaultPXEDiscoveryControl&PXEDiscoveryDisableMulticast == 0 {
		t.Errorf("DefaultPXEDiscoveryControl = %#x，应关闭广播和多播发现", DefaultPXEDiscoveryControl)
	}
}

func TestPXEVendorSubOptionLayout(t *testing.T) {
	// 直接校验字节布局：6 = DISCOVERY_CONTROL，71 = BOOT_ITEM，9 = BOOT_MENU。
	raw := EncodePXEVendor(PXEOptions{
		DiscoveryControl: DefaultPXEDiscoveryControl,
		HasBootItem:      true,
		BootItem:         0x0102,
	})
	if raw[0] != PXEOptDiscoveryControl || raw[1] != 1 {
		t.Fatalf("第一个子选项应为 (6,1)，实际 (%d,%d)", raw[0], raw[1])
	}
	if raw[2] != DefaultPXEDiscoveryControl {
		t.Errorf("discovery control 值 = %#x，期望 %#x", raw[2], DefaultPXEDiscoveryControl)
	}
	if raw[3] != PXEOptBootItem || raw[4] != 2 {
		t.Fatalf("第二个子选项应为 (71,2)，实际 (%d,%d)", raw[3], raw[4])
	}
	if !bytes.Equal(raw[5:7], []byte{0x01, 0x02}) {
		t.Errorf("boot item 大端编码错误: %v", raw[5:7])
	}
}

func TestDecodePXEVendorErrors(t *testing.T) {
	if _, err := DecodePXEVendor([]byte{0x09, 0x05, 0x00}); err == nil {
		t.Error("子选项长度越界应报错")
	}
	if _, err := DecodePXEVendor([]byte{0x09}); err == nil {
		t.Error("子选项缺少长度字节应报错")
	}
	// 空数据应安全返回零值
	got, err := DecodePXEVendor(nil)
	if err != nil || got.DiscoveryControl != 0 {
		t.Errorf("空数据解码 = %+v / %v", got, err)
	}
	// padding 与结束符
	if _, err := DecodePXEVendor([]byte{0, 0, 255, 6, 1, 8}); err != nil {
		t.Errorf("padding/结束符处理失败: %v", err)
	}
}

func TestDecodePXEBootMenuErrors(t *testing.T) {
	if _, _, err := decodePXEBootMenu(nil); err == nil {
		t.Error("空菜单应报错")
	}
	if _, _, err := decodePXEBootMenu([]byte{0, 'a'}); err == nil {
		t.Error("缺少提示语结束符应报错")
	}
	if _, _, err := decodePXEBootMenu([]byte{0, 'a', 0, 0, 0}); err == nil {
		t.Error("菜单项缺少引导项编号应报错")
	}
	// 只有提示语、没有菜单项也是合法的
	if prompt, entries, err := decodePXEBootMenu([]byte{0, 'a', 'b', 0, 0, 1}); err != nil || prompt != "ab" || len(entries) != 0 {
		t.Errorf("空菜单项解析 = %q/%v/%v", prompt, entries, err)
	}
}
