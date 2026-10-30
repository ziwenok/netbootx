package dhcp

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/wupengcheng/netbootx/internal/model"
)

// 默认引导文件名（上层没有注入 BootFileFor 时使用）。
const (
	defaultBootFileBIOS   = "undionly.kpxe"
	defaultBootFileUEFI32 = "ipxe32.efi"
	defaultBootFileUEFI64 = "ipxe.efi"
	defaultBootFileARM64  = "ipxe-arm64.efi"
	defaultBootFileLoong  = "ipxe-loongarch64.efi"
)

// Stats 是 DHCP 服务器的运行统计。
type Stats struct {
	Mode      string        `json:"mode"`
	Running   bool          `json:"running"`
	StartedAt time.Time     `json:"startedAt"`
	Uptime    time.Duration `json:"uptime"`

	Received uint64 `json:"received"` // 收到的报文总数
	Sent     uint64 `json:"sent"`     // 发出的报文总数
	Ignored  uint64 `json:"ignored"`  // 与 PXE 无关而忽略的报文
	Errors   uint64 `json:"errors"`   // 解析 / 发送失败次数

	// 以下计数区分方向：Discover / Request / Release / Decline / Inform 是
	// 收到的请求；Offer / Ack / Nak 是本机发出的应答。
	Discover uint64 `json:"discover"` // 收到的 DISCOVER
	Request  uint64 `json:"request"`  // 收到的 REQUEST
	Release  uint64 `json:"release"`  // 收到的 RELEASE
	Decline  uint64 `json:"decline"`  // 收到的 DECLINE
	Inform   uint64 `json:"inform"`   // 收到的 INFORM
	Offer    uint64 `json:"offer"`    // 发出的 OFFER
	Ack      uint64 `json:"ack"`      // 发出的 ACK
	Nak      uint64 `json:"nak"`      // 发出的 NAK

	Leases   int `json:"leases"`   // 当前租约数
	PoolSize int `json:"poolSize"` // IP 池容量
}

// Server 是 DHCP / proxyDHCP 服务器。
type Server struct {
	cfg   Config
	conn  *net.UDPConn
	table *LeaseTable

	mu        sync.RWMutex
	running   bool
	closed    bool
	startedAt time.Time
	wg        sync.WaitGroup

	// 以下字段是回包目的地的可覆盖项，正常使用走默认值，
	// 测试时可指向 127.0.0.1 的高位端口。
	replyPort int
	relayPort int

	// PXE 引导菜单相关（可通过 SetPXEMenu / SetPXEBootItem 配置）。
	pxeMenu             *pxeMenu
	pxeBootItem         uint16
	pxeDiscoveryControl byte

	stat struct {
		received, sent, ignored, errs      atomic.Uint64
		discover, offer, request, ack, nak atomic.Uint64
		release, decline, inform           atomic.Uint64
	}
}

// New 创建 DHCP 服务器。cfg 会被校验并补全默认值。
func New(cfg Config) (*Server, error) {
	if err := cfg.normalize(); err != nil {
		return nil, err
	}
	s := &Server{
		cfg:       cfg,
		replyPort: clientPort,
		relayPort: serverPort,
	}
	if cfg.Mode == ModeFull {
		start, err := parseIPv4(cfg.RangeStart)
		if err != nil {
			return nil, fmt.Errorf("dhcp: IP 池起始地址非法: %w", err)
		}
		end, err := parseIPv4(cfg.RangeEnd)
		if err != nil {
			return nil, fmt.Errorf("dhcp: IP 池结束地址非法: %w", err)
		}
		t, err := NewLeaseTable(start, end, cfg.leaseDuration())
		if err != nil {
			return nil, err
		}
		t.SetHashStart(true)
		s.table = t
	}
	return s, nil
}

// Mode 返回当前工作模式。
func (s *Server) Mode() string { return s.cfg.Mode }

// Config 返回服务器配置的副本。
func (s *Server) Config() Config { return s.cfg }

// BootFile 返回某架构应下发的引导文件名。
func (s *Server) BootFile(arch model.Arch) string {
	if s.cfg.BootFileFor != nil {
		if bf := s.cfg.BootFileFor(arch); bf != "" {
			return bf
		}
	}
	return defaultBootFile(arch)
}

// defaultBootFile 是上层未注入 BootFileFor 时的兜底映射。
func defaultBootFile(arch model.Arch) string {
	switch arch {
	case model.ArchBIOS:
		return defaultBootFileBIOS
	case model.ArchUEFI32:
		return defaultBootFileUEFI32
	case model.ArchUEFI64, model.ArchUEFI64HTTP:
		return defaultBootFileUEFI64
	case model.ArchARM64, model.ArchARM64HTTP:
		return defaultBootFileARM64
	case model.ArchLoongArch64:
		return defaultBootFileLoong
	}
	return defaultBootFileUEFI64
}

// Start 绑定端口并启动收包循环（非阻塞）。
//
// external 模式不监听任何端口，只把状态置为运行中，供上层查询引导文件。
func (s *Server) Start() error {
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return nil
	}
	s.mu.Unlock()

	if s.cfg.Mode == ModeExternal {
		s.mu.Lock()
		s.running = true
		s.startedAt = time.Now()
		s.mu.Unlock()
		s.emit("info", "", "external 模式：不监听 DHCP 端口，由外部 DHCP 服务器通告 next-server=%s", s.cfg.ServerIP)
		return nil
	}

	// 关键：DHCP 必须监听 0.0.0.0。
	//
	// 两个原因：
	//  1. 客户机的 DISCOVER 是发往 255.255.255.255 的广播包，socket 若绑在具体 IP 上
	//     就收不到；
	//  2. 绑在具体 IP 上的 UDP socket 无法向 255.255.255.255 发送受限广播
	//     （BSD/macOS/Linux 通用行为，会报 "can't assign requested address"），
	//     而客户机在拿到地址之前只能靠广播接收应答。
	//
	// cfg.BindIP 因此只作为「对外通告哪个 IP」的语义保留（实际由 ServerIP 承担），
	// 不影响监听地址；这样调用方传什么值都不会把服务弄坏。
	addr := net.JoinHostPort("0.0.0.0", strconv.Itoa(s.cfg.Port))
	udpAddr, err := net.ResolveUDPAddr("udp4", addr)
	if err != nil {
		return fmt.Errorf("dhcp: 解析监听地址 %s 失败: %w", addr, err)
	}
	conn, err := net.ListenUDP("udp4", udpAddr)
	if err != nil {
		return fmt.Errorf("dhcp: 绑定 %s 失败: %w（端口 %d 小于 1024 需要 root 权限，请用 sudo 运行；若提示地址已被占用，说明本机已有 DHCP 服务在运行，可改用 proxy 模式或先停掉它）",
			addr, err, s.cfg.Port)
	}
	// 广播能力是 DHCP 的硬需求：失败就直接报错，不要带着半残的 socket 启动，
	// 否则表现为「解析全对但客户机永远收不到应答」。
	if err := enableBroadcast(conn); err != nil {
		_ = conn.Close()
		return fmt.Errorf("dhcp: 在 %s 上开启 SO_BROADCAST 失败: %w（没有广播能力的 DHCP 无法给还没拿到 IP 的客户机回包）", addr, err)
	}

	s.mu.Lock()
	s.conn = conn
	s.running = true
	s.closed = false
	s.startedAt = time.Now()
	s.mu.Unlock()

	s.wg.Add(1)
	go s.readLoop(conn)
	s.emit("info", "", "DHCP 服务器已启动：模式=%s 监听=%s 服务器IP=%s", s.cfg.Mode, addr, s.cfg.ServerIP)
	return nil
}

// Stop 停止服务器。可重复调用。
func (s *Server) Stop() error {
	s.mu.Lock()
	if !s.running && s.conn == nil {
		s.mu.Unlock()
		return nil
	}
	s.running = false
	s.closed = true
	conn := s.conn
	s.conn = nil
	s.mu.Unlock()

	if conn != nil {
		if err := conn.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			return fmt.Errorf("dhcp: 关闭监听套接字失败: %w", err)
		}
	}
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		return fmt.Errorf("dhcp: 停止超时，收包协程未能及时退出")
	}
	s.emit("info", "", "DHCP 服务器已停止")
	return nil
}

// Leases 返回当前租约表快照（proxy / external 模式下为空）。
func (s *Server) Leases() []Lease {
	if s.table == nil {
		return nil
	}
	return s.table.All()
}

// Stats 返回运行统计。
func (s *Server) Stats() Stats {
	st := Stats{
		Mode:     s.cfg.Mode,
		Received: s.stat.received.Load(),
		Sent:     s.stat.sent.Load(),
		Ignored:  s.stat.ignored.Load(),
		Errors:   s.stat.errs.Load(),
		Discover: s.stat.discover.Load(),
		Offer:    s.stat.offer.Load(),
		Request:  s.stat.request.Load(),
		Ack:      s.stat.ack.Load(),
		Nak:      s.stat.nak.Load(),
		Release:  s.stat.release.Load(),
		Decline:  s.stat.decline.Load(),
		Inform:   s.stat.inform.Load(),
	}
	s.mu.RLock()
	st.Running = s.running
	st.StartedAt = s.startedAt
	s.mu.RUnlock()
	if !st.StartedAt.IsZero() {
		st.Uptime = time.Since(st.StartedAt)
	}
	if s.table != nil {
		st.Leases = len(s.table.All())
		st.PoolSize = s.table.Size()
	}
	return st
}

// readLoop 是主收包循环：只接收并处理 DHCP 报文。
func (s *Server) readLoop(conn *net.UDPConn) {
	defer s.wg.Done()
	buf := make([]byte, 4096)
	for {
		n, src, err := conn.ReadFromUDP(buf)
		if err != nil {
			s.mu.RLock()
			closed := s.closed
			s.mu.RUnlock()
			if closed || errors.Is(err, net.ErrClosed) {
				return
			}
			s.stat.errs.Add(1)
			s.emit("warn", "", "DHCP 收包失败: %v", err)
			continue
		}
		s.stat.received.Add(1)
		s.handleDatagram(buf[:n], src, conn)
	}
}

// handleDatagram 解析一个数据报并回包。
func (s *Server) handleDatagram(b []byte, src *net.UDPAddr, conn *net.UDPConn) {
	pkt, err := DecodePacket(b)
	if err != nil {
		s.stat.errs.Add(1)
		s.emit("warn", "", "丢弃无法解析的 DHCP 报文（来自 %s）: %v", src, err)
		return
	}
	if pkt.Op != bootRequest {
		s.stat.ignored.Add(1)
		return
	}
	rep := s.handlePacket(pkt, src)
	if rep == nil || rep.pkt == nil {
		return
	}
	out, err := rep.pkt.Encode()
	if err != nil {
		s.stat.errs.Add(1)
		s.emit("error", pkt.MAC(), "编码 DHCP 响应失败: %v", err)
		return
	}
	if _, err := conn.WriteToUDP(out, rep.dst); err != nil {
		s.stat.errs.Add(1)
		s.emit("error", pkt.MAC(), "发送 DHCP 响应到 %s 失败: %v", rep.dst, err)
		return
	}
	s.stat.sent.Add(1)
}

// emit 上报一条事件，回调可能为 nil。
func (s *Server) emit(level, mac, format string, args ...any) {
	if s.cfg.OnEvent == nil {
		return
	}
	s.cfg.OnEvent(model.Event{
		Time:  time.Now(),
		Level: level,
		MAC:   mac,
		Kind:  "dhcp",
		Msg:   fmt.Sprintf(format, args...),
	})
}

// enableBroadcast 在套接字上打开 SO_BROADCAST，否则向 255.255.255.255 发包会 EACCES。
func enableBroadcast(conn *net.UDPConn) error {
	raw, err := conn.SyscallConn()
	if err != nil {
		return fmt.Errorf("获取原始连接失败: %w", err)
	}
	var setErr error
	if err := raw.Control(func(fd uintptr) {
		setErr = setSockoptBroadcast(fd)
	}); err != nil {
		return fmt.Errorf("设置 SO_BROADCAST 失败: %w", err)
	}
	return setErr
}
