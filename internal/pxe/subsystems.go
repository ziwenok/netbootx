package pxe

import (
	"fmt"
	"net"
	"os/exec"
	"sync"

	"github.com/wupengcheng/netbootx/internal/dhcp"
	"github.com/wupengcheng/netbootx/internal/httpd"
	"github.com/wupengcheng/netbootx/internal/iso"
	"github.com/wupengcheng/netbootx/internal/model"
	"github.com/wupengcheng/netbootx/internal/store"
	"github.com/wupengcheng/netbootx/internal/tftp"
	"github.com/wupengcheng/netbootx/internal/web"
)

// BuildSubsystems 用真实的 dhcp/tftp/httpd/iso 实现组装出 Subsystems。
// main 调用它；测试里可以不用它，直接塞假实现。
func BuildSubsystems(st *store.Store) Subsystems {
	var (
		dhcpSrv *dhcp.Server
		tftpSrv *tftp.Server
		httpSrv *httpd.Server
		mu      sync.Mutex
	)

	ev := func(e model.Event) { st.AddEvent(e) }

	return Subsystems{
		StartDHCP: func(cfg model.Settings, bootFileFor func(model.Arch) string) error {
			mu.Lock()
			defer mu.Unlock()
			if cfg.DHCPMode == "external" {
				// external 模式：不监听 DHCP，只靠外部 DHCP 通告本机为 next-server
				st.AddEvent(model.Event{Level: "info", Kind: "dhcp",
					Msg: "external 模式：不启动 DHCP 服务，请确保外部 DHCP 通告 next-server = " + cfg.ServerIP})
				return nil
			}
			s, err := dhcp.New(dhcp.Config{
				Mode:        cfg.DHCPMode,
				ServerIP:    cfg.ServerIP,
				RangeStart:  cfg.RangeStart,
				RangeEnd:    cfg.RangeEnd,
				Netmask:     cfg.Netmask,
				Gateway:     cfg.Gateway,
				DNS:         cfg.DNS,
				Domain:      cfg.Domain,
				LeaseTime:   cfg.LeaseTime,
				Port:        cfg.DHCPPort,
				// BindIP 只表示「对外通告哪个 IP」，dhcp 包内部会强制监听 0.0.0.0
				// （客户机的 DISCOVER 是广播包，绑具体 IP 收不到广播）。
				BindIP:      cfg.ServerIP,
				BootFileFor: bootFileFor,
				OnEvent:     ev,
				OnClient: func(c *model.Client) {
					c.Vendor = store.VendorFromMAC(c.MAC)
					st.UpsertClient(*c)
				},
			})
			if err != nil {
				return err
			}
			if err := s.Start(); err != nil {
				return err
			}
			dhcpSrv = s
			return nil
		},

		StopDHCP: func() error {
			mu.Lock()
			defer mu.Unlock()
			if dhcpSrv == nil {
				return nil
			}
			err := dhcpSrv.Stop()
			dhcpSrv = nil
			return err
		},

		DHCPInfo: func() string {
			mu.Lock()
			defer mu.Unlock()
			if dhcpSrv == nil {
				return "未启动"
			}
			le := dhcpSrv.Leases()
			return fmt.Sprintf("运行中，%d 条租约", len(le))
		},

		StartTFTP: func(cfg model.Settings, root string) error {
			mu.Lock()
			defer mu.Unlock()
			s, err := tftp.New(tftp.Config{
				Root:      root,
				Addr:      fmt.Sprintf(":%d", cfg.TFTPPort),
				BlockSize: cfg.TFTPBlockSize,
				OnEvent:   ev,
			})
			if err != nil {
				return err
			}
			if err := s.Start(); err != nil {
				return err
			}
			tftpSrv = s
			return nil
		},

		StopTFTP: func() error {
			mu.Lock()
			defer mu.Unlock()
			if tftpSrv == nil {
				return nil
			}
			err := tftpSrv.Stop()
			tftpSrv = nil
			return err
		},

		TFTPInfo: func() string {
			mu.Lock()
			defer mu.Unlock()
			if tftpSrv == nil {
				return "未启动"
			}
			return fmt.Sprintf("运行中，%d 次传输", tftpSrv.Stats().Transfers)
		},

		StartHTTP: func(cfg model.Settings, scriptFor func(mac, isoID string) (string, error),
			isos func() []model.ISO, isoDir, userDir string) error {
			mu.Lock()
			defer mu.Unlock()
			s, err := httpd.New(httpd.Config{
				Addr:      fmt.Sprintf(":%d", cfg.HTTPPort),
				ISODir:    isoDir,
				UserDir:   userDir,
				BootDir:   st.BootDir(),
				ScriptFor: scriptFor,
				ISOs:      isos,
				OnEvent:   ev,
				OnClient: func(mac, ip, path string) {
					st.UpsertClient(model.Client{MAC: mac, IP: ip})
				},
			})
			if err != nil {
				return err
			}
			if err := s.Start(); err != nil {
				return err
			}
			httpSrv = s
			return nil
		},

		StopHTTP: func() error {
			mu.Lock()
			defer mu.Unlock()
			if httpSrv == nil {
				return nil
			}
			err := httpSrv.Stop()
			httpSrv = nil
			return err
		},

		HTTPInfo: func() string {
			mu.Lock()
			defer mu.Unlock()
			if httpSrv == nil {
				return "未启动"
			}
			return "运行中"
		},

		ScanISOs: func(dir string) ([]model.ISO, error) {
			return iso.Scan(dir)
		},
	}
}

// StartWeb 启动 Web 管理界面。
func StartWeb(srv *Server, st *store.Store) (*web.Server, error) {
	cfg := st.Settings()
	w, err := web.New(web.Config{
		Addr:    fmt.Sprintf(":%d", cfg.WebPort),
		Store:   st,
		PXE:     asController(srv),
		Version: Version,
	})
	if err != nil {
		return nil, err
	}
	if err := w.Start(); err != nil {
		return nil, err
	}
	return w, nil
}

// execCommand 是 exec.Command 的薄封装，方便 main 里替换。
func execCommand(name string, args ...string) error {
	c := exec.Command(name, args...)
	return c.Start()
}

// EnsurePortFree 检查端口是否可用（TCP 和 UDP 都查），给出友好错误。
//
// DHCP(67) 和 TFTP(69) 是 UDP，HTTP(16000) 和 Web(16080) 是 TCP，
// 所以两类都要探测。macOS 上允许在 0.0.0.0:port 已被 127.0.0.1:port 占用时
// 再次绑定 0.0.0.0，因此这里同时探测 0.0.0.0 和 127.0.0.1 两个地址。
func EnsurePortFree(port int) error {
	if port < 1 || port > 65535 {
		return fmt.Errorf("端口 %d 超出范围 1-65535", port)
	}
	for _, host := range []string{"0.0.0.0", "127.0.0.1"} {
		addr := fmt.Sprintf("%s:%d", host, port)
		l, err := net.Listen("tcp", addr)
		if err != nil {
			return fmt.Errorf("端口 %d/TCP 已被占用（%s）: %w", port, host, err)
		}
		_ = l.Close()

		pc, err := net.ListenPacket("udp", addr)
		if err != nil {
			return fmt.Errorf("端口 %d/UDP 已被占用（%s）: %w", port, host, err)
		}
		_ = pc.Close()
	}
	return nil
}
