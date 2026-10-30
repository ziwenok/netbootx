package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/wupengcheng/netbootx/internal/model"
)

func newTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	bootDir := filepath.Join(dir, "boot")
	if err := os.MkdirAll(bootDir, 0o755); err != nil {
		t.Fatal(err)
	}
	s, err := New(filepath.Join(dir, "data"), bootDir)
	if err != nil {
		t.Fatal(err)
	}
	return s, dir
}

func TestDefaultSettings(t *testing.T) {
	s, _ := newTestStore(t)
	cfg := s.Settings()
	if cfg.HTTPPort != 16000 {
		t.Errorf("默认 HTTP 端口 = %d, 期望 16000", cfg.HTTPPort)
	}
	if cfg.WebPort != 16080 {
		t.Errorf("默认 Web 端口 = %d, 期望 16080", cfg.WebPort)
	}
	if cfg.DHCPMode != "proxy" {
		t.Errorf("默认 DHCP 模式 = %q, 期望 proxy", cfg.DHCPMode)
	}
	if len(cfg.DNS) == 0 {
		t.Error("默认 DNS 不应为空")
	}
}

func TestSaveAndReloadSettings(t *testing.T) {
	s, dir := newTestStore(t)
	cfg := s.Settings()
	cfg.ServerIP = "10.0.0.5"
	cfg.HTTPPort = 16111
	cfg.DNS = []string{"8.8.8.8"}
	if err := s.SaveSettings(cfg); err != nil {
		t.Fatal(err)
	}

	// 文件应存在且是合法 JSON
	p := filepath.Join(dir, "data", "settings.json")
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("配置文件未写入: %v", err)
	}
	var check map[string]any
	if err := json.Unmarshal(b, &check); err != nil {
		t.Fatalf("配置不是合法 JSON: %v", err)
	}

	// 重新加载
	s2, err := New(filepath.Join(dir, "data"), filepath.Join(dir, "boot"))
	if err != nil {
		t.Fatal(err)
	}
	got := s2.Settings()
	if got.ServerIP != "10.0.0.5" || got.HTTPPort != 16111 {
		t.Errorf("重新加载后配置不对: %+v", got)
	}
	if len(got.DNS) != 1 || got.DNS[0] != "8.8.8.8" {
		t.Errorf("DNS 未正确持久化: %v", got.DNS)
	}
}

func TestCorruptSettingsFallsBackToDefaults(t *testing.T) {
	dir := t.TempDir()
	dataDir := filepath.Join(dir, "data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// 写一个坏文件
	if err := os.WriteFile(filepath.Join(dataDir, "settings.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := New(dataDir, filepath.Join(dir, "boot"))
	if err != nil {
		t.Fatalf("配置损坏时不应导致 New 失败: %v", err)
	}
	// 应回退到默认配置
	if s.Settings().HTTPPort != 16000 {
		t.Error("配置损坏时应回退到默认配置")
	}
	// 应有告警事件
	evs := s.Events(10)
	found := false
	for _, e := range evs {
		if e.Level == "warn" {
			found = true
		}
	}
	if !found {
		t.Error("配置损坏时应记录告警事件")
	}
}

func TestValidateSettings(t *testing.T) {
	base := model.DefaultSettings()
	base.ServerIP = "192.168.1.10"
	base.RangeStart = "192.168.1.100"
	base.RangeEnd = "192.168.1.200"

	cases := []struct {
		name    string
		mutate  func(*model.Settings)
		wantErr bool
	}{
		{"合法配置", func(c *model.Settings) {}, false},
		{"非法 DHCP 模式", func(c *model.Settings) { c.DHCPMode = "xxx" }, true},
		{"HTTP 端口越界", func(c *model.Settings) { c.HTTPPort = 70000 }, true},
		{"HTTP 端口为 0", func(c *model.Settings) { c.HTTPPort = 0 }, true},
		{"HTTP 与 Web 端口冲突", func(c *model.Settings) { c.HTTPPort = c.WebPort }, true},
		{"full 模式缺 IP", func(c *model.Settings) { c.ServerIP = "" }, true},
		{"full 模式缺地址池", func(c *model.Settings) { c.DHCPMode = "full"; c.RangeStart = "" }, true},
		{"proxy 模式不需要地址池", func(c *model.Settings) { c.DHCPMode = "proxy"; c.RangeStart = ""; c.RangeEnd = "" }, false},
		{"external 模式不需要 IP", func(c *model.Settings) { c.DHCPMode = "external"; c.ServerIP = "" }, false},
		{"租约时间过短", func(c *model.Settings) { c.LeaseTime = 10 }, true},
		{"菜单超时为负", func(c *model.Settings) { c.BootMenuTimeout = -1 }, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := base
			c.DNS = append([]string(nil), base.DNS...)
			tc.mutate(&c)
			err := ValidateSettings(&c)
			if tc.wantErr && err == nil {
				t.Error("期望报错，但没有")
			}
			if !tc.wantErr && err != nil {
				t.Errorf("期望通过，但报错: %v", err)
			}
		})
	}
}

func TestUpsertClient(t *testing.T) {
	s, _ := newTestStore(t)

	c := s.UpsertClient(model.Client{MAC: "AA-BB-CC-DD-EE-FF", IP: "192.168.1.10",
		Arch: model.ArchUEFI64, Hostname: "testhost"})
	if c.MAC != "aa:bb:cc:dd:ee:ff" {
		t.Errorf("MAC 未归一化: %q", c.MAC)
	}
	if c.FirstSeen.IsZero() || c.LastSeen.IsZero() {
		t.Error("首次/最后可见时间应被填充")
	}

	// 同 MAC 不同写法应更新同一条
	c2 := s.UpsertClient(model.Client{MAC: "aa:bb:cc:dd:ee:ff", IP: "192.168.1.11"})
	if c2.IP != "192.168.1.11" {
		t.Errorf("IP 未更新: %q", c2.IP)
	}
	if c2.Hostname != "testhost" {
		t.Error("空字段不应覆盖已有值")
	}
	if len(s.Clients()) != 1 {
		t.Errorf("客户机数量 = %d, 期望 1", len(s.Clients()))
	}
}

func TestClientStateTransitions(t *testing.T) {
	s, _ := newTestStore(t)
	mac := "aa:bb:cc:dd:ee:ff"
	s.UpsertClient(model.Client{MAC: mac})

	for _, st := range []model.ClientState{model.StateTFTP, model.StateMenu, model.StateBooting} {
		s.SetClientState(mac, st)
		c, ok := s.GetClient(mac)
		if !ok {
			t.Fatal("客户机不存在")
		}
		if c.State != st {
			t.Errorf("状态 = %q, 期望 %q", c.State, st)
		}
	}
}

func TestClientsSortedByLastSeen(t *testing.T) {
	s, _ := newTestStore(t)
	s.UpsertClient(model.Client{MAC: "00:00:00:00:00:01"})
	s.UpsertClient(model.Client{MAC: "00:00:00:00:00:02"})
	// 再更新第一台，它应该排到最前
	s.UpsertClient(model.Client{MAC: "00:00:00:00:00:01"})

	cs := s.Clients()
	if len(cs) != 2 {
		t.Fatal("应有 2 台")
	}
	if cs[0].MAC != "00:00:00:00:00:01" {
		t.Errorf("最近活动的应排最前, 实际 %q", cs[0].MAC)
	}
}

func TestClearClients(t *testing.T) {
	s, _ := newTestStore(t)
	s.UpsertClient(model.Client{MAC: "aa:bb:cc:dd:ee:01"})
	s.UpsertClient(model.Client{MAC: "aa:bb:cc:dd:ee:02"})
	s.ClearClients()
	if n := len(s.Clients()); n != 0 {
		t.Errorf("清空后 = %d 台", n)
	}
}

func TestEventRingBufferWrap(t *testing.T) {
	s, _ := newTestStore(t)
	for i := 0; i < EventCap+10; i++ {
		s.AddEvent(model.Event{Level: "info", Msg: "e"})
	}
	evs := s.Events(0)
	if len(evs) != EventCap {
		t.Fatalf("事件数 = %d, 期望 %d", len(evs), EventCap)
	}
}

func TestEventsLimit(t *testing.T) {
	s, _ := newTestStore(t)
	for i := 0; i < 50; i++ {
		s.AddEvent(model.Event{Level: "info", Msg: "x"})
	}
	if n := len(s.Events(10)); n != 10 {
		t.Errorf("限制 10 条实际返回 %d", n)
	}
	if n := len(s.Events(0)); n != 50 {
		t.Errorf("限制 0 应返回全部, 实际 %d", n)
	}
	if n := len(s.Events(1000)); n != 50 {
		t.Errorf("限制超过总数应返回全部, 实际 %d", n)
	}
}

func TestBootFileFor(t *testing.T) {
	s, _ := newTestStore(t)
	cases := map[model.Arch]string{
		model.ArchBIOS:        "bios/undionly.kpxe",
		model.ArchUEFI32:      "i386/ipxe.efi",
		model.ArchUEFI64:      "x86_64/ipxe.efi",
		model.ArchUEFI64HTTP:  "x86_64/ipxe.efi",
		model.ArchARM64:       "arm64/ipxe.efi",
		model.ArchARM64HTTP:   "arm64/ipxe.efi",
		model.ArchLoongArch64: "loong64/ipxe.efi",
	}
	for a, want := range cases {
		if got := s.BootFileFor(a); got != want {
			t.Errorf("BootFileFor(%s) = %q, 期望 %q", a, got, want)
		}
	}
}

func TestBootFileExistsAndMissing(t *testing.T) {
	s, dir := newTestStore(t)
	bootDir := filepath.Join(dir, "boot")

	// 一开始全缺
	if m := s.MissingBootFiles(); len(m) != 4 {
		t.Errorf("应缺 4 个引导文件, 实际 %d: %v", len(m), m)
	}

	for _, f := range []string{"bios/undionly.kpxe", "x86_64/ipxe.efi", "arm64/ipxe.efi", "wimboot"} {
		p := filepath.Join(bootDir, filepath.FromSlash(f))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if m := s.MissingBootFiles(); len(m) != 0 {
		t.Errorf("文件齐了仍报缺失: %v", m)
	}
	if !s.BootFileExists("wimboot") {
		t.Error("wimboot 应存在")
	}
	// 空文件视为缺失
	_ = os.WriteFile(filepath.Join(bootDir, "wimboot"), nil, 0o644)
	if s.BootFileExists("wimboot") {
		t.Error("空文件应视为缺失")
	}
}

func TestVendorFromMAC(t *testing.T) {
	cases := map[string]string{
		"00:50:56:12:34:56": "VMware",
		"52:54:00:12:34:56": "QEMU/KVM",
		"08:00:27:12:34:56": "VirtualBox",
		"ff:ff:ff:ff:ff:ff": "",
	}
	for mac, want := range cases {
		if got := VendorFromMAC(mac); got != want {
			t.Errorf("VendorFromMAC(%q) = %q, 期望 %q", mac, got, want)
		}
	}
}

func TestConcurrentAccess(t *testing.T) {
	s, _ := newTestStore(t)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(3)
		go func(i int) {
			defer wg.Done()
			s.UpsertClient(model.Client{MAC: "aa:bb:cc:dd:ee:ff"})
		}(i)
		go func(i int) {
			defer wg.Done()
			s.AddEvent(model.Event{Level: "info", Msg: "x"})
		}(i)
		go func(i int) {
			defer wg.Done()
			_ = s.Settings()
			_ = s.Clients()
			_ = s.Events(10)
		}(i)
	}
	wg.Wait()
	if n := len(s.Clients()); n != 1 {
		t.Errorf("并发后客户机数 = %d, 期望 1", n)
	}
}

func TestEnsureDirs(t *testing.T) {
	s, dir := newTestStore(t)
	cfg := s.Settings()
	cfg.ServerIP = "192.168.1.10"
	cfg.ISODir = "iso"
	cfg.UserDir = "user"
	cfg.DataDir = "data"
	cfg.LogDir = "log"
	if err := s.SaveSettings(cfg); err != nil {
		t.Fatal(err)
	}
	if err := s.EnsureDirs(dir); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"iso", "user", "data", "log"} {
		if st, err := os.Stat(filepath.Join(dir, d)); err != nil || !st.IsDir() {
			t.Errorf("目录 %s 未创建", d)
		}
	}
}

func TestNormalizeMACVariants(t *testing.T) {
	cases := map[string]string{
		"AA:BB:CC:DD:EE:FF": "aa:bb:cc:dd:ee:ff",
		"aa-bb-cc-dd-ee-ff": "aa:bb:cc:dd:ee:ff",
		"a:b:c:d:e:f":       "0a:0b:0c:0d:0e:0f",
	}
	for in, want := range cases {
		if got := model.NormalizeMAC(in); got != want {
			t.Errorf("NormalizeMAC(%q) = %q, 期望 %q", in, got, want)
		}
	}
}

func TestSettingsCopyIsolation(t *testing.T) {
	s, _ := newTestStore(t)
	cfg := s.Settings()
	cfg.DNS[0] = "1.1.1.1"
	// 修改返回的副本不应影响内部状态
	if s.Settings().DNS[0] == "1.1.1.1" {
		t.Error("Settings() 应返回深拷贝")
	}
}
