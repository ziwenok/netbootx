package web

import (
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/wupengcheng/netbootx/internal/model"
)

// SSE 事件名。前端按事件名分发，未知事件直接忽略（方便以后加字段）。
const (
	sseEventSnapshot = "snapshot" // 连接建立时的一次性全量快照
	sseEventStatus   = "status"   // 服务器状态变化
	sseEventClients  = "clients"  // 客户机列表变化
	sseEventEvents   = "events"   // 新增日志
)

// sseHeartbeatInterval 是心跳注释的间隔，防止中间代理掐断空闲连接。
const sseHeartbeatInterval = 15 * time.Second

// sseClientBuffer 是每个订阅者的发送缓冲，满了就丢消息（慢客户端不拖累服务端）。
const sseClientBuffer = 64

// sseEventPrefix 是消息的前缀，便于测试和调试。
type sseMessage struct {
	event string
	data  []byte
}

// encode 把一条 SSE 消息编码成线上格式（data 里的换行必须逐行加前缀）。
func (m sseMessage) encode() []byte {
	var b strings.Builder
	b.Grow(len(m.data) + 32)
	b.WriteString("event: ")
	b.WriteString(m.event)
	b.WriteByte('\n')
	for _, line := range strings.Split(string(m.data), "\n") {
		b.WriteString("data: ")
		b.WriteString(line)
		b.WriteByte('\n')
	}
	b.WriteByte('\n')
	return []byte(b.String())
}

// hub 是 SSE 订阅者集合，负责把一条消息广播给所有在线浏览器。
type hub struct {
	mu     sync.RWMutex
	subs   map[int64]chan sseMessage
	nextID int64
}

func newHub() *hub {
	return &hub{subs: make(map[int64]chan sseMessage)}
}

// subscribe 注册一个新订阅者。
func (h *hub) subscribe() (int64, chan sseMessage) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.nextID++
	id := h.nextID
	ch := make(chan sseMessage, sseClientBuffer)
	h.subs[id] = ch
	return id, ch
}

// unsubscribe 注销订阅者并关闭通道。
func (h *hub) unsubscribe(id int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if ch, ok := h.subs[id]; ok {
		delete(h.subs, id)
		close(ch)
	}
}

// count 返回当前在线订阅数（测试与状态展示用）。
func (h *hub) count() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.subs)
}

// publish 把消息广播给所有订阅者；通道满则丢弃这一条。
func (h *hub) publish(event string, data []byte) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for _, ch := range h.subs {
		select {
		case ch <- sseMessage{event: event, data: data}:
		default:
			// 客户端太慢，丢掉这条消息，避免阻塞整个广播。
		}
	}
}

// SetStreamInterval 调整 SSE 轮询间隔（测试用；生产默认 2 秒）。
func (s *Server) SetStreamInterval(d time.Duration) {
	if d <= 0 {
		d = defaultStreamInterval
	}
	atomic.StoreInt64(&s.intervalNanos, int64(d))
}

// streamInterval 读取当前 SSE 轮询间隔。
func (s *Server) streamInterval() time.Duration {
	return time.Duration(atomic.LoadInt64(&s.intervalNanos))
}

// initSSEState 记录初始状态，避免连接建立后第一条增量把快照内容重复推一遍。
func (s *Server) initSSEState() {
	if b, err := json.Marshal(s.safeStatus()); err == nil {
		s.lastStatus = string(b)
	}
	if b, err := json.Marshal(sortedClients(s.cfg.PXE.Clients())); err == nil {
		s.lastClients = string(b)
	}
	// 记下当前最新的一条事件作为增量游标（没有事件时游标为空）。
	events := s.cfg.PXE.Events(1)
	if len(events) > 0 {
		s.lastEvent = events[len(events)-1]
		s.hasLastEvent = true
	}
}

// pollLoop 周期性比对状态并推送增量，直到服务器关闭。
func (s *Server) pollLoop() {
	for {
		timer := time.NewTimer(s.streamInterval())
		select {
		case <-s.done:
			timer.Stop()
			return
		case <-timer.C:
			s.pushUpdate()
		}
	}
}

// pushUpdate 比对一次状态，把变化广播出去。测试里可以直接调用它。
func (s *Server) pushUpdate() {
	// 1) 服务器状态
	if b, err := json.Marshal(s.cfg.PXE.Status()); err == nil {
		if string(b) != s.lastStatus {
			s.lastStatus = string(b)
			s.hub.publish(sseEventStatus, b)
		}
	}

	// 2) 客户机列表（排序后比较，避免 map 遍历顺序导致误判变化）
	clients := sortedClients(s.cfg.PXE.Clients())
	if b, err := json.Marshal(clients); err == nil {
		if string(b) != s.lastClients {
			s.lastClients = string(b)
			s.hub.publish(sseEventClients, b)
		}
	}

	// 3) 新增日志
	events := s.cfg.PXE.Events(200)
	fresh := s.newEvents(events)
	if len(fresh) > 0 {
		if b, err := json.Marshal(fresh); err == nil {
			s.hub.publish(sseEventEvents, b)
		}
	}
}

// newEvents 从最近事件里挑出上次推送之后产生的部分，并推进游标。
// 用「上一条已推送事件」做锚点而不是只比时间戳，这样同一毫秒内的多条事件也不会丢。
func (s *Server) newEvents(events []model.Event) []model.Event {
	if len(events) == 0 {
		return nil
	}
	idx := -1
	for i := len(events) - 1; i >= 0; i-- {
		if s.hasLastEvent && sameEvent(events[i], s.lastEvent) {
			idx = i
			break
		}
	}

	var fresh []model.Event
	switch {
	case idx >= 0:
		fresh = events[idx+1:]
	case !s.hasLastEvent:
		// 从未推送过任何事件：全部都是新的。
		fresh = events
	default:
		// 环形缓冲已经滚过了游标，保守地把当前可见的事件都推一遍。
		fresh = events
	}
	if len(fresh) == 0 {
		return nil
	}
	// 防御：一次最多推 maxEventLimit 条，避免慢客户端把内存撑爆。
	if len(fresh) > maxEventLimit {
		fresh = fresh[len(fresh)-maxEventLimit:]
	}
	s.lastEvent = fresh[len(fresh)-1]
	s.hasLastEvent = true
	// 按时间正序输出（时间相同时保持原顺序），前端直接 append 即可。
	sort.SliceStable(fresh, func(i, j int) bool { return fresh[i].Time.Before(fresh[j].Time) })
	return fresh
}

// sameEvent 判断两条事件是否是同一条（时间 + 类型 + 内容）。
func sameEvent(a, b model.Event) bool {
	return a.Time.Equal(b.Time) && a.Kind == b.Kind && a.Msg == b.Msg && a.MAC == b.MAC && a.Level == b.Level
}

// sortedClients 复制一份客户机列表并按最近活跃时间倒序。
func sortedClients(clients []model.Client) []model.Client {
	out := make([]model.Client, len(clients))
	copy(out, clients)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].LastSeen.Equal(out[j].LastSeen) {
			return out[i].MAC < out[j].MAC
		}
		return out[i].LastSeen.After(out[j].LastSeen)
	})
	return out
}

// snapshotPayload 是 SSE 连接建立时下发的一次性全量数据。
func (s *Server) snapshotPayload() ([]byte, error) {
	st := s.cfg.PXE.Status()
	if st.Version == "" {
		st.Version = s.version
	}
	return json.Marshal(map[string]any{
		"status":     st,
		"clients":    sortedClients(s.cfg.PXE.Clients()),
		"isos":       s.cfg.PXE.ISOs(),
		"events":     s.cfg.PXE.Events(defaultEventLimit),
		"defaultISO": s.cfg.PXE.DefaultISO(),
		"interfaces": s.cfg.PXE.Interfaces(),
		"version":    s.version,
		"serverTime": time.Now(),
	})
}

// handleStream 是 SSE 长连接：先发全量快照，再持续推送增量。
func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		methodNotAllowed(w, r, http.MethodGet)
		return
	}

	// SSE 必须能逐条 flush；拿不到 Flusher 就没法工作。
	rc := http.NewResponseController(w)
	if _, ok := w.(http.Flusher); !ok {
		writeErr(w, http.StatusInternalServerError, "当前服务器不支持流式响应（缺少 http.Flusher）")
		return
	}

	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache, no-transform")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no") // 告诉 nginx 不要缓冲
	w.WriteHeader(http.StatusOK)

	// 建议浏览器断线 3 秒后重连。
	if _, err := io.WriteString(w, "retry: 3000\n\n"); err != nil {
		return
	}

	// 先发全量快照，让新打开的页面立刻有数据。
	if payload, err := s.snapshotPayload(); err == nil {
		if _, err := w.Write(sseMessage{event: sseEventSnapshot, data: payload}.encode()); err != nil {
			return
		}
	} else {
		if _, err := w.Write(sseMessage{event: sseEventStatus, data: []byte(`{"error":"快照生成失败"}`)}.encode()); err != nil {
			return
		}
	}
	if err := rc.Flush(); err != nil {
		return
	}

	id, ch := s.hub.subscribe()
	defer s.hub.unsubscribe(id)

	heartbeat := time.NewTicker(sseHeartbeatInterval)
	defer heartbeat.Stop()

	for {
		select {
		case <-r.Context().Done():
			// 客户端断开
			return
		case <-s.done:
			// 服务器关闭
			return
		case msg, ok := <-ch:
			if !ok {
				return
			}
			if _, err := w.Write(msg.encode()); err != nil {
				return
			}
			if err := rc.Flush(); err != nil {
				return
			}
		case <-heartbeat.C:
			if _, err := io.WriteString(w, ": ping\n\n"); err != nil {
				return
			}
			if err := rc.Flush(); err != nil {
				return
			}
		}
	}
}
