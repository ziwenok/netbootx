package dhcp

import (
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wupengcheng/netbootx/internal/model"
)

// TestProbeScriptRealClient 用 Lead 写的 scripts/dhcp-probe.py 作为「真实客户端」，
// 通过真实 UDP socket 打一个 DISCOVER 过来，断言三个架构的应答都正确。
//
// 这条用例覆盖了「服务端能收到广播 DISCOVER」+「能发出应答」的完整链路，
// 是 TestEndToEndBroadcastReply 之外的独立交叉验证。
// python3 或脚本不存在时跳过，不影响常规测试。
func TestProbeScriptRealClient(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("未找到 python3，跳过探针交叉验证")
	}
	script, err := filepath.Abs(filepath.Join("..", "..", "scripts", "dhcp-probe.py"))
	if err != nil {
		t.Skipf("解析探针脚本路径失败（%v），跳过", err)
	}
	if _, err := os.Stat(script); err != nil {
		t.Skipf("探针脚本不存在（%v），跳过", err)
	}
	repoRoot := filepath.Dir(filepath.Dir(script))

	port := testServerPort(t, 16781)
	s, err := New(Config{
		Mode:      ModeProxy,
		ServerIP:  "127.0.0.1",
		Netmask:   "255.255.255.0",
		Gateway:   "127.0.0.1",
		DNS:       []string{"223.5.5.5"},
		Domain:    "netbootx.lan",
		LeaseTime: 3600,
		Port:      port,
		BindIP:    "127.0.0.1", // 故意传具体 IP：包必须自己忽略
		BootFileFor: func(a model.Arch) string {
			switch a {
			case model.ArchBIOS:
				return "bios/undionly.kpxe"
			case model.ArchARM64:
				return "arm64/ipxe.efi"
			}
			return "x86_64/ipxe.efi"
		},
	})
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	if err := s.Start(); err != nil {
		t.Fatalf("Start 失败: %v", err)
	}
	defer s.Stop()

	cases := []struct {
		arch     string
		bootfile string
	}{
		{"7", "x86_64/ipxe.efi"},
		{"0", "bios/undionly.kpxe"},
		{"11", "arm64/ipxe.efi"},
	}
	for _, c := range cases {
		t.Run("arch="+c.arch, func(t *testing.T) {
			cmd := exec.Command(python, script, "--port", itoa(port), "--arch", c.arch)
			cmd.Dir = repoRoot
			out, err := cmd.CombinedOutput()
			text := string(out)
			if err != nil {
				t.Fatalf("探针失败: %v\n%s", err, text)
			}
			if !strings.Contains(text, "OFFER") {
				t.Errorf("应答不是 OFFER:\n%s", text)
			}
			if !strings.Contains(text, c.bootfile) {
				t.Errorf("引导文件应为 %q:\n%s", c.bootfile, text)
			}
			if !strings.Contains(text, "127.0.0.1") {
				t.Errorf("next-server 应为 127.0.0.1:\n%s", text)
			}
			if strings.Contains(text, "❌") {
				t.Errorf("探针报告错误:\n%s", text)
			}
		})
	}
	// 三次真实请求都应被统计到
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && s.Stats().Sent < 3 {
		time.Sleep(20 * time.Millisecond)
	}
	if st := s.Stats(); st.Sent < 3 || st.Offer < 3 || st.Discover < 3 {
		t.Errorf("统计异常: sent=%d offer=%d discover=%d", st.Sent, st.Offer, st.Discover)
	}
	_ = net.IPv4zero
}

// itoa 是 strconv.Itoa 的本地别名，避免测试文件多一个 import。
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [8]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
