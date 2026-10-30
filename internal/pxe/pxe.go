// Package pxe 是整个 NetBootX 的编排器。
//
// 它把 dhcp / tftp / httpd / iso / boot 五个模块接在一起，负责：
//   - 服务的启停与状态机
//   - ISO 库的扫描与缓存
//   - 客户机状态跟踪
//   - 向 web 层暴露 Controller 接口
//
// 上层的 web 包只认识 Controller 接口，不关心底层细节。
package pxe

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"time"

	"github.com/wupengcheng/netbootx/internal/model"
	"github.com/wupengcheng/netbootx/internal/store"
)

// Version 是软件版本号。
const Version = "1.0.0"

// NetInterface 描述一块本机网卡，供 Web 界面选择。
type NetInterface struct {
	Name      string   `json:"name"`
	IPs       []string `json:"ips"`
	Netmask   string   `json:"netmask"`
	Broadcast string   `json:"broadcast"`
	Up        bool     `json:"up"`
	Loopback  bool     `json:"loopback"`
}

// Subsystems 是 pxe 编排器依赖的下层服务集合。
// 由 main 注入，便于测试时替换成假实现。
type Subsystems struct {
	// StartDHCP 启动 DHCP 服务（mode 决定行为）。
	StartDHCP func(cfg model.Settings, bootFileFor func(model.Arch) string) error
	StopDHCP  func() error
	DHCPInfo  func() string

	// StartTFTP 启动 TFTP 服务，root 是引导文件根目录。
	StartTFTP func(cfg model.Settings, root string) error
	StopTFTP  func() error
	TFTPInfo  func() string

	// StartHTTP 启动 HTTP 文件服务。
	StartHTTP func(cfg model.Settings, scriptFor func(mac, isoID string) (string, error),
		isos func() []model.ISO, isoDir, userDir string) error
	StopHTTP func() error
	HTTPInfo func() string

	// ScanISOs 扫描 ISO 目录。
	ScanISOs func(dir string) ([]model.ISO, error)
}

// Server 是 PXE 编排器。
type Server struct {
	mu      sync.RWMutex
	store   *store.Store
	subs    Subsystems
	root    string
	running bool
	started time.Time
	lastErr string

	isos     []model.ISO
	isoByID  map[string]model.ISO
	scanning bool
	scanErr  string
	lastScan time.Time

	ctx    context.Context
	cancel context.CancelFunc
}

// New 创建编排器。root 是软件根目录（含 iso/ user/ data/ log/）。
func New(root string, st *store.Store, subs Subsystems) *Server {
	s := &Server{
		store:   st,
		subs:    subs,
		root:    root,
		isoByID: make(map[string]model.ISO),
	}
	s.ctx, s.cancel = context.WithCancel(context.Background())
	return s
}

// ---- 生命周期 ----

// Start 启动全部子服务。
func (s *Server) Start() error {
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return fmt.Errorf("服务已经在运行中")
	}
	s.mu.Unlock()

	cfg := s.store.Settings()

	// 自检：引导文件是否齐全
	if missing := s.store.MissingBootFiles(); len(missing) > 0 {
		return fmt.Errorf("引导文件缺失: %v（请检查 %s 目录）", missing, s.store.BootDir())
	}

	// 先扫 ISO（失败不阻断启动，只记录）
	if _, err := s.RescanISOs(); err != nil {
		s.store.AddEvent(model.Event{Level: "warn", Kind: "system",
			Msg: "扫描 ISO 目录失败: " + err.Error()})
	}

	// 启动顺序：TFTP 先起来（DHCP 回包后客户机立刻要拉文件）
	if err := s.subs.StartTFTP(cfg, s.store.BootDir()); err != nil {
		return fmt.Errorf("启动 TFTP 服务失败: %w", err)
	}
	if err := s.subs.StartHTTP(cfg, s.ScriptFor, s.ISOs, s.isoDir(), s.userDir()); err != nil {
		_ = s.subs.StopTFTP()
		return fmt.Errorf("启动 HTTP 服务失败: %w", err)
	}
	if err := s.subs.StartDHCP(cfg, s.store.BootFileFor); err != nil {
		_ = s.subs.StopHTTP()
		_ = s.subs.StopTFTP()
		return fmt.Errorf("启动 DHCP 服务失败: %w", err)
	}

	s.mu.Lock()
	s.running = true
	s.started = time.Now()
	s.lastErr = ""
	s.mu.Unlock()

	s.store.AddEvent(model.Event{Level: "info", Kind: "system",
		Msg: fmt.Sprintf("PXE 服务已启动 — IP %s，模式 %s，%d 个 ISO",
			cfg.ServerIP, cfg.DHCPMode, len(s.ISOs()))})
	return nil
}

// Stop 停止全部子服务。幂等。
func (s *Server) Stop() error {
	s.mu.Lock()
	if !s.running {
		s.mu.Unlock()
		return nil
	}
	s.running = false
	s.mu.Unlock()

	// 逆序停止，每步都不阻塞太久
	var firstErr error
	if err := s.subs.StopDHCP(); err != nil && firstErr == nil {
		firstErr = err
	}
	if err := s.subs.StopHTTP(); err != nil && firstErr == nil {
		firstErr = err
	}
	if err := s.subs.StopTFTP(); err != nil && firstErr == nil {
		firstErr = err
	}
	s.store.AddEvent(model.Event{Level: "info", Kind: "system", Msg: "PXE 服务已停止"})
	return firstErr
}

// Restart 重启服务。
func (s *Server) Restart() error {
	if err := s.Stop(); err != nil {
		s.store.AddEvent(model.Event{Level: "warn", Kind: "system",
			Msg: "停止服务时出错（继续重启）: " + err.Error()})
	}
	time.Sleep(300 * time.Millisecond) // 等端口释放
	return s.Start()
}

// Running 报告服务是否在运行。
func (s *Server) Running() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.running
}

// ---- 状态 ----

// Status 返回给 Web 界面的实时状态。
func (s *Server) Status() model.ServerStatus {
	s.mu.RLock()
	running := s.running
	started := s.started
	s.mu.RUnlock()

	cfg := s.store.Settings()
	clients := s.store.Clients()
	hostname, _ := os.Hostname()

	var uptime int64
	if running && !started.IsZero() {
		uptime = int64(time.Since(started).Seconds())
	}

	return model.ServerStatus{
		Running:      running,
		ServerIP:     cfg.ServerIP,
		DHCPMode:     cfg.DHCPMode,
		StartedAt:    started,
		UptimeSec:    uptime,
		ClientCount:  len(clients),
		ISOCount:     len(s.ISOs()),
		HTTPPort:     cfg.HTTPPort,
		TFTPPort:     cfg.TFTPPort,
		WebPort:      cfg.WebPort,
		Version:      Version,
		Hostname:     hostname,
		OS:           runtime.GOOS,
		Arch:         runtime.GOARCH,
		BootFileBIOS: s.store.BootFileFor(model.ArchBIOS),
		BootFileUEFI: s.store.BootFileFor(model.ArchUEFI64),
	}
}

// ---- ISO ----

// RescanISOs 重新扫描 ISO 目录。
func (s *Server) RescanISOs() (int, error) {
	s.mu.Lock()
	if s.scanning {
		s.mu.Unlock()
		return 0, fmt.Errorf("正在扫描中，请稍候")
	}
	s.scanning = true
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		s.scanning = false
		s.mu.Unlock()
	}()

	dir := s.isoDir()
	isos, err := s.subs.ScanISOs(dir)

	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastScan = time.Now()
	if err != nil {
		s.scanErr = err.Error()
		return 0, fmt.Errorf("扫描 %s: %w", dir, err)
	}
	s.scanErr = ""
	s.isos = isos
	s.isoByID = make(map[string]model.ISO, len(isos))
	for _, is := range isos {
		s.isoByID[is.ID] = is
	}
	return len(isos), nil
}

// ISOs 返回当前 ISO 列表（副本）。
func (s *Server) ISOs() []model.ISO {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]model.ISO, len(s.isos))
	copy(out, s.isos)
	return out
}

// ISOByID 按 ID 查 ISO。
func (s *Server) ISOByID(id string) (model.ISO, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	is, ok := s.isoByID[id]
	return is, ok
}

// LastScan 返回上次扫描的时间。
func (s *Server) LastScan() time.Time {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.lastScan
}

// ---- 客户机 ----

// Clients 返回客户机列表。
func (s *Server) Clients() []model.Client {
	return s.store.Clients()
}

// ClearClients 清空客户机列表。
func (s *Server) ClearClients() {
	s.store.ClearClients()
}

// Events 返回最近事件。
func (s *Server) Events(limit int) []model.Event {
	return s.store.Events(limit)
}

// ---- 配置 ----

// Settings 返回当前配置。
func (s *Server) Settings() model.Settings {
	return s.store.Settings()
}

// SaveSettings 保存配置。如果服务正在运行，自动重启以生效。
func (s *Server) SaveSettings(cfg model.Settings) error {
	wasRunning := s.Running()
	if wasRunning {
		if err := s.Stop(); err != nil {
			return fmt.Errorf("应用新配置前停止服务失败: %w", err)
		}
	}
	if err := s.store.SaveSettings(cfg); err != nil {
		if wasRunning {
			_ = s.Start() // 尽力恢复
		}
		return err
	}
	if wasRunning {
		if err := s.Start(); err != nil {
			return fmt.Errorf("新配置已保存，但重启服务失败: %w", err)
		}
	}
	return nil
}

// DefaultISO 返回默认启动的 ISO ID。
func (s *Server) DefaultISO() string {
	return s.store.Settings().DefaultISO
}

// SetDefaultISO 设置默认启动项。
func (s *Server) SetDefaultISO(id string) error {
	if id != "" {
		if _, ok := s.ISOByID(id); !ok {
			return fmt.Errorf("ISO %q 不存在", id)
		}
	}
	cfg := s.store.Settings()
	cfg.DefaultISO = id
	return s.store.SaveSettings(cfg)
}

// ScriptFor 为某台客户机生成 iPXE 启动脚本。
// 这是 httpd 回调进来的入口。
func (s *Server) ScriptFor(mac, isoID string) (string, error) {
	cfg := s.store.Settings()
	c, ok := s.store.GetClient(mac)
	if !ok {
		// 客户机还没在 DHCP 阶段被记录（例如外部 DHCP 模式），
		// 造一个最小记录，靠后续 iPXE 上报的 SMBIOS 补齐。
		c = model.Client{
			MAC:       model.NormalizeMAC(mac),
			Arch:      model.ArchUEFI64,
			FirstSeen: time.Now(),
			LastSeen:  time.Now(),
		}
		s.store.UpsertClient(c)
	}
	c.LastSeen = time.Now()

	baseURL := fmt.Sprintf("http://%s:%d", cfg.ServerIP, cfg.HTTPPort)
	isos := s.ISOs()

	// 如果指定了 ISO，直接生成该 ISO 的启动脚本（菜单里的具体项跳转过来）
	if isoID != "" {
		is, ok := s.ISOByID(isoID)
		if !ok {
			return "", fmt.Errorf("ISO %q 不存在", isoID)
		}
		// 持久化「这台机器选了哪个 ISO」，界面才能显示
		s.store.UpsertClient(model.Client{MAC: c.MAC, Selected: isoID})
		s.store.SetClientState(c.MAC, model.StateBooting)
		c.Selected = isoID
		return s.entryScript(c, is, baseURL), nil
	}

	s.store.SetClientState(c.MAC, model.StateMenu)
	return s.menuScript(c, isos, baseURL), nil
}

// ---- 内部工具 ----

func (s *Server) isoDir() string  { return s.resolveDir(s.store.Settings().ISODir) }
func (s *Server) userDir() string { return s.resolveDir(s.store.Settings().UserDir) }

// resolveDir 把配置里的相对路径解析成绝对路径。
func (s *Server) resolveDir(d string) string {
	if d == "" {
		return s.root
	}
	if filepath.IsAbs(d) {
		return d
	}
	return filepath.Join(s.root, d)
}

// Root 返回软件根目录。
func (s *Server) Root() string { return s.root }

// ---- 网卡枚举 ----

// Interfaces 枚举本机所有非回环网卡的 IPv4 地址，供界面选择。
func Interfaces() []NetInterface {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var out []NetInterface
	for _, ifi := range ifaces {
		ni := NetInterface{
			Name:     ifi.Name,
			Up:       ifi.Flags&net.FlagUp != 0,
			Loopback: ifi.Flags&net.FlagLoopback != 0,
		}
		addrs, err := ifi.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip4 := ipnet.IP.To4()
			if ip4 == nil {
				continue
			}
			ni.IPs = append(ni.IPs, ip4.String())
			if ni.Netmask == "" {
				ni.Netmask = net.IP(ipnet.Mask).String()
				if b := broadcastAddr(ipnet); b != nil {
					ni.Broadcast = b.String()
				}
			}
		}
		if len(ni.IPs) == 0 && !ni.Loopback {
			continue
		}
		out = append(out, ni)
	}
	// 有 IP 的、非回环的排前面
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Loopback != b.Loopback {
			return !a.Loopback
		}
		return len(a.IPs) > len(b.IPs)
	})
	return out
}

// broadcastAddr 计算网段的广播地址。
func broadcastAddr(n *net.IPNet) net.IP {
	ip := n.IP.To4()
	if ip == nil || len(n.Mask) != 4 {
		return nil
	}
	b := make(net.IP, 4)
	for i := 0; i < 4; i++ {
		b[i] = ip[i] | ^n.Mask[i]
	}
	return b
}

// LocalIPForInterface 取某块网卡的第一个 IPv4 地址。
func LocalIPForInterface(name string) (string, error) {
	ifi, err := net.InterfaceByName(name)
	if err != nil {
		return "", fmt.Errorf("找不到网卡 %q: %w", name, err)
	}
	addrs, err := ifi.Addrs()
	if err != nil {
		return "", err
	}
	for _, a := range addrs {
		if ipnet, ok := a.(*net.IPNet); ok {
			if ip4 := ipnet.IP.To4(); ip4 != nil {
				return ip4.String(), nil
			}
		}
	}
	return "", fmt.Errorf("网卡 %q 没有 IPv4 地址", name)
}
