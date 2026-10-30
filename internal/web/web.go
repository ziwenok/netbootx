// Package web 实现 NetBootX 的 Web 管理界面。
//
// 它由三部分组成：
//   - REST API（/api/*），统一返回 {"ok":bool,"data":...,"error":string} 包装；
//   - SSE 实时推送（/api/events/stream），周期性推送状态、客户机与日志增量；
//   - 单页前端（assets/），通过 //go:embed 编进二进制，零构建、零 CDN、离线可用。
//
// 数据来源通过 Controller 接口注入（由 internal/pxe 实现），
// 因此本包不直接依赖 pxe / store，可以独立编译和单元测试。
package web

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/wupengcheng/netbootx/internal/model"
)

//go:embed assets
var assetsFS embed.FS

const (
	// DefaultAddr 是 Web 管理界面的默认监听地址。
	DefaultAddr = ":16080"

	// defaultStreamInterval 是 SSE 推送间隔（秒级实时，2 秒足够且不打扰）。
	defaultStreamInterval = 2 * time.Second

	// shutdownTimeout 是 Stop 时等待存量连接收尾的时间，契约要求不超过 2 秒。
	shutdownTimeout = 2 * time.Second

	// maxUploadBytes 是单个 ISO 上传的大小上限（8 GiB）。
	maxUploadBytes = int64(8) << 30
)

// Config 是 Web 服务器的装配参数。
type Config struct {
	// Addr 是监听地址，如 ":16080"；为空时使用 DefaultAddr。
	Addr string
	// Store 是可选的存储层（*store.Store）。web 层只用到 AddEvent 记录
	// 自身产生的操作事件（启动/停止/上传/保存配置），传 nil 也能正常工作。
	Store Store
	// PXE 是必填的 PXE 编排器，由 internal/pxe 实现。
	PXE Controller
	// Version 是展示在界面上的版本号，如 "0.1.0"。
	Version string
}

// Controller 是 web 层控制 PXE 服务的接口，由 pxe 包实现。
// 本接口是 ARCHITECTURE.md 3.6 节定义的契约，不要随意增删方法。
type Controller interface {
	Status() model.ServerStatus
	Start() error
	Stop() error
	Restart() error
	RescanISOs() (int, error)
	ISOs() []model.ISO
	Clients() []model.Client
	Events(limit int) []model.Event
	Settings() model.Settings
	SaveSettings(model.Settings) error
	Interfaces() []NetInterface
	DefaultISO() string
	SetDefaultISO(id string) error
}

// Store 是 web 层对存储层的最小依赖（可选）。
// *store.Store 只要实现 AddEvent(model.Event) 就能直接传给 Config.Store。
type Store interface {
	// AddEvent 向全局事件环形缓冲追加一条事件，界面日志面板会显示它。
	AddEvent(model.Event)
}

// ClientClearer 是 Controller 的可选扩展：实现后 DELETE /api/clients 才能真正清空客户机表。
type ClientClearer interface {
	ClearClients() error
}

// Previewer 是 Controller 的可选扩展：实现后 /api/preview/{isoID} 返回
// 与真实下发完全一致的 iPXE 脚本；未实现时 web 层会按 ISO 探测结果生成一份近似预览。
type Previewer interface {
	Preview(isoID, mac string) (string, error)
}

// NetInterface 描述一块本机网卡，供界面顶部的 IP 下拉框使用。
type NetInterface struct {
	Name    string `json:"name"`          // 网卡名，如 "en0" / "eth0"
	IP      string `json:"ip"`            // IPv4 地址
	Mask    string `json:"mask"`          // 子网掩码
	Up      bool   `json:"up"`            // 是否已启用
	MAC     string `json:"mac,omitempty"` // MAC 地址
	Default bool   `json:"default"`       // 是否系统默认路由所在网卡
}

// Server 是 Web 管理界面服务器。
type Server struct {
	cfg     Config
	version string

	addr      string
	handler   http.Handler
	indexHTML []byte

	httpSrv *http.Server

	mu      sync.Mutex
	ln      net.Listener
	stopped bool

	done      chan struct{}
	closeOnce sync.Once

	hub           *hub
	eventSink     Store
	intervalNanos int64 // SSE 轮询间隔（纳秒），用原子操作读写

	// 以下字段只由 pollLoop 这个 goroutine（以及测试）访问，无需加锁。
	lastStatus   string
	lastClients  string
	lastEvent    model.Event
	hasLastEvent bool
}

// New 构造一个 Web 服务器。此时不会监听端口，调用 Start 后才对外服务；
// Handler 可以随时取出来交给 httptest 或复用。
func New(cfg Config) (*Server, error) {
	if cfg.PXE == nil {
		return nil, errors.New("web: 缺少 PXE 控制器（Config.PXE 不能为空）")
	}

	addr := strings.TrimSpace(cfg.Addr)
	if addr == "" {
		addr = DefaultAddr
	}

	indexHTML, err := assetsFS.ReadFile("assets/index.html")
	if err != nil {
		return nil, fmt.Errorf("web: 内嵌前端缺失（assets/index.html）: %w", err)
	}

	s := &Server{
		cfg:       cfg,
		version:   strings.TrimSpace(cfg.Version),
		addr:      addr,
		indexHTML: indexHTML,
		done:      make(chan struct{}),
		hub:       newHub(),
		eventSink: cfg.Store,
	}
	if s.version == "" {
		s.version = "dev"
	}
	atomic.StoreInt64(&s.intervalNanos, int64(defaultStreamInterval))

	mux, err := s.routes()
	if err != nil {
		return nil, err
	}
	s.handler = recoverMiddleware(mux)
	s.httpSrv = &http.Server{
		Handler:           s.handler,
		ReadHeaderTimeout: 10 * time.Second,
	}

	// 记录初始状态，避免 SSE 第一条增量把刚下发的快照重复推一遍。
	s.initSSEState()
	go s.pollLoop()

	return s, nil
}

// routes 组装 HTTP 路由表。
func (s *Server) routes() (http.Handler, error) {
	staticFS, err := fs.Sub(assetsFS, "assets")
	if err != nil {
		return nil, fmt.Errorf("web: 内嵌前端资源损坏: %w", err)
	}

	mux := http.NewServeMux()
	mux.Handle("/static/", http.StripPrefix("/static/", cacheStatic(http.FileServer(http.FS(staticFS)))))
	mux.HandleFunc("/api/", s.handleAPI)
	mux.HandleFunc("/healthz", s.handleHealthz)
	mux.HandleFunc("/", s.handleIndex)
	return mux, nil
}

// safeStatus 调用控制器取状态，并兜住控制器里的 panic。
// 用于「不能让一个坏控制器拖垮整个 Web 服务」的场景（健康检查、SSE 初始化）。
func (s *Server) safeStatus() (st model.ServerStatus) {
	defer func() {
		if rec := recover(); rec != nil {
			log.Printf("web: 读取服务器状态时控制器 panic: %v", rec)
			st = model.ServerStatus{}
		}
	}()
	return s.cfg.PXE.Status()
}

// Handler 返回完整的 HTTP 处理器，方便单测或挂到别的 mux 下。
func (s *Server) Handler() http.Handler { return s.handler }

// Addr 返回实际监听地址（Start 之前是配置值，Start 之后是内核分配的地址）。
func (s *Server) Addr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.addr
}

// Start 绑定端口并开始对外服务。非阻塞，重复调用是幂等的。
func (s *Server) Start() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.stopped {
		return errors.New("web: 服务器已关闭，不能再次启动")
	}
	if s.ln != nil {
		return nil // 已在运行
	}

	ln, err := net.Listen("tcp", s.addr)
	if err != nil {
		return fmt.Errorf("web: 监听 %s 失败: %w", s.addr, err)
	}
	s.ln = ln
	s.addr = ln.Addr().String()
	s.httpSrv.Addr = s.addr

	go func() {
		if err := s.httpSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("web: HTTP 服务意外退出: %v", err)
		}
	}()
	return nil
}

// Stop 关闭 Web 服务器，幂等且最多等待 2 秒。
func (s *Server) Stop() error {
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return nil
	}
	s.stopped = true
	s.mu.Unlock()

	// 先让 SSE 长连接退出，Shutdown 才不会被它们拖住。
	s.closeOnce.Do(func() { close(s.done) })

	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := s.httpSrv.Shutdown(ctx); err != nil {
		_ = s.httpSrv.Close()
		return fmt.Errorf("web: 关闭 HTTP 服务: %w", err)
	}
	return nil
}

// handleIndex 返回单页前端。
func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		methodNotAllowed(w, r, "GET")
		return
	}
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	if _, err := w.Write(s.indexHTML); err != nil {
		log.Printf("web: 输出首页失败: %v", err)
	}
}

// handleHealthz 是给外部探活用的极简接口。
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		methodNotAllowed(w, r, "GET")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":  "ok",
		"version": s.version,
		"running": s.safeStatus().Running,
	})
}

// cacheStatic 给内嵌静态资源加上缓存头，避免每次刷新都重新下载。
func cacheStatic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=300")
		next.ServeHTTP(w, r)
	})
}

// trackedWriter 记录响应是否已经写出，供 recover 中间件判断还能不能回错误 JSON。
type trackedWriter struct {
	http.ResponseWriter
	wrote bool
}

func (t *trackedWriter) WriteHeader(code int) {
	t.wrote = true
	t.ResponseWriter.WriteHeader(code)
}

func (t *trackedWriter) Write(p []byte) (int, error) {
	t.wrote = true
	return t.ResponseWriter.Write(p)
}

// Flush 透传，保证 SSE 在中间件包装后仍能拿到 http.Flusher。
func (t *trackedWriter) Flush() {
	if f, ok := t.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap 让 http.ResponseController 能找到底层 ResponseWriter。
func (t *trackedWriter) Unwrap() http.ResponseWriter { return t.ResponseWriter }

// recoverMiddleware 兜住 handler 里的 panic，避免整个进程崩溃。
func recoverMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tw := &trackedWriter{ResponseWriter: w}
		defer func() {
			if rec := recover(); rec != nil {
				log.Printf("web: 处理 %s %s 时发生 panic: %v", r.Method, r.URL.Path, rec)
				if !tw.wrote {
					writeErr(tw, http.StatusInternalServerError, "服务器内部错误")
				}
			}
		}()
		next.ServeHTTP(tw, r)
	})
}
