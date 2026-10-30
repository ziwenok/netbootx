package iso

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/wupengcheng/netbootx/internal/model"
)

// 探测时读取小文本文件的上限，避免把大文件读进内存。
const (
	maxTextProbe = 256 << 10 // 256KB
	maxWIMProbe  = 1 << 20   // 1MB（WIM 头里读发行版名字用）
)

// detect 识别 ISO 的操作系统家族、名字、架构和引导方式。
// 任何一步失败都只是「识别不到」，不会返回错误：识别是尽力而为的。
func (r *Reader) detect(img *model.ISO) {
	if r.boot != nil {
		img.Bootable = r.boot.Bootable
	}
	// 优先级：Windows → WinPE → ESXi → BSD → Linux → 其它。
	switch {
	case r.isWindowsInstaller():
		img.Family = model.FamilyWindows
		r.fillWindowsBoot(img)
		img.OSName = r.windowsName(img)
		img.OSArch = r.windowsArch()
	case r.isWinPE():
		img.Family = model.FamilyWinPE
		r.fillWindowsBoot(img)
		img.OSName = r.windowsName(img)
		img.OSArch = r.windowsArch()
	case r.isESXi():
		img.Family = model.FamilyESXi
		img.OSName = r.esxiName(img)
		img.OSArch = model.ArchUEFI64
		img.Boot = model.BootSpec{Method: "sanboot"}
	case r.isBSD():
		img.Family = model.FamilyBSD
		img.OSName = r.bsdName(img)
		img.OSArch = r.genericArch()
		img.Boot = model.BootSpec{Method: "sanboot"}
	default:
		if spec, ok := r.detectLinux(); ok {
			img.Family = model.FamilyLinux
			img.Boot = spec
			img.OSName = r.linuxName(img)
			img.OSArch = r.linuxArch()
			return
		}
		img.Family = model.FamilyOther
		img.OSName = r.genericName(img)
		img.OSArch = r.genericArch()
		if img.Bootable {
			img.Boot = model.BootSpec{Method: "sanboot"}
		}
	}
}

// isWindowsInstaller 判断是否为 Windows 安装盘：
// sources/boot.wim 是 WinPE 引导镜像，install.wim / install.esd 是安装镜像。
func (r *Reader) isWindowsInstaller() bool {
	if !r.Exists("/sources/boot.wim") && !r.Exists("/sources/boot.esd") {
		return false
	}
	return r.Exists("/sources/install.wim") || r.Exists("/sources/install.esd") ||
		r.Exists("/sources/install.swm")
}

// isWinPE 判断是否为纯 WinPE 盘：有 boot.wim 但没有 install.*。
func (r *Reader) isWinPE() bool {
	if r.Exists("/sources/install.wim") || r.Exists("/sources/install.esd") {
		return false
	}
	if r.Exists("/sources/boot.wim") || r.Exists("/sources/boot.esd") {
		return true
	}
	// 老式 WinPE 结构：根目录有 bootmgr + boot/bcd。
	return r.Exists("/bootmgr") && r.Exists("/boot/bcd")
}

// fillWindowsBoot 填写 wimboot 方式需要的四个文件。
func (r *Reader) fillWindowsBoot(img *model.ISO) {
	spec := model.BootSpec{Method: "wimboot"}
	spec.BootMGR = firstExisting(r, "/bootmgr")
	spec.BCD = firstExisting(r, "/boot/bcd", "/efi/microsoft/boot/bcd")
	spec.BootSDI = firstExisting(r, "/boot/boot.sdi")
	spec.BootWIM = firstExisting(r, "/sources/boot.wim", "/sources/boot.esd")
	img.Boot = spec
}

// isESXi 判断是否为 VMware ESXi 安装盘。
func (r *Reader) isESXi() bool {
	if r.Exists("/vmxboot/efi/bootx64.efi") || r.Exists("/vmxboot/syslinux.cfg") {
		return true
	}
	return r.Exists("/boot.cfg") && r.Exists("/tboot.b00")
}

// isBSD 判断是否为 FreeBSD 等 BSD 安装盘。
func (r *Reader) isBSD() bool {
	if r.Exists("/boot/loader") && r.Exists("/etc/rc") {
		return true
	}
	// FreeBSD 12+ 的安装盘根目录还有 boot/loader.efi 与 boot/kernel。
	return r.Exists("/boot/loader.efi") && r.Exists("/boot/kernel")
}

// detectLinux 按优先级尝试各发行版的内核/initrd 组合，返回引导描述。
func (r *Reader) detectLinux() (model.BootSpec, bool) {
	label := r.VolumeID()
	for _, c := range linuxCandidates(label) {
		if !r.Exists(c.kernel) {
			continue
		}
		// 附加 initrd（微码等）：存在就加，且排在主 initrd 前面。
		var initrds []string
		for _, p := range c.extraInitrds {
			if r.Exists(p) {
				initrds = append(initrds, p)
			}
		}
		// 主 initrd：取第一个存在的候选。
		main := ""
		for _, p := range c.initrds {
			if r.Exists(p) {
				main = p
				break
			}
		}
		if main == "" {
			continue
		}
		initrds = append(initrds, main)
		return model.BootSpec{
			Method:     "kernel",
			Kernel:     c.kernel,
			Initrd:     initrds,
			KernelArgs: expandArgs(c.args, label),
		}, true
	}
	// 兜底：找得到任意内核就当 Linux，但只能整体挂载（sanboot）。
	if r.Exists("/boot/vmlinuz") || r.Exists("/boot/Image") || r.Exists("/isolinux/vmlinuz") {
		return model.BootSpec{Method: "sanboot"}, true
	}
	return model.BootSpec{}, false
}

// linuxCandidate 是一种发行版的引导文件组合。
//
//   - initrds 是「候选」列表：取第一个存在的（不同版本 initrd 名字不同）
//   - extraInitrds 是「附加」列表：存在的都加上（如 Arch 的 CPU 微码），
//     顺序排在主 initrd 之前
type linuxCandidate struct {
	name         string
	kernel       string
	initrds      []string
	extraInitrds []string
	args         []string
}

// linuxCandidates 返回按优先级排列的引导候选列表。
//
// 顺序很重要：Ubuntu 的 casper 结构也会在 isolinux 下放一份 vmlinuz，
// 必须先匹配更具体的目录，否则会拿到错误的内核参数。
func linuxCandidates(label string) []linuxCandidate {
	stage2 := "inst.stage2=hd:LABEL=" + label
	return []linuxCandidate{
		// Ubuntu Live（casper）
		{
			name:    "ubuntu-casper",
			kernel:  "/casper/vmlinuz",
			initrds: []string{"/casper/initrd", "/casper/initrd.img"},
			args:    []string{"boot=casper", "iso-scan/filename={iso}", "quiet", "splash", "---"},
		},
		// Debian Live
		{
			name:    "debian-live",
			kernel:  "/live/vmlinuz",
			initrds: []string{"/live/initrd.img", "/live/initrd"},
			args:    []string{"boot=live", "components", "quiet", "splash"},
		},
		// RHEL / Fedora / Rocky / Alma / CentOS Stream 安装盘
		{
			name:    "rhel-pxeboot",
			kernel:  "/images/pxeboot/vmlinuz",
			initrds: []string{"/images/pxeboot/initrd.img"},
			args:    []string{stage2, "inst.repo=hd:LABEL=" + label, "quiet"},
		},
		// 老版 CentOS / openEuler / 麒麟（isolinux 目录）
		{
			name:    "isolinux",
			kernel:  "/isolinux/vmlinuz",
			initrds: []string{"/isolinux/initrd.img", "/isolinux/initrd0.img"},
			args:    []string{stage2, "quiet"},
		},
		// SUSE / openSUSE
		{
			name:    "suse",
			kernel:  "/boot/x86_64/loader/linux",
			initrds: []string{"/boot/x86_64/loader/initrd"},
			args:    []string{"install=" + label, "quiet", "splash=silent"},
		},
		// Arch Linux（archiso）：微码要在主 initrd 之前加载
		{
			name:         "archiso",
			kernel:       "/arch/boot/x86_64/vmlinuz",
			extraInitrds: []string{"/arch/boot/intel_ucode.img", "/arch/boot/amd_ucode.img"},
			initrds:      []string{"/arch/boot/x86_64/archiso.img"},
			args:         []string{"archisobasedir=arch", "archisolabel=" + label},
		},
		// Arch Linux ARM64
		{
			name:    "archiso-arm64",
			kernel:  "/arch/boot/aarch64/vmlinuz",
			initrds: []string{"/arch/boot/aarch64/archiso.img"},
			args:    []string{"archisobasedir=arch", "archisolabel=" + label},
		},
		// Alpine Linux
		{
			name:    "alpine",
			kernel:  "/boot/vmlinuz-lts",
			initrds: []string{"/boot/initramfs-lts"},
			args:    []string{"modules=loop,squashfs,sd-mod,usb-storage", "alpine_dev=cdrom", "quiet"},
		},
		{
			name:    "alpine-edge",
			kernel:  "/boot/vmlinuz-edge",
			initrds: []string{"/boot/initramfs-edge"},
			args:    []string{"modules=loop,squashfs,sd-mod,usb-storage", "alpine_dev=cdrom", "quiet"},
		},
		// TinyCore：core.gz 先于通用规则匹配，避免被 /boot/vmlinuz 抢走
		{
			name:    "tinycore",
			kernel:  "/boot/vmlinuz",
			initrds: []string{"/boot/core.gz"},
			args:    []string{"quiet", "cde"},
		},
		// 通用 x86_64 Linux：/boot/vmlinuz + /boot/initrd*
		{
			name:    "generic-boot",
			kernel:  "/boot/vmlinuz",
			initrds: []string{"/boot/initrd.img", "/boot/initrd", "/boot/initramfs.img"},
			args:    []string{"quiet"},
		},
		// 通用 arm64 Linux：/boot/Image + /boot/initrd
		{
			name:    "generic-arm64",
			kernel:  "/boot/Image",
			initrds: []string{"/boot/initrd.img", "/boot/initrd"},
			args:    []string{"quiet"},
		},
	}
}

// expandArgs 把参数模板里的占位符替换掉，并丢弃空参数。
// {iso} 由 boot 包在生成脚本时替换成真实 URL，这里保留占位符不动。
func expandArgs(args []string, label string) []string {
	if len(args) == 0 {
		return nil
	}
	out := make([]string, 0, len(args))
	for _, a := range args {
		a = strings.TrimSpace(strings.ReplaceAll(a, "{label}", label))
		if a != "" {
			out = append(out, a)
		}
	}
	return out
}

// linuxArch 判断 Linux 镜像支持的架构。
func (r *Reader) linuxArch() model.Arch {
	if r.Exists("/arch/boot/aarch64") || r.Exists("/arch/boot/aarch64/vmlinuz") {
		return model.ArchARM64
	}
	// 有 arm64 的 EFI 引导文件且没有 x86 内核，判为 arm64。
	if r.Exists("/EFI/BOOT/grubaa64.efi") && !r.Exists("/isolinux/isolinux.bin") {
		return model.ArchARM64
	}
	if r.Exists("/boot/Image") && !r.Exists("/boot/vmlinuz") {
		return model.ArchARM64
	}
	return model.ArchUEFI64
}

// genericArch 用特征文件和 El Torito 平台信息推断架构。
func (r *Reader) genericArch() model.Arch {
	if r.Exists("/arch/boot/aarch64") {
		return model.ArchARM64
	}
	if r.Exists("/boot/Image") && !r.Exists("/boot/vmlinuz") {
		return model.ArchARM64
	}
	if r.boot != nil {
		for _, e := range r.boot.Entries {
			if e.Bootable && e.PlatformID == platformARM64 {
				return model.ArchARM64
			}
		}
	}
	return model.ArchUEFI64
}

// windowsArch 判断 Windows 安装盘的架构。
func (r *Reader) windowsArch() model.Arch {
	if r.Exists("/efi/boot/bootaa64.efi") {
		return model.ArchARM64
	}
	if r.Exists("/efi/boot/bootia32.efi") && !r.Exists("/efi/boot/bootx64.efi") {
		return model.ArchUEFI32
	}
	return model.ArchUEFI64
}

// linuxName 从各种线索文件里提取发行版名字，失败则用文件名。
func (r *Reader) linuxName(img *model.ISO) string {
	// 1. Ubuntu / Debian 的 .disk/info
	if s := r.readText("/.disk/info"); s != "" {
		if name := parseDiskInfo(s); name != "" {
			return name
		}
	}
	// 2. RHEL 系 / Fedora 的 .treeinfo（name = Rocky Linux 9.4）
	if s := r.readText("/.treeinfo"); s != "" {
		if name := parseTreeInfo(s); name != "" {
			return name
		}
	}
	// 3. Arch 的 /arch/version
	if v := strings.TrimSpace(r.readText("/arch/version")); v != "" {
		return "Arch Linux " + v
	}
	// 4. Alpine 的 /etc/alpine-release 或 /.alpine-release（内容形如 "3.20.10" 或 "alpine-standard-3.20.10 240618"）
	for _, p := range []string{"/etc/alpine-release", "/.alpine-release", "/boot/alpine-release"} {
		if v := strings.TrimSpace(r.readText(p)); v != "" {
			return "Alpine Linux " + alpineVersion(v)
		}
	}
	// 5. SUSE 的 /content
	if s := r.readText("/content"); s != "" {
		if name := parseSUSEProduct(s); name != "" {
			return name
		}
	}
	// 6. 通用：/etc/os-release 或 /usr/lib/os-release
	for _, p := range []string{"/etc/os-release", "/usr/lib/os-release"} {
		if s := r.readText(p); s != "" {
			if name := parseOSRelease(s); name != "" {
				return name
			}
		}
	}
	// 7. 兜底：PVD 卷标识
	if v := strings.TrimSpace(r.VolumeID()); v != "" && !looksGenericLabel(v) {
		return v
	}
	return strings.TrimSuffix(img.Name, filepath.Ext(img.Name))
}

// windowsName 提取 Windows 发行版名字。
func (r *Reader) windowsName(img *model.ISO) string {
	// 优先看 sources/ 下的 pid.txt / ei.cfg，再退回 wim 头里的 XML 元数据。
	for _, p := range []string{"/sources/pid.txt", "/sources/ei.cfg"} {
		if s := r.readText(p); s != "" {
			if name := parseWindowsProduct(s); name != "" {
				return name
			}
		}
	}
	for _, p := range []string{"/sources/boot.wim", "/sources/install.wim", "/sources/install.esd"} {
		if s := r.readTextLimit(p, maxWIMProbe); s != "" {
			if name := parseWIMName(s); name != "" {
				return name
			}
		}
	}
	return strings.TrimSuffix(img.Name, filepath.Ext(img.Name))
}

// esxiName 从 boot.cfg 的 title= 行提取 ESXi 版本。
func (r *Reader) esxiName(img *model.ISO) string {
	if s := r.readText("/boot.cfg"); s != "" {
		for _, line := range strings.Split(s, "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "title=") {
				if name := strings.TrimSpace(strings.TrimPrefix(line, "title=")); name != "" {
					return name
				}
			}
		}
	}
	return strings.TrimSuffix(img.Name, filepath.Ext(img.Name))
}

// bsdName 提取 BSD 发行版名字。
//
// 能走到这里的镜像已经满足 isBSD() 的判定（/boot/loader + /etc/rc 是 FreeBSD
// 安装盘的典型结构），所以实在没有线索时直接叫 FreeBSD，比用文件名更有用。
func (r *Reader) bsdName(img *model.ISO) string {
	if s := r.readText("/etc/os-release"); s != "" {
		if name := parseOSRelease(s); name != "" {
			return name
		}
	}
	if v := strings.TrimSpace(r.readText("/etc/version")); v != "" {
		return "FreeBSD " + v
	}
	// 卷标识 / 文件名里带 BSD 的，直接用它（如 "FreeBSD-14.0-RELEASE-amd64"）。
	if v := strings.TrimSpace(r.VolumeID()); v != "" && strings.Contains(strings.ToLower(v), "bsd") {
		return v
	}
	if strings.Contains(strings.ToLower(img.Name), "bsd") {
		return strings.TrimSuffix(img.Name, filepath.Ext(img.Name))
	}
	return "FreeBSD"
}

// genericName 给识别不出家族的镜像一个名字。
func (r *Reader) genericName(img *model.ISO) string {
	if v := strings.TrimSpace(r.VolumeID()); v != "" && !looksGenericLabel(v) {
		return v
	}
	return strings.TrimSuffix(img.Name, filepath.Ext(img.Name))
}

// readText 读取 ISO 内小文本文件，失败返回空串。
func (r *Reader) readText(p string) string {
	return r.readTextLimit(p, maxTextProbe)
}

// readTextLimit 读取 ISO 内文本文件的前 limit 字节，失败返回空串。
func (r *Reader) readTextLimit(p string, limit int64) string {
	b, err := r.ReadFile(p, limit)
	if err != nil || len(b) == 0 {
		return ""
	}
	return string(b)
}

// parseDiskInfo 解析 Ubuntu/Debian 的 .disk/info：
//
//	Ubuntu 24.04.2 LTS "Noble Numbat" - Release amd64 (20250214)
//	Debian GNU/Linux 12.5.0 "Bookworm" - Official amd64 NETINST (2024-02-10)
func parseDiskInfo(s string) string {
	line := strings.TrimSpace(strings.SplitN(strings.TrimSpace(s), "\n", 2)[0])
	if line == "" {
		return ""
	}
	// 去掉 " - Release ..." / " - Official ..." 之类的尾巴。
	for _, sep := range []string{" - Release", " - Official", " - "} {
		if i := strings.Index(line, sep); i > 0 {
			line = strings.TrimSpace(line[:i])
			break
		}
	}
	// 去掉引号里的代号："Noble Numbat"
	if i := strings.IndexByte(line, '"'); i > 0 {
		line = strings.TrimSpace(line[:i])
	}
	return line
}

// versionRe 匹配 "3.20.10" / "9.4" / "12" 这类版本号。
var versionRe = regexp.MustCompile("[0-9]+(?:\\.[0-9]+){0,2}")

// alpineVersion 从 Alpine 的 release 文件内容里提取纯版本号。
// 内容可能是 "3.20.10"，也可能是 "alpine-standard-3.20.10 240618"。
func alpineVersion(s string) string {
	// 优先匹配 "alpine-*-x.y.z" 里的版本段。
	if i := strings.Index(s, "alpine"); i >= 0 {
		if m := versionRe.FindString(s[i:]); m != "" {
			return m
		}
	}
	if m := versionRe.FindString(s); m != "" {
		return m
	}
	return s
}

// parseTreeInfo 解析 RHEL 系的 .treeinfo，返回 name 字段（可拼上 version）。
func parseTreeInfo(s string) string {
	name, version := "", ""
	for _, line := range strings.Split(s, "\n") {
		kv := strings.SplitN(strings.TrimSpace(line), "=", 2)
		if len(kv) != 2 {
			continue
		}
		k, v := strings.TrimSpace(kv[0]), strings.TrimSpace(kv[1])
		switch k {
		case "name":
			name = v
		case "version":
			version = v
		}
	}
	if name == "" {
		return ""
	}
	if version != "" && !strings.Contains(name, version) {
		return name + " " + version
	}
	return name
}

// parseOSRelease 解析 /etc/os-release，返回 PRETTY_NAME 或 "NAME VERSION"。
func parseOSRelease(s string) string {
	pretty, name, version := "", "", ""
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		kv := strings.SplitN(line, "=", 2)
		if len(kv) != 2 {
			continue
		}
		k := strings.TrimSpace(kv[0])
		v := strings.Trim(strings.TrimSpace(kv[1]), "\"")
		switch k {
		case "PRETTY_NAME":
			pretty = v
		case "NAME":
			name = v
		case "VERSION_ID", "VERSION":
			version = v
		}
	}
	if pretty != "" {
		return pretty
	}
	if name == "" {
		return ""
	}
	if version != "" && !strings.Contains(name, version) {
		return name + " " + version
	}
	return name
}

// parseSUSEProduct 从 SUSE 的 /content 文件里提取产品名。
//
// 形如：
//
//	PRODUCT / SUSE Linux Enterprise Server 15 SP5
//	VERSION 15.5
func parseSUSEProduct(s string) string {
	for _, line := range strings.Split(s, "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) < 2 {
			continue
		}
		switch strings.ToUpper(fields[0]) {
		case "PRODUCT", "NAME":
			return strings.Join(fields[1:], " ")
		}
	}
	return ""
}

// parseWindowsProduct 从 pid.txt / ei.cfg 里提取 Windows 产品名。
func parseWindowsProduct(s string) string {
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "[") || strings.HasPrefix(line, ";") {
			continue
		}
		kv := strings.SplitN(line, "=", 2)
		if len(kv) == 2 && strings.EqualFold(strings.TrimSpace(kv[0]), "ProductName") {
			if v := strings.TrimSpace(kv[1]); v != "" {
				return v
			}
		}
	}
	// ei.cfg 里 Edition=Professional 之类
	lower := strings.ToLower(s)
	if i := strings.Index(lower, "edition="); i >= 0 {
		rest := s[i+len("edition="):]
		if j := strings.IndexAny(rest, "\r\n"); j >= 0 {
			rest = rest[:j]
		}
		if v := strings.TrimSpace(rest); v != "" {
			return "Windows " + v
		}
	}
	return ""
}

// wimNameRe 匹配 WIM 头里的 <DISPLAYNAME> / <NAME> / <DESCRIPTION> XML 元数据。
var wimNameRe = regexp.MustCompile("(?is)<(?:DISPLAYNAME|NAME|DESCRIPTION)>([^<]{1,120})</")

// parseWIMName 从 WIM 头部 XML 里提取发行版名字（如 "Windows 11 Pro"）。
func parseWIMName(s string) string {
	// WIM 头是 UTF-16LE 编码，先宽松解码再匹配。
	if m := wimNameRe.FindStringSubmatch(decodeUTF16LELoose(s)); len(m) == 2 {
		if name := strings.TrimSpace(m[1]); name != "" && !strings.EqualFold(name, "Microsoft Windows PE") {
			return name
		}
	}
	// 有些 WIM 直接存 ASCII。
	if m := wimNameRe.FindStringSubmatch(s); len(m) == 2 {
		if name := strings.TrimSpace(m[1]); name != "" {
			return name
		}
	}
	return ""
}

// decodeUTF16LELoose 把字节流里连续的 UTF-16LE 可打印字符解码成 UTF-8，
// 用于从二进制 WIM 头里提取 XML 元数据（非严格解码，够用即可）。
func decodeUTF16LELoose(s string) string {
	b := []byte(s)
	var sb strings.Builder
	sb.Grow(len(b) / 2)
	for i := 0; i+1 < len(b); i += 2 {
		r := uint16(b[i]) | uint16(b[i+1])<<8
		switch {
		case r == '<' || r == '>' || r == '/' || r == '=' || r == '"':
			sb.WriteRune(rune(r))
		case r >= 0x20 && r < 0x7F:
			sb.WriteRune(rune(r))
		case r == 0x0A || r == 0x0D || r == 0x09:
			sb.WriteRune(rune(r))
		default:
			sb.WriteByte(0)
		}
	}
	return sb.String()
}

// looksGenericLabel 判断卷标识是不是「无信息量的通用标签」。
func looksGenericLabel(s string) bool {
	u := strings.ToUpper(strings.TrimSpace(s))
	switch u {
	case "", "CDROM", "CD-ROM", "ISO", "ISOIMAGE", "MYISO", "NEW", "UDF", "BOOT", "DVD", "INSTALL":
		return true
	}
	// 纯数字标签也没意义。
	if _, err := strconv.Atoi(u); err == nil {
		return true
	}
	return false
}

// firstExisting 返回第一个在 ISO 内存在的路径，都不存在返回空串。
func firstExisting(r *Reader, paths ...string) string {
	for _, p := range paths {
		if r.Exists(p) {
			return p
		}
	}
	return ""
}

// ArchCompatible 判断镜像架构 img 能否被架构 client 的客户机引导。
//
// 映射规则（model.Arch 同时用于描述客户机固件和镜像架构）：
//   - 镜像 x86_64  → model.ArchUEFI64；可被 BIOS / UEFI64 / UEFI64HTTP 客户机引导
//   - 镜像 x86 32 位 → model.ArchUEFI32；只可被 IA32 UEFI 客户机引导
//   - 镜像 arm64   → model.ArchARM64；可被 ARM64 / ARM64HTTP 客户机引导
//   - 未知镜像架构 → 一律显示（宁可多显示也不漏）
func ArchCompatible(img, client model.Arch) bool {
	switch img {
	case model.ArchUEFI64, model.ArchBIOS:
		switch client {
		case model.ArchBIOS, model.ArchUEFI64, model.ArchUEFI64HTTP:
			return true
		}
		return false
	case model.ArchUEFI32:
		return client == model.ArchUEFI32
	case model.ArchARM64, model.ArchARM64HTTP:
		return client == model.ArchARM64 || client == model.ArchARM64HTTP
	case model.ArchLoongArch64:
		return client == model.ArchLoongArch64
	}
	return true
}

// AutoInstallFor 返回该镜像推荐的无人值守安装方式，供上层在用户启用自动安装时使用。
// Probe 本身不填 ISO.Boot.AutoInstall：自动安装是用户的显式选择，
// 不能让每张 ISO 默认都走无人值守。
func AutoInstallFor(img model.ISO) *model.AutoInstall {
	switch img.Family {
	case model.FamilyWindows, model.FamilyWinPE:
		return &model.AutoInstall{Kind: "autounattend", Template: "autounattend.xml"}
	case model.FamilyLinux:
		name := strings.ToLower(img.OSName)
		switch {
		case strings.Contains(name, "ubuntu"), strings.Contains(name, "debian"):
			return &model.AutoInstall{Kind: "preseed", Template: "preseed.cfg"}
		case strings.Contains(name, "suse"):
			return &model.AutoInstall{Kind: "autoyast", Template: "autoyast.xml"}
		case strings.Contains(name, "arch"), strings.Contains(name, "alpine"):
			return &model.AutoInstall{Kind: "cloudinit", Template: "cloud-init.yaml"}
		default:
			return &model.AutoInstall{Kind: "kickstart", Template: "ks.cfg"}
		}
	}
	return nil
}

// familyString 返回家族的中文调试描述。
func familyString(f model.ISOFamily) string {
	switch f {
	case model.FamilyWindows:
		return "Windows"
	case model.FamilyWinPE:
		return "WinPE"
	case model.FamilyLinux:
		return "Linux"
	case model.FamilyESXi:
		return "ESXi"
	case model.FamilyBSD:
		return "BSD"
	}
	return "其它"
}

// describe 生成一行人类可读的探测结果（日志用）。
func describe(img *model.ISO) string {
	if img.Error != "" {
		return fmt.Sprintf("%s: 解析失败（%s）", img.Name, img.Error)
	}
	return fmt.Sprintf("%s: %s / %s / %s", img.Name, familyString(img.Family), img.OSName, img.Boot.Method)
}