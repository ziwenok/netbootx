package boot

import (
	"strconv"
	"strings"

	"github.com/wupengcheng/netbootx/internal/model"
)

// 引导方式（model.BootSpec.Method）。
const (
	MethodKernel  = "kernel"
	MethodWimboot = "wimboot"
	MethodMemdisk = "memdisk"
	MethodSanboot = "sanboot"
)

// bootLines 生成「选中某个 ISO 之后要执行的全部 iPXE 命令」。
//
// base 是服务端基地址（不带结尾斜杠），脚本里统一用 ${base} 变量引用它，
// 这样菜单里每个条目的 URL 都很短，也便于人工阅读。
func bootLines(c *model.Client, is model.ISO, s model.Settings, base string) []string {
	method := resolveMethod(is)
	switch method {
	case "":
		// 既没有显式引导方式，也没有内核路径，且镜像本身不可引导：
		// 直接给出可读提示，别让客户机卡在 iPXE 里。
		return errorLines(is, "该镜像未被识别为可引导镜像")
	case MethodWimboot:
		return wimbootLines(c, is, s, base)
	case MethodKernel:
		return kernelLines(c, is, s, base)
	case MethodMemdisk:
		return memdiskLines(is, base)
	default:
		return sanbootLines(is, base)
	}
}

// resolveMethod 决定实际使用的引导方式。
// BootSpec.Method 为空时按家族与字段猜测，保证 iso 包只填了部分字段也能引导。
func resolveMethod(is model.ISO) string {
	switch is.Boot.Method {
	case MethodKernel, MethodWimboot, MethodMemdisk, MethodSanboot:
		return is.Boot.Method
	}
	switch is.Family {
	case model.FamilyWindows, model.FamilyWinPE:
		return MethodWimboot
	}
	if strings.TrimSpace(is.Boot.Kernel) != "" {
		return MethodKernel
	}
	if !is.Bootable {
		return ""
	}
	return MethodSanboot
}

// kernelLines 生成 Linux 的 kernel + initrd + boot 序列：
//
//	kernel ${base}/iso/abc123/casper/vmlinuz boot=casper iso-scan/filename=... quiet splash ---
//	initrd ${base}/iso/abc123/casper/initrd
//	boot
func kernelLines(c *model.Client, is model.ISO, s model.Settings, base string) []string {
	kernel := strings.TrimSpace(is.Boot.Kernel)
	if kernel == "" {
		return errorLines(is, "缺少内核路径")
	}
	args := kernelArgs(c, is, s, base)
	line := "kernel " + isoFileURL("${base}", is.ID, kernel)
	if len(args) > 0 {
		line += " " + strings.Join(args, " ")
	}
	lines := []string{line}
	for _, initrd := range is.Boot.Initrd {
		initrd = strings.TrimSpace(initrd)
		if initrd == "" {
			continue
		}
		lines = append(lines, "initrd "+isoFileURL("${base}", is.ID, initrd))
	}
	return append(lines, "boot")
}

// wimbootLines 生成 Windows / WinPE 的 wimboot 序列：
//
//	kernel wimboot
//	initrd --name bootmgr   ${base}/iso/def456/bootmgr
//	initrd --name bcd       ${base}/iso/def456/boot/bcd
//	initrd --name boot.sdi  ${base}/iso/def456/boot/boot.sdi
//	initrd --name boot.wim  ${base}/iso/def456/sources/boot.wim
//	boot
//
// 自动安装（autounattend）时额外注入一个名为 autounattend.xml 的 initrd：
// wimboot 会把它放进虚拟盘根目录，Windows Setup 启动时会自动找到它。
func wimbootLines(c *model.Client, is model.ISO, s model.Settings, base string) []string {
	spec := is.Boot
	files := []struct {
		name string
		path string
	}{
		{"bootmgr", spec.BootMGR},
		{"bcd", spec.BCD},
		{"boot.sdi", spec.BootSDI},
		{"boot.wim", spec.BootWIM},
	}
	// 缺省路径按 Windows 安装盘的标准布局补齐，iso 包没填也能跑。
	defaults := []string{"/bootmgr", "/boot/bcd", "/boot/boot.sdi", "/sources/boot.wim"}
	lines := []string{"kernel wimboot"}
	missing := make([]string, 0, 4)
	for i, f := range files {
		p := strings.TrimSpace(f.path)
		if p == "" {
			p = defaults[i]
			missing = append(missing, p)
		}
		lines = append(lines, "initrd --name "+padName(f.name)+isoFileURL("${base}", is.ID, p))
	}
	if url := templateURL(base, spec.AutoInstall); url != "" && autoInstallKind(spec.AutoInstall) == "autounattend" {
		lines = append(lines, "initrd --name autounattend.xml "+url)
	}
	if len(missing) > 0 {
		// 缺省路径只是兜底猜测，缺文件时给出可读提示，而不是让 iPXE 报一串 404。
		lines = append(lines, "# 提示: 以下路径为默认猜测，若返回 404 请检查该 ISO 是否为 Windows 安装盘: "+strings.Join(missing, " "))
	}
	return append(lines, "boot")
}

// memdiskLines 把小镜像整体载入内存引导。
func memdiskLines(is model.ISO, base string) []string {
	img := strings.TrimSpace(is.Boot.Kernel)
	if img == "" && len(is.Boot.Initrd) > 0 {
		img = strings.TrimSpace(is.Boot.Initrd[0])
	}
	if img == "" {
		return errorLines(is, "memdisk 方式缺少镜像路径")
	}
	return []string{
		"kernel memdisk",
		"initrd " + isoFileURL("${base}", is.ID, img),
		"boot",
	}
}

// sanbootLines 直接用 HTTP 挂载整个 ISO 引导（部分 Live 镜像支持）。
func sanbootLines(is model.ISO, base string) []string {
	img := isoImagePath(is)
	if img == "" {
		return errorLines(is, "无法确定 ISO 镜像路径")
	}
	return []string{
		"sanboot --no-describe " + isoFileURL("${base}", is.ID, img),
	}
}

// errorLines 生成「这个 ISO 引导不了」的可读提示，而不是让客户机挂死。
func errorLines(is model.ISO, reason string) []string {
	return []string{
		"echo NetBootX: 无法引导 " + sanitizeText(displayName(is)) + "（" + sanitizeText(reason) + "）",
		"echo 请检查该 ISO 是否可引导，或在 Web 界面查看解析日志",
		"sleep 5",
		"shell",
	}
}

// kernelArgs 拼装最终的内核命令行：发行版默认参数 + 自动安装参数 + 模板 URL。
func kernelArgs(c *model.Client, is model.ISO, s model.Settings, base string) []string {
	args := make([]string, 0, len(is.Boot.KernelArgs)+len(autoInstallKernelArgs(is.Boot.AutoInstall)))
	for _, a := range is.Boot.KernelArgs {
		if v := expandArg(a, c, is, s, base); v != "" {
			args = append(args, v)
		}
	}
	ai := is.Boot.AutoInstall
	if ai == nil {
		return args
	}
	for _, a := range ai.KernelArgs {
		if v := expandArg(a, c, is, s, base); v != "" {
			args = append(args, v)
		}
	}
	url := templateURL(base, ai)
	if url == "" {
		return args
	}
	// 模板里已经用 {autoinstall} 占位符引用过就不用再追加，避免参数重复。
	if containsAny(args, url) {
		return args
	}
	return appendArg(args, autoInstallArg(autoInstallKind(ai), url))
}

// autoInstallKernelArgs 安全返回自动安装参数（nil 时返回 nil）。
// 用独立函数而不是给 model 加方法，是为了不修改共享数据结构。
func autoInstallKernelArgs(ai *model.AutoInstall) []string {
	if ai == nil {
		return nil
	}
	return ai.KernelArgs
}

// autoInstallKind 返回规范化的自动安装方式（小写），nil 返回空串。
func autoInstallKind(ai *model.AutoInstall) string {
	if ai == nil {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(ai.Kind))
}

// autoInstallArg 把模板 URL 翻译成对应发行版的内核参数。
func autoInstallArg(kind, url string) string {
	switch kind {
	case "kickstart":
		return "inst.ks=" + url
	case "preseed":
		return "preseed/url=" + url
	case "autoyast":
		return "autoyast=" + url
	case "autoinstall":
		// Ubuntu Subiquity：ds=nocloud-net 的 s= 后面必须是目录 URL（以 / 结尾）。
		return "autoinstall ds=nocloud-net;s=" + strings.TrimRight(url, "/") + "/"
	case "cloudinit":
		return "ds=nocloud-net;s=" + strings.TrimRight(url, "/") + "/"
	default:
		return url
	}
}

// appendArg 追加一个内核参数；"---" 是 cmdline 分隔符，必须永远排在最后。
func appendArg(args []string, arg string) []string {
	if arg == "" {
		return args
	}
	for i, a := range args {
		if a == "---" {
			out := make([]string, 0, len(args)+1)
			out = append(out, args[:i]...)
			out = append(out, arg)
			out = append(out, args[i:]...)
			return out
		}
	}
	return append(args, arg)
}

// containsAny 报告 args 里是否已有参数包含 sub。
func containsAny(args []string, sub string) bool {
	for _, a := range args {
		if strings.Contains(a, sub) {
			return true
		}
	}
	return false
}

// expandArg 展开内核参数里的占位符。
//
// 支持的占位符：
//
//	{iso}         ISO 镜像自身的下载地址（iso-scan/filename= 需要的那个）
//	{id}          ISO 的短 ID
//	{base}        服务端基地址，不带结尾斜杠
//	{server}      服务端 IP
//	{port}        HTTP 端口
//	{mac}         客户机 MAC（小写冒号分隔）
//	{hostname}    客户机主机名
//	{autoinstall} 自动安装模板的下载地址
//	{kernel}      内核文件的下载地址
//	{initrd}      第一个 initrd 的下载地址
func expandArg(arg string, c *model.Client, is model.ISO, s model.Settings, base string) string {
	arg = strings.TrimSpace(arg)
	if arg == "" {
		return ""
	}
	if !strings.Contains(arg, "{") {
		return arg
	}
	host := strings.TrimSpace(s.ServerIP)
	port := s.HTTPPort
	if port <= 0 {
		port = DefaultHTTPPort
	}
	mac, hostname := "", ""
	if c != nil {
		mac = model.NormalizeMAC(c.MAC)
		hostname = strings.TrimSpace(c.Hostname)
	}
	initrd := ""
	if len(is.Boot.Initrd) > 0 {
		initrd = isoFileURL(base, is.ID, is.Boot.Initrd[0])
	}
	replacer := strings.NewReplacer(
		"{iso}", isoFileURL(base, is.ID, isoImagePath(is)),
		"{id}", is.ID,
		"{base}", base,
		"{server}", host,
		"{port}", strconv.Itoa(port),
		"{mac}", mac,
		"{hostname}", hostname,
		"{autoinstall}", templateURL(base, is.Boot.AutoInstall),
		"{kernel}", isoFileURL(base, is.ID, is.Boot.Kernel),
		"{initrd}", initrd,
	)
	return strings.TrimSpace(replacer.Replace(arg))
}

// isoImagePath 返回 ISO 镜像自身在 /iso/{id}/ 下的路径。
// 优先用相对 iso 根目录的路径（支持子目录），否则退回文件名。
func isoImagePath(is model.ISO) string {
	for _, p := range []string{is.RelPath, is.Name, is.Path} {
		p = strings.TrimSpace(strings.ReplaceAll(p, "\\", "/"))
		if p == "" {
			continue
		}
		if i := strings.LastIndex(p, "/iso/"); i >= 0 {
			// 绝对路径里取 iso 根目录之后的部分。
			p = p[i+len("/iso/"):]
		}
		if p = strings.Trim(p, "/"); p != "" {
			return p
		}
	}
	return ""
}

// padName 把 initrd 的 --name 参数对齐，纯为输出可读性（iPXE 不关心空格）。
func padName(name string) string {
	const width = 9
	if len(name) >= width {
		return name + " "
	}
	return name + strings.Repeat(" ", width-len(name))
}
