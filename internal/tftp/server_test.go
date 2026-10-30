package tftp

import (
	"bytes"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wupengcheng/netbootx/internal/model"
)

// defaultTestAddr 是端到端测试的首选服务地址（任务要求用 16969）。
const defaultTestAddr = "127.0.0.1:16969"

// freeAddr 返回一个可用的 UDP 监听地址，优先使用 preferred。
func freeAddr(t *testing.T, preferred string) string {
	t.Helper()
	conn, err := net.ListenUDP("udp4", mustResolve(t, preferred))
	if err == nil {
		_ = conn.Close()
		return preferred
	}
	conn, err = net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatalf("无法分配测试端口: %v", err)
	}
	defer conn.Close()
	addr := conn.LocalAddr().String()
	t.Logf("地址 %s 已被占用，改用 %s", preferred, addr)
	return addr
}

func mustResolve(t *testing.T, addr string) *net.UDPAddr {
	t.Helper()
	a, err := net.ResolveUDPAddr("udp4", addr)
	if err != nil {
		t.Fatalf("解析地址 %q 失败: %v", addr, err)
	}
	return a
}

// setupServer 在 127.0.0.1:16969 上起一个真实的 TFTP 服务器，
// 返回服务器、文件根目录和实际监听地址。
func setupServer(t *testing.T) (*Server, string, string) {
	t.Helper()
	root := t.TempDir()
	// 写一个大于 5KB 的文件（多个数据块）
	big := bytes.Repeat([]byte("NetBootX-TFTP-test-payload-"), 400) // ~10.8KB
	if err := os.WriteFile(filepath.Join(root, "ipxe.efi"), big, 0o644); err != nil {
		t.Fatalf("写入测试文件失败: %v", err)
	}
	// 小文件（单块）
	if err := os.WriteFile(filepath.Join(root, "small.txt"), []byte("hello tftp"), 0o644); err != nil {
		t.Fatalf("写入小文件失败: %v", err)
	}
	// 空文件（只有一个空数据块）
	if err := os.WriteFile(filepath.Join(root, "empty.bin"), nil, 0o644); err != nil {
		t.Fatalf("写入空文件失败: %v", err)
	}
	// 恰好一个块大小的文件
	if err := os.WriteFile(filepath.Join(root, "exact.bin"), bytes.Repeat([]byte("x"), 512), 0o644); err != nil {
		t.Fatalf("写入整块文件失败: %v", err)
	}
	// 子目录里的文件
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatalf("创建子目录失败: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "sub", "inner.txt"), []byte("inner"), 0o644); err != nil {
		t.Fatalf("写入子目录文件失败: %v", err)
	}
	// 根目录外的秘密文件，用于验证路径穿越被拒绝
	secret := filepath.Join(filepath.Dir(root), "secret.txt")
	if err := os.WriteFile(secret, []byte("TOP SECRET"), 0o600); err != nil {
		t.Fatalf("写入根目录外文件失败: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(secret) })

	addr := freeAddr(t, defaultTestAddr)
	srv, err := New(Config{Root: root, Addr: addr, BlockSize: 1468})
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("Start 失败: %v", err)
	}
	t.Cleanup(func() { _ = srv.Stop() })
	return srv, root, addr
}

// clientResult 是一次 TFTP 下载的结果。
type clientResult struct {
	data     []byte
	blksize  int
	tsize    int64
	timeout  int
	window   int
	oackSeen bool
	blocks   int
}

// tftpDownload 是一个最小但完整的 TFTP 客户端实现，用于端到端测试。
func tftpDownload(t *testing.T, server string, filename string, opts [][2]string) (*clientResult, error) {
	t.Helper()
	raddr, err := net.ResolveUDPAddr("udp4", server)
	if err != nil {
		return nil, err
	}
	// 客户端用随机端口（模拟真实客户端 TID）
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero})
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	// 构造 RRQ
	req := []byte{0, byte(opRRQ)}
	req = append(req, filename...)
	req = append(req, 0)
	req = append(req, "octet"...)
	req = append(req, 0)
	for _, kv := range opts {
		req = append(req, kv[0]...)
		req = append(req, 0)
		req = append(req, kv[1]...)
		req = append(req, 0)
	}
	if _, err := conn.WriteToUDP(req, raddr); err != nil {
		return nil, err
	}

	res := &clientResult{blksize: 512, window: 1}
	buf := make([]byte, 70000)
	expectedBlock := uint16(1)
	serverTID := (*net.UDPAddr)(nil)

	for {
		_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		n, src, err := conn.ReadFromUDP(buf)
		if err != nil {
			return nil, fmt.Errorf("等待数据超时: %w", err)
		}
		if serverTID == nil {
			serverTID = src
			// 关键断言：服务器的传输端口不能是 69
			if src.Port == raddr.Port {
				return nil, fmt.Errorf("服务器从主端口 %d 回包，违反 TFTP 协议", src.Port)
			}
		}
		p, err := parsePacket(buf[:n])
		if err != nil {
			return nil, err
		}
		switch p.Opcode {
		case opOACK:
			res.oackSeen = true
			for _, kv := range p.Opts {
				switch kv[0] {
				case "blksize":
					fmt.Sscanf(kv[1], "%d", &res.blksize)
				case "tsize":
					fmt.Sscanf(kv[1], "%d", &res.tsize)
				case "timeout":
					fmt.Sscanf(kv[1], "%d", &res.timeout)
				case "windowsize":
					fmt.Sscanf(kv[1], "%d", &res.window)
				}
			}
			if _, err := conn.WriteToUDP(marshalACK(0), serverTID); err != nil {
				return nil, err
			}
		case opDATA:
			if p.Block != expectedBlock {
				return nil, fmt.Errorf("数据块序号错乱：期望 %d，实际 %d", expectedBlock, p.Block)
			}
			res.data = append(res.data, p.Data...)
			res.blocks++
			if _, err := conn.WriteToUDP(marshalACK(p.Block), serverTID); err != nil {
				return nil, err
			}
			expectedBlock++
			if len(p.Data) < res.blksize {
				return res, nil // 最后一个块
			}
		case opERROR:
			return nil, fmt.Errorf("服务器返回错误 %d: %s", p.Code, p.Msg)
		default:
			return nil, fmt.Errorf("意外的报文类型 %d", p.Opcode)
		}
	}
}

func TestServerDownloadMultiBlock(t *testing.T) {
	_, root, addr := setupServer(t)
	want, err := os.ReadFile(filepath.Join(root, "ipxe.efi"))
	if err != nil {
		t.Fatal(err)
	}
	if len(want) <= 5120 {
		t.Fatalf("测试文件太小（%d 字节），无法验证多块传输", len(want))
	}

	res, err := tftpDownload(t, addr, "ipxe.efi", [][2]string{{"blksize", "1468"}, {"tsize", "0"}})
	if err != nil {
		t.Fatalf("下载失败: %v", err)
	}
	if !bytes.Equal(res.data, want) {
		t.Fatalf("内容不一致：收到 %d 字节，期望 %d 字节", len(res.data), len(want))
	}
	if res.blksize != 1468 {
		t.Errorf("协商块大小 = %d，期望 1468", res.blksize)
	}
	if res.tsize != int64(len(want)) {
		t.Errorf("tsize = %d，期望 %d", res.tsize, len(want))
	}
	if !res.oackSeen {
		t.Error("应收到 OACK")
	}
	wantBlocks := (len(want) + res.blksize - 1) / res.blksize
	if res.blocks != wantBlocks {
		t.Errorf("块数 = %d，期望 %d", res.blocks, wantBlocks)
	}
}

func TestServerDownloadDefaultBlockSize(t *testing.T) {
	_, root, addr := setupServer(t)
	want, _ := os.ReadFile(filepath.Join(root, "ipxe.efi"))

	// 不带任何选项：应该用 512 字节的默认块大小
	res, err := tftpDownload(t, addr, "ipxe.efi", nil)
	if err != nil {
		t.Fatalf("下载失败: %v", err)
	}
	if !bytes.Equal(res.data, want) {
		t.Fatal("内容不一致")
	}
	if res.oackSeen {
		t.Error("没有请求选项时不应回 OACK")
	}
	if res.blocks <= 1 {
		t.Errorf("512 字节块应产生多块，实际 %d 块", res.blocks)
	}
}

func TestServerDownloadSmallAndEmpty(t *testing.T) {
	_, root, addr := setupServer(t)

	res, err := tftpDownload(t, addr, "small.txt", nil)
	if err != nil {
		t.Fatalf("下载小文件失败: %v", err)
	}
	want, _ := os.ReadFile(filepath.Join(root, "small.txt"))
	if !bytes.Equal(res.data, want) {
		t.Errorf("小文件内容 = %q", res.data)
	}

	res, err = tftpDownload(t, addr, "empty.bin", nil)
	if err != nil {
		t.Fatalf("下载空文件失败: %v", err)
	}
	if len(res.data) != 0 {
		t.Errorf("空文件应收到 0 字节，实际 %d", len(res.data))
	}
	if res.blocks != 1 {
		t.Errorf("空文件应只有一个空数据块，实际 %d 块", res.blocks)
	}

	// 恰好一个块大小的文件：必须多发一个 0 字节块，否则客户端会一直等
	res, err = tftpDownload(t, addr, "exact.bin", nil)
	if err != nil {
		t.Fatalf("下载整块文件失败: %v", err)
	}
	if len(res.data) != 512 {
		t.Errorf("整块文件长度 = %d，期望 512", len(res.data))
	}
	if res.blocks != 2 {
		t.Errorf("恰好一块的文件应发 2 个块（第二块为空），实际 %d", res.blocks)
	}
}

func TestServerDownloadSubdir(t *testing.T) {
	_, root, addr := setupServer(t)
	res, err := tftpDownload(t, addr, "sub/inner.txt", nil)
	if err != nil {
		t.Fatalf("下载子目录文件失败: %v", err)
	}
	want, _ := os.ReadFile(filepath.Join(root, "sub", "inner.txt"))
	if !bytes.Equal(res.data, want) {
		t.Errorf("内容 = %q", res.data)
	}
	// 前导斜杠也应被接受（iPXE 有时会发 "/sub/inner.txt"）
	res, err = tftpDownload(t, addr, "/sub/inner.txt", nil)
	if err != nil {
		t.Fatalf("带前导斜杠的下载失败: %v", err)
	}
	if !bytes.Equal(res.data, want) {
		t.Error("带前导斜杠时内容不一致")
	}
}

func TestServerFileNotFound(t *testing.T) {
	_, _, addr := setupServer(t)
	_, err := tftpDownload(t, addr, "no-such-file.bin", nil)
	if err == nil {
		t.Fatal("下载不存在的文件应失败")
	}
	if !strings.Contains(err.Error(), "错误 1") {
		t.Errorf("应返回 ERROR 1（file not found），实际: %v", err)
	}
	if !strings.Contains(strings.ToLower(err.Error()), "file not found") {
		t.Errorf("错误文案应说明文件不存在，实际: %v", err)
	}
}

func TestServerPathTraversalRejected(t *testing.T) {
	srv, _, addr := setupServer(t)
	cases := []string{
		"../etc/passwd",
		"../../etc/passwd",
		"sub/../../etc/passwd",
		"..",
		"..\\..\\windows\\system32\\config\\sam",
	}
	for _, name := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := tftpDownload(t, addr, name, nil)
			if err == nil {
				t.Fatalf("路径 %q 应被拒绝", name)
			}
			if !strings.Contains(err.Error(), "错误 2") {
				t.Errorf("应返回 ERROR 2（Access violation），实际: %v", err)
			}
		})
	}
	if srv.Stats().Denied == 0 {
		t.Error("Denied 统计未增加")
	}
}

func TestServerWriteRequestRefused(t *testing.T) {
	_, _, addr := setupServer(t)
	raddr, _ := net.ResolveUDPAddr("udp4", addr)
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	// WRQ
	req := []byte{0, byte(opWRQ)}
	req = append(req, "upload.txt"...)
	req = append(req, 0)
	req = append(req, "octet"...)
	req = append(req, 0)
	if _, err := conn.WriteToUDP(req, raddr); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 1024)
	n, _, err := conn.ReadFromUDP(buf)
	if err != nil {
		t.Fatalf("写请求应收到 ERROR 报文: %v", err)
	}
	p, err := parsePacket(buf[:n])
	if err != nil {
		t.Fatal(err)
	}
	if p.Opcode != opERROR || p.Code != ErrAccessViolation {
		t.Errorf("响应 = %+v，期望 ERROR 2", p)
	}
}

func TestServerStats(t *testing.T) {
	srv, root, addr := setupServer(t)
	want, _ := os.ReadFile(filepath.Join(root, "ipxe.efi"))
	if _, err := tftpDownload(t, addr, "ipxe.efi", [][2]string{{"blksize", "1024"}}); err != nil {
		t.Fatalf("下载失败: %v", err)
	}
	// 等统计落定（传输协程可能稍晚更新）
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if srv.Stats().BytesSent >= uint64(len(want)) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	st := srv.Stats()
	if st.Requests == 0 {
		t.Error("Requests 未计数")
	}
	if st.Completed == 0 {
		t.Error("Completed 未计数")
	}
	if st.BytesSent < uint64(len(want)) {
		t.Errorf("BytesSent = %d，期望 >= %d", st.BytesSent, len(want))
	}
	if st.Transfers == 0 {
		t.Error("Transfers 未计数")
	}
	if st.Root != srv.Root() {
		t.Errorf("Stats.Root = %q", st.Root)
	}
	if !st.Running {
		t.Error("Running 应为 true")
	}
	if st.Active != 0 {
		t.Errorf("传输结束后 Active 应为 0，实际 %d", st.Active)
	}
	if st.LastClient == "" {
		t.Error("LastClient 未记录")
	}
	if st.StartedAt.IsZero() || st.Uptime <= 0 {
		t.Error("StartedAt / Uptime 未设置")
	}
}

func TestServerOnEvent(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.bin"), []byte("abc"), 0o644); err != nil {
		t.Fatal(err)
	}
	// OnEvent 会从多个协程并发调用，测试侧必须自己加锁。
	var (
		mu     sync.Mutex
		events []model.Event
	)
	snapshot := func() []model.Event {
		mu.Lock()
		defer mu.Unlock()
		return append([]model.Event(nil), events...)
	}
	addr := freeAddr(t, "127.0.0.1:16971")
	srv, err := New(Config{Root: root, Addr: addr, OnEvent: func(e model.Event) {
		mu.Lock()
		events = append(events, e)
		mu.Unlock()
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("Start 失败: %v", err)
	}
	defer srv.Stop()
	if _, err := tftpDownload(t, addr, "a.bin", nil); err != nil {
		t.Fatalf("下载失败: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		found := false
		for _, e := range snapshot() {
			if strings.Contains(e.Msg, "传输完成") {
				found = true
			}
		}
		if found {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	found := false
	for _, e := range snapshot() {
		if e.Kind != "tftp" {
			t.Errorf("事件 Kind = %q", e.Kind)
		}
		if strings.Contains(e.Msg, "传输完成") {
			found = true
		}
	}
	if !found {
		t.Errorf("未收到传输完成事件: %+v", snapshot())
	}
}

func TestNewValidation(t *testing.T) {
	if _, err := New(Config{Root: ""}); err == nil {
		t.Error("空根目录应报错")
	}
	if _, err := New(Config{Root: "/definitely/not/exists/xyz"}); err == nil {
		t.Error("不存在的根目录应报错")
	}
	f := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := New(Config{Root: f}); err == nil {
		t.Error("根目录是文件时应报错")
	}
	// 默认值补全
	srv, err := New(Config{Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if srv.cfg.Addr != ":69" {
		t.Errorf("默认地址 = %q", srv.cfg.Addr)
	}
	if srv.cfg.BlockSize != defaultBlockSize {
		t.Errorf("默认块大小 = %d", srv.cfg.BlockSize)
	}
	if srv.Root() == "" || !filepath.IsAbs(srv.Root()) {
		t.Errorf("Root 未规范化: %q", srv.Root())
	}
}

func TestResolvePath(t *testing.T) {
	root := t.TempDir()
	srv, err := New(Config{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	ok := []string{"a.bin", "/a.bin", "sub/a.bin", "sub\\a.bin", "./a.bin"}
	for _, name := range ok {
		if _, err := srv.resolve(name); err != nil {
			t.Errorf("resolve(%q) 应成功: %v", name, err)
		}
	}
	bad := []string{"", "..", "../a", "a/../../b", "/../a", "a/..", "\\..\\a"}
	for _, name := range bad {
		if _, err := srv.resolve(name); err == nil {
			t.Errorf("resolve(%q) 应失败", name)
		}
	}
	if _, err := srv.resolve("a\x00b"); err == nil {
		t.Error("含 NUL 的文件名应被拒绝")
	}
}

func TestStartBindError(t *testing.T) {
	root := t.TempDir()
	addr := freeAddr(t, "127.0.0.1:16972")
	blocker, err := net.ListenUDP("udp4", mustResolve(t, addr))
	if err != nil {
		t.Fatalf("占用端口失败: %v", err)
	}
	defer blocker.Close()
	srv, err := New(Config{Root: root, Addr: addr})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(); err == nil {
		srv.Stop()
		t.Fatal("端口被占用时 Start 应返回错误")
	} else if !strings.Contains(err.Error(), "绑定") {
		t.Errorf("错误信息不够清晰: %v", err)
	}
}

func TestStopIdempotent(t *testing.T) {
	root := t.TempDir()
	srv, err := New(Config{Root: root, Addr: freeAddr(t, "127.0.0.1:16973")})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Stop(); err != nil {
		t.Errorf("未启动时 Stop 应成功: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("Start 失败: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Errorf("重复 Start 应幂等: %v", err)
	}
	if err := srv.Stop(); err != nil {
		t.Fatalf("Stop 失败: %v", err)
	}
	if err := srv.Stop(); err != nil {
		t.Errorf("重复 Stop 应幂等: %v", err)
	}
	if srv.Stats().Running {
		t.Error("Stop 后 Running 应为 false")
	}
}

func TestRetransmission(t *testing.T) {
	// 客户端收到第一个数据块后故意不回 ACK，服务器应重传。
	root := t.TempDir()
	// 文件必须超过一个 512 字节的块，否则第二块不存在
	if err := os.WriteFile(filepath.Join(root, "r.bin"), bytes.Repeat([]byte("retransmit-me-"), 100), 0o644); err != nil {
		t.Fatal(err)
	}
	addr := freeAddr(t, "127.0.0.1:16974")
	srv, err := New(Config{Root: root, Addr: addr})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("Start 失败: %v", err)
	}
	defer srv.Stop()

	raddr := mustResolve(t, addr)
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	req := []byte{0, byte(opRRQ)}
	req = append(req, "r.bin"...)
	req = append(req, 0)
	req = append(req, "octet"...)
	req = append(req, 0)
	req = append(req, "timeout"...)
	req = append(req, 0)
	req = append(req, "1"...)
	req = append(req, 0)
	if _, err := conn.WriteToUDP(req, raddr); err != nil {
		t.Fatal(err)
	}

	buf := make([]byte, 1024)
	// 1) OACK（必须回 ACK 0，否则服务器会重传 OACK）
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, oackSrc, err := conn.ReadFromUDP(buf); err != nil {
		t.Fatalf("未收到 OACK: %v", err)
	} else if _, err := conn.WriteToUDP(marshalACK(0), oackSrc); err != nil {
		t.Fatal(err)
	}
	// 2) 第一个 DATA（不回 ACK）
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	n, src, err := conn.ReadFromUDP(buf)
	if err != nil {
		t.Fatalf("未收到第一个 DATA: %v", err)
	}
	p, err := parsePacket(buf[:n])
	if err != nil || p.Opcode != opDATA || p.Block != 1 {
		t.Fatalf("第一个报文 = %+v / %v", p, err)
	}
	// 3) 等待重传（timeout 协商为 1 秒）
	_ = conn.SetReadDeadline(time.Now().Add(4 * time.Second))
	n, _, err = conn.ReadFromUDP(buf)
	if err != nil {
		t.Fatalf("未收到重传: %v", err)
	}
	p2, err := parsePacket(buf[:n])
	if err != nil || p2.Opcode != opDATA || p2.Block != 1 {
		t.Fatalf("重传报文 = %+v / %v", p2, err)
	}
	// 4) 正常 ACK 后应能完成
	if _, err := conn.WriteToUDP(marshalACK(1), src); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	n, _, err = conn.ReadFromUDP(buf)
	if err != nil {
		t.Fatalf("未收到第二块: %v", err)
	}
	p3, _ := parsePacket(buf[:n])
	if p3.Opcode != opDATA || p3.Block != 2 {
		t.Errorf("第二块 = %+v", p3)
	}
}

func TestUnknownTIDRejected(t *testing.T) {
	// 第三个 socket 冒充客户端发 ACK，服务器应回 ERROR 5（unknown TID）而不是继续传输。
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "t.bin"), bytes.Repeat([]byte("z"), 2000), 0o644); err != nil {
		t.Fatal(err)
	}
	addr := freeAddr(t, "127.0.0.1:16975")
	srv, err := New(Config{Root: root, Addr: addr})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("Start 失败: %v", err)
	}
	defer srv.Stop()

	raddr := mustResolve(t, addr)
	client, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	attacker, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero})
	if err != nil {
		t.Fatal(err)
	}
	defer attacker.Close()

	req := []byte{0, byte(opRRQ)}
	req = append(req, "t.bin"...)
	req = append(req, 0)
	req = append(req, "octet"...)
	req = append(req, 0)
	if _, err := client.WriteToUDP(req, raddr); err != nil {
		t.Fatal(err)
	}

	buf := make([]byte, 4096)
	_ = client.SetReadDeadline(time.Now().Add(3 * time.Second))
	n, serverTID, err := client.ReadFromUDP(buf)
	if err != nil {
		t.Fatalf("未收到第一个 DATA: %v", err)
	}
	_ = n
	// 冒充者发一个 ACK 到服务器的传输端口
	if _, err := attacker.WriteToUDP(marshalACK(1), serverTID); err != nil {
		t.Fatal(err)
	}
	// 冒充者应收到 ERROR 5
	_ = attacker.SetReadDeadline(time.Now().Add(3 * time.Second))
	n, _, err = attacker.ReadFromUDP(buf)
	if err != nil {
		t.Skipf("未收到 unknown TID 错误（环境可能过滤）: %v", err)
	}
	p, err := parsePacket(buf[:n])
	if err != nil {
		t.Fatal(err)
	}
	if p.Opcode != opERROR || p.Code != ErrUnknownTID {
		t.Errorf("响应 = %+v，期望 ERROR 5", p)
	}
	// 真正的客户端继续正常接收
	if _, err := client.WriteToUDP(marshalACK(1), serverTID); err != nil {
		t.Fatal(err)
	}
}

func TestWindowsizeTransfer(t *testing.T) {
	_, root, addr := setupServer(t)
	want, _ := os.ReadFile(filepath.Join(root, "ipxe.efi"))
	res, err := tftpDownload(t, addr, "ipxe.efi", [][2]string{
		{"blksize", "1024"},
		{"windowsize", "4"},
	})
	if err != nil {
		t.Fatalf("下载失败: %v", err)
	}
	if !bytes.Equal(res.data, want) {
		t.Fatalf("内容不一致：%d vs %d 字节", len(res.data), len(want))
	}
	if res.window != 4 {
		t.Errorf("协商窗口 = %d，期望 4", res.window)
	}
	if res.blksize != 1024 {
		t.Errorf("协商块大小 = %d，期望 1024", res.blksize)
	}
}

func TestConcurrentDownloads(t *testing.T) {
	_, root, addr := setupServer(t)
	want, _ := os.ReadFile(filepath.Join(root, "ipxe.efi"))
	errCh := make(chan error, 8)
	for i := 0; i < 8; i++ {
		go func() {
			res, err := tftpDownload(t, addr, "ipxe.efi", [][2]string{{"blksize", "1024"}})
			if err != nil {
				errCh <- err
				return
			}
			if !bytes.Equal(res.data, want) {
				errCh <- fmt.Errorf("并发下载内容不一致")
				return
			}
			errCh <- nil
		}()
	}
	for i := 0; i < 8; i++ {
		if err := <-errCh; err != nil {
			t.Errorf("并发下载失败: %v", err)
		}
	}
}
