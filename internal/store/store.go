// Package store 负责 NetBootX 的全部持久化状态：
//   - 配置（data/settings.json）
//   - 客户机表（内存，可选落盘）
//   - 事件环形缓冲（内存）
//   - 内嵌引导文件的定位
//
// 所有方法并发安全。上层（pxe / web）通过本包读写状态，不直接碰文件。
package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/wupengcheng/netbootx/internal/model"
)

// EventCap 是事件环形缓冲的容量。界面只显示最近若干条，多了就丢最旧的。
const EventCap = 2000

// Store 是全局状态容器。
type Store struct {
	mu       sync.RWMutex
	settings model.Settings
	settingsPath string

	clients map[string]*model.Client // key: 归一化 MAC
	events  []model.Event
	evStart int // 环形缓冲起点

	// 引导文件目录（内嵌资源解压后的位置，或磁盘上的 boot/）
	bootDir string
}

// New 创建一个 Store。dataDir 用于存放 settings.json。
func New(dataDir, bootDir string) (*Store, error) {
	s := &Store{
		settings:     model.DefaultSettings(),
		settingsPath: filepath.Join(dataDir, "settings.json"),
		clients:      make(map[string]*model.Client),
		bootDir:      bootDir,
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, fmt.Errorf("创建数据目录 %s: %w", dataDir, err)
	}
	if err := s.Load(); err != nil {
		// 配置文件损坏不应导致启动失败，回退到默认配置并记录
		s.addEventLocked(model.Event{
			Time: time.Now(), Level: "warn", Kind: "system",
			Msg: "读取配置失败，已回退到默认配置: " + err.Error(),
		})
	}
	return s, nil
}

// Load 从磁盘读取配置。
func (s *Store) Load() error {
	b, err := os.ReadFile(s.settingsPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // 首次运行，用默认配置
		}
		return fmt.Errorf("读取 %s: %w", s.settingsPath, err)
	}
	var cfg model.Settings
	if err := json.Unmarshal(b, &cfg); err != nil {
		return fmt.Errorf("解析 %s: %w", s.settingsPath, err)
	}
	// 用默认值补齐缺失字段（版本升级时新增的字段）
	def := model.DefaultSettings()
	mergeDefaults(&cfg, &def)

	s.mu.Lock()
	s.settings = cfg
	s.mu.Unlock()
	return nil
}

// mergeDefaults 把 cfg 里为零值的字段用 def 补齐。
func mergeDefaults(cfg, def *model.Settings) {
	if cfg.Netmask == "" {
		cfg.Netmask = def.Netmask
	}
	if cfg.Domain == "" {
		cfg.Domain = def.Domain
	}
	if cfg.LeaseTime == 0 {
		cfg.LeaseTime = def.LeaseTime
	}
	if cfg.DHCPMode == "" {
		cfg.DHCPMode = def.DHCPMode
	}
	if cfg.DHCPPort == 0 {
		cfg.DHCPPort = def.DHCPPort
	}
	if cfg.HTTPPort == 0 {
		cfg.HTTPPort = def.HTTPPort
	}
	if cfg.TFTPPort == 0 {
		cfg.TFTPPort = def.TFTPPort
	}
	if cfg.WebPort == 0 {
		cfg.WebPort = def.WebPort
	}
	if cfg.ISODir == "" {
		cfg.ISODir = def.ISODir
	}
	if cfg.UserDir == "" {
		cfg.UserDir = def.UserDir
	}
	if cfg.DataDir == "" {
		cfg.DataDir = def.DataDir
	}
	if cfg.LogDir == "" {
		cfg.LogDir = def.LogDir
	}
	if cfg.BootMenuTimeout == 0 {
		cfg.BootMenuTimeout = def.BootMenuTimeout
	}
	if cfg.TFTPBlockSize == 0 {
		cfg.TFTPBlockSize = def.TFTPBlockSize
	}
	if cfg.Language == "" {
		cfg.Language = def.Language
	}
	if len(cfg.DNS) == 0 {
		cfg.DNS = def.DNS
	}
}

// Save 把配置写回磁盘（先写临时文件再 rename，避免半截文件）。
func (s *Store) Save() error {
	s.mu.RLock()
	cfg := s.settings
	path := s.settingsPath
	s.mu.RUnlock()

	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化配置: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return fmt.Errorf("写入 %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("替换 %s: %w", path, err)
	}
	return nil
}

// Settings 返回当前配置的副本。
func (s *Store) Settings() model.Settings {
	s.mu.RLock()
	defer s.mu.RUnlock()
	c := s.settings
	c.DNS = append([]string(nil), s.settings.DNS...)
	return c
}

// SaveSettings 校验并保存新配置。
func (s *Store) SaveSettings(cfg model.Settings) error {
	if err := ValidateSettings(&cfg); err != nil {
		return err
	}
	s.mu.Lock()
	s.settings = cfg
	s.mu.Unlock()
	return s.Save()
}

// ValidateSettings 检查配置的合法性。返回的错误可直接展示给用户。
func ValidateSettings(c *model.Settings) error {
	if c.DHCPMode != "full" && c.DHCPMode != "proxy" && c.DHCPMode != "external" {
		return fmt.Errorf("DHCP 模式必须是 full / proxy / external，当前是 %q", c.DHCPMode)
	}
	if c.HTTPPort < 1 || c.HTTPPort > 65535 {
		return fmt.Errorf("HTTP 端口 %d 超出范围 1-65535", c.HTTPPort)
	}
	if c.TFTPPort < 1 || c.TFTPPort > 65535 {
		return fmt.Errorf("TFTP 端口 %d 超出范围 1-65535", c.TFTPPort)
	}
	if c.WebPort < 1 || c.WebPort > 65535 {
		return fmt.Errorf("Web 端口 %d 超出范围 1-65535", c.WebPort)
	}
	if c.HTTPPort == c.WebPort {
		return fmt.Errorf("HTTP 端口和 Web 端口不能相同（都是 %d）", c.HTTPPort)
	}
	if c.DHCPMode == "full" || c.DHCPMode == "proxy" {
		if c.ServerIP == "" {
			return fmt.Errorf("DHCP 模式 %s 需要指定本机 IP", c.DHCPMode)
		}
	}
	if c.DHCPMode == "full" {
		if c.RangeStart == "" || c.RangeEnd == "" {
			return fmt.Errorf("full 模式需要指定 IP 地址池起止")
		}
	}
	if c.LeaseTime < 60 {
		return fmt.Errorf("租约时间不能小于 60 秒")
	}
	if c.BootMenuTimeout < 0 {
		return fmt.Errorf("启动菜单超时不能为负数")
	}
	return nil
}

// ---- 客户机 ----

// UpsertClient 插入或更新一台客户机，返回更新后的副本。
// 由 DHCP 层在收到报文时调用。
func (s *Store) UpsertClient(c model.Client) model.Client {
	mac := model.NormalizeMAC(c.MAC)
	now := time.Now()

	s.mu.Lock()
	defer s.mu.Unlock()

	old, ok := s.clients[mac]
	if !ok {
		nc := c
		nc.MAC = mac
		nc.FirstSeen = now
		nc.LastSeen = now
		s.clients[mac] = &nc
		s.addEventLocked(model.Event{
			Time: now, Level: "info", Kind: "dhcp", MAC: mac,
			Msg: "发现新客户机 " + mac + " (" + nc.Arch.String() + ")",
		})
		return nc
	}
	// 已有记录：只更新非空字段
	if c.IP != "" {
		old.IP = c.IP
	}
	if c.Hostname != "" {
		old.Hostname = c.Hostname
	}
	if c.Arch != "" && c.Arch != model.ArchUnknown {
		old.Arch = c.Arch
	}
	if c.Product != "" {
		old.Product = c.Product
	}
	if c.Serial != "" {
		old.Serial = c.Serial
	}
	if c.UUID != "" {
		old.UUID = c.UUID
	}
	if c.Vendor != "" {
		old.Vendor = c.Vendor
	}
	if c.State != "" {
		old.State = c.State
	}
	if c.Selected != "" {
		old.Selected = c.Selected
	}
	if c.BootFile != "" {
		old.BootFile = c.BootFile
	}
	if c.Note != "" {
		old.Note = c.Note
	}
	old.LastSeen = now
	return *old
}

// GetClient 按 MAC 取客户机。
func (s *Store) GetClient(mac string) (model.Client, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	c, ok := s.clients[model.NormalizeMAC(mac)]
	if !ok {
		return model.Client{}, false
	}
	return *c, true
}

// Clients 返回按最近活动时间倒序排列的客户机列表。
func (s *Store) Clients() []model.Client {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]model.Client, 0, len(s.clients))
	for _, c := range s.clients {
		out = append(out, *c)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].LastSeen.After(out[j].LastSeen)
	})
	return out
}

// ClearClients 清空客户机表。
func (s *Store) ClearClients() {
	s.mu.Lock()
	s.clients = make(map[string]*model.Client)
	s.mu.Unlock()
	s.AddEvent(model.Event{Level: "info", Kind: "system", Msg: "已清空客户机列表"})
}

// SetClientState 更新某台客户机的状态。
func (s *Store) SetClientState(mac string, st model.ClientState) {
	mac = model.NormalizeMAC(mac)
	s.mu.Lock()
	if c, ok := s.clients[mac]; ok {
		c.State = st
		c.LastSeen = time.Now()
	}
	s.mu.Unlock()
}

// ---- 事件 ----

// AddEvent 追加一条事件（线程安全，自动填时间）。
func (s *Store) AddEvent(e model.Event) {
	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	s.mu.Lock()
	s.addEventLocked(e)
	s.mu.Unlock()
}

// addEventLocked 必须在持有 s.mu 时调用。
func (s *Store) addEventLocked(e model.Event) {
	if len(s.events) < EventCap {
		s.events = append(s.events, e)
		return
	}
	// 环形覆盖
	s.events[s.evStart] = e
	s.evStart = (s.evStart + 1) % EventCap
}

// Events 返回最近 limit 条事件，按时间正序（最旧的在前）。
func (s *Store) Events(limit int) []model.Event {
	s.mu.RLock()
	defer s.mu.RUnlock()
	n := len(s.events)
	if n == 0 {
		return nil
	}
	// 展开环形缓冲
	ordered := make([]model.Event, 0, n)
	ordered = append(ordered, s.events[s.evStart:]...)
	ordered = append(ordered, s.events[:s.evStart]...)
	if limit > 0 && len(ordered) > limit {
		ordered = ordered[len(ordered)-limit:]
	}
	return ordered
}

// EventSeq 返回当前事件总数，用于 SSE 增量推送。
func (s *Store) EventSeq() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.events)
}

// ---- 引导文件 ----

// BootDir 返回引导文件所在目录。
func (s *Store) BootDir() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.bootDir
}

// BootFileFor 返回某架构客户机应该下发的引导文件名（相对 bootDir）。
// 这是 DHCP option 67 的内容。
func (s *Store) BootFileFor(a model.Arch) string {
	switch a {
	case model.ArchBIOS:
		return "bios/undionly.kpxe"
	case model.ArchUEFI32:
		return "i386/ipxe.efi"
	case model.ArchUEFI64:
		return "x86_64/ipxe.efi"
	case model.ArchUEFI64HTTP:
		// HTTPBoot 直接给 iPXE 的 EFI，客户机自己 HTTP 拉
		return "x86_64/ipxe.efi"
	case model.ArchARM64, model.ArchARM64HTTP:
		return "arm64/ipxe.efi"
	case model.ArchLoongArch64:
		return "loong64/ipxe.efi"
	}
	return "x86_64/ipxe.efi"
}

// BootFileExists 检查引导文件是否真的存在（下载失败时给出友好提示）。
func (s *Store) BootFileExists(rel string) bool {
	p := filepath.Join(s.BootDir(), filepath.FromSlash(rel))
	st, err := os.Stat(p)
	return err == nil && !st.IsDir() && st.Size() > 0
}

// MissingBootFiles 返回缺失的引导文件列表，供启动时自检。
func (s *Store) MissingBootFiles() []string {
	var missing []string
	need := []string{
		"bios/undionly.kpxe",
		"x86_64/ipxe.efi",
		"arm64/ipxe.efi",
		"wimboot",
	}
	for _, n := range need {
		if !s.BootFileExists(n) {
			missing = append(missing, n)
		}
	}
	return missing
}

// Paths 返回各个目录的绝对路径，并在需要时创建。
func (s *Store) EnsureDirs(root string) error {
	cfg := s.Settings()
	dirs := map[string]string{
		"ISO 目录":  cfg.ISODir,
		"用户目录":   cfg.UserDir,
		"数据目录":   cfg.DataDir,
		"日志目录":   cfg.LogDir,
	}
	for name, d := range dirs {
		if d == "" {
			continue
		}
		if !filepath.IsAbs(d) {
			d = filepath.Join(root, d)
		}
		if err := os.MkdirAll(d, 0o755); err != nil {
			return fmt.Errorf("创建%s %s: %w", name, d, err)
		}
	}
	return nil
}

// VendorFromMAC 由 MAC 的 OUI 猜网卡厂商，用于界面展示。
func VendorFromMAC(mac string) string {
	oui := strings.ToLower(strings.ReplaceAll(model.NormalizeMAC(mac), ":", ""))
	if len(oui) < 6 {
		return ""
	}
	if v, ok := ouiTable[oui[:6]]; ok {
		return v
	}
	return ""
}

// ouiTable 只收录常见网卡厂商，够界面展示用。
var ouiTable = map[string]string{
	"00155d": "Microsoft Hyper-V",
	"005056": "VMware",
	"000c29": "VMware",
	"000569": "VMware",
	"080027": "VirtualBox",
	"525400": "QEMU/KVM",
	"52540a": "QEMU/KVM",
	"001c42": "Parallels",
	"3c5282": "Intel",
	"001b21": "Intel",
	"002590": "Super Micro",
	"0cc47a": "Super Micro",
	"3c4a92": "HP",
	"001a4b": "HP",
	"b4b52f": "HP",
	"2c44fd": "HP",
	"5c8a38": "HP",
	"0018fe": "HP",
	"002128": "Oracle",
	"00144f": "Oracle/Sun",
	"000e1e": "Dell",
	"00219b": "Dell",
	"18a99b": "Dell",
	"b083fe": "Dell",
	"001124": "Dell",
	"00e04c": "Realtek",
	"52540": "QEMU/KVM",
	"001dd8": "Microsoft",
	"00ae1f": "Azure",
	"6045bd": "Microsoft",
	"0003ff": "Microsoft",
	"7c4d8f": "Cisco",
	"001a30": "Cisco",
	"0050f2": "Microsoft",
	"a4badb": "Intel",
	"94c691": "Intel",
	"3cecef": "Supermicro",
	"ac1f6b": "Supermicro",
}
