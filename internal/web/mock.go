package web

import (
	"errors"
	"sync"
	"time"

	"github.com/wupengcheng/netbootx/internal/model"
)

// Mock 是 Controller 的测试替身：内存态、可注入错误、可断言调用次数。
// 它只实现 Controller 契约本身；可选的扩展能力在 FullMock 上。
type Mock struct {
	mu sync.Mutex

	status     model.ServerStatus
	isos       []model.ISO
	clients    []model.Client
	events     []model.Event
	settings   model.Settings
	ifaces     []NetInterface
	defaultISO string

	// 可注入的错误，用于覆盖各接口的失败路径。
	StartErr         error
	StopErr          error
	RestartErr       error
	RescanErr        error
	SaveSettingsErr  error
	SetDefaultISOErr error

	// RescanCount 是 RescanISOs 返回的镜像数量。
	RescanCount int

	// 调用计数，便于断言副作用。
	StartCalls         int
	StopCalls          int
	RestartCalls       int
	RescanCalls        int
	SaveSettingsCalls  int
	SetDefaultISOCalls int

	// LastSaved 记录最后一次保存的配置。
	LastSaved model.Settings
}

// NewMock 返回一个带示例数据、可直接用于测试的 Mock。
func NewMock() *Mock {
	m := &Mock{
		settings: model.DefaultSettings(),
	}
	m.settings.ServerIP = "192.168.123.10"
	m.settings.DHCPMode = "proxy"
	m.status = model.ServerStatus{
		Running:      false,
		ServerIP:     "192.168.123.10",
		DHCPMode:     "proxy",
		HTTPPort:     16000,
		TFTPPort:     69,
		WebPort:      16080,
		Version:      "0.1.0-test",
		Hostname:     "netbootx-test",
		OS:           "linux",
		Arch:         "amd64",
		BootFileBIOS: "undionly.kpxe",
		BootFileUEFI: "ipxe.efi",
	}
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	m.isos = []model.ISO{
		{
			ID:       "iso-ubuntu",
			Path:     "/srv/iso/ubuntu-24.04.iso",
			RelPath:  "ubuntu-24.04.iso",
			Name:     "ubuntu-24.04.iso",
			Size:     3_200_000_000,
			ModTime:  now,
			Family:   model.FamilyLinux,
			OSName:   "Ubuntu 24.04 LTS",
			OSArch:   model.ArchUEFI64,
			Bootable: true,
			Boot: model.BootSpec{
				Method:     "kernel",
				Kernel:     "/casper/vmlinuz",
				Initrd:     []string{"/casper/initrd"},
				KernelArgs: []string{"boot=casper", "iso-scan/filename={iso}"},
			},
		},
		{
			ID:       "iso-win11",
			Path:     "/srv/iso/win11.iso",
			RelPath:  "win11.iso",
			Name:     "win11.iso",
			Size:     5_400_000_000,
			ModTime:  now.Add(time.Hour),
			Family:   model.FamilyWindows,
			OSName:   "Windows 11",
			OSArch:   model.ArchUEFI64,
			Bootable: true,
			Boot: model.BootSpec{
				Method:  "wimboot",
				BootMGR: "/bootmgr",
				BCD:     "/boot/bcd",
				BootSDI: "/boot/boot.sdi",
				BootWIM: "/sources/boot.wim",
			},
		},
	}
	m.clients = []model.Client{
		{
			MAC:       "aa:bb:cc:dd:ee:01",
			IP:        "192.168.123.201",
			Hostname:  "lab-node-1",
			Arch:      model.ArchUEFI64,
			Vendor:    "Intel",
			Product:   "OptiPlex 7080",
			Serial:    "SN0001",
			State:     model.StateBooting,
			Selected:  "iso-ubuntu",
			BootFile:  "ipxe.efi",
			BootCount: 2,
			FirstSeen: now,
			LastSeen:  now.Add(2 * time.Minute),
		},
		{
			MAC:       "aa:bb:cc:dd:ee:02",
			IP:        "192.168.123.202",
			Hostname:  "lab-node-2",
			Arch:      model.ArchBIOS,
			Vendor:    "Realtek",
			State:     model.StateDHCP,
			BootCount: 1,
			FirstSeen: now.Add(time.Minute),
			LastSeen:  now.Add(time.Minute),
		},
	}
	m.events = []model.Event{
		{Time: now, Level: "info", Kind: "system", Msg: "服务已初始化"},
		{Time: now.Add(time.Second), Level: "info", Kind: "dhcp", MAC: "aa:bb:cc:dd:ee:01", Msg: "DHCP DISCOVER 已处理"},
	}
	m.ifaces = []NetInterface{
		{Name: "en0", IP: "192.168.123.10", Mask: "255.255.255.0", Up: true, MAC: "aa:bb:cc:00:11:22", Default: true},
		{Name: "en1", IP: "10.0.0.5", Mask: "255.255.255.0", Up: false, MAC: "aa:bb:cc:00:11:23"},
	}
	m.defaultISO = "iso-ubuntu"
	m.RescanCount = len(m.isos)
	return m
}

// Status 返回当前状态快照。
func (m *Mock) Status() model.ServerStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := m.status
	st.ClientCount = len(m.clients)
	st.ISOCount = len(m.isos)
	if st.Running && st.StartedAt.IsZero() {
		st.StartedAt = time.Now()
	}
	if st.Running {
		st.UptimeSec = int64(time.Since(st.StartedAt).Seconds())
	}
	return st
}

// Start 把状态切到运行中。
func (m *Mock) Start() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.StartCalls++
	if m.StartErr != nil {
		return m.StartErr
	}
	if m.status.Running {
		return errors.New("服务已经在运行")
	}
	m.status.Running = true
	m.status.StartedAt = time.Now()
	m.appendEventLocked(model.Event{Time: time.Now(), Level: "info", Kind: "system", Msg: "服务已启动"})
	return nil
}

// Stop 把状态切到已停止。
func (m *Mock) Stop() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.StopCalls++
	if m.StopErr != nil {
		return m.StopErr
	}
	m.status.Running = false
	m.appendEventLocked(model.Event{Time: time.Now(), Level: "info", Kind: "system", Msg: "服务已停止"})
	return nil
}

// Restart 先停后启。
func (m *Mock) Restart() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.RestartCalls++
	if m.RestartErr != nil {
		return m.RestartErr
	}
	m.status.Running = true
	m.status.StartedAt = time.Now()
	m.appendEventLocked(model.Event{Time: time.Now(), Level: "info", Kind: "system", Msg: "服务已重启"})
	return nil
}

// RescanISOs 返回预设的扫描数量。
func (m *Mock) RescanISOs() (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.RescanCalls++
	if m.RescanErr != nil {
		return 0, m.RescanErr
	}
	if m.RescanCount == 0 {
		m.RescanCount = len(m.isos)
	}
	return m.RescanCount, nil
}

// ISOs 返回 ISO 列表的副本。
func (m *Mock) ISOs() []model.ISO {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]model.ISO, len(m.isos))
	copy(out, m.isos)
	return out
}

// Clients 返回客户机列表的副本。
func (m *Mock) Clients() []model.Client {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]model.Client, len(m.clients))
	copy(out, m.clients)
	return out
}

// Events 返回最近 limit 条事件（按时间正序）。
func (m *Mock) Events(limit int) []model.Event {
	m.mu.Lock()
	defer m.mu.Unlock()
	if limit <= 0 || limit > len(m.events) {
		limit = len(m.events)
	}
	out := make([]model.Event, limit)
	copy(out, m.events[len(m.events)-limit:])
	return out
}

// Settings 返回当前配置。
func (m *Mock) Settings() model.Settings {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.settings
	s.DNS = append([]string(nil), m.settings.DNS...)
	return s
}

// SaveSettings 保存配置并记录调用。
func (m *Mock) SaveSettings(s model.Settings) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.SaveSettingsCalls++
	if m.SaveSettingsErr != nil {
		return m.SaveSettingsErr
	}
	m.LastSaved = s
	m.settings = s
	return nil
}

// Interfaces 返回网卡列表。
func (m *Mock) Interfaces() []NetInterface {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]NetInterface, len(m.ifaces))
	copy(out, m.ifaces)
	return out
}

// DefaultISO 返回默认 ISO ID。
func (m *Mock) DefaultISO() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.defaultISO
}

// SetDefaultISO 设置默认 ISO。
func (m *Mock) SetDefaultISO(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.SetDefaultISOCalls++
	if m.SetDefaultISOErr != nil {
		return m.SetDefaultISOErr
	}
	m.defaultISO = id
	return nil
}

// AddClient 追加一台客户机（测试用）。
func (m *Mock) AddClient(c model.Client) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.clients = append(m.clients, c)
}

// AddEvent 追加一条事件（测试用）。
func (m *Mock) AddEvent(ev model.Event) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.appendEventLocked(ev)
}

// SetRunning 直接设置运行状态（测试用）。
func (m *Mock) SetRunning(v bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.status.Running = v
	if v {
		m.status.StartedAt = time.Now()
	}
}

func (m *Mock) appendEventLocked(ev model.Event) {
	m.events = append(m.events, ev)
	if len(m.events) > 500 {
		m.events = m.events[len(m.events)-500:]
	}
}

// FullMock 在 Mock 的基础上实现了 web 层的可选扩展接口
// （ClientClearer / Previewer），用来模拟完整的 pxe 实现。
type FullMock struct {
	*Mock

	// 可注入的错误。
	ClearClientsErr error
	PreviewErr      error
	// PreviewScript 是 Preview 返回的脚本；留空时 web 层会退回本地预览。
	PreviewScript string

	// 调用记录。
	ClearClientsCalls int
	PreviewCalls      int
	LastPreviewID     string
	LastPreviewMAC    string
}

// NewFullMock 返回一个带示例数据的完整 Mock。
func NewFullMock() *FullMock {
	return &FullMock{Mock: NewMock()}
}

// ClearClients 清空客户机列表。
func (f *FullMock) ClearClients() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ClearClientsCalls++
	if f.ClearClientsErr != nil {
		return f.ClearClientsErr
	}
	f.clients = nil
	return nil
}

// Preview 返回预设的 iPXE 脚本；未预设时返回空串，让 web 层走本地预览。
func (f *FullMock) Preview(isoID, mac string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.PreviewCalls++
	f.LastPreviewID = isoID
	f.LastPreviewMAC = mac
	if f.PreviewErr != nil {
		return "", f.PreviewErr
	}
	if f.PreviewScript != "" {
		return f.PreviewScript, nil
	}
	return "", nil
}

// MockStore 是 Store 接口的测试替身，记录 web 层写入的事件。
type MockStore struct {
	mu     sync.Mutex
	events []model.Event
}

// AddEvent 实现 Store 接口。
func (s *MockStore) AddEvent(ev model.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, ev)
}

// Events 返回记录到的事件。
func (s *MockStore) Events() []model.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]model.Event, len(s.events))
	copy(out, s.events)
	return out
}

// Count 返回事件条数。
func (s *MockStore) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.events)
}

// 编译期断言：确保 mock 满足接口契约。
var (
	_ Controller    = (*Mock)(nil)
	_ Controller    = (*FullMock)(nil)
	_ ClientClearer = (*FullMock)(nil)
	_ Previewer     = (*FullMock)(nil)
	_ Store         = (*MockStore)(nil)
)
