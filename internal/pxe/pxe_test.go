package pxe

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/wupengcheng/netbootx/internal/model"
	"github.com/wupengcheng/netbootx/internal/store"
)

// fakeSubs 构造一套假的下层服务，用于测编排器本身。
type fakeSubs struct {
	mu       sync.Mutex
	started  map[string]bool
	stopped  map[string]bool
	failOn   map[string]error
	isos     []model.ISO
	bootFile map[model.Arch]string
}

func newFakeSubs() *fakeSubs {
	return &fakeSubs{
		started:  map[string]bool{},
		stopped:  map[string]bool{},
		failOn:   map[string]error{},
		bootFile: map[model.Arch]string{},
	}
}

func (f *fakeSubs) subs() Subsystems {
	return Subsystems{
		StartDHCP: func(cfg model.Settings, bff func(model.Arch) string) error {
			f.mu.Lock()
			defer f.mu.Unlock()
			if err := f.failOn["dhcp"]; err != nil {
				return err
			}
			f.started["dhcp"] = true
			f.stopped["dhcp"] = false
			return nil
		},
		StopDHCP: func() error {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.stopped["dhcp"] = true
			return nil
		},
		DHCPInfo: func() string { return "fake" },
		StartTFTP: func(cfg model.Settings, root string) error {
			f.mu.Lock()
			defer f.mu.Unlock()
			if err := f.failOn["tftp"]; err != nil {
				return err
			}
			f.started["tftp"] = true
			return nil
		},
		StopTFTP: func() error {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.stopped["tftp"] = true
			return nil
		},
		TFTPInfo: func() string { return "fake" },
		StartHTTP: func(cfg model.Settings, sf func(string, string) (string, error),
			isos func() []model.ISO, isoDir, userDir string) error {
			f.mu.Lock()
			defer f.mu.Unlock()
			if err := f.failOn["http"]; err != nil {
				return err
			}
			f.started["http"] = true
			return nil
		},
		StopHTTP: func() error {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.stopped["http"] = true
			return nil
		},
		HTTPInfo: func() string { return "fake" },
		ScanISOs: func(dir string) ([]model.ISO, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			return f.isos, nil
		},
	}
}

// newTestServer 造一个带临时目录的编排器。
func newTestServer(t *testing.T) (*Server, *store.Store, *fakeSubs) {
	t.Helper()
	root := t.TempDir()
	for _, d := range []string{"iso", "user", "data", "log", "boot"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// 造出引导文件，否则 Start 会因自检失败
	for _, f := range []string{
		"bios/undionly.kpxe", "x86_64/ipxe.efi", "arm64/ipxe.efi", "wimboot",
	} {
		p := filepath.Join(root, "boot", filepath.FromSlash(f))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("fake boot binary"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	st, err := store.New(filepath.Join(root, "data"), filepath.Join(root, "boot"))
	if err != nil {
		t.Fatal(err)
	}
	fs := newFakeSubs()
	fs.isos = []model.ISO{
		{ID: "abc123", Name: "ubuntu-24.04.iso", RelPath: "ubuntu-24.04.iso",
			Size: 3_000_000_000, Family: model.FamilyLinux, OSName: "Ubuntu 24.04 LTS",
			OSArch: model.ArchUEFI64, Bootable: true},
		{ID: "def456", Name: "win11.iso", RelPath: "win11.iso",
			Size: 5_400_000_000, Family: model.FamilyWindows, OSName: "Windows 11",
			OSArch: model.ArchUEFI64, Bootable: true},
	}
	srv := New(root, st, fs.subs())
	return srv, st, fs
}

func TestStartStopLifecycle(t *testing.T) {
	srv, _, fs := newTestServer(t)

	if srv.Running() {
		t.Fatal("新建的服务不应处于运行状态")
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("启动失败: %v", err)
	}
	if !srv.Running() {
		t.Fatal("启动后 Running() 应为 true")
	}
	if !fs.started["dhcp"] || !fs.started["tftp"] || !fs.started["http"] {
		t.Fatalf("子服务未全部启动: %+v", fs.started)
	}

	if err := srv.Start(); err == nil {
		t.Fatal("重复启动应返回错误")
	}

	if err := srv.Stop(); err != nil {
		t.Fatalf("停止失败: %v", err)
	}
	if srv.Running() {
		t.Fatal("停止后 Running() 应为 false")
	}
	// 幂等
	if err := srv.Stop(); err != nil {
		t.Fatalf("重复停止应无错误: %v", err)
	}
}

func TestStartRollbackOnDHCPFailure(t *testing.T) {
	srv, _, fs := newTestServer(t)
	fs.failOn["dhcp"] = fmt.Errorf("端口 67 被占用")

	err := srv.Start()
	if err == nil {
		t.Fatal("DHCP 启动失败时 Start 应返回错误")
	}
	if srv.Running() {
		t.Fatal("启动失败后不应处于运行状态")
	}
	// 已启动的 TFTP/HTTP 应被回滚停止
	if !fs.stopped["tftp"] || !fs.stopped["http"] {
		t.Fatalf("启动失败后应回滚停止已启动的服务: stopped=%+v", fs.stopped)
	}
}

func TestStatusReflectsState(t *testing.T) {
	srv, _, _ := newTestServer(t)
	if _, err := srv.RescanISOs(); err != nil {
		t.Fatal(err)
	}
	cfg := srv.Settings()
	cfg.ServerIP = "192.168.1.10"
	cfg.HTTPPort = 16000
	cfg.WebPort = 16080
	if err := srv.SaveSettings(cfg); err != nil {
		t.Fatal(err)
	}

	st := srv.Status()
	if st.Running {
		t.Error("未启动时 Running 应为 false")
	}
	if st.ServerIP != "192.168.1.10" {
		t.Errorf("ServerIP = %q, 期望 192.168.1.10", st.ServerIP)
	}
	if st.ISOCount != 2 {
		t.Errorf("ISOCount = %d, 期望 2", st.ISOCount)
	}
	if st.Version != Version {
		t.Errorf("Version = %q", st.Version)
	}
	if st.BootFileBIOS == "" || st.BootFileUEFI == "" {
		t.Error("引导文件名不应为空")
	}
}

func TestRescanISOs(t *testing.T) {
	srv, _, fs := newTestServer(t)
	n, err := srv.RescanISOs()
	if err != nil {
		t.Fatalf("扫描失败: %v", err)
	}
	if n != 2 {
		t.Fatalf("扫描到 %d 个 ISO, 期望 2", n)
	}
	isos := srv.ISOs()
	if len(isos) != 2 {
		t.Fatalf("ISOs() 返回 %d 个", len(isos))
	}
	// 按 ID 查
	is, ok := srv.ISOByID("abc123")
	if !ok {
		t.Fatal("按 ID 查不到 ISO")
	}
	if is.OSName != "Ubuntu 24.04 LTS" {
		t.Errorf("OSName = %q", is.OSName)
	}

	// 换一批 ISO 再扫，旧缓存应被替换
	fs.mu.Lock()
	fs.isos = []model.ISO{{ID: "new1", Name: "alpine.iso", Family: model.FamilyLinux}}
	fs.mu.Unlock()
	if _, err := srv.RescanISOs(); err != nil {
		t.Fatal(err)
	}
	if len(srv.ISOs()) != 1 {
		t.Errorf("重新扫描后应有 1 个 ISO, 实际 %d", len(srv.ISOs()))
	}
	if _, ok := srv.ISOByID("abc123"); ok {
		t.Error("旧 ISO 缓存应被清除")
	}
}

func TestDefaultISO(t *testing.T) {
	srv, _, _ := newTestServer(t)
	// proxy 模式要求有本机 IP
	cfg0 := srv.Settings()
	cfg0.ServerIP = "192.168.1.10"
	if err := srv.SaveSettings(cfg0); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.RescanISOs(); err != nil {
		t.Fatal(err)
	}

	if err := srv.SetDefaultISO("abc123"); err != nil {
		t.Fatalf("设置默认 ISO 失败: %v", err)
	}
	if got := srv.DefaultISO(); got != "abc123" {
		t.Errorf("DefaultISO() = %q, 期望 abc123", got)
	}
	if err := srv.SetDefaultISO("nonexistent"); err == nil {
		t.Error("设置不存在的 ISO 应返回错误")
	}
	// 清空是允许的
	if err := srv.SetDefaultISO(""); err != nil {
		t.Errorf("清空默认 ISO 应允许: %v", err)
	}
}

func TestScriptForUnknownClientCreatesRecord(t *testing.T) {
	srv, _, _ := newTestServer(t)
	if _, err := srv.RescanISOs(); err != nil {
		t.Fatal(err)
	}

	// 一个从未在 DHCP 阶段出现过的 MAC
	script, err := srv.ScriptFor("AA:BB:CC:DD:EE:FF", "")
	if err != nil {
		t.Fatalf("生成脚本失败: %v", err)
	}
	if script == "" {
		t.Fatal("脚本不应为空")
	}
	// 应该自动创建了客户机记录
	if _, ok := srv.store.GetClient("aa:bb:cc:dd:ee:ff"); !ok {
		t.Error("应为未知客户机自动创建记录")
	}

	// 指定 ISO 直接启动
	if _, err := srv.ScriptFor("AA:BB:CC:DD:EE:FF", "abc123"); err != nil {
		t.Fatalf("生成指定 ISO 脚本失败: %v", err)
	}
	// 不存在的 ISO
	if _, err := srv.ScriptFor("AA:BB:CC:DD:EE:FF", "nope"); err == nil {
		t.Error("不存在的 ISO 应返回错误")
	}
}

func TestSaveSettingsRestartsWhenRunning(t *testing.T) {
	srv, _, fs := newTestServer(t)
	// proxy 模式要求有本机 IP
	cfg0 := srv.Settings()
	cfg0.ServerIP = "192.168.1.10"
	if err := srv.SaveSettings(cfg0); err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	fs.mu.Lock()
	fs.stopped["dhcp"] = false
	fs.mu.Unlock()

	cfg := srv.Settings()
	cfg.HTTPPort = 16123
	if err := srv.SaveSettings(cfg); err != nil {
		t.Fatalf("保存配置失败: %v", err)
	}
	if !srv.Running() {
		t.Error("保存配置后服务应仍在运行（自动重启）")
	}
	if srv.Settings().HTTPPort != 16123 {
		t.Errorf("新配置未生效: %d", srv.Settings().HTTPPort)
	}
}

func TestSaveSettingsValidation(t *testing.T) {
	srv, _, _ := newTestServer(t)
	cfg := srv.Settings()

	cfg.HTTPPort = cfg.WebPort
	if err := srv.SaveSettings(cfg); err == nil {
		t.Error("HTTP 端口与 Web 端口相同时应报错")
	}

	cfg = srv.Settings()
	cfg.DHCPMode = "bogus"
	if err := srv.SaveSettings(cfg); err == nil {
		t.Error("非法 DHCP 模式应报错")
	}

	cfg = srv.Settings()
	cfg.LeaseTime = 10
	if err := srv.SaveSettings(cfg); err == nil {
		t.Error("租约时间过短应报错")
	}
}

func TestClientsAndEvents(t *testing.T) {
	srv, _, _ := newTestServer(t)

	c := srv.store.UpsertClient(model.Client{MAC: "11:22:33:44:55:66", IP: "192.168.1.50",
		Arch: model.ArchUEFI64})
	if c.MAC != "11:22:33:44:55:66" {
		t.Errorf("MAC 归一化失败: %q", c.MAC)
	}
	if len(srv.Clients()) != 1 {
		t.Fatalf("客户机数量 = %d", len(srv.Clients()))
	}

	// 事件
	srv.store.AddEvent(model.Event{Level: "info", Kind: "system", Msg: "测试事件"})
	evs := srv.Events(10)
	if len(evs) == 0 {
		t.Fatal("事件列表不应为空")
	}
	found := false
	for _, e := range evs {
		if e.Msg == "测试事件" {
			found = true
		}
	}
	if !found {
		t.Error("未找到刚添加的事件")
	}

	srv.ClearClients()
	if len(srv.Clients()) != 0 {
		t.Error("清空后客户机列表应为空")
	}
}

func TestBootFileForArch(t *testing.T) {
	srv, _, _ := newTestServer(t)
	cases := map[model.Arch]string{
		model.ArchBIOS:        "bios/undionly.kpxe",
		model.ArchUEFI32:      "i386/ipxe.efi",
		model.ArchUEFI64:      "x86_64/ipxe.efi",
		model.ArchARM64:       "arm64/ipxe.efi",
		model.ArchLoongArch64: "loong64/ipxe.efi",
	}
	for arch, want := range cases {
		got := srv.store.BootFileFor(arch)
		if got != want {
			t.Errorf("BootFileFor(%s) = %q, 期望 %q", arch, got, want)
		}
	}
}

func TestMissingBootFilesDetection(t *testing.T) {
	srv, _, _ := newTestServer(t)
	if m := srv.store.MissingBootFiles(); len(m) != 0 {
		t.Fatalf("引导文件应齐全，却报告缺失: %v", m)
	}
	// 删掉一个
	root := srv.Root()
	if err := os.Remove(filepath.Join(root, "boot", "arm64", "ipxe.efi")); err != nil {
		t.Fatal(err)
	}
	m := srv.store.MissingBootFiles()
	if len(m) != 1 || m[0] != "arm64/ipxe.efi" {
		t.Fatalf("应报告 arm64/ipxe.efi 缺失, 实际 %v", m)
	}
}

func TestStartFailsWhenBootFilesMissing(t *testing.T) {
	srv, _, _ := newTestServer(t)
	_ = os.Remove(filepath.Join(srv.Root(), "boot", "wimboot"))
	if err := srv.Start(); err == nil {
		t.Fatal("引导文件缺失时启动应失败并给出明确错误")
	}
}

func TestInterfaces(t *testing.T) {
	ifs := Interfaces()
	if len(ifs) == 0 {
		t.Skip("本机没有可用网卡")
	}
	// 非回环的应该排在前面
	seenLoopback := false
	for _, i := range ifs {
		if i.Loopback {
			seenLoopback = true
			continue
		}
		if seenLoopback {
			t.Error("非回环网卡不应排在回环网卡之后")
			break
		}
	}
	for _, i := range ifs {
		if len(i.IPs) == 0 && !i.Loopback {
			t.Errorf("网卡 %s 被列出但没有 IP", i.Name)
		}
	}
}

func TestNormalizeMACInStore(t *testing.T) {
	srv, _, _ := newTestServer(t)
	// 各种写法的 MAC 应归一到同一条记录
	forms := []string{"AA-BB-CC-DD-EE-01", "aa:bb:cc:dd:ee:01"}
	for _, f := range forms {
		srv.store.UpsertClient(model.Client{MAC: f})
	}
	if n := len(srv.Clients()); n != 1 {
		t.Errorf("同一 MAC 的不同写法应归为 1 条记录, 实际 %d", n)
	}
}

func TestEventRingBuffer(t *testing.T) {
	srv, _, _ := newTestServer(t)
	// 写超过容量的次数，验证不丢最新
	n := store.EventCap + 50
	for i := 0; i < n; i++ {
		srv.store.AddEvent(model.Event{Level: "info", Kind: "test", Msg: fmt.Sprintf("事件%d", i)})
	}
	evs := srv.Events(0)
	if len(evs) != store.EventCap {
		t.Fatalf("事件数量 = %d, 期望 %d（环形缓冲上限）", len(evs), store.EventCap)
	}
	// 最新的应该在最末尾
	last := evs[len(evs)-1].Msg
	if last != fmt.Sprintf("事件%d", n-1) {
		t.Errorf("最新事件 = %q, 期望 %q", last, fmt.Sprintf("事件%d", n-1))
	}
	// 顺序应递增
	for i := 1; i < len(evs); i++ {
		if evs[i].Time.Before(evs[i-1].Time) {
			t.Errorf("事件时间乱序 @%d", i)
			break
		}
	}
}

func TestUptimeIncreases(t *testing.T) {
	srv, _, _ := newTestServer(t)
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	u1 := srv.Status().UptimeSec
	time.Sleep(1100 * time.Millisecond)
	u2 := srv.Status().UptimeSec
	if u2 <= u1 {
		t.Errorf("运行时长未增长: %d -> %d", u1, u2)
	}
	_ = srv.Stop()
}
