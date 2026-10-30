package dhcp

import (
	"net"
	"testing"
	"time"
)

func newTestTable(t *testing.T) *LeaseTable {
	t.Helper()
	tb, err := NewLeaseTable(net.ParseIP("192.168.1.100"), net.ParseIP("192.168.1.103"), time.Hour)
	if err != nil {
		t.Fatalf("创建租约表失败: %v", err)
	}
	return tb
}

func TestLeaseTableAllocate(t *testing.T) {
	tb := newTestTable(t)
	if tb.Size() != 4 {
		t.Fatalf("池容量 = %d，期望 4", tb.Size())
	}
	seen := map[string]bool{}
	for i := 0; i < 4; i++ {
		mac := net.HardwareAddr{0, 0, 0, 0, 0, byte(i + 1)}.String()
		ip, err := tb.Allocate(mac)
		if err != nil {
			t.Fatalf("第 %d 次分配失败: %v", i, err)
		}
		if seen[ip.String()] {
			t.Fatalf("分配了重复地址 %v", ip)
		}
		seen[ip.String()] = true
		if !tb.InPool(ip) {
			t.Errorf("%v 不在池内", ip)
		}
	}
	if _, err := tb.Allocate("00:00:00:00:00:ff"); err == nil {
		t.Fatal("池耗尽时应报错")
	}

	// 同一 MAC 重复分配应复用同一地址
	ip1, _ := tb.Allocate("00:00:00:00:00:01")
	ip2, _ := tb.Allocate("00:00:00:00:00:01")
	if !ip1.Equal(ip2) {
		t.Errorf("同一 MAC 分到不同地址: %v vs %v", ip1, ip2)
	}
}

func TestLeaseTableOfferBindRelease(t *testing.T) {
	tb := newTestTable(t)
	mac := "aa:bb:cc:dd:ee:ff"
	ip := net.ParseIP("192.168.1.101")

	if err := tb.Offered(mac, ip, "uefi64", "host1"); err != nil {
		t.Fatalf("Offered 失败: %v", err)
	}
	l, ok := tb.Get(mac)
	if !ok || l.State != LeaseStateOffered || l.IP != "192.168.1.101" {
		t.Fatalf("租约 = %+v / %v", l, ok)
	}
	if l.Arch != "uefi64" || l.Hostname != "host1" {
		t.Errorf("租约字段 = %+v", l)
	}

	if err := tb.Bind(mac, ip, "uefi64", "host1"); err != nil {
		t.Fatalf("Bind 失败: %v", err)
	}
	l, _ = tb.Get(mac)
	if l.State != LeaseStateBound {
		t.Errorf("状态 = %q，期望 bound", l.State)
	}

	if !tb.Release(mac) {
		t.Error("Release 应返回 true")
	}
	if _, ok := tb.Get(mac); ok {
		t.Error("释放后仍能查到租约")
	}
	if tb.Release(mac) {
		t.Error("重复 Release 应返回 false")
	}
}

func TestLeaseTableConflict(t *testing.T) {
	tb := newTestTable(t)
	ip := net.ParseIP("192.168.1.100")
	if err := tb.Offered("aa:aa:aa:aa:aa:01", ip, "bios", ""); err != nil {
		t.Fatal(err)
	}
	// 另一个 MAC 抢同一地址应失败
	if err := tb.Offered("aa:aa:aa:aa:aa:02", ip, "bios", ""); err == nil {
		t.Error("地址冲突时应报错")
	}
	if err := tb.Bind("aa:aa:aa:aa:aa:02", ip, "bios", ""); err == nil {
		t.Error("Bind 到已被占用的地址应报错")
	}
	// 同一 MAC 换地址应释放旧地址
	newIP := net.ParseIP("192.168.1.102")
	if err := tb.Offered("aa:aa:aa:aa:aa:01", newIP, "bios", ""); err != nil {
		t.Fatal(err)
	}
	if _, ok := tb.ByIP(ip); ok {
		t.Error("换地址后旧地址未释放")
	}
}

func TestLeaseTableExpiry(t *testing.T) {
	tb := newTestTable(t)
	now := time.Now()
	tb.now = func() time.Time { return now }
	mac := "11:22:33:44:55:66"
	ip := net.ParseIP("192.168.1.100")
	if err := tb.Bind(mac, ip, "bios", ""); err != nil {
		t.Fatal(err)
	}
	if _, ok := tb.Get(mac); !ok {
		t.Fatal("刚绑定的租约应存在")
	}
	// 时间前进 2 小时，租约应过期并被回收
	now = now.Add(2 * time.Hour)
	if _, ok := tb.Get(mac); ok {
		t.Error("过期租约应被清理")
	}
	if tb.Size() != 4 {
		t.Errorf("池容量变化: %d", tb.Size())
	}
	// 过期后地址可重新分配
	got, err := tb.Allocate("99:99:99:99:99:99")
	if err != nil {
		t.Fatal(err)
	}
	if !got.Equal(ip) {
		t.Errorf("过期地址未被复用: %v", got)
	}
}

func TestLeaseTableDeleteAndAll(t *testing.T) {
	tb := newTestTable(t)
	tb.SetHashStart(false)
	for i, mac := range []string{"00:00:00:00:00:01", "00:00:00:00:00:02"} {
		ip := net.IPv4(192, 168, 1, byte(100+i)).To4()
		if err := tb.Bind(mac, ip, "bios", ""); err != nil {
			t.Fatal(err)
		}
	}
	all := tb.All()
	if len(all) != 2 {
		t.Fatalf("租约数 = %d，期望 2", len(all))
	}
	// 按 IP 升序
	if all[0].IP != "192.168.1.100" || all[1].IP != "192.168.1.101" {
		t.Errorf("排序错误: %+v", all)
	}
	if !tb.Delete(net.ParseIP("192.168.1.100")) {
		t.Error("Delete 应返回 true")
	}
	if tb.Delete(net.ParseIP("192.168.1.100")) {
		t.Error("重复 Delete 应返回 false")
	}
	if len(tb.All()) != 1 {
		t.Errorf("删除后租约数 = %d", len(tb.All()))
	}
	tb.Reset()
	if len(tb.All()) != 0 {
		t.Error("Reset 后租约表应为空")
	}
}

func TestLeaseTableValidation(t *testing.T) {
	if _, err := NewLeaseTable(net.ParseIP("192.168.1.200"), net.ParseIP("192.168.1.100"), time.Hour); err == nil {
		t.Error("起止倒置应报错")
	}
	if _, err := NewLeaseTable(net.ParseIP("fe80::1"), net.ParseIP("fe80::2"), time.Hour); err == nil {
		t.Error("IPv6 地址应报错")
	}
	tb, err := NewLeaseTable(net.ParseIP("10.0.0.1"), net.ParseIP("10.0.0.9"), 0)
	if err != nil {
		t.Fatal(err)
	}
	if tb.Size() != 9 {
		t.Errorf("池容量 = %d，期望 9", tb.Size())
	}
	if !tb.Start().Equal(net.ParseIP("10.0.0.1")) || !tb.End().Equal(net.ParseIP("10.0.0.9")) {
		t.Errorf("边界 = %v - %v", tb.Start(), tb.End())
	}
	if tb.InPool(net.ParseIP("10.0.0.10")) {
		t.Error("10.0.0.10 不应在池内")
	}
}

func TestLeaseTableHashStart(t *testing.T) {
	// 不同 MAC 在哈希起步策略下应尽量分散到不同地址。
	tb, err := NewLeaseTable(net.ParseIP("192.168.1.1"), net.ParseIP("192.168.1.250"), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	tb.SetHashStart(true)
	seen := map[string]bool{}
	for i := 0; i < 20; i++ {
		mac := net.HardwareAddr{0x02, 0, 0, 0, 0, byte(i)}.String()
		ip, err := tb.Allocate(mac)
		if err != nil {
			t.Fatal(err)
		}
		if seen[ip.String()] {
			t.Fatalf("哈希起步策略下出现重复地址 %v", ip)
		}
		seen[ip.String()] = true
	}
}

func TestLeaseTableConcurrent(t *testing.T) {
	tb, err := NewLeaseTable(net.ParseIP("10.1.0.1"), net.ParseIP("10.1.0.200"), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	for w := 0; w < 8; w++ {
		go func(w int) {
			defer func() { done <- struct{}{} }()
			for i := 0; i < 50; i++ {
				mac := net.HardwareAddr{0x02, 0, 0, byte(w), 0, byte(i)}.String()
				ip, err := tb.Allocate(mac)
				if err != nil {
					continue
				}
				_ = tb.Offered(mac, ip, "bios", "")
				_ = tb.Bind(mac, ip, "bios", "")
				_, _ = tb.Get(mac)
				_ = tb.All()
				_, _ = tb.ByIP(ip)
			}
		}(w)
	}
	for i := 0; i < 8; i++ {
		<-done
	}
	// 所有存活租约的 IP 必须唯一
	all := tb.All()
	seen := map[string]bool{}
	for _, l := range all {
		if seen[l.IP] {
			t.Fatalf("并发下出现重复租约 IP %s", l.IP)
		}
		seen[l.IP] = true
	}
}
