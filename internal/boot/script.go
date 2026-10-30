// Package boot 负责把「客户机 + ISO 列表 + 服务端配置」翻译成 iPXE 启动脚本。
//
// 本包全部是纯函数：不打开文件、不访问网络、不依赖全局状态，
// 因此可以放心地在 HTTP 请求处理路径上并发调用，也非常容易单测。
//
// 生成的脚本以 "#!ipxe" 开头，可直接被 iPXE 的 chain 命令加载执行：
//
//	chain http://192.168.123.10:16000/boot/menu.ipxe?mac=52:54:00:12:34:56
//
// 设计要点：
//   - 菜单按客户机架构过滤 ISO（x86 客户机不会看到 arm64 镜像）；
//   - 菜单按操作系统家族（Windows / Linux / WinPE / ESXi / BSD / 其他）分组；
//   - 支持超时自动启动默认项（Settings.BootMenuTimeout，单位秒）；
//   - Linux 走 kernel + initrd，Windows/WinPE 走 wimboot；
//   - 自动安装模板 URL 会自动追加到内核命令行。
package boot

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/wupengcheng/netbootx/internal/model"
)

// DefaultHTTPPort 是未配置 HTTP 端口时使用的默认端口（与 model.DefaultSettings 一致）。
const DefaultHTTPPort = 16000

// menuTitlePrefix 是启动菜单的标题前缀。
const menuTitlePrefix = "NetBootX 启动菜单"

// familyGroups 定义菜单里各操作系统家族的分组顺序与显示名。
// 菜单按这里的顺序排列，保证同一批 ISO 每次生成的脚本完全一致。
var familyGroups = []struct {
	family model.ISOFamily
	title  string
}{
	{model.FamilyWindows, "Windows"},
	{model.FamilyLinux, "Linux"},
	{model.FamilyWinPE, "WinPE"},
	{model.FamilyESXi, "ESXi"},
	{model.FamilyBSD, "BSD"},
	{model.FamilyOther, "其他"},
}

// Script 生成某台客户机的 iPXE 启动脚本。
//
// 参数 c 可以为 nil（视为未知架构的匿名客户机，显示所有 ISO）。
// 参数 baseURL 为空时由 s.ServerIP + s.HTTPPort 推导。
//
// 输出是确定性的：同一组输入永远生成完全相同的脚本，方便做快照测试。
func Script(c *model.Client, isos []model.ISO, s model.Settings, baseURL string) string {
	base := normalizeBase(baseURL, s)
	visible := Filter(c, isos)
	ordered := orderISOs(visible)

	var b strings.Builder
	b.WriteString("#!ipxe\n")
	b.WriteString("# NetBootX iPXE 启动脚本（由服务端自动生成，请勿手工编辑）\n")
	b.WriteString("# 客户机: ")
	b.WriteString(clientDesc(c))
	b.WriteString("\n# 服务端: ")
	b.WriteString(base)
	b.WriteString("\n")

	if len(ordered) == 0 {
		b.WriteString("# 当前没有适用于这台客户机的可引导镜像\n")
		b.WriteString("echo NetBootX: 没有可用的 ISO 镜像\n")
		b.WriteString("echo 请把 ISO 文件放入服务端的 iso/ 目录，然后在 Web 界面重新扫描\n")
		b.WriteString("shell\n")
		return b.String()
	}

	// 先拿地址再拉脚本是 iPXE 的标准姿势：dhcp 保证网络就绪，set base 简化后续 URL。
	b.WriteString("dhcp || goto netbootx_failed\n")
	b.WriteString("set base ")
	b.WriteString(base)
	b.WriteString("\n")
	b.WriteString("menu ")
	b.WriteString(menuTitle(c))
	b.WriteString("\n")

	// 每个 ISO 分配一个不含空格、不含特殊字符的菜单标签。
	labels := make(map[int]string, len(ordered))
	used := make(map[string]bool, len(ordered))
	for _, idx := range ordered {
		labels[idx] = uniqueLabel(visible[idx], used)
	}

	number := 0
	for _, g := range groupISOs(visible, ordered) {
		b.WriteString("item --gap --  ")
		b.WriteString(g.title)
		b.WriteString("\n")
		for _, idx := range g.items {
			number++
			b.WriteString("item ")
			b.WriteString(labels[idx])
			b.WriteString(" ")
			b.WriteString(itemText(visible[idx], number))
			b.WriteString("\n")
		}
	}

	def := defaultISOIndex(visible, ordered, c, s)
	b.WriteString("choose")
	b.WriteString(timeoutFlag(s.BootMenuTimeout))
	b.WriteString(" --default ")
	b.WriteString(labels[def])
	b.WriteString(" target || goto netbootx_cancelled\n")
	b.WriteString("goto ${target}\n")

	blocks := make([]string, 0, len(ordered))
	for _, idx := range ordered {
		lines := bootLines(c, visible[idx], s, base)
		// 引导失败时回到菜单，而不是把客户机丢在 iPXE 命令行里。
		if n := len(lines); n > 0 && lines[n-1] == "boot" {
			lines[n-1] = "boot || goto netbootx_failed"
		}
		blocks = append(blocks, ":"+labels[idx]+"\n"+strings.Join(lines, "\n"))
	}
	b.WriteString(strings.Join(blocks, "\n\n"))
	b.WriteString("\n")
	b.WriteString("\n")
	b.WriteString(menuFailureTail(base, c))
	return b.String()
}

// menuFailureTail 生成菜单脚本的失败兜底段：引导失败回菜单，用户取消进命令行。
func menuFailureTail(base string, c *model.Client) string {
	var b strings.Builder
	b.WriteString(":netbootx_failed\n")
	b.WriteString("echo 引导失败，5 秒后返回启动菜单\n")
	b.WriteString("sleep 5\n")
	// 回菜单前重新拿一次地址；连 DHCP 都失败就别再循环，交给用户手工处理。
	b.WriteString("dhcp || goto netbootx_cancelled\n")
	b.WriteString("chain ")
	b.WriteString(base)
	b.WriteString("/boot/menu.ipxe?mac=")
	b.WriteString(macQuery(c))
	b.WriteString("\n\n")
	b.WriteString(":netbootx_cancelled\n")
	b.WriteString("echo 已取消启动，进入 iPXE 命令行\n")
	b.WriteString("shell\n")
	return b.String()
}

// Entry 生成「直接启动某个 ISO」的完整 iPXE 脚本（跳过菜单）。
//
// 返回的是可以直接交给 iPXE 的独立脚本：带 #!ipxe 头、自己执行 dhcp、
// 自己设置 ${base} 变量，因此可以单独用于 UEFI HTTPBoot 直达场景，例如
// httpd 的 /boot/menu.ipxe?mac=X&iso=Y。
//
// 形如：
//
//	#!ipxe
//	dhcp
//	set base http://192.168.123.10:16000
//	:iso_abc123
//	kernel ${base}/iso/abc123/casper/vmlinuz quiet splash ---
//	initrd ${base}/iso/abc123/casper/initrd
//	boot
//
// 脚本末尾带失败兜底：引导失败时回显提示并链回主菜单，客户机不会挂死。
func Entry(c *model.Client, is model.ISO, s model.Settings, baseURL string) string {
	base := normalizeBase(baseURL, s)
	label := uniqueLabel(is, map[string]bool{})

	var b strings.Builder
	b.WriteString("#!ipxe\n")
	b.WriteString("# NetBootX 直接启动脚本（由服务端自动生成，请勿手工编辑）\n")
	b.WriteString("# 客户机: ")
	b.WriteString(clientDesc(c))
	b.WriteString("\n# 镜像: ")
	b.WriteString(sanitizeText(displayName(is)))
	b.WriteString("\n# 服务端: ")
	b.WriteString(base)
	b.WriteString("\n")
	b.WriteString("dhcp || goto netbootx_failed\n")
	b.WriteString("set base ")
	b.WriteString(base)
	b.WriteString("\n")
	lines := bootLines(c, is, s, base)
	if n := len(lines); n > 0 && lines[n-1] == "boot" {
		lines[n-1] = "boot || goto netbootx_failed"
	}
	b.WriteString(":" + label + "\n")
	b.WriteString(strings.Join(lines, "\n"))
	b.WriteString("\n")
	b.WriteString("\n")
	// 失败兜底：链回主菜单，避免客户机停在 iPXE shell。
	b.WriteString(":netbootx_failed\n")
	b.WriteString("echo 引导失败，5 秒后返回启动菜单\n")
	b.WriteString("sleep 5\n")
	b.WriteString("chain ${base}/boot/menu.ipxe?mac=" + macQuery(c) + "\n")
	return b.String()
}

// macQuery 返回写进 URL 查询串的 MAC 地址。
// MAC 规范化后只含 [0-9a-f:]，这些字符在查询串里无需转义，保持可读性。
func macQuery(c *model.Client) string {
	if c == nil {
		return ""
	}
	mac := model.NormalizeMAC(c.MAC)
	var b strings.Builder
	for _, r := range mac {
		if r >= '0' && r <= '9' || r >= 'a' && r <= 'f' || r == ':' || r == '-' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// menuGroup 是菜单里的一组条目。
type menuGroup struct {
	title string
	items []int // 指向 visible 切片的下标
}

// orderISOs 返回菜单里 ISO 的排列顺序：先按家族，再按显示名，最后按文件名。
// 返回的是 isos 切片的下标，排序是稳定的，保证输出确定性。
func orderISOs(isos []model.ISO) []int {
	idx := make([]int, len(isos))
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(a, b int) bool {
		x, y := isos[idx[a]], isos[idx[b]]
		if rx, ry := familyRank(x.Family), familyRank(y.Family); rx != ry {
			return rx < ry
		}
		if nx, ny := strings.ToLower(displayName(x)), strings.ToLower(displayName(y)); nx != ny {
			return nx < ny
		}
		if nx, ny := strings.ToLower(x.Name), strings.ToLower(y.Name); nx != ny {
			return nx < ny
		}
		return x.ID < y.ID
	})
	return idx
}

// groupISOs 把已排序的下标切成按家族分组的小块。
func groupISOs(isos []model.ISO, ordered []int) []menuGroup {
	var out []menuGroup
	for _, idx := range ordered {
		title := familyTitle(isos[idx].Family)
		if len(out) == 0 || out[len(out)-1].title != title {
			out = append(out, menuGroup{title: title})
		}
		out[len(out)-1].items = append(out[len(out)-1].items, idx)
	}
	return out
}

// familyRank 返回家族在菜单里的排序权重，未知家族排最后。
func familyRank(f model.ISOFamily) int {
	for i, g := range familyGroups {
		if g.family == f {
			return i
		}
	}
	return len(familyGroups)
}

// familyTitle 返回家族在菜单里的显示名。
func familyTitle(f model.ISOFamily) string {
	for _, g := range familyGroups {
		if g.family == f {
			return g.title
		}
	}
	return "其他"
}

// itemText 生成菜单项的显示文本：序号 + 操作系统名 + 文件名 + 大小。
func itemText(is model.ISO, number int) string {
	name := sanitizeText(displayName(is))
	file := sanitizeText(fileNameOf(is))
	var b strings.Builder
	b.WriteString(strconv.Itoa(number))
	b.WriteString(") ")
	b.WriteString(name)
	if file != "" && file != name {
		b.WriteString(" - ")
		b.WriteString(file)
	}
	if is.Size > 0 {
		b.WriteString(" (")
		b.WriteString(HumanSize(is.Size))
		b.WriteString(")")
	}
	if is.Error != "" {
		b.WriteString(" [解析异常]")
	}
	return b.String()
}

// menuTitle 生成菜单标题，带上 MAC 方便多台机器同时启动时辨认。
func menuTitle(c *model.Client) string {
	title := menuTitlePrefix
	if c != nil && strings.TrimSpace(c.MAC) != "" {
		title += " - " + sanitizeText(model.NormalizeMAC(c.MAC))
	}
	return title
}

// clientDesc 描述客户机，写进脚本头部的注释里。
func clientDesc(c *model.Client) string {
	if c == nil {
		return "未知客户机"
	}
	var parts []string
	if mac := strings.TrimSpace(c.MAC); mac != "" {
		parts = append(parts, sanitizeText(model.NormalizeMAC(mac)))
	}
	if host := strings.TrimSpace(c.Hostname); host != "" {
		parts = append(parts, sanitizeText(host))
	}
	if c.Arch != "" {
		parts = append(parts, c.Arch.String())
	}
	if len(parts) == 0 {
		return "未知客户机"
	}
	return strings.Join(parts, " / ")
}

// timeoutFlag 生成 choose 命令的超时参数（毫秒）。秒数 <= 0 表示不自动启动。
func timeoutFlag(seconds int) string {
	if seconds <= 0 {
		return ""
	}
	ms := seconds * 1000
	if ms < 1000 {
		ms = 1000
	}
	return " --timeout " + strconv.Itoa(ms)
}

// defaultISOIndex 选出默认启动项：客户机上次选择 > 服务端默认 ISO > 菜单第一项。
func defaultISOIndex(isos []model.ISO, ordered []int, c *model.Client, s model.Settings) int {
	if c != nil && c.Selected != "" {
		for _, idx := range ordered {
			if isos[idx].ID == c.Selected {
				return idx
			}
		}
	}
	if s.DefaultISO != "" {
		for _, idx := range ordered {
			if isos[idx].ID == s.DefaultISO {
				return idx
			}
		}
	}
	return ordered[0]
}

// displayName 返回 ISO 的显示名，尽力而为。
func displayName(is model.ISO) string {
	if name := strings.TrimSpace(is.OSName); name != "" {
		return name
	}
	if name := strings.TrimSpace(is.Name); name != "" {
		return name
	}
	if id := strings.TrimSpace(is.ID); id != "" {
		return id
	}
	return "未知镜像"
}

// fileNameOf 返回 ISO 的文件名，用于菜单展示（去掉路径部分）。
func fileNameOf(is model.ISO) string {
	name := strings.TrimSpace(is.Name)
	if name == "" {
		p := strings.ReplaceAll(strings.TrimSpace(is.RelPath), "\\", "/")
		if i := strings.LastIndex(p, "/"); i >= 0 {
			p = p[i+1:]
		}
		name = strings.TrimSpace(p)
	}
	return name
}

// uniqueLabel 生成 iPXE 菜单标签（不能含空格）。
// 标签只保留 [A-Za-z0-9_-]，重复时自动加序号后缀。
func uniqueLabel(is model.ISO, used map[string]bool) string {
	base := sanitizeLabel(is.ID)
	if base == "" {
		base = sanitizeLabel(is.Name)
	}
	if base == "" {
		base = "iso"
	}
	// 截断时给重复后缀（_2、_3 …）留出空间，同时保证不切坏多字节字符。
	base = truncateRunes(base, 40)
	label := "iso_" + base
	if !used[label] {
		used[label] = true
		return label
	}
	for i := 2; ; i++ {
		cand := label + "_" + strconv.Itoa(i)
		if !used[cand] {
			used[cand] = true
			return cand
		}
	}
}

// truncateRunes 按字符数截断字符串（不会切坏 UTF-8）。
func truncateRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// sanitizeLabel 只保留 iPXE 标签允许的安全字符。
func sanitizeLabel(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		}
	}
	return b.String()
}

// sanitizeText 清理要写进 iPXE 命令行的展示文本。
//
// iPXE 的命令行在解析参数之前会按 "&&" / "||" 切分命令，所以展示文本里的
// & | ; $ 必须换成全角字符，否则会破坏脚本结构。
func sanitizeText(s string) string {
	replacer := strings.NewReplacer(
		"&", "＆",
		"|", "｜",
		";", "；",
		"$", "＄",
		"\n", " ",
		"\r", " ",
		"\t", " ",
		"\x00", "",
	)
	s = replacer.Replace(s)
	var b strings.Builder
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			continue
		}
		b.WriteRune(r)
	}
	return strings.TrimSpace(b.String())
}

// HumanSize 把字节数格式化成人类可读的大小，例如 3113851289 -> "2.9GB"。
func HumanSize(n int64) string {
	if n <= 0 {
		return "0B"
	}
	units := [...]string{"B", "KB", "MB", "GB", "TB", "PB"}
	f := float64(n)
	i := 0
	for f >= 1024 && i < len(units)-1 {
		f /= 1024
		i++
	}
	if i == 0 {
		return strconv.FormatInt(n, 10) + units[0]
	}
	if f >= 100 {
		return fmt.Sprintf("%.0f%s", f, units[i])
	}
	return fmt.Sprintf("%.1f%s", f, units[i])
}
