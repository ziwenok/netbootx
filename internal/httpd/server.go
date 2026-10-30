// Package httpd 实现 NetBootX 的 HTTP 文件服务。
//
// 它承担四类请求：
//
//	/boot/menu.ipxe?mac=X[&iso=Y]  —— 按客户机动态生成 iPXE 脚本
//	/boot/{file}                   —— 下发引导文件（ipxe.efi / undionly.kpxe / wimboot …）
//	/iso/{id}/{path...}            —— 从 ISO 内部按字节偏移流式读取（必须支持 Range）
//	/user/{path...}                —— 自动安装模板等用户文件
//	/healthz                       —— 健康检查
//
// 设计要点：
//   - ISO 从不挂载、从不解压、绝不整读进内存。/iso 路由用 ISOReader 拿到
//     io.ReaderAt + 大小，包成 io.ReadSeeker 交给 http.ServeContent，
//     由标准库负责 Range / If-Range / ETag / Last-Modified 全套语义。
//   - ISO 的目录树只解析一次（打开一次 *iso.Reader），按文件 mtime + size 失效。
//   - 所有文件路径都做路径穿越防护。
//   - 访问日志采样后写进 OnEvent，避免刷爆界面。
package httpd

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/wupengcheng/netbootx/internal/iso"
	"github.com/wupengcheng/netbootx/internal/model"
)

// ISOReader 是 httpd 对 ISO 读取器的最小依赖。
//
// 定义成接口而不是直接依赖 *iso.Reader，一是为了单测可以塞假实现，
// 二是避免把 iso 包的具体类型泄漏到 HTTP 层。
// 生产环境由 *iso.Reader 经 isoReaderAdapter 适配（见下方编译期断言）。
type ISOReader interface {
	// SectionReader 返回 ISO 内文件 p 的随机读取句柄与字节数。
	// p 为 ISO 内绝对路径（"/casper/vmlinuz"），也接受相对形式。
	SectionReader(p string) (io.ReaderAt, int64, error)
	// Exists 报告 ISO 内路径是否存在。
	Exists(p string) bool
	// Close 释放底层资源。
	Close() error
}

// isoReaderAdapter 把 *iso.Reader 适配成 ISOReader。
//
// 用适配器而不是让 *iso.Reader 直接满足接口，是为了把「iso 包可能演化」
// 的影响收敛在一个文件里。
type isoReaderAdapter struct{ r *iso.Reader }

func (a isoReaderAdapter) SectionReader(p string) (io.ReaderAt, int64, error) {
	return a.r.SectionReader(p)
}
func (a isoReaderAdapter) Exists(p string) bool { return a.r.Exists(p) }
func (a isoReaderAdapter) Close() error         { return a.r.Close() }

// 编译期断言：*iso.Reader 必须能满足 ISOReader。
// 下面两条任何一条编译失败，都说明 iso 包的接口变了，需要同步适配。
var _ ISOReader = isoReaderAdapter{}
var _ ISOReader = (*iso.Reader)(nil)

// Config 是 HTTP 服务的配置。
type Config struct {
	Addr      string                                  // 监听地址，如 ":16000"
	ISODir    string                                  // ISO 根目录
	UserDir   string                                  // 用户文件目录（自动安装模板等）
	BootDir   string                                  // 引导文件目录（ipxe.efi / wimboot …），空则取 ISODir 的兄弟目录 boot
	ScriptFor func(mac, isoID string) (string, error) // 注入：动态生成 iPXE 脚本
	ISOs      func() []model.ISO                      // 注入：当前 ISO 列表
	OnEvent   func(model.Event)                       // 注入：事件回调（可为 nil）
	OnClient  func(mac, ip, path string)              // 注入：客户机访问回调（可为 nil）
	// OpenISO 可注入自定义的 ISO 打开函数（单测用）；为 nil 时使用 iso.Open。
	OpenISO func(path string) (ISOReader, error)
	// Logger 为空时不输出任何日志（由 OnEvent 承担可观测性）。
	Logger *log.Logger
	// EventSample 是访问日志的采样率：每 N 次请求记一条事件，<=1 表示全记。
	EventSample uint64
}

// Server 是 HTTP 文件服务器。
type Server struct {
	cfg     Config
	logger  *log.Logger
	httpSrv *http.Server

	mu      sync.Mutex
	ln      net.Listener
	running bool

	// ISO 读取器缓存：按 ISO ID 缓存，按文件 mtime/size 失效。
	cacheMu sync.Mutex
	cache   map[string]*cacheEntry

	// 事件采样计数。
	reqCount atomic.Uint64
	// 正在处理的请求数（用于 Stop 时判断是否还有活跃请求）。
	inflight sync.WaitGroup

	// 客户机回调去重，避免 iPXE 重试刷爆客户端表。
	clientMu   sync.Mutex
	lastClient map[string]time.Time
}

// cacheEntry 是一个已打开的 ISO 读取器及其失效信息。
type cacheEntry struct {
	reader  ISOReader
	path    string
	modTime time.Time
	size    int64
	// refs 是正在使用该 reader 的请求数；归零且被标记淘汰时才真正 Close。
	refs int
	dead bool
}

// New 创建 HTTP 服务。不会绑定端口，Start 才绑定。
func New(cfg Config) (*Server, error) {
	if strings.TrimSpace(cfg.Addr) == "" {
		cfg.Addr = ":16000"
	}
	if cfg.ScriptFor == nil {
		cfg.ScriptFor = func(mac, isoID string) (string, error) {
			return "", fmt.Errorf("httpd: 未配置 ScriptFor，无法生成启动脚本")
		}
	}
	if cfg.ISOs == nil {
		cfg.ISOs = func() []model.ISO { return nil }
	}
	if cfg.OpenISO == nil {
		cfg.OpenISO = func(p string) (ISOReader, error) {
			r, err := iso.Open(p)
			if err != nil {
				return nil, err
			}
			return isoReaderAdapter{r: r}, nil
		}
	}
	logger := cfg.Logger
	if logger == nil {
		logger = log.New(io.Discard, "", 0)
	}
	if cfg.EventSample == 0 {
		cfg.EventSample = 20
	}
	return &Server{
		cfg:        cfg,
		logger:     logger,
		cache:      make(map[string]*cacheEntry),
		lastClient: make(map[string]time.Time),
	}, nil
}

// Handler 返回 http.Handler，方便单测直接挂到 httptest 上。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealth)
	mux.HandleFunc("/boot/menu.ipxe", s.handleMenu)
	mux.HandleFunc("/boot/", s.handleBootFile)
	mux.HandleFunc("/iso/", s.handleISO)
	mux.HandleFunc("/user/", s.handleUser)
	mux.HandleFunc("/", s.handleIndex)
	return s.withAccessLog(mux)
}

// Start 绑定端口并在后台接受请求（非阻塞）。
func (s *Server) Start() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running {
		return fmt.Errorf("httpd: 服务已经在运行中")
	}
	ln, err := net.Listen("tcp", s.cfg.Addr)
	if err != nil {
		return fmt.Errorf("httpd: 监听 %s 失败（端口可能被占用）: %w", s.cfg.Addr, err)
	}
	s.ln = ln
	s.httpSrv = &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		// 大文件传输可能持续很久，不设 WriteTimeout；用 IdleTimeout 回收空连接。
		IdleTimeout: 120 * time.Second,
	}
	s.running = true
	go func() {
		if err := s.httpSrv.Serve(ln); err != nil && err != http.ErrServerClosed {
			s.logger.Printf("httpd: 服务退出: %v", err)
			s.emit(model.Event{Level: "error", Kind: "http", Msg: "HTTP 服务异常退出: " + err.Error()})
		}
	}()
	return nil
}

// Addr 返回实际监听的地址（Start 之后才有意义，便于测试取随机端口）。
func (s *Server) Addr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ln == nil {
		return s.cfg.Addr
	}
	return s.ln.Addr().String()
}

// Stop 关闭服务并释放所有缓存的 ISO 读取器。幂等，且不阻塞超过 2 秒。
func (s *Server) Stop() error {
	s.mu.Lock()
	if !s.running {
		s.mu.Unlock()
		// 即使没 Start 过，也要释放可能已经缓存的 ISO 读取器。
		s.closeAllReaders()
		return nil
	}
	s.running = false
	srv := s.httpSrv
	s.httpSrv = nil
	s.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var err error
	if srv != nil {
		if e := srv.Shutdown(ctx); e != nil {
			// 超时说明还有连接没结束，强制关闭，但整体不超过契约规定的 2 秒。
			_ = srv.Close()
			err = fmt.Errorf("httpd: 关闭服务: %w", e)
		}
	}
	s.closeAllReaders()
	return err
}

// closeAllReaders 关闭并清空 ISO 读取器缓存。
func (s *Server) closeAllReaders() {
	s.cacheMu.Lock()
	entries := make([]*cacheEntry, 0, len(s.cache))
	for id, e := range s.cache {
		delete(s.cache, id)
		e.dead = true
		if e.refs <= 0 {
			entries = append(entries, e)
		}
	}
	s.cacheMu.Unlock()
	for _, e := range entries {
		_ = e.reader.Close()
	}
}

// emit 安全地发送一条事件。
func (s *Server) emit(ev model.Event) {
	if s.cfg.OnEvent == nil {
		return
	}
	if ev.Time.IsZero() {
		ev.Time = time.Now()
	}
	if ev.Kind == "" {
		ev.Kind = "http"
	}
	s.cfg.OnEvent(ev)
}

// ---- 路由处理 ----

// handleHealth 处理 GET /healthz。
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, "{\"ok\":true,\"service\":\"netbootx-httpd\"}\n")
}

// handleIndex 处理根路径，给出可读的服务信息（而不是干巴巴的 404）。
func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = io.WriteString(w, "NetBootX HTTP 文件服务\n\n"+
		"GET /boot/menu.ipxe?mac=<MAC>[&iso=<ID>]  iPXE 启动脚本\n"+
		"GET /boot/<file>                          引导文件（ipxe.efi 等）\n"+
		"GET /iso/<id>/<path>                      ISO 内部文件（支持 Range）\n"+
		"GET /user/<path>                          用户文件\n"+
		"GET /healthz                              健康检查\n")
}

// handleMenu 处理 GET /boot/menu.ipxe。
//
// 查询参数：
//
//	mac  客户机 MAC（用于按架构过滤与记录状态）
//	iso  可选，指定 ISO ID 时直接下发「直达启动」脚本而不是菜单
func (s *Server) handleMenu(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "只支持 GET", http.StatusMethodNotAllowed)
		return
	}
	q := r.URL.Query()
	mac := model.NormalizeMAC(q.Get("mac"))
	isoID := strings.TrimSpace(q.Get("iso"))
	// 部分固件/代理会把 MAC 放在自定义头里。
	if mac == "" {
		mac = model.NormalizeMAC(r.Header.Get("X-NetBootX-MAC"))
	}

	script, err := s.cfg.ScriptFor(mac, isoID)
	if err != nil {
		s.emit(model.Event{Level: "warn", Kind: "boot", MAC: mac,
			Msg: "生成 iPXE 脚本失败: " + err.Error()})
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusInternalServerError)
		// 回一个「能看懂」的 iPXE 脚本，客户机屏幕上能看到原因。
		_, _ = io.WriteString(w, "#!ipxe\n"+
			"echo NetBootX: 生成启动脚本失败\n"+
			"echo "+sanitizeIPXE(err.Error())+"\n"+
			"sleep 10\nshell\n")
		return
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Accept-Ranges", "bytes")
	_, _ = io.WriteString(w, script)
}

// handleBootFile 处理 GET /boot/{file}，下发引导文件。
//
// 支持子目录（如 boot/x86_64/ipxe.efi），但必须落在 BootDir 之内。
func (s *Server) handleBootFile(w http.ResponseWriter, r *http.Request) {
	rel := strings.TrimSpace(strings.TrimPrefix(r.URL.Path, "/boot/"))
	if rel == "" {
		http.NotFound(w, r)
		return
	}
	root := s.bootDir()
	if root == "" {
		http.Error(w, "引导文件目录未配置", http.StatusInternalServerError)
		return
	}
	full, ok := safeJoin(root, rel)
	if !ok {
		s.emit(model.Event{Level: "warn", Kind: "http", Msg: "拒绝可疑的引导文件路径: " + rel})
		http.Error(w, "非法路径", http.StatusBadRequest)
		return
	}
	s.serveFile(w, r, full, bootContentType(rel))
}

// handleUser 处理 GET /user/{path}，下发自动安装模板等用户文件。
func (s *Server) handleUser(w http.ResponseWriter, r *http.Request) {
	rel := strings.TrimSpace(strings.TrimPrefix(r.URL.Path, "/user/"))
	if rel == "" {
		http.NotFound(w, r)
		return
	}
	full, ok := safeJoin(s.cfg.UserDir, rel)
	if !ok {
		s.emit(model.Event{Level: "warn", Kind: "http", Msg: "拒绝可疑的用户文件路径: " + rel})
		http.Error(w, "非法路径", http.StatusBadRequest)
		return
	}
	s.serveFile(w, r, full, "")
}

// serveFile 用 http.ServeContent 发送磁盘文件，自带 Range / ETag / Last-Modified。
func (s *Server) serveFile(w http.ResponseWriter, r *http.Request, full, ctype string) {
	f, err := os.Open(full)
	if err != nil {
		if os.IsNotExist(err) {
			http.NotFound(w, r)
			return
		}
		s.emit(model.Event{Level: "error", Kind: "http", Msg: "打开文件失败: " + err.Error()})
		http.Error(w, "读取文件失败", http.StatusInternalServerError)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		http.Error(w, "读取文件状态失败", http.StatusInternalServerError)
		return
	}
	if st.IsDir() {
		http.NotFound(w, r)
		return
	}
	if ctype != "" {
		w.Header().Set("Content-Type", ctype)
	}
	w.Header().Set("Accept-Ranges", "bytes")
	http.ServeContent(w, r, st.Name(), st.ModTime(), f)
}

// handleISO 处理 GET /iso/{id}/{path...}。
//
// 这是性能关键路径：从 ISO 内部按偏移读，绝不整读进内存。
func (s *Server) handleISO(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "只支持 GET", http.StatusMethodNotAllowed)
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/iso/")
	id, inner := splitISOID(rest)
	if id == "" || inner == "" {
		http.Error(w, "请求格式应为 /iso/{id}/{ISO 内路径}", http.StatusBadRequest)
		return
	}
	is, ok := s.findISO(id)
	if !ok {
		http.Error(w, "ISO 不存在: "+id, http.StatusNotFound)
		return
	}
	if !isoPathInside(inner) {
		s.emit(model.Event{Level: "warn", Kind: "http", Msg: "拒绝可疑的 ISO 内路径: " + inner})
		http.Error(w, "非法路径", http.StatusBadRequest)
		return
	}

	entry, err := s.acquire(is)
	if err != nil {
		s.emit(model.Event{Level: "error", Kind: "http", Msg: "打开 ISO 失败: " + err.Error()})
		http.Error(w, "打开 ISO 失败", http.StatusInternalServerError)
		return
	}
	defer s.releaseEntry(entry)

	ra, size, err := entry.reader.SectionReader(inner)
	if err != nil {
		// ISO 内不存在该文件：404，而不是 500。
		http.NotFound(w, r)
		return
	}
	// io.NewSectionReader 返回的 *io.SectionReader 同时满足 io.ReaderAt / io.Seeker，
	// 正好是 http.ServeContent 需要的 io.ReadSeeker。
	sr := io.NewSectionReader(ra, 0, size)
	w.Header().Set("Accept-Ranges", "bytes")
	// ETag 由「文件大小 + ISO mtime」构成：ISO 被替换后自动变化。
	// http.ServeContent 会用它与 If-None-Match / If-Range 做协商。
	w.Header().Set("ETag", isoETag(size, is.ModTime))
	if ct := contentTypeByExt(inner); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	// modTime 用 ISO 文件的 mtime，客户机就能用 If-Modified-Since 走 304。
	http.ServeContent(w, r, path.Base(inner), is.ModTime, sr)
}

// ---- ISO 读取器缓存 ----

// acquire 取得某个 ISO 的读取器，必要时打开并缓存。
//
// 缓存按 (路径, mtime, size) 失效：ISO 被替换后会重新解析。
func (s *Server) acquire(is model.ISO) (*cacheEntry, error) {
	key := is.ID
	st, err := os.Stat(is.Path)
	if err != nil {
		return nil, fmt.Errorf("stat ISO %q: %w", is.Path, err)
	}

	s.cacheMu.Lock()
	if e, ok := s.cache[key]; ok && e.path == is.Path && e.modTime.Equal(st.ModTime()) &&
		e.size == st.Size() && !e.dead {
		e.refs++
		s.cacheMu.Unlock()
		return e, nil
	}
	// 旧条目标记淘汰：还有请求在用就等它释放，否则立即关闭。
	var stale *cacheEntry
	if old, ok := s.cache[key]; ok {
		delete(s.cache, key)
		old.dead = true
		if old.refs <= 0 {
			stale = old
		}
	}
	s.cacheMu.Unlock()
	if stale != nil {
		_ = stale.reader.Close()
	}

	reader, err := s.cfg.OpenISO(is.Path)
	if err != nil {
		return nil, fmt.Errorf("打开 ISO %q: %w", is.Path, err)
	}
	e := &cacheEntry{reader: reader, path: is.Path, modTime: st.ModTime(), size: st.Size(), refs: 1}

	s.cacheMu.Lock()
	if cur, ok := s.cache[key]; ok {
		// 并发打开：别人先放进去了，用别人的，关掉自己这份。
		cur.refs++
		s.cacheMu.Unlock()
		_ = reader.Close()
		return cur, nil
	}
	s.cache[key] = e
	s.cacheMu.Unlock()
	return e, nil
}

// releaseEntry 释放一次对缓存条目的引用。
func (s *Server) releaseEntry(e *cacheEntry) {
	if e == nil {
		return
	}
	s.cacheMu.Lock()
	e.refs--
	closeNow := e.refs <= 0 && e.dead
	s.cacheMu.Unlock()
	if closeNow {
		_ = e.reader.Close()
	}
}

// findISO 在注入的 ISO 列表里按 ID 查找。
func (s *Server) findISO(id string) (model.ISO, bool) {
	for _, is := range s.cfg.ISOs() {
		if is.ID == id {
			return is, true
		}
	}
	return model.ISO{}, false
}

// isoPathInside 做 ISO 内路径的穿越防护。
//
// ISO9660 路径用 / 分隔；拒绝空段、"."、".."、反斜杠和 NUL。
// （http.Request.URL.Path 已经做过一次百分号解码。）
func isoPathInside(p string) bool {
	p = strings.ReplaceAll(p, "\\", "/")
	if strings.ContainsRune(p, 0) {
		return false
	}
	// 调用方传进来的是 "/casper/vmlinuz" 形式，去掉前导斜杠再逐段检查。
	p = strings.TrimPrefix(p, "/")
	if p == "" {
		return false
	}
	for _, seg := range strings.Split(p, "/") {
		switch seg {
		case "", ".", "..":
			return false
		}
	}
	return true
}

// splitISOID 把 "abc123/casper/vmlinuz" 拆成 ("abc123", "/casper/vmlinuz")。
func splitISOID(rest string) (id, inner string) {
	rest = strings.TrimPrefix(rest, "/")
	if rest == "" {
		return "", ""
	}
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		return rest[:i], rest[i:]
	}
	return rest, "/"
}

// bootDir 返回引导文件目录。
//
// 优先用 Config.BootDir；没配时退回 ISODir 的兄弟目录 boot（与项目结构一致）。
func (s *Server) bootDir() string {
	if d := strings.TrimSpace(s.cfg.BootDir); d != "" {
		return d
	}
	isoDir := strings.TrimSpace(s.cfg.ISODir)
	if isoDir == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(filepath.Clean(isoDir)), "boot")
}

// safeJoin 把相对路径拼到根目录下，并确保结果仍在根目录内。
// 返回 false 表示路径非法（穿越、绝对路径、空路径）。
func safeJoin(root, rel string) (string, bool) {
	root = strings.TrimSpace(root)
	if root == "" {
		return "", false
	}
	rel = strings.ReplaceAll(rel, "\\", "/")
	rel = strings.TrimPrefix(rel, "/")
	if rel == "" || strings.ContainsRune(rel, 0) {
		return "", false
	}
	// 逐段检查，拒绝 ".." 与空段。
	for _, seg := range strings.Split(rel, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return "", false
		}
	}
	cleanRoot := filepath.Clean(root)
	full := filepath.Clean(filepath.Join(cleanRoot, filepath.FromSlash(rel)))
	if full != cleanRoot && !strings.HasPrefix(full, cleanRoot+string(filepath.Separator)) {
		return "", false
	}
	return full, true
}

// bootContentType 给引导文件挑一个合适的 Content-Type。
//
// iPXE 二进制统一用 application/octet-stream 最稳妥（部分固件对 MIME 敏感）。
func bootContentType(name string) string {
	switch strings.ToLower(path.Ext(name)) {
	case ".ipxe", ".cfg", ".txt":
		return "text/plain; charset=utf-8"
	}
	return "application/octet-stream"
}

// isoETag 为 ISO 内文件生成强 ETag，格式与 nginx 类似：\"大小-修改时间\"。
func isoETag(size int64, mod time.Time) string {
	return fmt.Sprintf("\"%x-%x\"", size, mod.UnixNano())
}

// contentTypeByExt 按扩展名推断 ISO 内文件的 Content-Type。
// 返回空串表示交给 http.ServeContent 的自动嗅探。
func contentTypeByExt(name string) string {
	switch strings.ToLower(path.Ext(name)) {
	case ".cfg", ".txt", ".ks", ".xml", ".json", ".conf":
		return "text/plain; charset=utf-8"
	}
	return ""
}

// sanitizeIPXE 清理要写进 iPXE echo 的文本，避免破坏脚本结构。
func sanitizeIPXE(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '\n', '\r', '\t':
			b.WriteByte(' ')
		case '&', '|', '$', ';':
			// 这些字符会截断 iPXE 命令，直接丢掉。
		default:
			if r < 0x20 || r == 0x7f {
				continue
			}
			b.WriteRune(r)
		}
	}
	out := strings.TrimSpace(b.String())
	if out == "" {
		out = "未知错误"
	}
	return out
}

// ---- 访问日志 ----

// withAccessLog 包一层访问日志：采样写 OnEvent，并回调 OnClient。
func (s *Server) withAccessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		s.inflight.Add(1)
		defer s.inflight.Done()

		next.ServeHTTP(rec, r)

		// /healthz 探活请求太频繁，不记事件。
		if r.URL.Path == "/healthz" {
			return
		}
		n := s.reqCount.Add(1)
		sample := s.cfg.EventSample
		if sample <= 1 || n%sample == 0 || rec.status >= 400 {
			level := "info"
			if rec.status >= 400 {
				level = "warn"
			}
			s.emit(model.Event{
				Level: level,
				Kind:  "http",
				MAC:   model.NormalizeMAC(r.URL.Query().Get("mac")),
				Msg: fmt.Sprintf("HTTP %s %s → %d（%s，%s）",
					r.Method, r.URL.RequestURI(), rec.status,
					humanBytes(rec.written), time.Since(start).Round(time.Millisecond)),
			})
		}
		if s.cfg.OnClient != nil && r.URL.Path == "/boot/menu.ipxe" {
			mac := model.NormalizeMAC(r.URL.Query().Get("mac"))
			if mac != "" && s.shouldNotifyClient(mac) {
				s.cfg.OnClient(mac, clientIP(r), r.URL.Path)
			}
		}
	})
}

// shouldNotifyClient 对同一客户机的回调做 1 秒去重，避免 iPXE 重试刷爆客户端表。
func (s *Server) shouldNotifyClient(mac string) bool {
	now := time.Now()
	s.clientMu.Lock()
	defer s.clientMu.Unlock()
	if last, ok := s.lastClient[mac]; ok && now.Sub(last) < time.Second {
		return false
	}
	s.lastClient[mac] = now
	return true
}

// statusRecorder 记录响应状态码与字节数。
type statusRecorder struct {
	http.ResponseWriter
	status  int
	written int64
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(p []byte) (int, error) {
	n, err := r.ResponseWriter.Write(p)
	r.written += int64(n)
	return n, err
}

// Flush 让 Range 大文件传输能及时刷出（实现 http.Flusher）。
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// ReadFrom 转发给底层 ResponseWriter 的 io.ReaderFrom 实现。
//
// 这一条对性能很关键：不实现它，http.ServeContent 传输 12MB 内核时只能走
// 32KB 的缓冲拷贝；实现了才能命中 net/http 的 sendfile 快路径。
func (r *statusRecorder) ReadFrom(src io.Reader) (int64, error) {
	if rf, ok := r.ResponseWriter.(io.ReaderFrom); ok {
		n, err := rf.ReadFrom(src)
		r.written += n
		return n, err
	}
	// 只暴露 Write，避免 io.Copy 再次进入 ReadFrom 造成递归。
	return io.Copy(struct{ io.Writer }{r}, src)
}

// clientIP 从请求里取出客户机 IP。
func clientIP(r *http.Request) string {
	host := r.RemoteAddr
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	return host
}

// humanBytes 把字节数格式化成简短形式，仅用于日志。
func humanBytes(n int64) string {
	if n < 1024 {
		return strconv.FormatInt(n, 10) + "B"
	}
	units := [...]string{"KB", "MB", "GB", "TB"}
	f := float64(n) / 1024
	i := 0
	for f >= 1024 && i < len(units)-1 {
		f /= 1024
		i++
	}
	return strconv.FormatFloat(f, 'f', 1, 64) + units[i]
}
