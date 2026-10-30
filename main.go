// NetBootX —— 增强版 PXE / UEFI HTTPBoot 服务器。
//
// 用法：
//
//	netbootx                     启动 Web 界面（默认端口 16080），在界面上点「开始」
//	netbootx start               直接启动 PXE 服务（headless）
//	netbootx stop                停止 PXE 服务
//	netbootx -h                  查看全部选项
//
// 典型用法：把 ISO 丢进 iso/ 目录，双击运行，浏览器打开 http://127.0.0.1:16080，
// 选网卡 IP、点绿色按钮，客户机开机按 PXE 启动即可。
package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"github.com/wupengcheng/netbootx/internal/model"
	"github.com/wupengcheng/netbootx/internal/pxe"
	"github.com/wupengcheng/netbootx/internal/store"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "\n❌ 错误: %v\n\n", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		rootFlag  = flag.String("root", "", "软件根目录（默认：可执行文件所在目录）")
		webPort   = flag.Int("web", 0, "Web 管理界面端口（默认 16080）")
		httpPort  = flag.Int("http", 0, "HTTP 文件服务端口（默认 16000）")
		tftpPort  = flag.Int("tftp", 0, "TFTP 端口（默认 69）")
		dhcpPort  = flag.Int("dhcp", 0, "DHCP 端口（默认 67）")
		serverIP  = flag.String("ip", "", "本机对外 IP（留空则自动探测）")
		iface     = flag.String("iface", "", "绑定的网卡名（留空则用 -ip 或自动探测）")
		dhcpMode  = flag.String("mode", "", "DHCP 模式：full / proxy / external")
		rangeS    = flag.String("range-start", "", "IP 地址池起始")
		rangeE    = flag.String("range-end", "", "IP 地址池结束")
		autoStart = flag.Bool("autostart", false, "启动后立即开启 PXE 服务（不显示界面操作）")
		noBrowser = flag.Bool("no-browser", false, "不自动打开浏览器")
		showVer   = flag.Bool("version", false, "显示版本号")
	)
	flag.Usage = usage
	flag.Parse()

	if *showVer {
		fmt.Printf("NetBootX %s (%s/%s)\n", pxe.Version, runtime.GOOS, runtime.GOARCH)
		return nil
	}

	root, err := resolveRoot(*rootFlag)
	if err != nil {
		return err
	}

	// 创建数据目录
	dataDir := filepath.Join(root, "data")
	st, err := store.New(dataDir, filepath.Join(root, "boot"))
	if err != nil {
		return fmt.Errorf("初始化数据存储: %w", err)
	}

	// 应用命令行覆盖的配置
	cfg := st.Settings()
	changed := false
	if *webPort != 0 {
		cfg.WebPort = *webPort
		changed = true
	}
	if *httpPort != 0 {
		cfg.HTTPPort = *httpPort
		changed = true
	}
	if *tftpPort != 0 {
		cfg.TFTPPort = *tftpPort
		changed = true
	}
	if *dhcpPort != 0 {
		cfg.DHCPPort = *dhcpPort
		changed = true
	}
	if *dhcpMode != "" {
		cfg.DHCPMode = *dhcpMode
		changed = true
	}
	if *rangeS != "" {
		cfg.RangeStart = *rangeS
		changed = true
	}
	if *rangeE != "" {
		cfg.RangeEnd = *rangeE
		changed = true
	}
	if *iface != "" {
		cfg.Interface = *iface
	}
	if *serverIP != "" {
		cfg.ServerIP = *serverIP
		changed = true
	}
	if cfg.ServerIP == "" {
		// 自动探测：取第一块有 IP 的非回环网卡
		if ifs := pxe.Interfaces(); len(ifs) > 0 {
			for _, i := range ifs {
				if i.Loopback || len(i.IPs) == 0 {
					continue
				}
				if cfg.Interface != "" && i.Name != cfg.Interface {
					continue
				}
				cfg.ServerIP = i.IPs[0]
				cfg.Interface = i.Name
				if i.Netmask != "" {
					cfg.Netmask = i.Netmask
				}
				changed = true
				break
			}
		}
	}
	if changed {
		if err := st.SaveSettings(cfg); err != nil {
			fmt.Fprintf(os.Stderr, "⚠️  保存配置失败（继续）: %v\n", err)
		}
	}

	// 建目录
	if err := st.EnsureDirs(root); err != nil {
		return err
	}

	printBanner(st.Settings(), root)

	// 组装编排器
	srv := pxe.New(root, st, pxe.BuildSubsystems(st))

	// 启动 Web 界面
	webSrv, err := pxe.StartWeb(srv, st)
	if err != nil {
		return fmt.Errorf("启动 Web 界面: %w", err)
	}
	defer func() { _ = webSrv.Stop() }()

	fmt.Printf("  🌐 管理界面: http://127.0.0.1:%d\n", st.Settings().WebPort)
	fmt.Printf("  💡 把 ISO 文件放进: %s\n", filepath.Join(root, st.Settings().ISODir))
	fmt.Println()

	if !*noBrowser {
		openBrowser(fmt.Sprintf("http://127.0.0.1:%d", st.Settings().WebPort))
	}

	if *autoStart {
		if err := srv.Start(); err != nil {
			fmt.Fprintf(os.Stderr, "⚠️  自动启动 PXE 服务失败: %v\n", err)
		}
	}

	// 等信号
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
	fmt.Println("\n正在退出…")
	_ = srv.Stop()
	return nil
}

// resolveRoot 确定软件根目录。
func resolveRoot(flagVal string) (string, error) {
	if flagVal != "" {
		abs, err := filepath.Abs(flagVal)
		if err != nil {
			return "", err
		}
		return abs, nil
	}
	// 优先用当前工作目录（开发时）
	if wd, err := os.Getwd(); err == nil {
		if isProjectRoot(wd) {
			return wd, nil
		}
	}
	// 否则用可执行文件目录
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.Dir(exe), nil
}

// isProjectRoot 判断某个目录是不是软件根目录。
func isProjectRoot(dir string) bool {
	for _, name := range []string{"iso", "boot", "data"} {
		if st, err := os.Stat(filepath.Join(dir, name)); err != nil || !st.IsDir() {
			return false
		}
	}
	return true
}

func printBanner(cfg model.Settings, root string) {
	fmt.Println()
	fmt.Println("  ╔══════════════════════════════════════════════════════╗")
	fmt.Printf("  ║   NetBootX %-42s║\n", "v"+pxe.Version)
	fmt.Println("  ║   增强版 PXE / UEFI HTTPBoot 服务器                   ║")
	fmt.Println("  ╚══════════════════════════════════════════════════════╝")
	fmt.Printf("   根目录   %s\n", root)
	fmt.Printf("   本机 IP  %s\n", cfg.ServerIP)
	fmt.Printf("   DHCP 模式 %s\n", modeName(cfg.DHCPMode))
	fmt.Println()
}

func modeName(m string) string {
	switch m {
	case "full":
		return "full（本机作为唯一 DHCP 服务器）"
	case "proxy":
		return "proxy（局域已有 DHCP，本机只做 PXE）"
	case "external":
		return "external（完全由外部 DHCP 通告）"
	}
	return m
}

func usage() {
	fmt.Fprintf(os.Stderr, `NetBootX —— 增强版 PXE / UEFI HTTPBoot 服务器

用法:
  netbootx [选项]

常用选项:
  -ip string            本机对外 IP（留空自动探测）
  -iface string         绑定网卡名
  -mode string          DHCP 模式: full / proxy / external（默认 proxy）
  -range-start string   IP 地址池起始（full 模式必填）
  -range-end string     IP 地址池结束（full 模式必填）
  -web int              Web 界面端口（默认 16080）
  -http int             HTTP 服务端口（默认 16000）
  -tftp int             TFTP 端口（默认 69，需要 root）
  -dhcp int             DHCP 端口（默认 67，需要 root）
  -autostart            启动后立即开启 PXE 服务
  -no-browser           不自动打开浏览器
  -version              显示版本

说明:
  把 ISO 文件放进 iso/ 目录，运行本程序，浏览器打开管理界面，
  选好网卡 IP 和 IP 池，点绿色启动按钮即可。
  客户机开机进入 PXE / 网络启动，就会看到 ISO 列表。

权限:
  DHCP(67) 和 TFTP(69) 是特权端口，Linux/macOS 下需要 sudo 运行。
  也可以在配置里改成高位端口，但客户机侧需要额外配置。

`)
}

// openBrowser 尽力打开系统默认浏览器。
func openBrowser(url string) {
	var cmd string
	var args []string
	switch runtime.GOOS {
	case "darwin":
		cmd = "open"
		args = []string{url}
	case "windows":
		cmd = "cmd"
		args = []string{"/c", "start", url}
	default:
		cmd = "xdg-open"
		args = []string{url}
	}
	go func() {
		time.Sleep(400 * time.Millisecond)
		c := exec.Command(cmd, args...)
		_ = c.Start()
	}()
}
