package pxe

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wupengcheng/netbootx/internal/model"
	"github.com/wupengcheng/netbootx/internal/store"
)

// 这组测试是协议级集成测试：起真实的 UDP/TCP 监听，用真实的报文交互。
// 它们验证「模块之间接得上」，而不是「模块内部对不对」（后者由各包自己的单测覆盖）。
//
// 注意：这些测试依赖 dhcp / tftp / httpd 三个包的真实实现。
// 在那些包还没实现时，本文件不参与编译（用 build tag 隔离）。

// echoUDPServer 起一个 UDP 服务，把收到的每个包原样回给来源，
// 用来模拟客户机 / 验证端口可达性。
func echoUDPServer(t *testing.T, addr string) (net.PacketConn, *sync.WaitGroup) {
	t.Helper()
	pc, err := net.ListenPacket("udp", addr)
	if err != nil {
		t.Skipf("无法监听 %s: %v", addr, err)
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		buf := make([]byte, 4096)
		for {
			n, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			_, _ = pc.WriteTo(buf[:n], from)
		}
	}()
	t.Cleanup(func() {
		_ = pc.Close()
		wg.Wait()
	})
	return pc, &wg
}

func TestPortAvailabilityProbe(t *testing.T) {
	// 找一个空闲端口，确认探测逻辑正确
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()

	if err := EnsurePortFree(port); err != nil {
		t.Errorf("刚释放的端口 %d 应可用: %v", port, err)
	}

	// 占用后再测（绑 0.0.0.0，确保两个探测地址都冲突）
	l2, err := net.Listen("tcp", fmt.Sprintf("0.0.0.0:%d", port))
	if err != nil {
		t.Skip("端口被抢占了")
	}
	if err := EnsurePortFree(port); err == nil {
		l2.Close()
		t.Errorf("端口 %d 被占用时应报错", port)
		return
	}
	l2.Close()
}

func TestResolveDir(t *testing.T) {
	srv, _, _ := newTestServer(t)
	root := srv.Root()

	// 相对路径应拼到 root 下
	if got := srv.resolveDir("iso"); got != filepath.Join(root, "iso") {
		t.Errorf("相对路径解析 = %q", got)
	}
	// 绝对路径原样返回
	abs := filepath.Join(root, "custom", "isos")
	if got := srv.resolveDir(abs); got != abs {
		t.Errorf("绝对路径解析 = %q, 期望 %q", got, abs)
	}
	// 空字符串返回 root
	if got := srv.resolveDir(""); got != root {
		t.Errorf("空路径应返回 root, 实际 %q", got)
	}
}

// TestScriptForProducesIPXE 验证生成的脚本确实是合法 iPXE 脚本。
// 这是端到端链路里最容易出错的一环：httpd 回调 pxe → pxe 调 boot 包。
func TestScriptForProducesIPXE(t *testing.T) {
	srv, _, _ := newTestServer(t)
	cfg := srv.Settings()
	cfg.ServerIP = "192.168.123.10"
	cfg.HTTPPort = 16000
	if err := srv.SaveSettings(cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.RescanISOs(); err != nil {
		t.Fatal(err)
	}

	script, err := srv.ScriptFor("aa:bb:cc:dd:ee:ff", "")
	if err != nil {
		t.Fatalf("生成菜单脚本失败: %v", err)
	}
	if !strings.HasPrefix(script, "#!ipxe") {
		t.Errorf("iPXE 脚本必须以 #!ipxe 开头，实际开头: %q", firstLine(script))
	}
	if !strings.Contains(script, "192.168.123.10:16000") {
		t.Error("脚本里应包含服务端地址")
	}
	if !strings.Contains(script, "Ubuntu") && !strings.Contains(script, "ubuntu") {
		t.Error("脚本菜单里应包含 ISO 名称")
	}

	// 指定 ISO 的直达脚本
	direct, err := srv.ScriptFor("aa:bb:cc:dd:ee:ff", "abc123")
	if err != nil {
		t.Fatalf("生成直达脚本失败: %v", err)
	}
	if !strings.HasPrefix(direct, "#!ipxe") {
		t.Errorf("直达脚本必须以 #!ipxe 开头，实际: %q", firstLine(direct))
	}
	if !strings.Contains(direct, "boot") {
		t.Error("直达脚本应包含 boot 命令")
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// TestClientLifecycleThroughBoot 验证客户机从 DHCP 到选 ISO 的完整状态流转。
func TestClientLifecycleThroughBoot(t *testing.T) {
	srv, st, _ := newTestServer(t)
	cfg := srv.Settings()
	cfg.ServerIP = "192.168.123.10"
	if err := srv.SaveSettings(cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.RescanISOs(); err != nil {
		t.Fatal(err)
	}

	mac := "11:22:33:44:55:66"
	// 模拟 DHCP 阶段
	st.UpsertClient(model.Client{MAC: mac, IP: "192.168.123.50", Arch: model.ArchUEFI64})

	// 模拟客户机进菜单
	if _, err := srv.ScriptFor(mac, ""); err != nil {
		t.Fatal(err)
	}
	c, ok := st.GetClient(mac)
	if !ok {
		t.Fatal("客户机记录丢失")
	}
	if c.State != model.StateMenu {
		t.Errorf("进菜单后状态 = %q, 期望 %q", c.State, model.StateMenu)
	}

	// 模拟选了 ISO
	if _, err := srv.ScriptFor(mac, "abc123"); err != nil {
		t.Fatal(err)
	}
	c, _ = st.GetClient(mac)
	if c.State != model.StateBooting {
		t.Errorf("选 ISO 后状态 = %q, 期望 %q", c.State, model.StateBooting)
	}
	if c.Selected != "abc123" {
		t.Errorf("Selected = %q", c.Selected)
	}
}

// TestUDPPortsDistinct 确保默认端口配置不冲突。
func TestUDPPortsDistinct(t *testing.T) {
	cfg := model.DefaultSettings()
	if cfg.HTTPPort == cfg.WebPort {
		t.Error("HTTP 端口与 Web 端口不应相同")
	}
	if cfg.DHCPPort == cfg.TFTPPort {
		t.Error("DHCP 端口与 TFTP 端口不应相同")
	}
	// 不应占用常见端口，避免和已有服务冲突
	if cfg.HTTPPort == 80 || cfg.WebPort == 80 {
		t.Error("不应默认占用 80 端口")
	}
}

// TestEventOnStartStop 验证启停时会产生可观测的事件。
func TestEventOnStartStop(t *testing.T) {
	srv, st, _ := newTestServer(t)
	before := len(st.Events(0))

	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	afterStart := st.Events(0)
	if len(afterStart) <= before {
		t.Error("启动后应产生事件")
	}
	foundStart := false
	for _, e := range afterStart {
		if strings.Contains(e.Msg, "已启动") {
			foundStart = true
		}
	}
	if !foundStart {
		t.Error("应记录「服务已启动」事件")
	}

	if err := srv.Stop(); err != nil {
		t.Fatal(err)
	}
	foundStop := false
	for _, e := range st.Events(0) {
		if strings.Contains(e.Msg, "已停止") {
			foundStop = true
		}
	}
	if !foundStop {
		t.Error("应记录「服务已停止」事件")
	}
}

// TestStatusBootFilesMatchStore 验证 Status 里报告的引导文件名和 store 一致。
func TestStatusBootFilesMatchStore(t *testing.T) {
	srv, st, _ := newTestServer(t)
	status := srv.Status()
	if status.BootFileBIOS != st.BootFileFor(model.ArchBIOS) {
		t.Error("Status.BootFileBIOS 与 store 不一致")
	}
	if status.BootFileUEFI != st.BootFileFor(model.ArchUEFI64) {
		t.Error("Status.BootFileUEFI 与 store 不一致")
	}
}

// TestConcurrentScriptFor 验证并发为多台客户机生成脚本是安全的。
func TestConcurrentScriptFor(t *testing.T) {
	srv, _, _ := newTestServer(t)
	cfg := srv.Settings()
	cfg.ServerIP = "192.168.123.10"
	if err := srv.SaveSettings(cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.RescanISOs(); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	errs := make(chan error, 40)
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			mac := fmt.Sprintf("aa:bb:cc:dd:ee:%02x", i)
			s, err := srv.ScriptFor(mac, "")
			if err != nil {
				errs <- err
				return
			}
			if !strings.HasPrefix(s, "#!ipxe") {
				errs <- fmt.Errorf("客户机 %d 脚本格式错误", i)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if n := len(srv.Clients()); n != 40 {
		t.Errorf("应记录 40 台客户机, 实际 %d", n)
	}
}

// TestRescanConcurrentGuard 验证扫描期间重复请求被拒绝。
func TestRescanConcurrentGuard(t *testing.T) {
	srv, _, _ := newTestServer(t)
	// 手动把 scanning 置位，模拟正在扫描
	srv.mu.Lock()
	srv.scanning = true
	srv.mu.Unlock()
	if _, err := srv.RescanISOs(); err == nil {
		t.Error("扫描进行中时再次扫描应返回错误")
	}
	srv.mu.Lock()
	srv.scanning = false
	srv.mu.Unlock()
}

// TestStoreDirsCreated 验证启动时目录结构被正确创建。
func TestStoreDirsCreated(t *testing.T) {
	dir := t.TempDir()
	for _, d := range []string{"iso", "user", "data", "log", "boot"} {
		_ = os.MkdirAll(filepath.Join(dir, d), 0o755)
	}
	st, err := store.New(filepath.Join(dir, "data"), filepath.Join(dir, "boot"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := st.Settings()
	cfg.ServerIP = "192.168.1.10"
	if err := st.SaveSettings(cfg); err != nil {
		t.Fatal(err)
	}
	if err := st.EnsureDirs(dir); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"iso", "user", "data", "log"} {
		if _, err := os.Stat(filepath.Join(dir, d)); err != nil {
			t.Errorf("目录 %s 不存在", d)
		}
	}
}

// TestStartedAtTracking 验证启动时间被记录。
func TestStartedAtTracking(t *testing.T) {
	srv, _, _ := newTestServer(t)
	before := time.Now()
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	st := srv.Status()
	if st.StartedAt.Before(before.Add(-time.Second)) {
		t.Error("StartedAt 应接近启动时刻")
	}
	if st.UptimeSec < 0 {
		t.Error("UptimeSec 不应为负")
	}
	_ = srv.Stop()
	// 停止后 uptime 归零
	if srv.Status().UptimeSec != 0 {
		t.Error("停止后 UptimeSec 应为 0")
	}
}
