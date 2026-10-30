package dhcp

import (
	"fmt"
	"net"
	"sort"
	"sync"
	"time"

	"github.com/wupengcheng/netbootx/internal/model"
)

// Lease 是租约表中的一条记录。
type Lease struct {
	MAC      string     `json:"mac"`
	IP       string     `json:"ip"`
	Arch     model.Arch `json:"arch"`
	Hostname string     `json:"hostname"`
	Expires  time.Time  `json:"expires"`
	State    string     `json:"state"` // offered | bound
}

// 租约状态。
const (
	LeaseStateOffered = "offered"
	LeaseStateBound   = "bound"
)

// LeaseTable 是并发安全的租约表。
//
// 支持两种分配策略：
//   - 顺序扫描（默认）：从 IP 池起点开始找第一个空闲地址，行为可预期、方便排查；
//   - 哈希起步（HashStart）：按 MAC 的哈希在池内找一个起点，多台机器同时 PXE 时
//     不容易抢同一地址，减少 DECLINE。
type LeaseTable struct {
	mu        sync.RWMutex
	start     uint32 // IP 池起始（含）
	end       uint32 // IP 池结束（含）
	leaseTime time.Duration
	hashStart bool
	byMAC     map[string]*Lease
	byIP      map[uint32]*Lease
	now       func() time.Time // 便于测试注入时钟
}

// NewLeaseTable 创建一个租约表。start / end 是 IP 池的起止地址（含两端）。
func NewLeaseTable(start, end net.IP, leaseTime time.Duration) (*LeaseTable, error) {
	s, ok1 := ipToUint32(start)
	e, ok2 := ipToUint32(end)
	if !ok1 || !ok2 {
		return nil, fmt.Errorf("dhcp: IP 池地址非法（%v - %v）", start, end)
	}
	if s > e {
		return nil, fmt.Errorf("dhcp: IP 池起始地址 %v 大于结束地址 %v", start, end)
	}
	if leaseTime <= 0 {
		leaseTime = time.Hour
	}
	return &LeaseTable{
		start:     s,
		end:       e,
		leaseTime: leaseTime,
		byMAC:     make(map[string]*Lease),
		byIP:      make(map[uint32]*Lease),
		now:       time.Now,
	}, nil
}

// SetHashStart 设置是否按 MAC 哈希决定扫描起点。
func (t *LeaseTable) SetHashStart(v bool) { t.hashStart = v }

// Size 返回 IP 池容量。
func (t *LeaseTable) Size() int {
	if t == nil {
		return 0
	}
	return int(t.end-t.start) + 1
}

// Start / End 返回 IP 池边界。
func (t *LeaseTable) Start() net.IP { return uint32ToIP(t.start) }
func (t *LeaseTable) End() net.IP   { return uint32ToIP(t.end) }

// InPool 报告某个 IP 是否落在 IP 池内。
func (t *LeaseTable) InPool(ip net.IP) bool {
	v, ok := ipToUint32(ip)
	if !ok {
		return false
	}
	return v >= t.start && v <= t.end
}

// Get 按 MAC 查询有效租约。
func (t *LeaseTable) Get(mac string) (Lease, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.expireLocked()
	l, ok := t.byMAC[mac]
	if !ok {
		return Lease{}, false
	}
	return *l, true
}

// ByIP 按 IP 查询有效租约。
func (t *LeaseTable) ByIP(ip net.IP) (Lease, bool) {
	v, ok := ipToUint32(ip)
	if !ok {
		return Lease{}, false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.expireLocked()
	l, ok := t.byIP[v]
	if !ok {
		return Lease{}, false
	}
	return *l, true
}

// All 返回全部有效租约，按 IP 升序排列。
func (t *LeaseTable) All() []Lease {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.expireLocked()
	out := make([]Lease, 0, len(t.byMAC))
	for _, l := range t.byMAC {
		out = append(out, *l)
	}
	sort.Slice(out, func(i, j int) bool {
		a, _ := ipToUint32(net.ParseIP(out[i].IP))
		b, _ := ipToUint32(net.ParseIP(out[j].IP))
		return a < b
	})
	return out
}

// Offered 记录一个「已发出 OFFER 但客户端还没 REQUEST」的临时租约。
func (t *LeaseTable) Offered(mac string, ip net.IP, arch model.Arch, hostname string) error {
	v, ok := ipToUint32(ip)
	if !ok {
		return fmt.Errorf("dhcp: 非法 IP %v", ip)
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.expireLocked()
	// 同一 MAC 换地址时先释放旧记录。
	if old, ok := t.byMAC[mac]; ok && old.IP != uint32ToIP(v).String() {
		delete(t.byIP, mustIPToUint32(old.IP))
		delete(t.byMAC, mac)
	}
	if cur, ok := t.byIP[v]; ok && cur.MAC != mac {
		return fmt.Errorf("dhcp: IP %v 已被 %s 占用", ip, cur.MAC)
	}
	l := &Lease{
		MAC:      mac,
		IP:       uint32ToIP(v).String(),
		Arch:     arch,
		Hostname: hostname,
		Expires:  t.now().Add(t.leaseTime),
		State:    LeaseStateOffered,
	}
	t.byMAC[mac] = l
	t.byIP[v] = l
	return nil
}

// Bind 把租约标记为已绑定（收到 REQUEST 后调用）。租约不存在时直接创建。
func (t *LeaseTable) Bind(mac string, ip net.IP, arch model.Arch, hostname string) error {
	v, ok := ipToUint32(ip)
	if !ok {
		return fmt.Errorf("dhcp: 非法 IP %v", ip)
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.expireLocked()
	if cur, ok := t.byIP[v]; ok && cur.MAC != mac {
		return fmt.Errorf("dhcp: IP %v 已被 %s 占用", ip, cur.MAC)
	}
	if old, ok := t.byMAC[mac]; ok {
		if v2 := mustIPToUint32(old.IP); v2 != v {
			delete(t.byIP, v2)
		}
	}
	l := &Lease{
		MAC:      mac,
		IP:       uint32ToIP(v).String(),
		Arch:     arch,
		Hostname: hostname,
		Expires:  t.now().Add(t.leaseTime),
		State:    LeaseStateBound,
	}
	t.byMAC[mac] = l
	t.byIP[v] = l
	return nil
}

// Release 释放某个 MAC 的租约（客户端主动 RELEASE）。
func (t *LeaseTable) Release(mac string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	l, ok := t.byMAC[mac]
	if !ok {
		return false
	}
	delete(t.byIP, mustIPToUint32(l.IP))
	delete(t.byMAC, mac)
	return true
}

// Delete 按 IP 删除租约。
func (t *LeaseTable) Delete(ip net.IP) bool {
	v, ok := ipToUint32(ip)
	if !ok {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	l, ok := t.byIP[v]
	if !ok {
		return false
	}
	delete(t.byMAC, l.MAC)
	delete(t.byIP, v)
	return true
}

// Reset 清空租约表（DHCP DECLINE 后用于重新分配）。
func (t *LeaseTable) Reset() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.byMAC = make(map[string]*Lease)
	t.byIP = make(map[uint32]*Lease)
}

// Allocate 为某个 MAC 分配一个 IP：优先复用已有租约，否则在池中找空闲地址。
func (t *LeaseTable) Allocate(mac string) (net.IP, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.expireLocked()
	if l, ok := t.byMAC[mac]; ok {
		return net.ParseIP(l.IP).To4(), nil
	}
	size := int(t.end-t.start) + 1
	offset := 0
	if t.hashStart && size > 0 {
		offset = int(hashMAC(mac) % uint32(size))
	}
	for i := 0; i < size; i++ {
		v := t.start + uint32((offset+i)%size)
		if _, used := t.byIP[v]; used {
			continue
		}
		// 立即登记为临时租约，避免两个并发 DISCOVER 拿到同一个地址。
		l := &Lease{
			MAC:     mac,
			IP:      uint32ToIP(v).String(),
			Arch:    model.ArchUnknown,
			Expires: t.now().Add(t.leaseTime),
			State:   LeaseStateOffered,
		}
		t.byMAC[mac] = l
		t.byIP[v] = l
		return uint32ToIP(v), nil
	}
	return nil, fmt.Errorf("dhcp: IP 池 %v - %v 已耗尽（共 %d 个地址）",
		uint32ToIP(t.start), uint32ToIP(t.end), size)
}

// expireLocked 清理过期租约，调用方必须持有写锁。
func (t *LeaseTable) expireLocked() {
	if t.now == nil {
		return
	}
	now := t.now()
	for mac, l := range t.byMAC {
		if now.After(l.Expires) {
			delete(t.byIP, mustIPToUint32(l.IP))
			delete(t.byMAC, mac)
		}
	}
}

// hashMAC 是 FNV-1a 的简化版本，用于把 MAC 映射到 IP 池内偏移。
func hashMAC(mac string) uint32 {
	var h uint32 = 2166136261
	for i := 0; i < len(mac); i++ {
		h ^= uint32(mac[i])
		h *= 16777619
	}
	return h
}

// ipToUint32 把 IPv4 转成 uint32。
func ipToUint32(ip net.IP) (uint32, bool) {
	v4 := ip.To4()
	if v4 == nil {
		return 0, false
	}
	return uint32(v4[0])<<24 | uint32(v4[1])<<16 | uint32(v4[2])<<8 | uint32(v4[3]), true
}

// mustIPToUint32 用于内部已确认合法的地址。
func mustIPToUint32(s string) uint32 {
	v, _ := ipToUint32(net.ParseIP(s))
	return v
}

// uint32ToIP 把 uint32 转回 4 字节 IPv4。
func uint32ToIP(v uint32) net.IP {
	return net.IPv4(byte(v>>24), byte(v>>16), byte(v>>8), byte(v)).To4()
}
