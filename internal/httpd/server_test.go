package httpd

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wupengcheng/netbootx/internal/model"
)

// ---- 测试夹具 ----

// fakeISO 是一个内存里的假 ISO，实现 ISOReader。
// 每个文件的内容是一条确定性字节模式，方便校验 Range 读到的字节是否正确。
type fakeISO struct {
	files  map[string][]byte
	closed atomic.Bool
	reads  atomic.Int64
}

func newFakeISO() *fakeISO {
	mk := func(n int, seed byte) []byte {
		b := make([]byte, n)
		for i := range b {
			b[i] = byte((int(seed) + i) % 251)
		}
		return b
	}
	return &fakeISO{files: map[string][]byte{
		"/casper/vmlinuz":   mk(5000, 7),
		"/casper/initrd":    mk(4096, 11),
		"/bootmgr":          mk(1024, 3),
		"/boot/bcd":         mk(512, 5),
		"/sources/boot.wim": mk(8192, 13),
	}}
}

func (f *fakeISO) SectionReader(p string) (io.ReaderAt, int64, error) {
	data, ok := f.files[p]
	if !ok {
		return nil, 0, fmt.Errorf("iso: 路径不存在 %q", p)
	}
	f.reads.Add(1)
	return bytes.NewReader(data), int64(len(data)), nil
}

func (f *fakeISO) Exists(p string) bool {
	_, ok := f.files[p]
	return ok
}

func (f *fakeISO) Close() error {
	f.closed.Store(true)
	return nil
}

// testISO 构造一条指向临时文件的 model.ISO 记录。
func testISO(t *testing.T, dir, id string) model.ISO {
	t.Helper()
	p := filepath.Join(dir, id+".iso")
	if err := os.WriteFile(p, []byte("fake-iso-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	return model.ISO{ID: id, Path: p, Name: id + ".iso", Size: 14, ModTime: time.Now()}
}

// newTestServer 组装一个用于测试的 Server：ISO 读取器是内存假实现。
func newTestServer(t *testing.T, isos []model.ISO, fake *fakeISO) (*Server, *[]model.Event, *atomic.Int64) {
	t.Helper()
	var events []model.Event
	var mu sync.Mutex
	opens := &atomic.Int64{}

	s, err := New(Config{
		Addr:    "127.0.0.1:0",
		ISODir:  t.TempDir(),
		UserDir: t.TempDir(),
		BootDir: t.TempDir(),
		ScriptFor: func(mac, isoID string) (string, error) {
			if isoID == "missing" {
				return "", fmt.Errorf("ISO %q 不存在", isoID)
			}
			return "#!ipxe\n# mac=" + mac + " iso=" + isoID + "\nset base http://x\nboot\n", nil
		},
		ISOs: func() []model.ISO { return isos },
		OnEvent: func(ev model.Event) {
			mu.Lock()
			defer mu.Unlock()
			events = append(events, ev)
		},
		OpenISO: func(p string) (ISOReader, error) {
			opens.Add(1)
			if fake == nil {
				return nil, fmt.Errorf("测试未提供假 ISO")
			}
			return fake, nil
		},
		EventSample: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	return s, &events, opens
}

// do 发一个请求到 Handler，返回响应记录。
func do(s *Server, method, target string, hdr map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

// ---- Range 请求（核心验收项） ----

func TestISORangeRequest(t *testing.T) {
	dir := t.TempDir()
	is := testISO(t, dir, "abc123")
	fake := newFakeISO()
	s, _, _ := newTestServer(t, []model.ISO{is}, fake)

	rec := do(s, http.MethodGet, "/iso/abc123/casper/vmlinuz", map[string]string{
		"Range": "bytes=100-199",
	})
	if rec.Code != http.StatusPartialContent {
		t.Fatalf("Range 请求应返回 206，实际 %d（body=%q）", rec.Code, rec.Body.String())
	}
	body := rec.Body.Bytes()
	if len(body) != 100 {
		t.Fatalf("Range bytes=100-199 应返回 100 字节，实际 %d 字节", len(body))
	}
	want := fake.files["/casper/vmlinuz"][100:200]
	if !bytes.Equal(body, want) {
		t.Fatalf("Range 读到的字节不正确")
	}
	if got := rec.Header().Get("Content-Range"); got != "bytes 100-199/5000" {
		t.Errorf("Content-Range 不正确：%q", got)
	}
	if got := rec.Header().Get("Accept-Ranges"); got != "bytes" {
		t.Errorf("缺少 Accept-Ranges: bytes，实际 %q", got)
	}
}

func TestISOFullAndHeadRequest(t *testing.T) {
	dir := t.TempDir()
	is := testISO(t, dir, "abc123")
	fake := newFakeISO()
	s, _, _ := newTestServer(t, []model.ISO{is}, fake)

	rec := do(s, http.MethodGet, "/iso/abc123/casper/vmlinuz", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("完整下载应返回 200，实际 %d", rec.Code)
	}
	if !bytes.Equal(rec.Body.Bytes(), fake.files["/casper/vmlinuz"]) {
		t.Fatal("完整下载的内容与 ISO 内文件不一致")
	}

	// HEAD 只回头部，不返回 body。
	rec = do(s, http.MethodHead, "/iso/abc123/casper/vmlinuz", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("HEAD 应返回 200，实际 %d", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("HEAD 不应返回 body，实际 %d 字节", rec.Body.Len())
	}
	if got := rec.Header().Get("Content-Length"); got != "5000" {
		t.Errorf("HEAD 的 Content-Length 应为 5000，实际 %q", got)
	}
}

func TestISOIfNoneMatchReturns304(t *testing.T) {
	dir := t.TempDir()
	is := testISO(t, dir, "abc123")
	s, _, _ := newTestServer(t, []model.ISO{is}, newFakeISO())

	rec := do(s, http.MethodGet, "/iso/abc123/casper/vmlinuz", nil)
	etag := rec.Header().Get("ETag")
	if etag == "" {
		t.Fatal("应返回 ETag")
	}
	rec = do(s, http.MethodGet, "/iso/abc123/casper/vmlinuz", map[string]string{"If-None-Match": etag})
	if rec.Code != http.StatusNotModified {
		t.Fatalf("If-None-Match 命中应返回 304，实际 %d", rec.Code)
	}
}

func TestISOSuffixRange(t *testing.T) {
	dir := t.TempDir()
	is := testISO(t, dir, "abc123")
	fake := newFakeISO()
	s, _, _ := newTestServer(t, []model.ISO{is}, fake)

	rec := do(s, http.MethodGet, "/iso/abc123/casper/initrd", map[string]string{
		"Range": "bytes=-16",
	})
	if rec.Code != http.StatusPartialContent {
		t.Fatalf("后缀 Range 应返回 206，实际 %d", rec.Code)
	}
	data := fake.files["/casper/initrd"]
	if !bytes.Equal(rec.Body.Bytes(), data[len(data)-16:]) {
		t.Fatal("后缀 Range 读到的字节不正确")
	}
}

func TestISOUnknownFileReturns404(t *testing.T) {
	dir := t.TempDir()
	is := testISO(t, dir, "abc123")
	s, _, _ := newTestServer(t, []model.ISO{is}, newFakeISO())

	rec := do(s, http.MethodGet, "/iso/abc123/no/such/file", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("ISO 内不存在的文件应返回 404，实际 %d", rec.Code)
	}
}

func TestISOUnknownIDReturns404(t *testing.T) {
	s, _, _ := newTestServer(t, nil, newFakeISO())
	rec := do(s, http.MethodGet, "/iso/nope/casper/vmlinuz", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("未知 ISO ID 应返回 404，实际 %d", rec.Code)
	}
}

func TestISOReaderIsCached(t *testing.T) {
	dir := t.TempDir()
	is := testISO(t, dir, "abc123")
	s, _, opens := newTestServer(t, []model.ISO{is}, newFakeISO())

	for i := 0; i < 5; i++ {
		rec := do(s, http.MethodGet, "/iso/abc123/casper/vmlinuz", map[string]string{
			"Range": "bytes=0-9",
		})
		if rec.Code != http.StatusPartialContent {
			t.Fatalf("第 %d 次请求失败：%d", i, rec.Code)
		}
	}
	if n := opens.Load(); n != 1 {
		t.Fatalf("同一个 ISO 只应打开一次（目录树缓存），实际打开 %d 次", n)
	}
}

func TestISOReaderInvalidatedOnModTimeChange(t *testing.T) {
	dir := t.TempDir()
	is := testISO(t, dir, "abc123")
	s, _, opens := newTestServer(t, []model.ISO{is}, newFakeISO())

	do(s, http.MethodGet, "/iso/abc123/casper/vmlinuz", nil)
	// 模拟 ISO 被替换：mtime 变了，缓存必须失效并重新打开。
	newer := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(is.Path, newer, newer); err != nil {
		t.Fatal(err)
	}
	do(s, http.MethodGet, "/iso/abc123/casper/vmlinuz", nil)
	if n := opens.Load(); n != 2 {
		t.Fatalf("ISO 被替换后应重新打开，实际打开 %d 次", n)
	}
}

// ---- 路径穿越防护 ----

func TestPathTraversalRejected(t *testing.T) {
	dir := t.TempDir()
	// 在 user 目录的上一层放一个「机密文件」，确认读不到。
	secret := filepath.Join(filepath.Dir(dir), "secret.txt")
	if err := os.WriteFile(secret, []byte("TOP-SECRET"), 0o644); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(secret)

	s, _, _ := newTestServer(t, nil, newFakeISO())
	s.cfg.UserDir = dir

	// 直接调 handler，绕过 ServeMux 的路径清洗，验证最坏情况下也拒绝。
	cases := []string{
		"/user/../secret.txt",
		"/user/../../etc/passwd",
		"/user/%2e%2e/secret.txt",
		"/user/a/../../secret.txt",
		"/user//etc/passwd",
		"/boot/../../etc/passwd",
		"/boot/..%2f..%2fetc/passwd",
	}
	for _, target := range cases {
		req := httptest.NewRequest(http.MethodGet, target, nil)
		rec := httptest.NewRecorder()
		// 按路径选择对应的 handler，模拟路由已匹配的情况。
		if strings.HasPrefix(target, "/user/") {
			s.handleUser(rec, req)
		} else {
			s.handleBootFile(rec, req)
		}
		if rec.Code == http.StatusOK {
			t.Errorf("路径穿越 %q 竟然返回了 200，内容: %q", target, rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), "TOP-SECRET") {
			t.Fatalf("路径穿越 %q 泄漏了机密文件内容", target)
		}
	}
}

func TestISOPathTraversalRejected(t *testing.T) {
	dir := t.TempDir()
	is := testISO(t, dir, "abc123")
	s, _, _ := newTestServer(t, []model.ISO{is}, newFakeISO())

	cases := []string{
		"/iso/abc123/../../etc/passwd",
		"/iso/abc123/./casper/vmlinuz",
		"/iso/abc123//casper/vmlinuz",
	}
	for _, target := range cases {
		req := httptest.NewRequest(http.MethodGet, target, nil)
		rec := httptest.NewRecorder()
		s.handleISO(rec, req)
		if rec.Code == http.StatusOK {
			t.Errorf("ISO 路径穿越 %q 竟然返回了 200", target)
		}
	}
}

func TestSafeJoin(t *testing.T) {
	root := filepath.Join(string(filepath.Separator), "srv", "user")
	ok := []struct{ rel, want string }{
		{"a.txt", filepath.Join(root, "a.txt")},
		{"sub/b.txt", filepath.Join(root, "sub", "b.txt")},
		{"/a.txt", filepath.Join(root, "a.txt")},
	}
	for _, tc := range ok {
		got, valid := safeJoin(root, tc.rel)
		if !valid || got != tc.want {
			t.Errorf("safeJoin(%q) = (%q,%v)，期望 (%q,true)", tc.rel, got, valid, tc.want)
		}
	}
	bad := []string{"", "..", "../a", "a/../../b", ".", "./a", "a//b", "a\x00b", "..\\..\\a"}
	for _, rel := range bad {
		if got, valid := safeJoin(root, rel); valid {
			t.Errorf("safeJoin(%q) 应拒绝，却返回了 %q", rel, got)
		}
	}
}

// ---- 菜单路由 ----

func TestMenuRoute(t *testing.T) {
	s, events, _ := newTestServer(t, nil, newFakeISO())

	rec := do(s, http.MethodGet, "/boot/menu.ipxe?mac=AA-BB-CC-DD-EE-FF", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("菜单路由应返回 200，实际 %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("Content-Type 应为 text/plain，实际 %q", ct)
	}
	body := rec.Body.String()
	if !strings.HasPrefix(body, "#!ipxe") {
		t.Errorf("菜单内容应以 #!ipxe 开头：%q", body)
	}
	// MAC 必须被规范化后再传给 ScriptFor。
	if !strings.Contains(body, "mac=aa:bb:cc:dd:ee:ff") {
		t.Errorf("MAC 未规范化：%q", body)
	}
	if len(*events) == 0 {
		t.Error("应记录访问事件")
	}
}

func TestMenuRouteWithISOID(t *testing.T) {
	s, _, _ := newTestServer(t, nil, newFakeISO())
	rec := do(s, http.MethodGet, "/boot/menu.ipxe?mac=aa:bb:cc:dd:ee:ff&iso=abc123", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("直达脚本应返回 200，实际 %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "iso=abc123") {
		t.Errorf("isoID 未传给 ScriptFor：%q", rec.Body.String())
	}
}

func TestMenuRouteErrorStillReturnsIPXE(t *testing.T) {
	s, _, _ := newTestServer(t, nil, newFakeISO())
	rec := do(s, http.MethodGet, "/boot/menu.ipxe?mac=aa:bb:cc:dd:ee:ff&iso=missing", nil)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("生成失败应返回 500，实际 %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.HasPrefix(body, "#!ipxe") {
		t.Errorf("失败时也要回一个合法 iPXE 脚本，实际：%q", body)
	}
	if !strings.Contains(body, "生成启动脚本失败") {
		t.Errorf("失败脚本应说明原因：%q", body)
	}
}

func TestMenuRejectsPost(t *testing.T) {
	s, _, _ := newTestServer(t, nil, newFakeISO())
	rec := do(s, http.MethodPost, "/boot/menu.ipxe?mac=aa:bb:cc:dd:ee:ff", nil)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST 菜单应返回 405，实际 %d", rec.Code)
	}
}

// ---- 引导文件与用户文件 ----

func TestBootFileRoute(t *testing.T) {
	s, _, _ := newTestServer(t, nil, newFakeISO())
	bootDir := s.cfg.BootDir
	if err := os.MkdirAll(filepath.Join(bootDir, "x86_64"), 0o755); err != nil {
		t.Fatal(err)
	}
	payload := []byte("IPXE-BINARY-CONTENT")
	if err := os.WriteFile(filepath.Join(bootDir, "x86_64", "ipxe.efi"), payload, 0o644); err != nil {
		t.Fatal(err)
	}

	rec := do(s, http.MethodGet, "/boot/x86_64/ipxe.efi", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("引导文件应返回 200，实际 %d", rec.Code)
	}
	if !bytes.Equal(rec.Body.Bytes(), payload) {
		t.Fatal("引导文件内容不一致")
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/octet-stream" {
		t.Errorf("efi 文件应为 application/octet-stream，实际 %q", ct)
	}

	// 引导文件同样要支持 Range（iPXE 拉 wimboot 时会用）。
	rec = do(s, http.MethodGet, "/boot/x86_64/ipxe.efi", map[string]string{"Range": "bytes=0-3"})
	if rec.Code != http.StatusPartialContent || rec.Body.String() != "IPXE" {
		t.Fatalf("引导文件 Range 失败：%d %q", rec.Code, rec.Body.String())
	}

	rec = do(s, http.MethodGet, "/boot/not-exist.efi", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("不存在的引导文件应 404，实际 %d", rec.Code)
	}
}

func TestUserFileRoute(t *testing.T) {
	s, _, _ := newTestServer(t, nil, newFakeISO())
	userDir := s.cfg.UserDir
	if err := os.MkdirAll(filepath.Join(userDir, "autoinstall"), 0o755); err != nil {
		t.Fatal(err)
	}
	ks := []byte("# kickstart\nlang zh_CN.UTF-8\n")
	if err := os.WriteFile(filepath.Join(userDir, "autoinstall", "ks.cfg"), ks, 0o644); err != nil {
		t.Fatal(err)
	}

	rec := do(s, http.MethodGet, "/user/autoinstall/ks.cfg", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("用户文件应返回 200，实际 %d", rec.Code)
	}
	if !bytes.Equal(rec.Body.Bytes(), ks) {
		t.Fatal("用户文件内容不一致")
	}
	rec = do(s, http.MethodGet, "/user/autoinstall/nope.cfg", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("不存在的用户文件应 404，实际 %d", rec.Code)
	}
}

// ---- 健康检查与其它 ----

func TestHealthz(t *testing.T) {
	s, _, _ := newTestServer(t, nil, newFakeISO())
	rec := do(s, http.MethodGet, "/healthz", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("健康检查应返回 200，实际 %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "\"ok\":true") {
		t.Errorf("健康检查返回体不正确：%q", rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("健康检查 Content-Type 应为 JSON，实际 %q", ct)
	}
}

func TestIndexAndNotFound(t *testing.T) {
	s, _, _ := newTestServer(t, nil, newFakeISO())
	rec := do(s, http.MethodGet, "/", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "NetBootX HTTP 文件服务") {
		t.Errorf("根路径应给出服务说明，实际 %d %q", rec.Code, rec.Body.String())
	}
	rec = do(s, http.MethodGet, "/whatever", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("未知路径应 404，实际 %d", rec.Code)
	}
}

// ---- 生命周期 ----

func TestStartStopAndServe(t *testing.T) {
	dir := t.TempDir()
	is := testISO(t, dir, "abc123")
	fake := newFakeISO()
	s, _, _ := newTestServer(t, []model.ISO{is}, fake)

	if err := s.Start(); err != nil {
		t.Fatalf("启动失败: %v", err)
	}
	addr := s.Addr()
	if addr == "" || strings.HasSuffix(addr, ":0") {
		t.Fatalf("Addr 应返回真实监听地址，实际 %q", addr)
	}

	resp, err := http.Get("http://" + addr + "/iso/abc123/casper/vmlinuz")
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !bytes.Equal(body, fake.files["/casper/vmlinuz"]) {
		t.Fatalf("真实 HTTP 请求结果不正确：%d, %d 字节", resp.StatusCode, len(body))
	}

	// 重复 Start 应报错。
	if err := s.Start(); err == nil {
		t.Error("重复 Start 应返回错误")
	}
	if err := s.Stop(); err != nil {
		t.Fatalf("Stop 失败: %v", err)
	}
	// Stop 幂等。
	if err := s.Stop(); err != nil {
		t.Errorf("重复 Stop 应返回 nil，实际 %v", err)
	}
	// Stop 之后缓存的读取器必须被关闭。
	if !fake.closed.Load() {
		t.Error("Stop 后 ISO 读取器应被关闭")
	}
}

func TestStopClosesReaders(t *testing.T) {
	dir := t.TempDir()
	is := testISO(t, dir, "abc123")
	fake := newFakeISO()
	s, _, _ := newTestServer(t, []model.ISO{is}, fake)

	do(s, http.MethodGet, "/iso/abc123/casper/vmlinuz", nil)
	if fake.closed.Load() {
		t.Fatal("请求过程中不应关闭读取器")
	}
	if err := s.Stop(); err != nil {
		t.Fatal(err)
	}
	if !fake.closed.Load() {
		t.Error("Stop 后应关闭所有缓存的 ISO 读取器")
	}
}

// ---- 工具函数 ----

func TestHumanBytes(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{0, "0B"},
		{999, "999B"},
		{2048, "2.0KB"},
		{5 * 1024 * 1024, "5.0MB"},
	}
	for _, tc := range cases {
		if got := humanBytes(tc.in); got != tc.want {
			t.Errorf("humanBytes(%d) = %q，期望 %q", tc.in, got, tc.want)
		}
	}
}

func TestSanitizeIPXE(t *testing.T) {
	got := sanitizeIPXE("bad && reboot\nsecond line $x")
	for _, bad := range []string{"&&", "$", "\n"} {
		if strings.Contains(got, bad) {
			t.Errorf("sanitizeIPXE 结果里仍有危险字符 %q：%q", bad, got)
		}
	}
	if sanitizeIPXE("") == "" {
		t.Error("空输入应返回兜底文案")
	}
}

func TestISOETag(t *testing.T) {
	now := time.Unix(1700000000, 0)
	a := isoETag(100, now)
	b := isoETag(100, now)
	if a != b {
		t.Error("相同输入应生成相同 ETag")
	}
	if a == isoETag(101, now) || a == isoETag(100, now.Add(time.Second)) {
		t.Error("大小或 mtime 变化时 ETag 必须变化")
	}
}

func TestSplitISOID(t *testing.T) {
	cases := []struct{ in, id, inner string }{
		{"abc123/casper/vmlinuz", "abc123", "/casper/vmlinuz"},
		{"/abc123/casper/vmlinuz", "abc123", "/casper/vmlinuz"},
		{"abc123", "abc123", "/"},
		{"", "", ""},
	}
	for _, tc := range cases {
		id, inner := splitISOID(tc.in)
		if id != tc.id || inner != tc.inner {
			t.Errorf("splitISOID(%q) = (%q,%q)，期望 (%q,%q)", tc.in, id, inner, tc.id, tc.inner)
		}
	}
}

// TestConcurrentRangeReads 验证多台客户机同时从同一个 ISO 读不同片段是安全的。
// 这是真实场景：一台机器拉 vmlinuz，另一台拉 initrd，第三台拉 boot.wim。
func TestConcurrentRangeReads(t *testing.T) {
	dir := t.TempDir()
	is := testISO(t, dir, "abc123")
	fake := newFakeISO()
	s, _, _ := newTestServer(t, []model.ISO{is}, fake)

	files := []struct {
		path string
		data []byte
	}{
		{"/casper/vmlinuz", fake.files["/casper/vmlinuz"]},
		{"/casper/initrd", fake.files["/casper/initrd"]},
		{"/sources/boot.wim", fake.files["/sources/boot.wim"]},
	}

	const workers = 8
	var wg sync.WaitGroup
	errs := make(chan string, workers*len(files))
	for w := 0; w < workers; w++ {
		for _, f := range files {
			wg.Add(1)
			go func(path string, data []byte) {
				defer wg.Done()
				rec := do(s, http.MethodGet, "/iso/abc123"+path, map[string]string{
					"Range": "bytes=64-127",
				})
				if rec.Code != http.StatusPartialContent {
					errs <- fmt.Sprintf("%s: 状态码 %d", path, rec.Code)
					return
				}
				if !bytes.Equal(rec.Body.Bytes(), data[64:128]) {
					errs <- fmt.Sprintf("%s: 内容不一致", path)
				}
			}(f.path, f.data)
		}
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
	if n := fake.reads.Load(); n < int64(workers*len(files)) {
		t.Errorf("SectionReader 调用次数偏少：%d", n)
	}
}

// TestBootDirResolution 固定 BootDir 的解析优先级：
// 显式配置优先；未配置时才退回 ISODir 的兄弟目录 boot（推断，仅作兜底）。
func TestBootDirResolution(t *testing.T) {
	base := t.TempDir()
	isoDir := filepath.Join(base, "iso")
	userDir := filepath.Join(base, "user")
	bootDir := filepath.Join(base, "boot")
	for _, d := range []string{isoDir, userDir, bootDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	// 1) 显式配置优先（pxe 层就是这么传的，isoDir 配到别处也不会错）。
	s, err := New(Config{Addr: "127.0.0.1:0", ISODir: isoDir, UserDir: userDir, BootDir: bootDir})
	if err != nil {
		t.Fatal(err)
	}
	if got := s.bootDir(); got != bootDir {
		t.Errorf("显式 BootDir 应被优先使用，得到 %q，期望 %q", got, bootDir)
	}

	// 2) 未配置时退回 ISODir 的兄弟目录。
	s2, err := New(Config{Addr: "127.0.0.1:0", ISODir: isoDir, UserDir: userDir})
	if err != nil {
		t.Fatal(err)
	}
	if got := s2.bootDir(); got != bootDir {
		t.Errorf("未配置时应退回兄弟目录，得到 %q，期望 %q", got, bootDir)
	}

	// 3) ISODir 也没配时返回空串（调用方会回 500 而不是乱读文件）。
	s3, err := New(Config{Addr: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	if got := s3.bootDir(); got != "" {
		t.Errorf("都没配置时应返回空串，得到 %q", got)
	}
}

// TestStatusRecorderReadFrom 验证包装层把 io.ReaderFrom 转发给底层，
// 否则 http.ServeContent 的大文件传输会退化成 32KB 缓冲拷贝。
func TestStatusRecorderReadFrom(t *testing.T) {
	var buf bytes.Buffer
	rec := &statusRecorder{ResponseWriter: &readerFromRecorder{&buf}, status: http.StatusOK}
	n, err := rec.ReadFrom(strings.NewReader("hello-readfrom"))
	if err != nil {
		t.Fatalf("ReadFrom 失败: %v", err)
	}
	if n != 14 || buf.String() != "hello-readfrom" {
		t.Fatalf("ReadFrom 结果不正确：n=%d buf=%q", n, buf.String())
	}
	if rec.written != 14 {
		t.Errorf("written 应被统计为 14，实际 %d", rec.written)
	}
}

// readerFromRecorder 是一个支持 io.ReaderFrom 的最小 ResponseWriter。
type readerFromRecorder struct {
	buf *bytes.Buffer
}

func (r *readerFromRecorder) Header() http.Header         { return http.Header{} }
func (r *readerFromRecorder) Write(p []byte) (int, error) { return r.buf.Write(p) }
func (r *readerFromRecorder) WriteHeader(int)             {}
func (r *readerFromRecorder) ReadFrom(src io.Reader) (int64, error) {
	return r.buf.ReadFrom(src)
}
