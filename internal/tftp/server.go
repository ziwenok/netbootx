package tftp

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/wupengcheng/netbootx/internal/model"
)

// Config 是 TFTP 服务器的配置。
type Config struct {
	Root      string // 文件根目录
	Addr      string // 监听地址，如 ":69"
	BlockSize int    // 协商的块大小上限，默认 1468
	OnEvent   func(model.Event)
}

// Stats 是 TFTP 服务器的运行统计。
type Stats struct {
	Addr      string        `json:"addr"`
	Root      string        `json:"root"`
	Running   bool          `json:"running"`
	StartedAt time.Time     `json:"startedAt"`
	Uptime    time.Duration `json:"uptime"`

	Requests   uint64 `json:"requests"`  // RRQ 总数
	Completed  uint64 `json:"completed"` // 成功完成的传输
	Failed     uint64 `json:"failed"`    // 失败的传输
	Denied     uint64 `json:"denied"`    // 被安全策略拒绝的请求
	BytesSent  uint64 `json:"bytesSent"` // 累计发送字节数
	Active     int    `json:"active"`    // 正在进行的传输数
	Transfers  uint64 `json:"transfers"` // 累计传输次数（含失败）
	LastClient string `json:"lastClient"`
}

// Server 是 TFTP 服务器。
type Server struct {
	cfg  Config
	root string

	conn *net.UDPConn

	mu        sync.RWMutex
	running   bool
	closed    bool
	startedAt time.Time
	wg        sync.WaitGroup

	// xferConns 记录进行中的传输套接字，Stop 时统一关闭以便快速退出。
	xferConns map[*net.UDPConn]struct{}

	stat struct {
		requests, completed, failed, denied atomic.Uint64
		bytesSent, transfers                atomic.Uint64
		active                              atomic.Int64
	}
	lastClient atomic.Value // string
}

// New 创建 TFTP 服务器。
func New(cfg Config) (*Server, error) {
	if strings.TrimSpace(cfg.Root) == "" {
		return nil, fmt.Errorf("tftp: 文件根目录不能为空")
	}
	abs, err := filepath.Abs(cfg.Root)
	if err != nil {
		return nil, fmt.Errorf("tftp: 解析根目录 %q 失败: %w", cfg.Root, err)
	}
	st, err := os.Stat(abs)
	if err != nil {
		return nil, fmt.Errorf("tftp: 根目录 %q 不可用: %w", abs, err)
	}
	if !st.IsDir() {
		return nil, fmt.Errorf("tftp: 根目录 %q 不是目录", abs)
	}
	if cfg.Addr == "" {
		cfg.Addr = ":69"
	}
	if cfg.BlockSize <= 0 {
		cfg.BlockSize = defaultBlockSize
	}
	if cfg.BlockSize > maxBlockSize {
		cfg.BlockSize = maxBlockSize
	}
	s := &Server{cfg: cfg, root: abs, xferConns: make(map[*net.UDPConn]struct{})}
	s.lastClient.Store("")
	return s, nil
}

// Root 返回规范化后的文件根目录。
func (s *Server) Root() string { return s.root }

// Addr 返回实际监听地址（Start 之后有效）。
func (s *Server) Addr() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.conn != nil {
		return s.conn.LocalAddr().String()
	}
	return s.cfg.Addr
}

// Start 绑定端口并启动收包循环（非阻塞）。
func (s *Server) Start() error {
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return nil
	}
	s.mu.Unlock()

	addr, err := net.ResolveUDPAddr("udp4", s.cfg.Addr)
	if err != nil {
		return fmt.Errorf("tftp: 解析监听地址 %q 失败: %w", s.cfg.Addr, err)
	}
	conn, err := net.ListenUDP("udp4", addr)
	if err != nil {
		return fmt.Errorf("tftp: 绑定 %s 失败: %w（端口 69 需要 root 权限，请用 sudo 运行；若地址已被占用，说明本机已有 TFTP 服务在运行）", s.cfg.Addr, err)
	}

	s.mu.Lock()
	s.conn = conn
	s.running = true
	s.closed = false
	s.startedAt = time.Now()
	s.mu.Unlock()

	s.wg.Add(1)
	go s.readLoop(conn)
	s.emit("info", "", "TFTP 服务器已启动：监听=%s 根目录=%s 块大小上限=%d", conn.LocalAddr(), s.root, s.cfg.BlockSize)
	return nil
}

// Stop 停止服务器并等待进行中的传输结束。可重复调用。
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
	// 关闭进行中的传输套接字，让传输协程立刻从阻塞读里返回。
	active := make([]*net.UDPConn, 0, len(s.xferConns))
	for c := range s.xferConns {
		active = append(active, c)
	}
	s.xferConns = make(map[*net.UDPConn]struct{})
	s.mu.Unlock()

	if conn != nil {
		if err := conn.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			return fmt.Errorf("tftp: 关闭监听套接字失败: %w", err)
		}
	}
	for _, c := range active {
		_ = c.Close()
	}
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		return fmt.Errorf("tftp: 停止超时，仍有传输未结束")
	}
	s.emit("info", "", "TFTP 服务器已停止")
	return nil
}

// Stats 返回运行统计。
func (s *Server) Stats() Stats {
	st := Stats{
		Addr:      s.Addr(),
		Root:      s.root,
		Requests:  s.stat.requests.Load(),
		Completed: s.stat.completed.Load(),
		Failed:    s.stat.failed.Load(),
		Denied:    s.stat.denied.Load(),
		BytesSent: s.stat.bytesSent.Load(),
		Active:    int(s.stat.active.Load()),
		Transfers: s.stat.transfers.Load(),
	}
	if v, ok := s.lastClient.Load().(string); ok {
		st.LastClient = v
	}
	s.mu.RLock()
	st.Running = s.running
	st.StartedAt = s.startedAt
	s.mu.RUnlock()
	if !st.StartedAt.IsZero() {
		st.Uptime = time.Since(st.StartedAt)
	}
	return st
}

// readLoop 是主收包循环：只处理 RRQ / WRQ，每个请求起一个协程用独立端口传输。
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
			s.emit("warn", "", "TFTP 收包失败: %v", err)
			continue
		}
		data := make([]byte, n)
		copy(data, buf[:n])
		s.handleRequest(data, src)
	}
}

// handleRequest 处理一个 RRQ / WRQ。
func (s *Server) handleRequest(b []byte, src *net.UDPAddr) {
	req, err := parseRequest(b)
	if err != nil {
		// 连报文都解析不了时，用主 socket 回 ERROR，尽力而为。
		s.replyFromMain(marshalError(ErrIllegalOp, err.Error()), src)
		return
	}
	if req.Opcode == opWRQ {
		s.stat.denied.Add(1)
		s.emit("warn", "", "拒绝来自 %s 的写请求（%s）：本服务器只读", src, req.Filename)
		s.replyError(src, ErrAccessViolation, "write request refused: read-only server")
		return
	}
	s.stat.requests.Add(1)
	s.lastClient.Store(src.String())

	path, err := s.resolve(req.Filename)
	if err != nil {
		s.stat.denied.Add(1)
		s.emit("warn", "", "拒绝来自 %s 的非法路径请求 %q: %v", src, req.Filename, err)
		s.replyError(src, ErrAccessViolation, "access violation")
		return
	}
	f, err := os.Open(path)
	if err != nil {
		s.stat.failed.Add(1)
		if os.IsNotExist(err) {
			s.emit("info", "", "文件不存在：%q（来自 %s）", req.Filename, src)
			s.replyError(src, ErrFileNotFound, "file not found")
			return
		}
		if os.IsPermission(err) {
			s.emit("warn", "", "无权读取文件：%q（来自 %s）", req.Filename, src)
			s.replyError(src, ErrAccessViolation, "access violation")
			return
		}
		s.emit("error", "", "打开文件 %q 失败: %v", path, err)
		s.replyError(src, ErrNotDefined, "internal error")
		return
	}
	st, err := f.Stat()
	if err != nil || st.IsDir() {
		f.Close()
		s.stat.failed.Add(1)
		s.replyError(src, ErrFileNotFound, "file not found")
		return
	}

	// 关键：用新的临时端口回包，主 socket 只收请求。
	xferConn, err := net.ListenUDP("udp4", s.localAddr())
	if err != nil {
		f.Close()
		s.stat.failed.Add(1)
		s.emit("error", "", "创建传输套接字失败: %v", err)
		return
	}

	s.mu.Lock()
	if s.xferConns == nil {
		s.xferConns = make(map[*net.UDPConn]struct{})
	}
	s.xferConns[xferConn] = struct{}{}
	s.mu.Unlock()

	s.stat.transfers.Add(1)
	s.stat.active.Add(1)
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer s.stat.active.Add(-1)
		defer f.Close()
		defer func() {
			s.mu.Lock()
			delete(s.xferConns, xferConn)
			s.mu.Unlock()
			_ = xferConn.Close()
		}()
		s.transfer(req, f, st.Size(), xferConn, src)
	}()
}

// localAddr 返回传输套接字应绑定的本地地址：监听在具体 IP 上时沿用该 IP，
// 监听在通配地址上时由系统选择出口 IP。
func (s *Server) localAddr() *net.UDPAddr {
	laddr := &net.UDPAddr{IP: nil, Port: 0}
	s.mu.RLock()
	conn := s.conn
	s.mu.RUnlock()
	if conn != nil {
		if la, ok := conn.LocalAddr().(*net.UDPAddr); ok && la.IP != nil && !la.IP.IsUnspecified() {
			laddr.IP = la.IP
		}
	}
	return laddr
}

// replyError 从一个临时端口回一个 ERROR 报文。
//
// 即使请求在建立传输前就被拒绝（文件不存在 / 路径非法），也统一用临时端口回包：
// 客户端会拿「收到的第一个回包的来源端口」当服务器 TID，从 :69 回包会让部分
// 严格实现（以及我们的测试客户端）判为非法来源。
func (s *Server) replyError(dst *net.UDPAddr, code uint16, msg string) {
	if dst == nil {
		return
	}
	c, err := net.ListenUDP("udp4", s.localAddr())
	if err != nil {
		s.replyFromMain(marshalError(code, msg), dst)
		return
	}
	defer c.Close()
	if _, err := c.WriteToUDP(marshalError(code, msg), dst); err != nil {
		s.emit("warn", "", "发送 TFTP 错误报文到 %s 失败: %v", dst, err)
	}
}

// replyFromMain 用主 socket 回一个错误报文（请求还没建立传输上下文时使用）。
func (s *Server) replyFromMain(b []byte, dst *net.UDPAddr) {
	s.mu.RLock()
	conn := s.conn
	s.mu.RUnlock()
	if conn == nil {
		return
	}
	if _, err := conn.WriteToUDP(b, dst); err != nil {
		s.emit("warn", "", "发送 TFTP 错误报文到 %s 失败: %v", dst, err)
	}
}

// resolve 把请求里的文件名解析成磁盘路径，并做路径穿越防护。
func (s *Server) resolve(name string) (string, error) {
	// TFTP 客户端常用 "\\dir\\file" 形式，统一成 "/"。
	clean := strings.ReplaceAll(name, "\\", "/")
	if strings.ContainsRune(clean, 0) {
		return "", fmt.Errorf("文件名含 NUL 字符")
	}
	// 明确拒绝任何 ".." 片段（即使规范化后不会逃逸，也按安全策略直接拒）。
	for _, part := range strings.Split(clean, "/") {
		if part == ".." {
			return "", fmt.Errorf("路径包含 .. 片段")
		}
	}
	clean = strings.TrimPrefix(clean, "/")
	if clean == "" {
		return "", fmt.Errorf("文件名为空")
	}
	full := filepath.Join(s.root, filepath.FromSlash(clean))
	// 双保险：解析后的绝对路径必须仍在根目录内。
	abs, err := filepath.Abs(full)
	if err != nil {
		return "", fmt.Errorf("解析路径失败: %w", err)
	}
	rel, err := filepath.Rel(s.root, abs)
	if err != nil {
		return "", fmt.Errorf("计算相对路径失败: %w", err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("路径越出根目录")
	}
	return abs, nil
}

// transfer 执行一次完整的 DATA/ACK 传输。
func (s *Server) transfer(req *request, f *os.File, size int64, conn *net.UDPConn, dst *net.UDPAddr) {
	start := time.Now()
	blksize, timeout, windowsize, oack, err := negotiate(req, size, s.cfg.BlockSize)
	if err != nil {
		s.stat.failed.Add(1)
		s.emit("warn", "", "选项协商失败（%s）: %v", req.Filename, err)
		_, _ = conn.WriteToUDP(marshalError(ErrOptionNegotiation, err.Error()), dst)
		return
	}

	if len(oack) > 0 {
		if err := s.sendAndWait(conn, dst, marshalOACK(oack), 0, timeout); err != nil {
			s.stat.failed.Add(1)
			s.emit("warn", "", "OACK 未被确认（%s）: %v", req.Filename, err)
			return
		}
	}

	buf := make([]byte, blksize)
	var sent int64
	block := uint16(0)
	for {
		// 一次准备一个窗口的数据块（RFC 7440：窗口内连续发送，再等最后一个块的 ACK）。
		pkts := make([][]byte, 0, windowsize)
		var windowLast uint16
		lastShort := false
		for i := 0; i < windowsize; i++ {
			n, rerr := io.ReadFull(f, buf)
			if rerr != nil && rerr != io.ErrUnexpectedEOF && rerr != io.EOF {
				s.stat.failed.Add(1)
				_, _ = conn.WriteToUDP(marshalError(ErrNotDefined, "read error"), dst)
				s.emit("error", "", "读取文件 %q 失败: %v", req.Filename, rerr)
				return
			}
			block++ // uint16 自然回绕，符合 RFC 1350
			pkts = append(pkts, marshalData(block, buf[:n]))
			windowLast = block
			sent += int64(n)
			if n < blksize {
				lastShort = true // 最后一个数据块（不足一个块）
				break
			}
		}
		if err := s.sendWindow(conn, dst, pkts, windowLast, timeout); err != nil {
			s.stat.failed.Add(1)
			s.emit("warn", "", "传输中断（%s，块 %d）: %v", req.Filename, windowLast, err)
			return
		}
		if lastShort {
			break
		}
	}

	s.stat.completed.Add(1)
	s.stat.bytesSent.Add(uint64(sent))
	s.emit("info", "", "TFTP 传输完成：%s → %s，%d 字节，块大小 %d，窗口 %d，耗时 %s",
		req.Filename, dst, sent, blksize, windowsize, time.Since(start).Round(time.Millisecond))
}

// sendWindow 发送一个窗口内的全部数据块，然后等待窗口最后一个块的 ACK。
//
// 超时或收到错误时按「整窗重传」处理，最多重传 3 次。windowsize=1 时
// 退化成经典的「发一块等一个 ACK」。
func (s *Server) sendWindow(conn *net.UDPConn, dst *net.UDPAddr, pkts [][]byte, lastBlock uint16, timeoutSec int) error {
	const maxRetries = 3
	if timeoutSec <= 0 {
		timeoutSec = defaultTimeout
	}
	deadline := time.Duration(timeoutSec) * time.Second
	buf := make([]byte, 1024)
	for attempt := 0; attempt < maxRetries; attempt++ {
		for _, pkt := range pkts {
			if _, err := conn.WriteToUDP(pkt, dst); err != nil {
				return fmt.Errorf("发送数据报文失败: %w", err)
			}
		}
		if err := conn.SetReadDeadline(time.Now().Add(deadline)); err != nil {
			return fmt.Errorf("设置读超时失败: %w", err)
		}
		for {
			n, src, err := conn.ReadFromUDP(buf)
			if err != nil {
				var ne net.Error
				if errors.As(err, &ne) && ne.Timeout() {
					break // 本轮超时，整窗重传
				}
				return fmt.Errorf("接收 ACK 失败: %w", err)
			}
			// 只接受来自同一个客户端 TID 的报文。
			if src.Port != dst.Port || !src.IP.Equal(dst.IP) {
				_, _ = conn.WriteToUDP(marshalError(ErrUnknownTID, "unknown transfer ID"), src)
				continue
			}
			p, perr := parsePacket(buf[:n])
			if perr != nil {
				continue
			}
			switch p.Opcode {
			case opACK:
				if p.Block == lastBlock {
					return nil
				}
				// 窗口内更早的块被单独 ACK，或旧的重复 ACK：继续等窗口末尾的 ACK。
				continue
			case opERROR:
				return fmt.Errorf("客户端返回错误 %d: %s", p.Code, p.Msg)
			default:
				continue
			}
		}
	}
	return fmt.Errorf("重传 %d 次后仍未收到 ACK（块 %d）", maxRetries, lastBlock)
}

// sendAndWait 发送单个报文并等待对应 ACK（OACK 用，wantBlock=0）。
func (s *Server) sendAndWait(conn *net.UDPConn, dst *net.UDPAddr, pkt []byte, wantBlock uint16, timeoutSec int) error {
	const maxRetries = 3
	if timeoutSec <= 0 {
		timeoutSec = defaultTimeout
	}
	deadline := time.Duration(timeoutSec) * time.Second
	buf := make([]byte, 1024)
	for attempt := 0; attempt < maxRetries; attempt++ {
		if _, err := conn.WriteToUDP(pkt, dst); err != nil {
			return fmt.Errorf("发送数据报文失败: %w", err)
		}
		if err := conn.SetReadDeadline(time.Now().Add(deadline)); err != nil {
			return fmt.Errorf("设置读超时失败: %w", err)
		}
		for {
			n, src, err := conn.ReadFromUDP(buf)
			if err != nil {
				var ne net.Error
				if errors.As(err, &ne) && ne.Timeout() {
					break // 本轮超时，重传
				}
				return fmt.Errorf("接收 ACK 失败: %w", err)
			}
			// 只接受来自同一个客户端 TID 的报文。
			if src.Port != dst.Port || !src.IP.Equal(dst.IP) {
				_, _ = conn.WriteToUDP(marshalError(ErrUnknownTID, "unknown transfer ID"), src)
				continue
			}
			p, err := parsePacket(buf[:n])
			if err != nil {
				continue
			}
			switch p.Opcode {
			case opACK:
				if p.Block == wantBlock {
					return nil
				}
				// 旧的重复 ACK，忽略后继续等。
				continue
			case opERROR:
				return fmt.Errorf("客户端返回错误 %d: %s", p.Code, p.Msg)
			default:
				continue
			}
		}
	}
	return fmt.Errorf("重传 %d 次后仍未收到 ACK（块 %d）", maxRetries, wantBlock)
}

// emit 上报一条事件。
func (s *Server) emit(level, mac, format string, args ...any) {
	if s.cfg.OnEvent == nil {
		return
	}
	s.cfg.OnEvent(model.Event{
		Time:  time.Now(),
		Level: level,
		MAC:   mac,
		Kind:  "tftp",
		Msg:   fmt.Sprintf(format, args...),
	})
}
