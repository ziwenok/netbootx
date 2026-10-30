package web

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wupengcheng/netbootx/internal/model"
)

// 说明：本文件的 JSON 解析一律用「无 tag 结构体 + 大小写不敏感匹配」，
// 避免在测试里重复维护一份 json tag。

// ---------------------------------------------------------------------------
// 测试脚手架
// ---------------------------------------------------------------------------

// newTestServer 用 FullMock 组装一个只走 Handler 的服务器（不监听端口）。
func newTestServer(t *testing.T) (*Server, *FullMock) {
	t.Helper()
	mock := NewFullMock()
	srv, err := New(Config{
		PXE:     mock,
		Store:   &MockStore{},
		Version: "9.9.9-test",
	})
	if err != nil {
		t.Fatalf("New() 失败: %v", err)
	}
	t.Cleanup(func() { _ = srv.Stop() })
	return srv, mock
}

// do 发一个请求并返回响应记录器。
func do(t *testing.T, h http.Handler, method, target string, body io.Reader, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, body)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// decodeAPI 解析统一包装，并断言 HTTP 状态码。
func decodeAPI(t *testing.T, rec *httptest.ResponseRecorder, wantCode int) apiResponse {
	t.Helper()
	if rec.Code != wantCode {
		t.Fatalf("状态码 = %d，期望 %d，响应体: %s", rec.Code, wantCode, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("Content-Type = %q，期望包含 application/json", ct)
	}
	var resp apiResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析 JSON 失败: %v，原始内容: %s", err, rec.Body.String())
	}
	return resp
}

// decodeData 解析成功响应并返回 data 的原始 JSON 字段。
func decodeData(t *testing.T, rec *httptest.ResponseRecorder) map[string]json.RawMessage {
	t.Helper()
	resp := decodeAPI(t, rec, http.StatusOK)
	if !resp.OK {
		t.Fatalf("ok = false，期望 true，error = %q", resp.Error)
	}
	raw, err := json.Marshal(resp.Data)
	if err != nil {
		t.Fatalf("重新序列化 data 失败: %v", err)
	}
	var out map[string]json.RawMessage
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("data 不是对象: %v，原始内容: %s", err, raw)
	}
	return out
}

// decodeErr 断言错误响应并返回错误文案。
func decodeErr(t *testing.T, rec *httptest.ResponseRecorder, wantCode int) string {
	t.Helper()
	resp := decodeAPI(t, rec, wantCode)
	if resp.OK {
		t.Fatalf("ok = true，期望 false")
	}
	if strings.TrimSpace(resp.Error) == "" {
		t.Fatalf("错误响应缺少 error 字段")
	}
	return resp.Error
}

// ---------------------------------------------------------------------------
// 首页与静态资源
// ---------------------------------------------------------------------------

func TestIndexPage(t *testing.T) {
	srv, _ := newTestServer(t)

	rec := do(t, srv.Handler(), http.MethodGet, "/", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Fatalf("Content-Type = %q，期望 text/html", ct)
	}
	body := rec.Body.String()
	for _, want := range []string{"NetBootX", "app.js", "style.css", "powerBtn"} {
		if !strings.Contains(body, want) {
			t.Errorf("首页缺少内容 %q", want)
		}
	}

	if rec := do(t, srv.Handler(), http.MethodGet, "/nope", nil, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("未知路径状态码 = %d，期望 404", rec.Code)
	}

	rec = do(t, srv.Handler(), http.MethodPost, "/", nil, nil)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST / 状态码 = %d，期望 405", rec.Code)
	}
	if allow := rec.Header().Get("Allow"); allow != http.MethodGet {
		t.Fatalf("Allow = %q，期望 GET", allow)
	}

	rec = do(t, srv.Handler(), http.MethodHead, "/", nil, nil)
	if rec.Code != http.StatusOK || rec.Body.Len() != 0 {
		t.Fatalf("HEAD / 状态码 = %d，body 长度 = %d", rec.Code, rec.Body.Len())
	}
}

func TestStaticAssetsServed(t *testing.T) {
	srv, _ := newTestServer(t)

	cases := []struct {
		path string
		typ  string
		mark string
	}{
		{"/static/app.js", "javascript", "EventSource"},
		{"/static/style.css", "text/css", "--accent"},
	}
	for _, c := range cases {
		rec := do(t, srv.Handler(), http.MethodGet, c.path, nil, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s 状态码 = %d，期望 200", c.path, rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, c.typ) {
			t.Errorf("GET %s Content-Type = %q，期望包含 %q", c.path, ct, c.typ)
		}
		if !strings.Contains(rec.Body.String(), c.mark) {
			t.Errorf("GET %s 内容里缺少 %q", c.path, c.mark)
		}
		if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "max-age") {
			t.Errorf("GET %s 缺少静态资源缓存头，得到 %q", c.path, cc)
		}
	}

	if rec := do(t, srv.Handler(), http.MethodGet, "/static/missing.js", nil, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("缺失静态资源状态码 = %d，期望 404", rec.Code)
	}
}

// TestIndexReferencesExist 校验 index.html 里引用的本地资源都真实存在，
// 并且没有任何外部网络请求（离线可用）。
func TestIndexReferencesExist(t *testing.T) {
	html, err := assetsFS.ReadFile("assets/index.html")
	if err != nil {
		t.Fatalf("读取 index.html 失败: %v", err)
	}
	page := string(html)

	refRe := regexp.MustCompile("(?:src|href)=\"(/static/[^\"]+)\"")
	matches := refRe.FindAllStringSubmatch(page, -1)
	if len(matches) == 0 {
		t.Fatal("index.html 里没有引用任何 /static/ 资源")
	}
	seen := map[string]bool{}
	for _, m := range matches {
		path := m[1]
		seen[path] = true
		name := strings.TrimPrefix(path, "/static/")
		if _, err := assetsFS.ReadFile("assets/" + name); err != nil {
			t.Errorf("index.html 引用了不存在的资源 %s: %v", path, err)
		}
	}
	for _, want := range []string{"/static/app.js", "/static/style.css"} {
		if !seen[want] {
			t.Errorf("index.html 没有引用 %s", want)
		}
	}

	// 不允许任何外部网络依赖（CDN / 远程字体 / 远程图标）。
	for _, bad := range []string{"http://cdn", "https://cdn", "googleapis.com", "unpkg.com", "jsdelivr", "fonts.gstatic"} {
		if strings.Contains(page, bad) {
			t.Errorf("index.html 含外部资源引用 %q", bad)
		}
	}
	if regexp.MustCompile("(?:src|href)=\"https?://").MatchString(page) {
		t.Error("index.html 里有指向外部站点的 src/href")
	}

	// CSS 里不能有 @import 或外链 url()。
	css, err := assetsFS.ReadFile("assets/style.css")
	if err != nil {
		t.Fatalf("读取 style.css 失败: %v", err)
	}
	cssText := string(css)
	if strings.Contains(cssText, "@import") {
		t.Error("style.css 使用了 @import")
	}
	if regexp.MustCompile("url\\(\\s*['\"]?https?://").MatchString(cssText) {
		t.Error("style.css 里引用了外部 URL")
	}

	// JS 里不能请求外部地址，且必须真的调用关键接口。
	js, err := assetsFS.ReadFile("assets/app.js")
	if err != nil {
		t.Fatalf("读取 app.js 失败: %v", err)
	}
	jsText := string(js)
	if regexp.MustCompile("(?:fetch|open)\\(\\s*['\"]https?://").MatchString(jsText) {
		t.Error("app.js 里请求了外部 URL")
	}
	if !strings.Contains(jsText, "/api/events/stream") {
		t.Error("app.js 没有连接 SSE 接口 /api/events/stream")
	}
	if !strings.Contains(jsText, "/api/start") {
		t.Error("app.js 没有调用 /api/start")
	}
	if !strings.Contains(jsText, "/api/preview/") {
		t.Error("app.js 没有调用 /api/preview/")
	}
}

// ---------------------------------------------------------------------------
// 状态与控制
// ---------------------------------------------------------------------------

func TestStatusEndpoint(t *testing.T) {
	srv, _ := newTestServer(t)

	data := decodeData(t, do(t, srv.Handler(), http.MethodGet, "/api/status", nil, nil))
	var st model.ServerStatus
	if err := json.Unmarshal(data["status"], &st); err != nil {
		t.Fatalf("解析 status 失败: %v", err)
	}
	if st.Running {
		t.Error("初始状态应为未运行")
	}
	if st.ServerIP != "192.168.123.10" {
		t.Errorf("ServerIP = %q", st.ServerIP)
	}
	if st.Version != "0.1.0-test" {
		t.Errorf("Version 应沿用控制器提供的值，得到 %q", st.Version)
	}
	if st.HTTPPort != 16000 || st.TFTPPort != 69 {
		t.Errorf("端口字段不正确: %+v", st)
	}

	var defISO string
	if err := json.Unmarshal(data["defaultISO"], &defISO); err != nil {
		t.Fatalf("解析 defaultISO 失败: %v", err)
	}
	if defISO != "iso-ubuntu" {
		t.Errorf("defaultISO = %q", defISO)
	}
	if _, ok := data["settings"]; !ok {
		t.Error("status 响应里缺少 settings")
	}

	rec := do(t, srv.Handler(), http.MethodPost, "/api/status", nil, nil)
	decodeErr(t, rec, http.StatusMethodNotAllowed)
	if allow := rec.Header().Get("Allow"); allow != "GET" {
		t.Errorf("Allow = %q", allow)
	}

	if rec := do(t, srv.Handler(), http.MethodGet, "/api/status/", nil, nil); rec.Code != http.StatusOK {
		t.Errorf("带尾斜杠的状态码 = %d，期望 200", rec.Code)
	}
}

func TestStatusFallbacks(t *testing.T) {
	// 控制器没填主机信息时，web 层应补上本机信息。
	mock := NewMock()
	mock.status.Hostname = ""
	mock.status.OS = ""
	mock.status.Arch = ""
	mock.status.Version = ""
	srv, err := New(Config{PXE: mock, Version: "1.2.3"})
	if err != nil {
		t.Fatalf("New() 失败: %v", err)
	}
	t.Cleanup(func() { _ = srv.Stop() })

	data := decodeData(t, do(t, srv.Handler(), http.MethodGet, "/api/status", nil, nil))
	var st model.ServerStatus
	if err := json.Unmarshal(data["status"], &st); err != nil {
		t.Fatalf("解析 status 失败: %v", err)
	}
	if st.Hostname == "" || st.OS == "" || st.Arch == "" {
		t.Errorf("兜底字段未填充: %+v", st)
	}
	if st.Version != "1.2.3" {
		t.Errorf("Version = %q，期望回落到 Config.Version", st.Version)
	}
	if st.ISOCount != 2 || st.ClientCount != 2 {
		t.Errorf("计数兜底不正确: ISOCount=%d ClientCount=%d", st.ISOCount, st.ClientCount)
	}
}

func TestStartStopRestart(t *testing.T) {
	srv, mock := newTestServer(t)
	h := srv.Handler()

	data := decodeData(t, do(t, h, http.MethodPost, "/api/start", nil, nil))
	var st model.ServerStatus
	if err := json.Unmarshal(data["status"], &st); err != nil {
		t.Fatalf("解析 status 失败: %v", err)
	}
	if !st.Running {
		t.Error("启动后 running 应为 true")
	}
	if mock.StartCalls != 1 {
		t.Errorf("StartCalls = %d，期望 1", mock.StartCalls)
	}

	// 重复启动 → 控制器报错 → 500
	rec := do(t, h, http.MethodPost, "/api/start", nil, nil)
	msg := decodeErr(t, rec, http.StatusInternalServerError)
	if !strings.Contains(msg, "启动失败") {
		t.Errorf("错误文案 = %q，期望包含「启动失败」", msg)
	}

	data = decodeData(t, do(t, h, http.MethodPost, "/api/restart", nil, nil))
	if err := json.Unmarshal(data["status"], &st); err != nil {
		t.Fatalf("解析 status 失败: %v", err)
	}
	if !st.Running {
		t.Error("重启后 running 应为 true")
	}
	if mock.RestartCalls != 1 {
		t.Errorf("RestartCalls = %d", mock.RestartCalls)
	}

	data = decodeData(t, do(t, h, http.MethodPost, "/api/stop", nil, nil))
	if err := json.Unmarshal(data["status"], &st); err != nil {
		t.Fatalf("解析 status 失败: %v", err)
	}
	if st.Running {
		t.Error("停止后 running 应为 false")
	}
	if mock.StopCalls != 1 {
		t.Errorf("StopCalls = %d", mock.StopCalls)
	}

	for _, path := range []string{"/api/start", "/api/stop", "/api/restart"} {
		rec := do(t, h, http.MethodGet, path, nil, nil)
		decodeErr(t, rec, http.StatusMethodNotAllowed)
		if allow := rec.Header().Get("Allow"); allow != http.MethodPost {
			t.Errorf("%s Allow = %q", path, allow)
		}
	}
}

func TestControlErrors(t *testing.T) {
	srv, mock := newTestServer(t)
	h := srv.Handler()

	mock.StartErr = fmt.Errorf("端口 67 被占用")
	if msg := decodeErr(t, do(t, h, http.MethodPost, "/api/start", nil, nil), http.StatusInternalServerError); !strings.Contains(msg, "端口 67 被占用") {
		t.Errorf("启动错误文案 = %q", msg)
	}

	mock.StopErr = fmt.Errorf("dhcp 停止失败")
	if msg := decodeErr(t, do(t, h, http.MethodPost, "/api/stop", nil, nil), http.StatusInternalServerError); !strings.Contains(msg, "dhcp 停止失败") {
		t.Errorf("停止错误文案 = %q", msg)
	}

	mock.RestartErr = fmt.Errorf("重启被拒绝")
	if msg := decodeErr(t, do(t, h, http.MethodPost, "/api/restart", nil, nil), http.StatusInternalServerError); !strings.Contains(msg, "重启被拒绝") {
		t.Errorf("重启错误文案 = %q", msg)
	}

	// 操作事件应写进 Store（如果配置了）。
	store, ok := srv.eventSink.(*MockStore)
	if !ok {
		t.Fatal("测试服务器应配置 MockStore")
	}
	if store.Count() == 0 {
		t.Error("控制操作没有写入事件日志")
	}
}

// ---------------------------------------------------------------------------
// ISO
// ---------------------------------------------------------------------------

func TestISOsEndpoint(t *testing.T) {
	srv, _ := newTestServer(t)

	data := decodeData(t, do(t, srv.Handler(), http.MethodGet, "/api/isos", nil, nil))
	var items []isoItem
	if err := json.Unmarshal(data["isos"], &items); err != nil {
		t.Fatalf("解析 isos 失败: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("ISO 数量 = %d，期望 2", len(items))
	}
	if items[0].SizeText == "" || items[0].ArchText == "" {
		t.Errorf("派生字段为空: %+v", items[0])
	}
	if !items[0].Default {
		t.Error("第一个 ISO 应为默认项")
	}
	if items[1].Default {
		t.Error("第二个 ISO 不应是默认项")
	}
	var count int
	if err := json.Unmarshal(data["count"], &count); err != nil || count != 2 {
		t.Errorf("count = %d (%v)", count, err)
	}

	decodeErr(t, do(t, srv.Handler(), http.MethodPost, "/api/isos", nil, nil), http.StatusMethodNotAllowed)
}

func TestRescanEndpoint(t *testing.T) {
	srv, mock := newTestServer(t)

	data := decodeData(t, do(t, srv.Handler(), http.MethodPost, "/api/isos/rescan", nil, nil))
	var n int
	if err := json.Unmarshal(data["rescanCount"], &n); err != nil || n != 2 {
		t.Errorf("rescanCount = %d (%v)", n, err)
	}
	if _, ok := data["elapsedMs"]; !ok {
		t.Error("缺少 elapsedMs")
	}
	if mock.RescanCalls != 1 {
		t.Errorf("RescanCalls = %d", mock.RescanCalls)
	}

	mock.RescanErr = fmt.Errorf("目录不可读")
	msg := decodeErr(t, do(t, srv.Handler(), http.MethodPost, "/api/isos/rescan", nil, nil), http.StatusInternalServerError)
	if !strings.Contains(msg, "目录不可读") {
		t.Errorf("错误文案 = %q", msg)
	}

	decodeErr(t, do(t, srv.Handler(), http.MethodGet, "/api/isos/rescan", nil, nil), http.StatusMethodNotAllowed)
}

func TestUploadMultipart(t *testing.T) {
	srv, mock := newTestServer(t)
	dir := t.TempDir()
	cfg := mock.Settings()
	cfg.ISODir = dir
	if err := mock.SaveSettings(cfg); err != nil {
		t.Fatalf("准备 ISO 目录失败: %v", err)
	}

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	part, err := mw.CreateFormFile("files", "alpine-3.20.iso")
	if err != nil {
		t.Fatalf("构造表单失败: %v", err)
	}
	content := bytes.Repeat([]byte("ISO-"), 1024)
	if _, err := part.Write(content); err != nil {
		t.Fatalf("写表单内容失败: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("关闭表单失败: %v", err)
	}

	rec := do(t, srv.Handler(), http.MethodPost, "/api/isos/upload", &body, map[string]string{
		"Content-Type": mw.FormDataContentType(),
	})
	data := decodeData(t, rec)

	saved := filepath.Join(dir, "alpine-3.20.iso")
	got, err := os.ReadFile(saved)
	if err != nil {
		t.Fatalf("上传的文件不存在: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Errorf("落盘内容与上传内容不一致：%d vs %d 字节", len(got), len(content))
	}

	var uploaded []map[string]any
	if err := json.Unmarshal(data["uploaded"], &uploaded); err != nil || len(uploaded) != 1 {
		t.Fatalf("uploaded 字段不正确: %s (%v)", data["uploaded"], err)
	}
	if uploaded[0]["name"] != "alpine-3.20.iso" {
		t.Errorf("uploaded[0].name = %v", uploaded[0]["name"])
	}
	if mock.RescanCalls != 1 {
		t.Errorf("上传后应触发一次重新扫描，RescanCalls = %d", mock.RescanCalls)
	}

	// 临时文件不应残留
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("读取目录失败: %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".upload-") {
			t.Errorf("残留了临时文件 %s", e.Name())
		}
	}
}

func TestUploadRawBody(t *testing.T) {
	srv, mock := newTestServer(t)
	dir := t.TempDir()
	cfg := mock.Settings()
	cfg.ISODir = dir
	_ = mock.SaveSettings(cfg)

	rec := do(t, srv.Handler(), http.MethodPost, "/api/isos/upload?name=tiny.iso",
		strings.NewReader("raw-iso-bytes"), map[string]string{"Content-Type": "application/octet-stream"})
	data := decodeData(t, rec)
	if _, ok := data["uploaded"]; !ok {
		t.Error("响应缺少 uploaded")
	}
	if _, err := os.Stat(filepath.Join(dir, "tiny.iso")); err != nil {
		t.Fatalf("文件未落盘: %v", err)
	}

	rec = do(t, srv.Handler(), http.MethodPost, "/api/isos/upload",
		strings.NewReader("x"), map[string]string{"Content-Type": "application/octet-stream"})
	if msg := decodeErr(t, rec, http.StatusBadRequest); !strings.Contains(msg, "缺少文件名") {
		t.Errorf("错误文案 = %q", msg)
	}

	rec = do(t, srv.Handler(), http.MethodPost, "/api/isos/upload?name=evil.txt",
		strings.NewReader("x"), nil)
	if msg := decodeErr(t, rec, http.StatusBadRequest); !strings.Contains(msg, ".iso") {
		t.Errorf("错误文案 = %q", msg)
	}

	rec = do(t, srv.Handler(), http.MethodPost, "/api/isos/upload?name=empty.iso",
		strings.NewReader(""), nil)
	decodeErr(t, rec, http.StatusInternalServerError)

	decodeErr(t, do(t, srv.Handler(), http.MethodGet, "/api/isos/upload", nil, nil), http.StatusMethodNotAllowed)
}

func TestUploadPathTraversalIsContained(t *testing.T) {
	srv, mock := newTestServer(t)
	dir := t.TempDir()
	cfg := mock.Settings()
	cfg.ISODir = dir
	_ = mock.SaveSettings(cfg)

	rec := do(t, srv.Handler(), http.MethodPost, "/api/isos/upload?name=../../escape.iso",
		strings.NewReader("payload"), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（文件名应被裁剪而不是报错），响应: %s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "escape.iso")); err != nil {
		t.Fatalf("文件没有落在 ISO 目录内: %v", err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(dir), "escape.iso")); err == nil {
		t.Fatal("文件逃逸出了 ISO 目录")
	}
}

// ---------------------------------------------------------------------------
// 客户机
// ---------------------------------------------------------------------------

func TestClientsEndpoint(t *testing.T) {
	srv, mock := newTestServer(t)

	data := decodeData(t, do(t, srv.Handler(), http.MethodGet, "/api/clients", nil, nil))
	var clients []model.Client
	if err := json.Unmarshal(data["clients"], &clients); err != nil {
		t.Fatalf("解析 clients 失败: %v", err)
	}
	if len(clients) != 2 {
		t.Fatalf("客户机数量 = %d，期望 2", len(clients))
	}
	if clients[0].MAC != "aa:bb:cc:dd:ee:01" {
		t.Errorf("排序不正确，第一台是 %s", clients[0].MAC)
	}

	data = decodeData(t, do(t, srv.Handler(), http.MethodDelete, "/api/clients", nil, nil))
	var cleared bool
	if err := json.Unmarshal(data["cleared"], &cleared); err != nil || !cleared {
		t.Errorf("cleared = %v (%v)", cleared, err)
	}
	if mock.ClearClientsCalls != 1 {
		t.Errorf("ClearClientsCalls = %d", mock.ClearClientsCalls)
	}
	data = decodeData(t, do(t, srv.Handler(), http.MethodGet, "/api/clients", nil, nil))
	if err := json.Unmarshal(data["clients"], &clients); err != nil || len(clients) != 0 {
		t.Errorf("清空后仍有 %d 台客户机 (%v)", len(clients), err)
	}

	mock.ClearClientsErr = fmt.Errorf("存储不可写")
	if msg := decodeErr(t, do(t, srv.Handler(), http.MethodDelete, "/api/clients", nil, nil), http.StatusInternalServerError); !strings.Contains(msg, "存储不可写") {
		t.Errorf("错误文案 = %q", msg)
	}

	decodeErr(t, do(t, srv.Handler(), http.MethodPost, "/api/clients", nil, nil), http.StatusMethodNotAllowed)
}

func TestClientsClearUnsupported(t *testing.T) {
	mock := NewMock()
	srv, err := New(Config{PXE: mock})
	if err != nil {
		t.Fatalf("New() 失败: %v", err)
	}
	t.Cleanup(func() { _ = srv.Stop() })

	msg := decodeErr(t, do(t, srv.Handler(), http.MethodDelete, "/api/clients", nil, nil), http.StatusNotImplemented)
	if !strings.Contains(msg, "不支持") {
		t.Errorf("错误文案 = %q", msg)
	}
}

func TestClientsEmptyIsArray(t *testing.T) {
	mock := NewMock()
	mock.clients = nil
	srv, err := New(Config{PXE: mock})
	if err != nil {
		t.Fatalf("New() 失败: %v", err)
	}
	t.Cleanup(func() { _ = srv.Stop() })

	rec := do(t, srv.Handler(), http.MethodGet, "/api/clients", nil, nil)
	if !strings.Contains(rec.Body.String(), "\"clients\":[]") {
		t.Errorf("空客户机列表应序列化为 []，实际: %s", rec.Body.String())
	}
}

// ---------------------------------------------------------------------------
// 事件
// ---------------------------------------------------------------------------

func TestEventsEndpoint(t *testing.T) {
	srv, _ := newTestServer(t)

	data := decodeData(t, do(t, srv.Handler(), http.MethodGet, "/api/events", nil, nil))
	var events []model.Event
	if err := json.Unmarshal(data["events"], &events); err != nil {
		t.Fatalf("解析 events 失败: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("事件数量 = %d，期望 2", len(events))
	}
	var limit int
	if err := json.Unmarshal(data["limit"], &limit); err != nil || limit != defaultEventLimit {
		t.Errorf("默认 limit = %d (%v)", limit, err)
	}

	data = decodeData(t, do(t, srv.Handler(), http.MethodGet, "/api/events?limit=1", nil, nil))
	if err := json.Unmarshal(data["events"], &events); err != nil || len(events) != 1 {
		t.Errorf("limit=1 时事件数量 = %d (%v)", len(events), err)
	}

	data = decodeData(t, do(t, srv.Handler(), http.MethodGet, "/api/events?limit=999999", nil, nil))
	if err := json.Unmarshal(data["limit"], &limit); err != nil || limit != maxEventLimit {
		t.Errorf("超大 limit 应截断为 %d，得到 %d", maxEventLimit, limit)
	}

	for _, bad := range []string{"abc", "0", "-5"} {
		msg := decodeErr(t, do(t, srv.Handler(), http.MethodGet, "/api/events?limit="+bad, nil, nil), http.StatusBadRequest)
		if !strings.Contains(msg, "limit") {
			t.Errorf("limit=%s 错误文案 = %q", bad, msg)
		}
	}

	decodeErr(t, do(t, srv.Handler(), http.MethodPost, "/api/events", nil, nil), http.StatusMethodNotAllowed)
}

// ---------------------------------------------------------------------------
// 配置
// ---------------------------------------------------------------------------

func TestSettingsGetAndPost(t *testing.T) {
	srv, mock := newTestServer(t)
	h := srv.Handler()

	data := decodeData(t, do(t, h, http.MethodGet, "/api/settings", nil, nil))
	var cfg model.Settings
	raw, _ := json.Marshal(data)
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("解析 settings 失败: %v", err)
	}
	if cfg.ServerIP != "192.168.123.10" {
		t.Errorf("ServerIP = %q", cfg.ServerIP)
	}

	// 部分字段更新：只提交 dhcpMode 与 rangeStart
	data = decodeData(t, do(t, h, http.MethodPost, "/api/settings",
		strings.NewReader("{\"dhcpMode\":\"full\",\"rangeStart\":\"10.0.0.10\"}"), nil))
	var saved struct {
		Settings model.Settings
		Saved    bool
	}
	raw, _ = json.Marshal(data)
	if err := json.Unmarshal(raw, &saved); err != nil {
		t.Fatalf("解析保存结果失败: %v", err)
	}
	if !saved.Saved {
		t.Error("saved 应为 true")
	}
	if saved.Settings.DHCPMode != "full" || saved.Settings.RangeStart != "10.0.0.10" {
		t.Errorf("字段未更新: %+v", saved.Settings)
	}
	if saved.Settings.ServerIP != "192.168.123.10" {
		t.Errorf("未提交的 ServerIP 被覆盖为 %q", saved.Settings.ServerIP)
	}
	if saved.Settings.RangeEnd != mock.Settings().RangeEnd {
		t.Errorf("未提交的 RangeEnd 被覆盖为 %q", saved.Settings.RangeEnd)
	}
	if mock.SaveSettingsCalls != 1 {
		t.Errorf("SaveSettingsCalls = %d", mock.SaveSettingsCalls)
	}
	if mock.LastSaved.DHCPMode != "full" {
		t.Errorf("LastSaved.DHCPMode = %q", mock.LastSaved.DHCPMode)
	}

	// bool 字段显式传 false 必须生效（不能被当成「未提供」）
	data = decodeData(t, do(t, h, http.MethodPost, "/api/settings",
		strings.NewReader("{\"autoStart\":false,\"secureBoot\":true}"), nil))
	raw, _ = json.Marshal(data)
	if err := json.Unmarshal(raw, &saved); err != nil {
		t.Fatalf("解析保存结果失败: %v", err)
	}
	if saved.Settings.AutoStart || !saved.Settings.SecureBoot {
		t.Errorf("bool 字段合并错误: autoStart=%v secureBoot=%v", saved.Settings.AutoStart, saved.Settings.SecureBoot)
	}

	// DNS 数组
	data = decodeData(t, do(t, h, http.MethodPost, "/api/settings",
		strings.NewReader("{\"dns\":[\"1.1.1.1\",\"8.8.8.8\"]}"), nil))
	raw, _ = json.Marshal(data)
	if err := json.Unmarshal(raw, &saved); err != nil {
		t.Fatalf("解析保存结果失败: %v", err)
	}
	if len(saved.Settings.DNS) != 2 || saved.Settings.DNS[0] != "1.1.1.1" {
		t.Errorf("DNS 未更新: %v", saved.Settings.DNS)
	}

	decodeErr(t, do(t, h, http.MethodPut, "/api/settings", nil, nil), http.StatusMethodNotAllowed)
}

func TestSettingsErrors(t *testing.T) {
	srv, mock := newTestServer(t)
	h := srv.Handler()

	if msg := decodeErr(t, do(t, h, http.MethodPost, "/api/settings", strings.NewReader("{oops"), nil), http.StatusBadRequest); !strings.Contains(msg, "解析配置") {
		t.Errorf("错误文案 = %q", msg)
	}
	if msg := decodeErr(t, do(t, h, http.MethodPost, "/api/settings", strings.NewReader("[1,2]"), nil), http.StatusBadRequest); !strings.Contains(msg, "JSON 对象") {
		t.Errorf("错误文案 = %q", msg)
	}
	if msg := decodeErr(t, do(t, h, http.MethodPost, "/api/settings", strings.NewReader("{\"leaseTime\":\"abc\"}"), nil), http.StatusBadRequest); !strings.Contains(msg, "leaseTime") {
		t.Errorf("错误文案 = %q", msg)
	}
	if msg := decodeErr(t, do(t, h, http.MethodPost, "/api/settings", strings.NewReader("{\"dhcpMode\":\"bogus\"}"), nil), http.StatusBadRequest); !strings.Contains(msg, "dhcpMode") {
		t.Errorf("错误文案 = %q", msg)
	}
	if msg := decodeErr(t, do(t, h, http.MethodPost, "/api/settings", strings.NewReader("{\"dhcpMode\":\"full\",\"rangeStart\":\"not-an-ip\"}"), nil), http.StatusBadRequest); !strings.Contains(msg, "rangeStart") {
		t.Errorf("错误文案 = %q", msg)
	}
	if msg := decodeErr(t, do(t, h, http.MethodPost, "/api/settings", strings.NewReader("{\"httpPort\":70000}"), nil), http.StatusBadRequest); !strings.Contains(msg, "httpPort") {
		t.Errorf("错误文案 = %q", msg)
	}
	mock.SaveSettingsErr = fmt.Errorf("磁盘只读")
	if msg := decodeErr(t, do(t, h, http.MethodPost, "/api/settings", strings.NewReader("{\"domain\":\"x.lan\"}"), nil), http.StatusInternalServerError); !strings.Contains(msg, "磁盘只读") {
		t.Errorf("错误文案 = %q", msg)
	}
}

func TestSettingsServerIPValidation(t *testing.T) {
	srv, _ := newTestServer(t)
	rec := do(t, srv.Handler(), http.MethodPost, "/api/settings", strings.NewReader("{\"serverIP\":\"999.1.1.1\"}"), nil)
	if msg := decodeErr(t, rec, http.StatusBadRequest); !strings.Contains(msg, "serverIP") {
		t.Errorf("错误文案 = %q", msg)
	}
}

// ---------------------------------------------------------------------------
// 网卡 / 版本
// ---------------------------------------------------------------------------

func TestInterfacesEndpoint(t *testing.T) {
	srv, mock := newTestServer(t)

	data := decodeData(t, do(t, srv.Handler(), http.MethodGet, "/api/interfaces", nil, nil))
	var ifaces []NetInterface
	if err := json.Unmarshal(data["interfaces"], &ifaces); err != nil {
		t.Fatalf("解析 interfaces 失败: %v", err)
	}
	if len(ifaces) != 2 {
		t.Fatalf("网卡数量 = %d", len(ifaces))
	}
	if ifaces[0].Name != "en0" || ifaces[0].IP != "192.168.123.10" || !ifaces[0].Up || !ifaces[0].Default {
		t.Errorf("网卡字段不正确: %+v", ifaces[0])
	}
	if ifaces[1].Up {
		t.Error("en1 应为 down")
	}

	mock.ifaces = nil
	data = decodeData(t, do(t, srv.Handler(), http.MethodGet, "/api/interfaces", nil, nil))
	if err := json.Unmarshal(data["interfaces"], &ifaces); err != nil || len(ifaces) != 0 {
		t.Errorf("空网卡列表应为 []，得到 %d (%v)", len(ifaces), err)
	}

	decodeErr(t, do(t, srv.Handler(), http.MethodPost, "/api/interfaces", nil, nil), http.StatusMethodNotAllowed)
}

func TestVersionEndpoint(t *testing.T) {
	srv, _ := newTestServer(t)
	data := decodeData(t, do(t, srv.Handler(), http.MethodGet, "/api/version", nil, nil))
	var v struct {
		Version   string
		Name      string
		GoVersion string
		OS        string
		Arch      string
	}
	raw, _ := json.Marshal(data)
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("解析 version 失败: %v", err)
	}
	if v.Version != "9.9.9-test" || v.Name != "NetBootX" {
		t.Errorf("版本信息不正确: %+v", v)
	}
	if v.GoVersion == "" || v.OS == "" || v.Arch == "" {
		t.Errorf("运行时信息缺失: %+v", v)
	}
	decodeErr(t, do(t, srv.Handler(), http.MethodPost, "/api/version", nil, nil), http.StatusMethodNotAllowed)
}

// ---------------------------------------------------------------------------
// 预览
// ---------------------------------------------------------------------------

func TestPreviewUsesPXEGenerator(t *testing.T) {
	srv, mock := newTestServer(t)
	mock.PreviewScript = "#!ipxe\n# 来自 pxe 层\necho hello\nboot\n"

	data := decodeData(t, do(t, srv.Handler(), http.MethodGet, "/api/preview/iso-ubuntu?mac=AA-BB-CC-DD-EE-01", nil, nil))
	var out struct {
		ISOID  string
		Mac    string
		Source string
		Script string
	}
	raw, _ := json.Marshal(data)
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("解析 preview 失败: %v", err)
	}
	if out.ISOID != "iso-ubuntu" {
		t.Errorf("isoID = %q", out.ISOID)
	}
	if out.Source != "pxe" {
		t.Errorf("source = %q，期望 pxe", out.Source)
	}
	if out.Script != mock.PreviewScript {
		t.Errorf("脚本不是 pxe 层生成的内容: %q", out.Script)
	}
	if out.Mac != "aa:bb:cc:dd:ee:01" {
		t.Errorf("MAC 未规范化: %q", out.Mac)
	}
	if mock.PreviewCalls != 1 || mock.LastPreviewID != "iso-ubuntu" {
		t.Errorf("Preview 调用记录不正确: calls=%d id=%q", mock.PreviewCalls, mock.LastPreviewID)
	}

	msg := decodeErr(t, do(t, srv.Handler(), http.MethodGet, "/api/preview/no-such-iso", nil, nil), http.StatusNotFound)
	if !strings.Contains(msg, "找不到 ISO") {
		t.Errorf("错误文案 = %q", msg)
	}

	decodeErr(t, do(t, srv.Handler(), http.MethodPost, "/api/preview/iso-ubuntu", nil, nil), http.StatusMethodNotAllowed)
}

func TestPreviewFallbackGenerator(t *testing.T) {
	mock := NewMock()
	srv, err := New(Config{PXE: mock})
	if err != nil {
		t.Fatalf("New() 失败: %v", err)
	}
	t.Cleanup(func() { _ = srv.Stop() })

	data := decodeData(t, do(t, srv.Handler(), http.MethodGet, "/api/preview/iso-ubuntu", nil, nil))
	var out struct {
		Source string
		Script string
	}
	raw, _ := json.Marshal(data)
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("解析 preview 失败: %v", err)
	}
	if out.Source != "web" {
		t.Errorf("source = %q，期望 web", out.Source)
	}
	for _, want := range []string{"#!ipxe", "kernel http://", "initrd http://", "boot || goto failed"} {
		if !strings.Contains(out.Script, want) {
			t.Errorf("脚本缺少 %q:\n%s", want, out.Script)
		}
	}
	if !strings.Contains(out.Script, "00:00:00:00:00:00") {
		t.Error("未使用默认 MAC")
	}

	data = decodeData(t, do(t, srv.Handler(), http.MethodGet, "/api/preview/win11.iso", nil, nil))
	raw, _ = json.Marshal(data)
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("解析 preview 失败: %v", err)
	}
	if !strings.Contains(out.Script, "kernel wimboot") {
		t.Errorf("Windows 镜像应生成 wimboot 脚本:\n%s", out.Script)
	}
	for _, want := range []string{"initrd --name bootmgr", "initrd --name bcd", "initrd --name boot.sdi", "initrd --name boot.wim"} {
		if !strings.Contains(out.Script, want) {
			t.Errorf("wimboot 脚本缺少 %q:\n%s", want, out.Script)
		}
	}
}

func TestPreviewPXEErrorFallsBack(t *testing.T) {
	srv, mock := newTestServer(t)
	mock.PreviewErr = fmt.Errorf("生成器炸了")

	data := decodeData(t, do(t, srv.Handler(), http.MethodGet, "/api/preview/iso-ubuntu", nil, nil))
	var out struct {
		Source string
	}
	raw, _ := json.Marshal(data)
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("解析 preview 失败: %v", err)
	}
	if out.Source != "web" {
		t.Errorf("pxe 生成失败时应退回 web 生成器，source = %q", out.Source)
	}
}

// ---------------------------------------------------------------------------
// 路由兜底 / panic 恢复 / 健康检查
// ---------------------------------------------------------------------------

func TestUnknownAPIRoute(t *testing.T) {
	srv, _ := newTestServer(t)
	msg := decodeErr(t, do(t, srv.Handler(), http.MethodGet, "/api/not-exist", nil, nil), http.StatusNotFound)
	if !strings.Contains(msg, "未知接口") {
		t.Errorf("错误文案 = %q", msg)
	}
	decodeErr(t, do(t, srv.Handler(), http.MethodGet, "/api/", nil, nil), http.StatusNotFound)
	decodeErr(t, do(t, srv.Handler(), http.MethodGet, "/api/preview/", nil, nil), http.StatusNotFound)
}

func TestHealthz(t *testing.T) {
	srv, _ := newTestServer(t)
	rec := do(t, srv.Handler(), http.MethodGet, "/healthz", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if body["status"] != "ok" {
		t.Errorf("status = %v", body["status"])
	}
	if body["version"] != "9.9.9-test" {
		t.Errorf("version = %v", body["version"])
	}
	decodeErr(t, do(t, srv.Handler(), http.MethodPost, "/healthz", nil, nil), http.StatusMethodNotAllowed)
}

// panicController 让 Status 直接 panic，验证 recover 中间件。
type panicController struct{ *FullMock }

func (p panicController) Status() model.ServerStatus { panic("故意 panic") }

func TestRecoverMiddleware(t *testing.T) {
	srv, err := New(Config{PXE: panicController{NewFullMock()}})
	if err != nil {
		t.Fatalf("New() 失败: %v", err)
	}
	t.Cleanup(func() { _ = srv.Stop() })

	rec := do(t, srv.Handler(), http.MethodGet, "/api/status", nil, nil)
	msg := decodeErr(t, rec, http.StatusInternalServerError)
	if !strings.Contains(msg, "内部错误") {
		t.Errorf("错误文案 = %q", msg)
	}

	rec = do(t, srv.Handler(), http.MethodGet, "/healthz", nil, nil)
	if rec.Code != http.StatusOK {
		t.Errorf("panic 后 healthz 状态码 = %d", rec.Code)
	}
}

func TestNewRequiresPXE(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatal("缺少 PXE 时应返回错误")
	}
}

// ---------------------------------------------------------------------------
// 监听 / 优雅关闭
// ---------------------------------------------------------------------------

func TestServerStartStop(t *testing.T) {
	mock := NewFullMock()
	srv, err := New(Config{Addr: "127.0.0.1:0", PXE: mock, Version: "1.0.0"})
	if err != nil {
		t.Fatalf("New() 失败: %v", err)
	}

	if srv.Addr() != "127.0.0.1:0" {
		t.Errorf("启动前 Addr = %q", srv.Addr())
	}

	if err := srv.Start(); err != nil {
		t.Fatalf("Start() 失败: %v", err)
	}
	addr := srv.Addr()
	if addr == "127.0.0.1:0" || strings.HasSuffix(addr, ":0") {
		t.Fatalf("Start 后没有拿到真实地址: %q", addr)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("重复 Start 应幂等，得到: %v", err)
	}

	base := "http://" + addr
	resp, err := http.Get(base + "/api/version")
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "1.0.0") {
		t.Errorf("响应 = %s", body)
	}

	resp2, err := http.Get(base + "/")
	if err != nil {
		t.Fatalf("请求首页失败: %v", err)
	}
	html, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()
	if !strings.Contains(string(html), "NetBootX") {
		t.Error("首页内容不正确")
	}

	start := time.Now()
	if err := srv.Stop(); err != nil {
		t.Fatalf("Stop() 失败: %v", err)
	}
	if cost := time.Since(start); cost > 2*time.Second {
		t.Errorf("Stop 耗时 %s，超过 2 秒上限", cost)
	}
	if err := srv.Stop(); err != nil {
		t.Fatalf("重复 Stop 应幂等，得到: %v", err)
	}
	if err := srv.Start(); err == nil {
		t.Error("关闭后 Start 应报错")
	}
	if _, err := http.Get(base + "/healthz"); err == nil {
		t.Error("关闭后仍能访问服务")
	}
}

func TestStartPortInUse(t *testing.T) {
	first, err := New(Config{Addr: "127.0.0.1:0", PXE: NewFullMock()})
	if err != nil {
		t.Fatalf("New() 失败: %v", err)
	}
	if err := first.Start(); err != nil {
		t.Fatalf("Start() 失败: %v", err)
	}
	t.Cleanup(func() { _ = first.Stop() })

	second, err := New(Config{Addr: first.Addr(), PXE: NewFullMock()})
	if err != nil {
		t.Fatalf("New() 失败: %v", err)
	}
	t.Cleanup(func() { _ = second.Stop() })
	if err := second.Start(); err == nil {
		t.Fatal("端口被占用时应返回错误")
	} else if !strings.Contains(err.Error(), "监听") {
		t.Errorf("错误信息不友好: %v", err)
	}
}

// ---------------------------------------------------------------------------
// SSE
// ---------------------------------------------------------------------------

// readSSE 从流里读取直到出现 marker。
func readSSE(t *testing.T, r io.Reader, marker string, timeout time.Duration) string {
	t.Helper()
	type result struct {
		text string
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		var sb strings.Builder
		buf := make([]byte, 2048)
		for {
			n, err := r.Read(buf)
			if n > 0 {
				sb.Write(buf[:n])
				if strings.Contains(sb.String(), marker) {
					ch <- result{sb.String(), nil}
					return
				}
			}
			if err != nil {
				ch <- result{sb.String(), err}
				return
			}
		}
	}()
	select {
	case got := <-ch:
		if !strings.Contains(got.text, marker) {
			t.Fatalf("等待 SSE 消息 %q 失败: %v，已收到:\n%s", marker, got.err, got.text)
		}
		return got.text
	case <-time.After(timeout):
		t.Fatalf("等待 SSE 消息 %q 超时", marker)
		return ""
	}
}

func TestSSEStreamSnapshot(t *testing.T) {
	mock := NewFullMock()
	srv, err := New(Config{Addr: "127.0.0.1:0", PXE: mock, Version: "1.0.0"})
	if err != nil {
		t.Fatalf("New() 失败: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("Start() 失败: %v", err)
	}
	t.Cleanup(func() { _ = srv.Stop() })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+srv.Addr()+"/api/events/stream", nil)
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("连接 SSE 失败: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q", ct)
	}
	if cc := resp.Header.Get("Cache-Control"); !strings.Contains(cc, "no-cache") {
		t.Errorf("Cache-Control = %q", cc)
	}
	if conn := resp.Header.Get("Connection"); conn != "keep-alive" {
		t.Errorf("Connection = %q", conn)
	}

	text := readSSE(t, resp.Body, "event: snapshot", 3*time.Second)
	if !strings.Contains(text, "retry: 3000") {
		t.Errorf("缺少 retry 指令:\n%s", text)
	}
	for _, want := range []string{"status", "clients", "isos", "events", "interfaces"} {
		if !strings.Contains(text, want) {
			t.Errorf("快照缺少字段 %s:\n%s", want, text)
		}
	}

	mock.SetRunning(true)
	srv.pushUpdate()
	text = readSSE(t, resp.Body, "event: status", 3*time.Second)
	if !strings.Contains(text, "\"running\":true") {
		t.Errorf("status 增量内容不正确:\n%s", text)
	}
}

func TestSSEPushClientsAndEvents(t *testing.T) {
	srv, mock := newTestServer(t)

	id, ch := srv.hub.subscribe()
	defer srv.hub.unsubscribe(id)

	if srv.hub.count() != 1 {
		t.Fatalf("订阅数 = %d", srv.hub.count())
	}

	// 注意：新增客户机也会让 Status().ClientCount 变化，所以 status 事件可能先到，
	// 这里按「等到目标事件为止」的方式收集。
	collect := func(want string) string {
		deadline := time.After(2 * time.Second)
		for {
			select {
			case m := <-ch:
				if m.event == want {
					return string(m.data)
				}
			case <-deadline:
				t.Fatalf("超时：没有收到 %q 事件", want)
			}
		}
	}

	mock.AddClient(model.Client{MAC: "aa:bb:cc:dd:ee:09", IP: "192.168.123.209", LastSeen: time.Now()})
	srv.pushUpdate()
	if data := collect(sseEventClients); !strings.Contains(data, "aa:bb:cc:dd:ee:09") {
		t.Errorf("客户机增量内容不正确: %s", data)
	}

	mock.AddEvent(model.Event{Time: time.Now().Add(time.Second), Level: "warn", Kind: "tftp", Msg: "传输重试"})
	srv.pushUpdate()
	if data := collect(sseEventEvents); !strings.Contains(data, "传输重试") {
		t.Errorf("日志增量内容不正确: %s", data)
	}

	srv.pushUpdate()
	select {
	case m := <-ch:
		t.Fatalf("没有变化却收到了 %q: %s", m.event, m.data)
	case <-time.After(80 * time.Millisecond):
	}
}

func TestSSEEncodeFormat(t *testing.T) {
	msg := sseMessage{event: "status", data: []byte("{\n\"a\":1\n}")}
	got := string(msg.encode())
	want := "event: status\ndata: {\ndata: \"a\":1\ndata: }\n\n"
	if got != want {
		t.Fatalf("编码结果 = %q，期望 %q", got, want)
	}
}

func TestSSEStreamClosesOnStop(t *testing.T) {
	mock := NewFullMock()
	srv, err := New(Config{Addr: "127.0.0.1:0", PXE: mock})
	if err != nil {
		t.Fatalf("New() 失败: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("Start() 失败: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+srv.Addr()+"/api/events/stream", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("连接 SSE 失败: %v", err)
	}
	defer resp.Body.Close()
	readSSE(t, resp.Body, "event: snapshot", 3*time.Second)

	done := make(chan error, 1)
	go func() {
		buf := make([]byte, 512)
		for {
			if _, err := resp.Body.Read(buf); err != nil {
				done <- err
				return
			}
		}
	}()
	time.Sleep(50 * time.Millisecond)
	if err := srv.Stop(); err != nil {
		t.Fatalf("Stop() 失败: %v", err)
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop 之后 SSE 长连接没有关闭")
	}
}

func TestSSEHubSlowClientDrops(t *testing.T) {
	h := newHub()
	id, ch := h.subscribe()
	defer h.unsubscribe(id)

	for i := 0; i < sseClientBuffer+10; i++ {
		h.publish(sseEventStatus, []byte("{\"i\":1}"))
	}
	if len(ch) != sseClientBuffer {
		t.Errorf("缓冲区长度 = %d，期望 %d", len(ch), sseClientBuffer)
	}
}

func TestSetStreamInterval(t *testing.T) {
	srv, _ := newTestServer(t)
	if got := srv.streamInterval(); got != defaultStreamInterval {
		t.Errorf("默认间隔 = %s", got)
	}
	srv.SetStreamInterval(25 * time.Millisecond)
	if got := srv.streamInterval(); got != 25*time.Millisecond {
		t.Errorf("设置后间隔 = %s", got)
	}
	srv.SetStreamInterval(0)
	if got := srv.streamInterval(); got != defaultStreamInterval {
		t.Errorf("非法值应回落到默认，得到 %s", got)
	}
}

func TestSSEPollLoopPushes(t *testing.T) {
	srv, mock := newTestServer(t)
	srv.SetStreamInterval(20 * time.Millisecond)

	id, ch := srv.hub.subscribe()
	defer srv.hub.unsubscribe(id)

	mock.SetRunning(true)
	select {
	case msg := <-ch:
		if msg.event != sseEventStatus && msg.event != sseEventEvents {
			t.Fatalf("收到意外事件 %q", msg.event)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("pollLoop 没有推送状态变化")
	}
}

func TestSnapshotPayload(t *testing.T) {
	srv, _ := newTestServer(t)
	payload, err := srv.snapshotPayload()
	if err != nil {
		t.Fatalf("生成快照失败: %v", err)
	}
	var snap map[string]json.RawMessage
	if err := json.Unmarshal(payload, &snap); err != nil {
		t.Fatalf("解析快照失败: %v", err)
	}
	for _, key := range []string{"status", "clients", "isos", "events", "defaultISO", "interfaces", "version", "serverTime"} {
		if _, ok := snap[key]; !ok {
			t.Errorf("快照缺少字段 %s", key)
		}
	}
}

// ---------------------------------------------------------------------------
// 纯函数
// ---------------------------------------------------------------------------

func TestHumanSize(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{1024, "1.0 KB"},
		{1536, "1.5 KB"},
		{1024 * 1024, "1.0 MB"},
		{3_200_000_000, "3.0 GB"},
		{1 << 40, "1.0 TB"},
		{1 << 50, "1.0 PB"},
	}
	for _, c := range cases {
		if got := humanSize(c.in); got != c.want {
			t.Errorf("humanSize(%d) = %q，期望 %q", c.in, got, c.want)
		}
	}
}

func TestSafeISOName(t *testing.T) {
	ok := map[string]string{
		"ubuntu.iso":          "ubuntu.iso",
		"  win11.ISO  ":       "win11.ISO",
		"C:\\Users\\a\\b.iso": "b.iso",
		"/tmp/../etc/x.iso":   "x.iso",
		"../../escape.iso":    "escape.iso",
	}
	for in, want := range ok {
		got, err := safeISOName(in)
		if err != nil {
			t.Errorf("safeISOName(%q) 意外失败: %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("safeISOName(%q) = %q，期望 %q", in, got, want)
		}
	}

	bad := []string{"", "   ", "..", ".", "evil.txt", "noext", "a/b", "a\\b"}
	for _, in := range bad {
		if got, err := safeISOName(in); err == nil {
			t.Errorf("safeISOName(%q) 应失败，却返回 %q", in, got)
		}
	}
}

func TestParseLimit(t *testing.T) {
	if n, err := parseLimit("", 200, 2000); err != nil || n != 200 {
		t.Errorf("空值应返回默认值，得到 %d, %v", n, err)
	}
	if n, err := parseLimit("50", 200, 2000); err != nil || n != 50 {
		t.Errorf("解析 50 得到 %d, %v", n, err)
	}
	if n, err := parseLimit("5000", 200, 2000); err != nil || n != 2000 {
		t.Errorf("超限应截断，得到 %d, %v", n, err)
	}
	for _, bad := range []string{"abc", "0", "-1", "1.5"} {
		if _, err := parseLimit(bad, 200, 2000); err == nil {
			t.Errorf("parseLimit(%q) 应报错", bad)
		}
	}
}

func TestMergeSettings(t *testing.T) {
	cur := model.DefaultSettings()
	cur.ServerIP = "192.168.1.10"
	cur.DNS = []string{"1.1.1.1"}

	out, err := mergeSettings(cur, []byte("{}"))
	if err != nil {
		t.Fatalf("合并失败: %v", err)
	}
	if out.ServerIP != "192.168.1.10" || len(out.DNS) != 1 {
		t.Errorf("空对象不应修改配置: %+v", out)
	}

	out, err = mergeSettings(cur, []byte("{\"serverIP\":\"10.0.0.1\"}"))
	if err != nil {
		t.Fatalf("合并失败: %v", err)
	}
	if out.ServerIP != "10.0.0.1" {
		t.Errorf("ServerIP 未更新: %q", out.ServerIP)
	}
	if out.DHCPMode != cur.DHCPMode || out.HTTPPort != cur.HTTPPort {
		t.Errorf("其他字段被意外修改: %+v", out)
	}

	out, err = mergeSettings(cur, []byte("{\"leaseTime\":0,\"autoStart\":false}"))
	if err != nil {
		t.Fatalf("合并失败: %v", err)
	}
	if out.LeaseTime != 0 || out.AutoStart {
		t.Errorf("显式零值未生效: leaseTime=%d autoStart=%v", out.LeaseTime, out.AutoStart)
	}

	if _, err := mergeSettings(cur, []byte("{bad")); err == nil {
		t.Error("非法 JSON 应报错")
	}
	if _, err := mergeSettings(cur, []byte("null")); err == nil {
		t.Error("null 应报错")
	}
	if _, err := mergeSettings(cur, []byte("{\"leaseTime\":\"x\"}")); err == nil {
		t.Error("类型错误应报错")
	}
}

func TestValidateSettings(t *testing.T) {
	good := model.DefaultSettings()
	good.ServerIP = "192.168.1.10"
	if err := validateSettings(good); err != nil {
		t.Errorf("默认配置应合法: %v", err)
	}

	bad := []struct {
		name string
		mod  func(*model.Settings)
	}{
		{"dhcpMode", func(s *model.Settings) { s.DHCPMode = "bogus" }},
		{"serverIP", func(s *model.Settings) { s.ServerIP = "300.1.1.1" }},
		{"rangeStart", func(s *model.Settings) { s.DHCPMode = "full"; s.RangeStart = "x" }},
		{"rangeEnd", func(s *model.Settings) { s.DHCPMode = "full"; s.RangeEnd = "x" }},
		{"httpPort", func(s *model.Settings) { s.HTTPPort = 70000 }},
		{"tftpPort", func(s *model.Settings) { s.TFTPPort = -1 }},
		{"leaseTime", func(s *model.Settings) { s.LeaseTime = -1 }},
		{"bootMenuTimeout", func(s *model.Settings) { s.BootMenuTimeout = -1 }},
	}
	for _, c := range bad {
		cfg := model.DefaultSettings()
		cfg.ServerIP = "192.168.1.10"
		c.mod(&cfg)
		if err := validateSettings(cfg); err == nil {
			t.Errorf("%s 非法时未报错", c.name)
		} else if !strings.Contains(err.Error(), c.name) {
			t.Errorf("%s 的错误信息不明确: %v", c.name, err)
		}
	}
}

func TestBuildPreviewScript(t *testing.T) {
	s := model.DefaultSettings()
	s.ServerIP = "192.168.1.10"
	s.HTTPPort = 16000
	s.BootMenuTimeout = 15

	kernelISO := model.ISO{
		ID:       "abc123",
		Name:     "ubuntu.iso",
		OSName:   "Ubuntu 24.04 LTS",
		Family:   model.FamilyLinux,
		Bootable: true,
		Boot: model.BootSpec{
			Method:     "kernel",
			Kernel:     "/casper/vmlinuz",
			Initrd:     []string{"/casper/initrd"},
			KernelArgs: []string{"boot=casper", "iso-scan/filename={iso}"},
		},
	}
	script := buildPreviewScript(kernelISO, "aa:bb:cc:dd:ee:01", s, "192.168.1.10", 16000)
	checks := []string{
		"#!ipxe",
		"kernel http://192.168.1.10:16000/iso/abc123/casper/vmlinuz boot=casper",
		"initrd http://192.168.1.10:16000/iso/abc123/casper/initrd",
		"boot || goto failed",
		"sleep 15",
	}
	for _, want := range checks {
		if !strings.Contains(script, want) {
			t.Errorf("kernel 脚本缺少 %q:\n%s", want, script)
		}
	}
	if strings.Contains(script, "{iso}") {
		t.Errorf("占位符未被替换:\n%s", script)
	}

	autoISO := kernelISO
	autoISO.Boot.AutoInstall = &model.AutoInstall{
		Kind:       "kickstart",
		Template:   "rhel9.ks",
		KernelArgs: []string{"inst.ks={server}/user/autoinstall/rhel9.ks"},
	}
	script = buildPreviewScript(autoISO, "aa:bb:cc:dd:ee:01", s, "192.168.1.10", 16000)
	if !strings.Contains(script, "inst.ks=http://192.168.1.10:16000/user/autoinstall/rhel9.ks") {
		t.Errorf("自动安装参数未展开:\n%s", script)
	}
	if !strings.Contains(script, "netbootx_autoinstall=") {
		t.Errorf("缺少自动安装模板参数:\n%s", script)
	}

	for _, method := range []string{"sanboot", "memdisk"} {
		is := kernelISO
		is.Boot.Method = method
		out := buildPreviewScript(is, "aa:bb:cc:dd:ee:01", s, "", 0)
		if !strings.Contains(out, "#!ipxe") || !strings.Contains(out, "boot || goto failed") {
			t.Errorf("%s 脚本结构不完整:\n%s", method, out)
		}
		if !strings.Contains(out, "http://192.168.1.10:16000/iso/abc123") {
			t.Errorf("%s 脚本的 URL 回落不正确:\n%s", method, out)
		}
	}

	bad := kernelISO
	bad.Bootable = false
	bad.Error = "没有找到 El Torito 引导记录"
	out := buildPreviewScript(bad, "aa:bb:cc:dd:ee:01", s, "192.168.1.10", 16000)
	if !strings.Contains(out, "可引导     : 否") || !strings.Contains(out, "El Torito") {
		t.Errorf("不可引导提示缺失:\n%s", out)
	}

	out = buildPreviewScript(model.ISO{ID: "x"}, "", model.Settings{}, "", 0)
	if !strings.Contains(out, "#!ipxe") {
		t.Errorf("空 ISO 生成失败:\n%s", out)
	}
}

func TestURLPathEscape(t *testing.T) {
	cases := map[string]string{
		"/casper/vmlinuz":     "/casper/vmlinuz",
		"casper/vmlinuz":      "/casper/vmlinuz",
		"/boot/my kernel.img": "/boot/my%20kernel.img",
		"/中文/内核":              "/%E4%B8%AD%E6%96%87/%E5%86%85%E6%A0%B8",
		"/a?b#c":              "/a%3Fb%23c",
		"":                    "",
	}
	for in, want := range cases {
		if got := urlPathEscape(in); got != want {
			t.Errorf("urlPathEscape(%q) = %q，期望 %q", in, got, want)
		}
	}
}

func TestSanitizeEcho(t *testing.T) {
	if got := sanitizeEcho("", ""); got != "镜像" {
		t.Errorf("空输入应返回默认值，得到 %q", got)
	}
	if got := sanitizeEcho("a\nb\tc"); strings.ContainsAny(got, "\n\t") {
		t.Errorf("换行/制表符未清理: %q", got)
	}
	if got := sanitizeEcho("", "第二个"); got != "第二个" {
		t.Errorf("应跳过空值，得到 %q", got)
	}
}

func TestFamilyAndMethodText(t *testing.T) {
	if familyText(model.FamilyWindows) != "Windows 安装盘" {
		t.Error("Windows 家族文案不正确")
	}
	if familyText(model.ISOFamily("nope")) != "未知类型" {
		t.Error("未知家族文案不正确")
	}
	if methodText("kernel") != "kernel + initrd" || methodText("") != "未识别" || methodText("weird") != "weird" {
		t.Error("引导方式文案不正确")
	}
}

func TestSameEvent(t *testing.T) {
	now := time.Now()
	a := model.Event{Time: now, Level: "info", Kind: "dhcp", MAC: "aa", Msg: "x"}
	if !sameEvent(a, a) {
		t.Error("相同事件判定失败")
	}
	b := a
	b.Msg = "y"
	if sameEvent(a, b) {
		t.Error("不同事件被判为相同")
	}
}

func TestNewEventsCursor(t *testing.T) {
	srv, mock := newTestServer(t)
	srv.initSSEState()

	if got := srv.newEvents(mock.Events(10)); len(got) != 0 {
		t.Errorf("不应有新事件，得到 %d 条", len(got))
	}

	base := time.Now()
	mock.AddEvent(model.Event{Time: base, Level: "info", Kind: "dhcp", Msg: "第一条新事件"})
	mock.AddEvent(model.Event{Time: base.Add(time.Millisecond), Level: "info", Kind: "dhcp", Msg: "第二条新事件"})
	got := srv.newEvents(mock.Events(10))
	if len(got) != 2 {
		t.Fatalf("新事件数量 = %d，期望 2", len(got))
	}
	if got[0].Msg != "第一条新事件" || got[1].Msg != "第二条新事件" {
		t.Errorf("顺序不正确: %+v", got)
	}
	if again := srv.newEvents(mock.Events(10)); len(again) != 0 {
		t.Errorf("游标未推进，又返回了 %d 条", len(again))
	}
}

func TestMockContract(t *testing.T) {
	m := NewMock()
	if m.Status().Running {
		t.Error("初始应为未运行")
	}
	if err := m.Start(); err != nil {
		t.Fatalf("Start() 失败: %v", err)
	}
	if !m.Status().Running {
		t.Error("启动后应为运行中")
	}
	if m.Status().UptimeSec < 0 {
		t.Error("运行时长不应为负")
	}
	if err := m.Start(); err == nil {
		t.Error("重复启动应报错")
	}
	if err := m.Stop(); err != nil {
		t.Fatalf("Stop() 失败: %v", err)
	}
	if m.Status().Running {
		t.Error("停止后应为未运行")
	}
	if err := m.SetDefaultISO("iso-win11"); err != nil {
		t.Fatalf("SetDefaultISO() 失败: %v", err)
	}
	if m.DefaultISO() != "iso-win11" {
		t.Errorf("DefaultISO() = %q", m.DefaultISO())
	}
	if got := m.Events(1); len(got) != 1 {
		t.Errorf("Events(1) 返回 %d 条", len(got))
	}
	if got := m.Events(0); len(got) != len(m.events) {
		t.Errorf("Events(0) 应返回全部，得到 %d 条", len(got))
	}
}

func TestMockStoreRecordsEvents(t *testing.T) {
	store := &MockStore{}
	mock := NewFullMock()
	srv, err := New(Config{PXE: mock, Store: store})
	if err != nil {
		t.Fatalf("New() 失败: %v", err)
	}
	t.Cleanup(func() { _ = srv.Stop() })

	do(t, srv.Handler(), http.MethodPost, "/api/isos/rescan", nil, nil)
	if store.Count() == 0 {
		t.Fatal("操作没有写入事件")
	}
	found := false
	for _, ev := range store.Events() {
		if strings.Contains(ev.Msg, "扫描完成") {
			found = true
			if ev.Kind != "system" {
				t.Errorf("事件 kind = %q，期望 system", ev.Kind)
			}
		}
	}
	if !found {
		t.Errorf("没有找到扫描事件: %+v", store.Events())
	}
}

// 并发压一下各个读接口，确认没有数据竞争（配合 -race 使用）。
func TestConcurrentReads(t *testing.T) {
	srv, _ := newTestServer(t)
	h := srv.Handler()
	paths := []string{"/api/status", "/api/isos", "/api/clients", "/api/events", "/api/settings", "/api/interfaces", "/api/version", "/"}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				for _, p := range paths {
					rec := httptest.NewRecorder()
					h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil))
					if rec.Code != http.StatusOK {
						t.Errorf("GET %s 状态码 = %d", p, rec.Code)
						return
					}
				}
			}
		}()
	}
	wg.Wait()
}

// ---------------------------------------------------------------------------
// 端到端：用真实 curl 打一遍所有端点
// ---------------------------------------------------------------------------

// TestCurlEveryEndpoint 启动真实监听端口，用 curl 逐个验证 API。
// 本机没有 curl 时自动跳过（CI 里一般都有）。
func TestCurlEveryEndpoint(t *testing.T) {
	if testing.Short() {
		t.Skip("短模式跳过 curl 端到端测试")
	}
	curlPath, err := exec.LookPath("curl")
	if err != nil {
		t.Skip("本机没有 curl，跳过端到端测试")
	}

	mock := NewFullMock()
	srv, err := New(Config{Addr: "127.0.0.1:0", PXE: mock, Store: &MockStore{}, Version: "e2e-test"})
	if err != nil {
		t.Fatalf("New() 失败: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("Start() 失败: %v", err)
	}
	t.Cleanup(func() { _ = srv.Stop() })

	dir := t.TempDir()
	cfg := mock.Settings()
	cfg.ISODir = dir
	_ = mock.SaveSettings(cfg)

	baseURL := "http://" + srv.Addr()

	// curlJSON 执行 curl 并返回 (HTTP 状态码, 响应体)。
	curlJSON := func(args ...string) (int, string) {
		t.Helper()
		full := append([]string{"-sS", "--max-time", "10", "-o", "-", "-w", "\n%{http_code}"}, args...)
		out, err := exec.Command(curlPath, full...).Output()
		if err != nil {
			t.Fatalf("curl %v 执行失败: %v", args, err)
		}
		text := string(out)
		idx := strings.LastIndex(text, "\n")
		if idx < 0 {
			t.Fatalf("curl 输出格式异常: %q", text)
		}
		code := 0
		fmt.Sscanf(strings.TrimSpace(text[idx+1:]), "%d", &code)
		return code, text[:idx]
	}

	// assertOK 断言返回的是成功包装。
	assertOK := func(name string, code int, body string) map[string]json.RawMessage {
		t.Helper()
		if code != http.StatusOK {
			t.Errorf("%s HTTP 状态码 = %d，响应: %s", name, code, body)
			return nil
		}
		var resp struct {
			OK    bool
			Error string
		}
		if err := json.Unmarshal([]byte(body), &resp); err != nil {
			t.Errorf("%s 返回的不是 JSON: %v，内容: %s", name, err, body)
			return nil
		}
		if !resp.OK {
			t.Errorf("%s ok=false: %s", name, resp.Error)
			return nil
		}
		var env struct {
			Data map[string]json.RawMessage
		}
		_ = json.Unmarshal([]byte(body), &env)
		return env.Data
	}

	// GET 类
	gets := []string{
		"/api/status", "/api/isos", "/api/clients", "/api/events?limit=10",
		"/api/settings", "/api/interfaces", "/api/version", "/api/preview/iso-ubuntu?mac=aa:bb:cc:dd:ee:01",
		"/healthz",
	}
	for _, path := range gets {
		code, body := curlJSON(baseURL + path)
		if path == "/healthz" {
			if code != http.StatusOK || !strings.Contains(body, "\"ok\"") && !strings.Contains(body, "\"status\"") {
				t.Errorf("GET %s 失败: %d %s", path, code, body)
			}
			continue
		}
		assertOK("GET "+path, code, body)
	}

	// 首页与静态资源
	for _, path := range []string{"/", "/static/app.js", "/static/style.css"} {
		code, body := curlJSON(baseURL + path)
		if code != http.StatusOK || len(body) == 0 {
			t.Errorf("GET %s 失败: %d，长度 %d", path, code, len(body))
		}
	}

	// 控制类 POST
	for _, path := range []string{"/api/start", "/api/restart", "/api/isos/rescan", "/api/stop"} {
		code, body := curlJSON("-X", "POST", baseURL+path)
		assertOK("POST "+path, code, body)
	}

	// 配置：先读后写
	code, body := curlJSON("-X", "POST", "-H", "Content-Type: application/json",
		"-d", "{\"domain\":\"e2e.lan\"}", baseURL+"/api/settings")
	if data := assertOK("POST /api/settings", code, body); data != nil {
		if !strings.Contains(string(data["settings"]), "e2e.lan") {
			t.Errorf("配置未生效: %s", data["settings"])
		}
	}

	// 上传：用 curl -F 传一个假 ISO
	isoFile := filepath.Join(t.TempDir(), "curl-upload.iso")
	if err := os.WriteFile(isoFile, []byte("CURL-ISO-PAYLOAD"), 0o644); err != nil {
		t.Fatalf("准备上传文件失败: %v", err)
	}
	code, body = curlJSON("-X", "POST", "-F", "files=@"+isoFile, baseURL+"/api/isos/upload")
	assertOK("POST /api/isos/upload", code, body)
	if _, err := os.Stat(filepath.Join(dir, "curl-upload.iso")); err != nil {
		t.Errorf("curl 上传的文件没有落盘: %v", err)
	}

	// 错误路径：404 / 405 / 400
	cases := []struct {
		name string
		args []string
		want int
	}{
		{"未知接口", []string{baseURL + "/api/nope"}, http.StatusNotFound},
		{"错误方法", []string{"-X", "GET", baseURL + "/api/start"}, http.StatusMethodNotAllowed},
		{"非法 limit", []string{baseURL + "/api/events?limit=abc"}, http.StatusBadRequest},
		{"未知 ISO", []string{baseURL + "/api/preview/nope"}, http.StatusNotFound},
	}
	for _, c := range cases {
		code, body := curlJSON(c.args...)
		if code != c.want {
			t.Errorf("%s 状态码 = %d，期望 %d，响应: %s", c.name, code, c.want, body)
		}
		if !strings.Contains(body, "\"ok\":false") {
			t.Errorf("%s 响应缺少 ok:false: %s", c.name, body)
		}
	}

	// DELETE 客户机
	code, body = curlJSON("-X", "DELETE", baseURL+"/api/clients")
	assertOK("DELETE /api/clients", code, body)

	// SSE：curl 读 2 秒，必须看到 event: snapshot
	out, _ := exec.Command(curlPath, "-sS", "--max-time", "2", "-N", baseURL+"/api/events/stream").Output()
	if !strings.Contains(string(out), "event: snapshot") {
		t.Errorf("SSE 没有推送快照，收到: %s", string(out))
	}
	if !strings.Contains(string(out), "text/event-stream") && !strings.Contains(string(out), "retry: 3000") {
		t.Errorf("SSE 响应格式不正确: %s", string(out))
	}
}

// ---------------------------------------------------------------------------
// 前端静态一致性：app.js 里用到的 DOM id 必须都存在于 index.html
// ---------------------------------------------------------------------------

func TestDOMIDsExist(t *testing.T) {
	htmlBytes, err := assetsFS.ReadFile("assets/index.html")
	if err != nil {
		t.Fatalf("读取 index.html 失败: %v", err)
	}
	jsBytes, err := assetsFS.ReadFile("assets/app.js")
	if err != nil {
		t.Fatalf("读取 app.js 失败: %v", err)
	}
	htmlText := string(htmlBytes)
	jsText := string(jsBytes)

	// index.html 里定义的所有 id
	defined := map[string]bool{}
	for _, m := range regexp.MustCompile("id=\"([A-Za-z0-9_-]+)\"").FindAllStringSubmatch(htmlText, -1) {
		defined[m[1]] = true
	}
	if len(defined) < 15 {
		t.Fatalf("index.html 只定义了 %d 个 id，页面结构可能不完整", len(defined))
	}

	// app.js 里 $('xxx') / getElementById('xxx') 引用到的 id
	refs := map[string]bool{}
	for _, re := range []*regexp.Regexp{
		regexp.MustCompile("\\$\\('([A-Za-z0-9_-]+)'\\)"),
		regexp.MustCompile("getElementById\\('([A-Za-z0-9_-]+)'\\)"),
	} {
		for _, m := range re.FindAllStringSubmatch(jsText, -1) {
			refs[m[1]] = true
		}
	}
	if len(refs) < 15 {
		t.Fatalf("app.js 只引用了 %d 个 id，解析可能失败", len(refs))
	}
	for id := range refs {
		if !defined[id] {
			t.Errorf("app.js 引用了 index.html 中不存在的 id %q", id)
		}
	}

	// 前端用到的 SVG 图标 symbol 必须都在 index.html 里定义
	symbols := map[string]bool{}
	for _, m := range regexp.MustCompile("symbol id=\"([A-Za-z0-9_-]+)\"").FindAllStringSubmatch(htmlText, -1) {
		symbols[m[1]] = true
	}
	usedIcons := map[string]bool{}
	for _, re := range []*regexp.Regexp{
		regexp.MustCompile("use href=\"#([A-Za-z0-9_-]+)\""),
		regexp.MustCompile("icon\\('([A-Za-z0-9_-]+)'\\)"),
	} {
		for _, m := range re.FindAllStringSubmatch(htmlText+"\n"+jsText, -1) {
			usedIcons[m[1]] = true
		}
	}
	for id := range usedIcons {
		if !symbols[id] {
			t.Errorf("引用了未定义的图标 symbol #%s", id)
		}
	}

	// 关键交互元素必须存在
	for _, id := range []string{"powerBtn", "isoList", "clientList", "logList", "ifaceSelect", "modal", "toasts"} {
		if !defined[id] {
			t.Errorf("index.html 缺少关键元素 #%s", id)
		}
	}
}

// TestHTMLAttributesWellFormed 粗检 HTML 属性引号是否配对，
// 防止 data URI / 内联样式里混入裸引号把页面结构弄坏。
func TestHTMLAttributesWellFormed(t *testing.T) {
	htmlBytes, err := assetsFS.ReadFile("assets/index.html")
	if err != nil {
		t.Fatalf("读取 index.html 失败: %v", err)
	}
	page := string(htmlBytes)

	for i, line := range strings.Split(page, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || !strings.HasPrefix(trimmed, "<") {
			continue
		}
		if strings.Count(line, "\"")%2 != 0 {
			t.Errorf("第 %d 行引号数量为奇数（属性未闭合）: %s", i+1, trimmed)
		}
	}

	// 不允许出现内联事件处理器（CSP 友好 + 便于维护）
	if regexp.MustCompile("\\son[a-z]+=\"").MatchString(page) {
		t.Error("index.html 里出现了内联事件处理器（onclick 等），请用 addEventListener")
	}

	// favicon 必须是自包含的 data URI，不能是外部地址
	iconRe := regexp.MustCompile("rel=\"icon\"\\s+href=\"([^\"]*)\"")
	m := iconRe.FindStringSubmatch(page)
	if m == nil {
		t.Error("index.html 缺少 favicon")
	} else if !strings.HasPrefix(m[1], "data:image/svg+xml,") {
		t.Errorf("favicon 应为内联 data URI，实际: %.60s", m[1])
	}
}

// ---------------------------------------------------------------------------
// 前端行为测试：用 node 真实执行 assets/app.js
// ---------------------------------------------------------------------------

// TestFrontendDOMBehavior 用 testdata/dom_test.mjs（零依赖的最小 DOM 实现）
// 真实执行 app.js，断言日志/客户机/ISO 面板的渲染结果。
//
// 这条测试专门用来抓「渲染函数顺手把数据清空」这类只有跑起来才暴露的 bug：
// 曾经的 renderLogs(reset) 会执行 state.events = []，导致日志面板永远显示
// 「暂无日志」，而纯 API 测试完全测不出来。
func TestFrontendDOMBehavior(t *testing.T) {
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skip("本机没有 node，跳过前端 DOM 测试")
	}
	assetsDir, err := filepath.Abs("assets")
	if err != nil {
		t.Fatalf("解析 assets 目录失败: %v", err)
	}
	script, err := filepath.Abs(filepath.Join("testdata", "dom_test.mjs"))
	if err != nil {
		t.Fatalf("解析测试脚本失败: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, nodePath, script, assetsDir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("前端 DOM 测试失败: %v\n%s", err, out)
	}
	text := string(out)
	if !strings.Contains(text, "前端 DOM 测试全部通过") {
		t.Fatalf("前端 DOM 测试没有报告成功:\n%s", text)
	}
	t.Logf("%s", text)
}

// TestAppJSSyntax 用 node --check 校验 app.js 语法。
func TestAppJSSyntax(t *testing.T) {
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skip("本机没有 node，跳过 JS 语法检查")
	}
	appPath, err := filepath.Abs(filepath.Join("assets", "app.js"))
	if err != nil {
		t.Fatalf("解析 app.js 路径失败: %v", err)
	}
	out, err := exec.Command(nodePath, "--check", appPath).CombinedOutput()
	if err != nil {
		t.Fatalf("app.js 语法检查失败: %v\n%s", err, out)
	}
}
