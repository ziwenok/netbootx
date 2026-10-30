package web

import (
	"fmt"
	"sort"
	"strings"

	"github.com/wupengcheng/netbootx/internal/model"
)

// 本文件实现 /api/preview 的兜底脚本生成器。
//
// 真正的菜单生成属于 internal/boot（由 pxe 层通过 Previewer 扩展接口提供）。
// 当 pxe 层没有实现 Previewer 时，web 层用这里的最小实现给出「格式正确、
// 能看懂」的 iPXE 脚本，方便调试与演示；两者不一致时以 pxe 层为准。

// defaultPreviewPort 是状态里没有 HTTP 端口时的兜底值。
const defaultPreviewPort = 16000

// buildPreviewScript 生成给定 ISO 的 iPXE 脚本（近似实现，不依赖 boot 包）。
func buildPreviewScript(is model.ISO, mac string, s model.Settings, serverIP string, httpPort int) string {
	if serverIP == "" {
		serverIP = s.ServerIP
	}
	if serverIP == "" {
		serverIP = "127.0.0.1"
	}
	if httpPort <= 0 {
		httpPort = s.HTTPPort
	}
	if httpPort <= 0 {
		httpPort = defaultPreviewPort
	}

	base := fmt.Sprintf("http://%s:%d", serverIP, httpPort)
	isoBase := base + "/iso/" + urlSegmentEscape(is.ID)
	timeout := s.BootMenuTimeout
	if timeout <= 0 {
		timeout = 30
	}

	var b strings.Builder
	b.WriteString("#!ipxe\n")
	b.WriteString("# NetBootX 启动脚本预览（由 Web 管理界面生成）\n")
	fmt.Fprintf(&b, "# 客户机 MAC : %s\n", mac)
	fmt.Fprintf(&b, "# 镜像       : %s\n", is.Name)
	if is.OSName != "" {
		fmt.Fprintf(&b, "# 操作系统   : %s（%s）\n", is.OSName, familyText(is.Family))
	}
	fmt.Fprintf(&b, "# 引导方式   : %s\n", methodText(is.Boot.Method))
	if is.Bootable {
		b.WriteString("# 可引导     : 是\n")
	} else {
		b.WriteString("# 可引导     : 否（该镜像可能无法直接启动）\n")
	}
	if is.Error != "" {
		fmt.Fprintf(&b, "# 探测告警   : %s\n", is.Error)
	}
	b.WriteString("\n")
	b.WriteString("set server " + serverIP + "\n")
	b.WriteString(fmt.Sprintf("set port %d\n", httpPort))
	b.WriteString("echo NetBootX 正在引导 " + sanitizeEcho(is.OSName, is.Name) + "\n")

	switch is.Boot.Method {
	case "wimboot":
		b.WriteString("kernel wimboot\n")
		wim := []struct {
			name string
			path string
		}{
			{"bootmgr", is.Boot.BootMGR},
			{"bcd", is.Boot.BCD},
			{"boot.sdi", is.Boot.BootSDI},
			{"boot.wim", is.Boot.BootWIM},
		}
		fallbacks := []string{"/bootmgr", "/boot/bcd", "/boot/boot.sdi", "/sources/boot.wim"}
		for i, item := range wim {
			path := item.path
			if path == "" {
				path = fallbacks[i]
			}
			fmt.Fprintf(&b, "initrd --name %s %s%s\n", item.name, isoBase, urlPathEscape(path))
		}
	case "memdisk":
		fmt.Fprintf(&b, "kernel memdisk initrd=%s%s\n", isoBase, urlPathEscape(is.Boot.Kernel))
	case "sanboot":
		fmt.Fprintf(&b, "sanboot --no-describe --drive 0x80 %s%s\n", isoBase, urlPathEscape(firstNonEmpty(is.Boot.Kernel, "/")))
	default: // kernel
		kernel := firstNonEmpty(is.Boot.Kernel, "/boot/vmlinuz")
		fmt.Fprintf(&b, "kernel %s%s", isoBase, urlPathEscape(kernel))
		args := kernelArgs(is, base)
		if len(args) > 0 {
			b.WriteString(" " + strings.Join(args, " "))
		}
		b.WriteString("\n")
		initrds := is.Boot.Initrd
		if len(initrds) == 0 {
			initrds = []string{"/boot/initrd.img"}
		}
		for _, initrd := range initrds {
			fmt.Fprintf(&b, "initrd %s%s\n", isoBase, urlPathEscape(initrd))
		}
	}

	b.WriteString("boot || goto failed\n")
	b.WriteString(":failed\n")
	fmt.Fprintf(&b, "echo 引导失败，%d 秒后返回菜单\n", timeout)
	fmt.Fprintf(&b, "sleep %d\n", timeout)
	fmt.Fprintf(&b, "chain %s/boot/menu.ipxe?mac=${mac}\n", base)
	return b.String()
}

// kernelArgs 拼装内核命令行：模板参数 + 自动安装参数，并替换占位符。
func kernelArgs(is model.ISO, base string) []string {
	var args []string
	for _, a := range is.Boot.KernelArgs {
		args = append(args, expandPlaceholders(a, is, base))
	}
	if is.Boot.AutoInstall != nil {
		for _, a := range is.Boot.AutoInstall.KernelArgs {
			args = append(args, expandPlaceholders(a, is, base))
		}
		if tmpl := strings.TrimSpace(is.Boot.AutoInstall.Template); tmpl != "" {
			args = append(args, "netbootx_autoinstall="+base+"/user/autoinstall/"+urlPathEscape(tmpl))
		}
	}
	return args
}

// expandPlaceholders 替换内核参数模板里的占位符。
func expandPlaceholders(arg string, is model.ISO, base string) string {
	replacer := strings.NewReplacer(
		"{iso}", base+"/iso/"+urlSegmentEscape(is.ID)+urlPathEscape(is.Boot.Kernel),
		"{isoName}", is.Name,
		"{server}", base,
		"{id}", is.ID,
		"{arch}", string(is.OSArch),
	)
	return replacer.Replace(arg)
}

// urlPathEscape 对 ISO 内路径做最小转义：保留 / 分隔符，其余不安全字符百分号编码。
func urlPathEscape(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	var b strings.Builder
	b.Grow(len(p))
	for i := 0; i < len(p); i++ {
		c := p[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
			b.WriteByte(c)
		case strings.IndexByte("/-_.~:+,@!$&()*=", c) >= 0:
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// urlSegmentEscape 转义单个路径段（不含前导斜杠），用于 ISO ID。
func urlSegmentEscape(s string) string {
	return strings.TrimPrefix(urlPathEscape(s), "/")
}

// sanitizeEcho 清理要 echo 到控制台的文本，避免 iPXE 脚本被特殊字符破坏。
func sanitizeEcho(parts ...string) string {
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		var b strings.Builder
		for _, r := range p {
			switch r {
			case '\n', '\r', '\t':
				b.WriteByte(' ')
			default:
				b.WriteRune(r)
			}
		}
		return b.String()
	}
	return "镜像"
}

// familyText 返回 ISO 家族的中文描述。
func familyText(f model.ISOFamily) string {
	switch f {
	case model.FamilyWindows:
		return "Windows 安装盘"
	case model.FamilyWinPE:
		return "WinPE"
	case model.FamilyLinux:
		return "Linux"
	case model.FamilyESXi:
		return "VMware ESXi"
	case model.FamilyBSD:
		return "BSD"
	}
	return "未知类型"
}

// methodText 返回引导方式的中文描述。
func methodText(m string) string {
	switch m {
	case "kernel":
		return "kernel + initrd"
	case "wimboot":
		return "wimboot"
	case "memdisk":
		return "memdisk"
	case "sanboot":
		return "sanboot"
	}
	if m == "" {
		return "未识别"
	}
	return m
}

// firstNonEmpty 返回第一个非空字符串。
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// sortISOList 按名称排序 ISO（供预览/列表稳定输出）。
func sortISOList(isos []model.ISO) []model.ISO {
	out := make([]model.ISO, len(isos))
	copy(out, isos)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
