package boot

import (
	"strings"
	"testing"

	"github.com/wupengcheng/netbootx/internal/model"
)

// ---- 测试夹具 ----

func uefiClient() *model.Client {
	return &model.Client{
		MAC:      "52:54:00:12:34:56",
		IP:       "192.168.123.200",
		Hostname: "pxe-test",
		Arch:     model.ArchUEFI64,
	}
}

func testSettings() model.Settings {
	s := model.DefaultSettings()
	s.ServerIP = "192.168.123.10"
	s.HTTPPort = 16000
	s.BootMenuTimeout = 30
	s.DefaultISO = "ubuntu2404"
	return s
}

func ubuntuISO() model.ISO {
	return model.ISO{
		ID:       "ubuntu2404",
		Path:     "/srv/netbootx/iso/ubuntu-24.04.2-desktop-amd64.iso",
		RelPath:  "ubuntu-24.04.2-desktop-amd64.iso",
		Name:     "ubuntu-24.04.2-desktop-amd64.iso",
		Size:     3113851289,
		Family:   model.FamilyLinux,
		OSName:   "Ubuntu 24.04.2 LTS",
		OSArch:   model.ArchUEFI64,
		Bootable: true,
		Boot: model.BootSpec{
			Method:     MethodKernel,
			Kernel:     "/casper/vmlinuz",
			Initrd:     []string{"/casper/initrd"},
			KernelArgs: []string{"boot=casper", "iso-scan/filename={iso}", "quiet", "splash", "---"},
		},
	}
}

func windowsISO() model.ISO {
	return model.ISO{
		ID:       "win11",
		Path:     "/srv/netbootx/iso/Win11_24H2_x64.iso",
		RelPath:  "Win11_24H2_x64.iso",
		Name:     "Win11_24H2_x64.iso",
		Size:     5798205849,
		Family:   model.FamilyWindows,
		OSName:   "Windows 11 24H2",
		OSArch:   model.ArchUEFI64,
		Bootable: true,
		Boot: model.BootSpec{
			Method:  MethodWimboot,
			BootMGR: "/bootmgr",
			BCD:     "/boot/bcd",
			BootSDI: "/boot/boot.sdi",
			BootWIM: "/sources/boot.wim",
		},
	}
}

func arm64ISO() model.ISO {
	return model.ISO{
		ID:      "ubuntu2404arm",
		RelPath: "ubuntu-24.04.2-live-server-arm64.iso",
		Name:    "ubuntu-24.04.2-live-server-arm64.iso",
		Size:    2040109465,
		Family:  model.FamilyLinux,
		OSName:  "Ubuntu 24.04.2 LTS (arm64)",
		OSArch:  model.ArchARM64,
		Boot: model.BootSpec{
			Method:     MethodKernel,
			Kernel:     "/casper/vmlinuz",
			Initrd:     []string{"/casper/initrd"},
			KernelArgs: []string{"boot=casper", "---"},
		},
	}
}

// ---- 快照测试：完整脚本必须逐字节稳定 ----

const snapshotScript = `#!ipxe
# NetBootX iPXE 启动脚本（由服务端自动生成，请勿手工编辑）
# 客户机: 52:54:00:12:34:56 / pxe-test / UEFI x86_64
# 服务端: http://192.168.123.10:16000
dhcp || goto netbootx_failed
set base http://192.168.123.10:16000
menu NetBootX 启动菜单 - 52:54:00:12:34:56
item --gap --  Windows
item iso_win11 1) Windows 11 24H2 - Win11_24H2_x64.iso (5.4GB)
item --gap --  Linux
item iso_ubuntu2404 2) Ubuntu 24.04.2 LTS - ubuntu-24.04.2-desktop-amd64.iso (2.9GB)
choose --timeout 30000 --default iso_ubuntu2404 target || goto netbootx_cancelled
goto ${target}
:iso_win11
kernel wimboot
initrd --name bootmgr  ${base}/iso/win11/bootmgr
initrd --name bcd      ${base}/iso/win11/boot/bcd
initrd --name boot.sdi ${base}/iso/win11/boot/boot.sdi
initrd --name boot.wim ${base}/iso/win11/sources/boot.wim
boot || goto netbootx_failed

:iso_ubuntu2404
kernel ${base}/iso/ubuntu2404/casper/vmlinuz boot=casper iso-scan/filename=http://192.168.123.10:16000/iso/ubuntu2404/ubuntu-24.04.2-desktop-amd64.iso quiet splash ---
initrd ${base}/iso/ubuntu2404/casper/initrd
boot || goto netbootx_failed

:netbootx_failed
echo 引导失败，5 秒后返回启动菜单
sleep 5
dhcp || goto netbootx_cancelled
chain http://192.168.123.10:16000/boot/menu.ipxe?mac=52:54:00:12:34:56

:netbootx_cancelled
echo 已取消启动，进入 iPXE 命令行
shell` + "\n"

func TestScriptSnapshot(t *testing.T) {
	isos := []model.ISO{arm64ISO(), windowsISO(), ubuntuISO()}
	got := Script(uefiClient(), isos, testSettings(), "http://192.168.123.10:16000")
	if got != snapshotScript {
		t.Fatalf("生成的脚本与快照不一致。\n===== 实际输出 =====\n%s\n===== 期望输出 =====\n%s", got, snapshotScript)
	}
}

func TestScriptIsDeterministic(t *testing.T) {
	isos := []model.ISO{arm64ISO(), windowsISO(), ubuntuISO()}
	a := Script(uefiClient(), isos, testSettings(), "")
	b := Script(uefiClient(), isos, testSettings(), "")
	if a != b {
		t.Fatal("同样的输入两次生成的脚本不一致")
	}
	// 输入切片顺序打乱后，输出也应保持一致（内部会排序）。
	c := Script(uefiClient(), []model.ISO{ubuntuISO(), arm64ISO(), windowsISO()}, testSettings(), "")
	if a != c {
		t.Fatal("ISO 输入顺序影响了脚本输出，排序不稳定")
	}
}

// ---- 架构过滤 ----

func TestFilterByArch(t *testing.T) {
	isos := []model.ISO{ubuntuISO(), windowsISO(), arm64ISO()}

	x86 := Filter(uefiClient(), isos)
	if len(x86) != 2 {
		t.Fatalf("x86_64 客户机应看到 2 个 ISO，实际 %d 个", len(x86))
	}
	for _, is := range x86 {
		if is.ID == "ubuntu2404arm" {
			t.Fatal("x86_64 客户机不应看到 arm64 镜像")
		}
	}

	arm := Filter(&model.Client{MAC: "00:11:22:33:44:55", Arch: model.ArchARM64}, isos)
	if len(arm) != 1 || arm[0].ID != "ubuntu2404arm" {
		t.Fatalf("arm64 客户机应只看到 arm64 镜像，实际 %+v", arm)
	}

	// BIOS 与 UEFI x86 属于同一架构大类，看到的东西必须一样。
	bios := Filter(&model.Client{Arch: model.ArchBIOS}, isos)
	if len(bios) != 2 {
		t.Fatalf("Legacy BIOS 客户机应看到 2 个 x86 ISO，实际 %d 个", len(bios))
	}
}

func TestFilterUnknownClientSeesEverything(t *testing.T) {
	isos := []model.ISO{ubuntuISO(), windowsISO(), arm64ISO()}
	if got := Filter(nil, isos); len(got) != 3 {
		t.Fatalf("架构未知的客户机应看到全部 3 个 ISO，实际 %d 个", len(got))
	}
	if got := Filter(&model.Client{Arch: model.ArchUnknown}, isos); len(got) != 3 {
		t.Fatalf("ArchUnknown 客户机应看到全部 3 个 ISO，实际 %d 个", len(got))
	}
}

func TestScriptHidesArm64FromX86(t *testing.T) {
	script := Script(uefiClient(), []model.ISO{ubuntuISO(), arm64ISO()}, testSettings(), "http://10.0.0.1:16000")
	if strings.Contains(script, "ubuntu2404arm") || strings.Contains(script, "arm64") {
		t.Fatalf("x86_64 客户机的脚本里出现了 arm64 镜像：\n%s", script)
	}
}

// ---- 菜单结构 ----

func TestScriptMenuStructure(t *testing.T) {
	script := Script(uefiClient(), []model.ISO{ubuntuISO(), windowsISO()}, testSettings(), "http://192.168.123.10:16000")

	mustContain := []string{
		"#!ipxe\n",
		"dhcp || goto netbootx_failed\n",
		"set base http://192.168.123.10:16000\n",
		"menu NetBootX 启动菜单 - 52:54:00:12:34:56\n",
		"item --gap --  Windows\n",
		"item --gap --  Linux\n",
		"choose --timeout 30000 --default iso_ubuntu2404 target || goto netbootx_cancelled\n",
		"goto " + "${target}" + "\n",
		":iso_win11\n",
		":iso_ubuntu2404\n",
		"boot || goto netbootx_failed\n",
	}
	for _, s := range mustContain {
		if !strings.Contains(script, s) {
			t.Errorf("脚本缺少片段 %q\n完整脚本：\n%s", s, script)
		}
	}

	// Windows 分组必须排在 Linux 之前。
	if strings.Index(script, "item --gap --  Windows") > strings.Index(script, "item --gap --  Linux") {
		t.Error("Windows 分组应排在 Linux 之前")
	}

	// 菜单项文本：序号 + OS 名 + 文件名 + 大小。
	if !strings.Contains(script, "1) Windows 11 24H2 - Win11_24H2_x64.iso (5.4GB)") {
		t.Errorf("Windows 菜单项格式不对：\n%s", script)
	}
	if !strings.Contains(script, "2) Ubuntu 24.04.2 LTS - ubuntu-24.04.2-desktop-amd64.iso (2.9GB)") {
		t.Errorf("Linux 菜单项格式不对：\n%s", script)
	}

	// 菜单标签里不能有空格。
	for _, line := range strings.Split(script, "\n") {
		if !strings.HasPrefix(line, "item ") || strings.HasPrefix(line, "item --gap") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		if strings.ContainsAny(fields[1], " \t") {
			t.Errorf("菜单标签含空格：%q", fields[1])
		}
	}
}

func TestScriptEmptyISOList(t *testing.T) {
	script := Script(uefiClient(), nil, testSettings(), "")
	for _, want := range []string{"#!ipxe", "echo NetBootX: 没有可用的 ISO 镜像", "shell"} {
		if !strings.Contains(script, want) {
			t.Errorf("空列表脚本缺少 %q：\n%s", want, script)
		}
	}
	if strings.Contains(script, "choose ") {
		t.Error("没有 ISO 时不应生成 choose 菜单")
	}
}

// ---- 超时与默认项 ----

func TestTimeoutFlag(t *testing.T) {
	cases := []struct {
		seconds int
		want    string
	}{
		{30, " --timeout 30000"},
		{0, ""},
		{-1, ""},
		{1, " --timeout 1000"},
	}
	for _, tc := range cases {
		if got := timeoutFlag(tc.seconds); got != tc.want {
			t.Errorf("timeoutFlag(%d) = %q，期望 %q", tc.seconds, got, tc.want)
		}
	}
}

func TestDefaultSelection(t *testing.T) {
	isos := []model.ISO{ubuntuISO(), windowsISO()}

	// 服务端默认 ISO。
	s := testSettings()
	script := Script(uefiClient(), isos, s, "http://10.0.0.1:16000")
	if !strings.Contains(script, "--default iso_ubuntu2404") {
		t.Errorf("应默认选中 Settings.DefaultISO：\n%s", script)
	}

	// 客户机自己的选择优先于服务端默认值。
	c := uefiClient()
	c.Selected = "win11"
	script = Script(c, isos, s, "http://10.0.0.1:16000")
	if !strings.Contains(script, "--default iso_win11") {
		t.Errorf("客户机上次选择应优先：\n%s", script)
	}

	// 都没有时退回菜单第一项（Windows 排在 Linux 前面）。
	s.DefaultISO = ""
	script = Script(uefiClient(), isos, s, "http://10.0.0.1:16000")
	if !strings.Contains(script, "--default iso_win11") {
		t.Errorf("无默认配置时应选菜单第一项：\n%s", script)
	}

	// 默认 ISO 不存在时同样退回第一项。
	s.DefaultISO = "not-exist"
	script = Script(uefiClient(), isos, s, "http://10.0.0.1:16000")
	if !strings.Contains(script, "--default iso_win11") {
		t.Errorf("默认 ISO 不存在时应退回第一项：\n%s", script)
	}

	// 超时为 0 时不生成 --timeout。
	s.BootMenuTimeout = 0
	script = Script(uefiClient(), isos, s, "http://10.0.0.1:16000")
	if strings.Contains(script, "--timeout") {
		t.Errorf("超时为 0 时不应出现 --timeout：\n%s", script)
	}
}

// ---- Linux kernel 引导 ----

func TestKernelBootLines(t *testing.T) {
	script := Script(uefiClient(), []model.ISO{ubuntuISO()}, testSettings(), "http://192.168.123.10:16000")
	want := ":iso_ubuntu2404\n" +
		"kernel ${base}/iso/ubuntu2404/casper/vmlinuz boot=casper iso-scan/filename=http://192.168.123.10:16000/iso/ubuntu2404/ubuntu-24.04.2-desktop-amd64.iso quiet splash ---\n" +
		"initrd ${base}/iso/ubuntu2404/casper/initrd\n" +
		"boot || goto netbootx_failed\n"
	if !strings.Contains(script, want) {
		t.Fatalf("Linux 引导段不正确。\n期望包含：\n%s\n实际脚本：\n%s", want, script)
	}
	// 多个 initrd 必须都生成。
	is := ubuntuISO()
	is.Boot.Initrd = []string{"/casper/initrd", "/casper/initrd.gz"}
	script = Script(uefiClient(), []model.ISO{is}, testSettings(), "http://192.168.123.10:16000")
	if !strings.Contains(script, "initrd ${base}/iso/ubuntu2404/casper/initrd\ninitrd ${base}/iso/ubuntu2404/casper/initrd.gz\n") {
		t.Errorf("多 initrd 未全部生成：\n%s", script)
	}
}

func TestKernelArgsPlaceholders(t *testing.T) {
	is := ubuntuISO()
	is.Boot.KernelArgs = []string{
		"boot=casper",
		"iso-scan/filename={iso}",
		"url={base}/auto/mac={mac}",
		"host={hostname}",
		"port={port}",
		"---",
	}
	script := Script(uefiClient(), []model.ISO{is}, testSettings(), "http://192.168.123.10:16000")
	for _, want := range []string{
		"iso-scan/filename=http://192.168.123.10:16000/iso/ubuntu2404/ubuntu-24.04.2-desktop-amd64.iso",
		"url=http://192.168.123.10:16000/auto/mac=52:54:00:12:34:56",
		"host=pxe-test",
		"port=16000",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("占位符未展开：%q\n%s", want, script)
		}
	}
}

// ---- Windows wimboot 引导 ----

func TestWimbootBootLines(t *testing.T) {
	script := Script(uefiClient(), []model.ISO{windowsISO()}, testSettings(), "http://192.168.123.10:16000")
	want := ":iso_win11\n" +
		"kernel wimboot\n" +
		"initrd --name bootmgr  ${base}/iso/win11/bootmgr\n" +
		"initrd --name bcd      ${base}/iso/win11/boot/bcd\n" +
		"initrd --name boot.sdi ${base}/iso/win11/boot/boot.sdi\n" +
		"initrd --name boot.wim ${base}/iso/win11/sources/boot.wim\n" +
		"boot || goto netbootx_failed\n"
	if !strings.Contains(script, want) {
		t.Fatalf("wimboot 引导段不正确。\n期望包含：\n%s\n实际脚本：\n%s", want, script)
	}
}

func TestWimbootDefaultPaths(t *testing.T) {
	// iso 包只填了 Method，其余路径为空时按 Windows 安装盘标准布局补齐。
	is := windowsISO()
	is.Boot = model.BootSpec{Method: MethodWimboot}
	script := Script(uefiClient(), []model.ISO{is}, testSettings(), "http://10.0.0.1:16000")
	for _, want := range []string{
		"initrd --name bootmgr  ${base}/iso/win11/bootmgr",
		"initrd --name bcd      ${base}/iso/win11/boot/bcd",
		"initrd --name boot.sdi ${base}/iso/win11/boot/boot.sdi",
		"initrd --name boot.wim ${base}/iso/win11/sources/boot.wim",
		"# 提示:",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("wimboot 默认路径缺少 %q：\n%s", want, script)
		}
	}
}

func TestWinPEFamilyUsesWimboot(t *testing.T) {
	is := windowsISO()
	is.ID = "winpe"
	is.Family = model.FamilyWinPE
	is.Boot = model.BootSpec{Method: ""} // 方法为空时按家族推断
	script := Script(uefiClient(), []model.ISO{is}, testSettings(), "http://10.0.0.1:16000")
	if !strings.Contains(script, "kernel wimboot") {
		t.Errorf("WinPE 家族应自动使用 wimboot：\n%s", script)
	}
}

// ---- 自动安装 ----

func TestAutoInstallKickstart(t *testing.T) {
	is := ubuntuISO()
	is.Family = model.FamilyLinux
	is.Boot.AutoInstall = &model.AutoInstall{Kind: "kickstart", Template: "ks/rocky9.ks"}
	script := Script(uefiClient(), []model.ISO{is}, testSettings(), "http://192.168.123.10:16000")
	want := "inst.ks=http://192.168.123.10:16000/user/ks/rocky9.ks ---"
	if !strings.Contains(script, want) {
		t.Fatalf("自动安装参数未追加：期望 %q\n实际：\n%s", want, script)
	}
	// "---" 必须仍然在最后。
	kernelLine := ""
	for _, line := range strings.Split(script, "\n") {
		if strings.HasPrefix(line, "kernel ") {
			kernelLine = line
		}
	}
	if !strings.HasSuffix(kernelLine, " ---") {
		t.Errorf("--- 必须位于内核命令行末尾：%q", kernelLine)
	}
}

func TestAutoInstallVariants(t *testing.T) {
	cases := []struct {
		kind     string
		template string
		want     string
	}{
		{"kickstart", "ai/ks.cfg", "inst.ks=http://10.0.0.1:16000/user/ai/ks.cfg"},
		{"preseed", "ai/preseed.cfg", "preseed/url=http://10.0.0.1:16000/user/ai/preseed.cfg"},
		{"autoyast", "ai/autoinst.xml", "autoyast=http://10.0.0.1:16000/user/ai/autoinst.xml"},
		{"autoinstall", "ai/", "autoinstall ds=nocloud-net;s=http://10.0.0.1:16000/user/ai/"},
		{"cloudinit", "ai/", "ds=nocloud-net;s=http://10.0.0.1:16000/user/ai/"},
		{"autounattend", "ai/autounattend.xml", "http://10.0.0.1:16000/user/ai/autounattend.xml"},
	}
	for _, tc := range cases {
		is := ubuntuISO()
		is.Boot.AutoInstall = &model.AutoInstall{Kind: tc.kind, Template: tc.template}
		script := Script(uefiClient(), []model.ISO{is}, testSettings(), "http://10.0.0.1:16000")
		if !strings.Contains(script, tc.want) {
			t.Errorf("kind=%s 期望包含 %q，实际：\n%s", tc.kind, tc.want, script)
		}
	}
}

func TestAutoInstallNotDuplicated(t *testing.T) {
	is := ubuntuISO()
	is.Boot.AutoInstall = &model.AutoInstall{
		Kind:       "kickstart",
		Template:   "ks/rocky9.ks",
		KernelArgs: []string{"inst.ks={autoinstall}", "inst.text"},
	}
	script := Script(uefiClient(), []model.ISO{is}, testSettings(), "http://10.0.0.1:16000")
	if n := strings.Count(script, "inst.ks="); n != 1 {
		t.Fatalf("inst.ks 参数重复了 %d 次：\n%s", n, script)
	}
	if !strings.Contains(script, "inst.text") {
		t.Errorf("模板自带的内核参数丢失：\n%s", script)
	}
}

func TestAutoInstallAutounattendInjectWimboot(t *testing.T) {
	is := windowsISO()
	is.Boot.AutoInstall = &model.AutoInstall{Kind: "autounattend", Template: "windows/win11.xml"}
	script := Script(uefiClient(), []model.ISO{is}, testSettings(), "http://10.0.0.1:16000")
	want := "initrd --name autounattend.xml http://10.0.0.1:16000/user/windows/win11.xml\nboot || goto netbootx_failed\n"
	if !strings.Contains(script, want) {
		t.Fatalf("autounattend 未注入 wimboot 序列：\n%s", script)
	}
}

// ---- Entry ----

// TestEntry 验证 Entry 生成的是「完整独立」的 iPXE 脚本：
// 带 #!ipxe 头、自己 dhcp、自己 set base，可以被 httpd 直接回给客户机。
func TestEntry(t *testing.T) {
	entry := Entry(uefiClient(), ubuntuISO(), testSettings(), "http://192.168.123.10:16000")
	if !strings.HasPrefix(entry, "#!ipxe\n") {
		t.Fatalf("Entry 必须以 #!ipxe 开头（否则 iPXE 无法解析），实际开头：%q", firstLine(entry))
	}
	for _, want := range []string{
		"dhcp || goto netbootx_failed\n",
		"set base http://192.168.123.10:16000\n",
		":iso_ubuntu2404\n",
		"kernel ${base}/iso/ubuntu2404/casper/vmlinuz",
		"initrd ${base}/iso/ubuntu2404/casper/initrd",
		"boot || goto netbootx_failed\n",
		// 引导失败要能回到菜单，不能把客户机丢在 iPXE shell 里。
		"chain ${base}/boot/menu.ipxe?mac=52:54:00:12:34:56\n",
	} {
		if !strings.Contains(entry, want) {
			t.Errorf("Entry 缺少 %q：\n%s", want, entry)
		}
	}
	// set base 必须出现在 kernel 之前，否则变量还没定义。
	if strings.Index(entry, "set base ") > strings.Index(entry, "kernel ") {
		t.Errorf("set base 必须在 kernel 之前：\n%s", entry)
	}
	// 脚本里不应残留未展开的 Go 格式化占位。
	if strings.Contains(entry, "%s") {
		t.Errorf("Entry 里出现格式化残留：\n%s", entry)
	}
}

// TestEntryWimboot 验证直达脚本在 Windows 镜像上走 wimboot 分支。
func TestEntryWimboot(t *testing.T) {
	entry := Entry(uefiClient(), windowsISO(), testSettings(), "http://192.168.123.10:16000")
	if !strings.HasPrefix(entry, "#!ipxe\n") {
		t.Fatalf("Entry 必须以 #!ipxe 开头：%q", firstLine(entry))
	}
	for _, want := range []string{"kernel wimboot", "initrd --name boot.wim "} {
		if !strings.Contains(entry, want) {
			t.Errorf("Entry(windows) 缺少 %q：\n%s", want, entry)
		}
	}
}

// firstLine 返回字符串的第一行，用于失败信息。
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// ---- 转义与边界 ----

func TestSanitizeTextKeepsScriptSafe(t *testing.T) {
	is := ubuntuISO()
	is.OSName = "Evil && reboot || echo pwned"
	script := Script(uefiClient(), []model.ISO{is}, testSettings(), "http://10.0.0.1:16000")
	for _, line := range strings.Split(script, "\n") {
		if strings.HasPrefix(line, "item ") {
			if strings.Contains(line, "&&") || strings.Contains(line, "||") {
				t.Fatalf("菜单项里出现了会截断命令的字符：%q", line)
			}
		}
	}
}

func TestLabelsUnique(t *testing.T) {
	a := ubuntuISO()
	b := ubuntuISO()
	b.ID = "ubuntu2404" // 故意撞 ID，但文件名不同
	b.Name = "other.iso"
	used := map[string]bool{}
	l1 := uniqueLabel(a, used)
	l2 := uniqueLabel(b, used)
	if l1 == l2 {
		t.Fatalf("标签重复：%q", l1)
	}
	if strings.ContainsAny(l1+l2, " ") {
		t.Fatalf("标签里不能有空格：%q %q", l1, l2)
	}
}

func TestEscapePath(t *testing.T) {
	cases := []struct{ in, want string }{
		{"/casper/vmlinuz", "casper/vmlinuz"},
		{"casper/initrd", "casper/initrd"},
		{"/sources/boot.wim", "sources/boot.wim"},
		// "+" 在路径段里是合法字符（只有查询串里才表示空格），保持原样更可读。
		{"/a b/c+d.iso", "a%20b/c+d.iso"},
		{"", ""},
		{"/", ""},
		{"/Ubuntu 24.04/中文.iso", "Ubuntu%2024.04/%E4%B8%AD%E6%96%87.iso"},
	}
	for _, tc := range cases {
		if got := escapePath(tc.in); got != tc.want {
			t.Errorf("escapePath(%q) = %q，期望 %q", tc.in, got, tc.want)
		}
	}
}

func TestHumanSize(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{0, "0B"},
		{512, "512B"},
		{1024, "1.0KB"},
		{3113851289, "2.9GB"},
		{5798205849, "5.4GB"},
		{1024 * 1024, "1.0MB"},
	}
	for _, tc := range cases {
		if got := HumanSize(tc.in); got != tc.want {
			t.Errorf("HumanSize(%d) = %q，期望 %q", tc.in, got, tc.want)
		}
	}
}

func TestNormalizeBase(t *testing.T) {
	s := testSettings()
	if got := normalizeBase("http://1.2.3.4:8000/", s); got != "http://1.2.3.4:8000" {
		t.Errorf("传入 baseURL 时应去掉结尾斜杠，得到 %q", got)
	}
	if got := normalizeBase("", s); got != "http://192.168.123.10:16000" {
		t.Errorf("应由 Settings 推导，得到 %q", got)
	}
	s.ServerIP = ""
	s.HTTPPort = 0
	if got := normalizeBase("", s); got != "http://127.0.0.1:16000" {
		t.Errorf("空配置时应回落到 127.0.0.1:16000，得到 %q", got)
	}
	// IPv6 必须加方括号。
	s.ServerIP = "fd00::1"
	s.HTTPPort = 16000
	if got := normalizeBase("", s); got != "http://[fd00::1]:16000" {
		t.Errorf("IPv6 地址未加方括号：%q", got)
	}
}

func TestISOImagePath(t *testing.T) {
	cases := []struct {
		is   model.ISO
		want string
	}{
		{model.ISO{RelPath: "sub/a.iso"}, "sub/a.iso"},
		{model.ISO{Name: "a.iso"}, "a.iso"},
		{model.ISO{Path: "/srv/netbootx/iso/a.iso"}, "a.iso"},
		{model.ISO{}, ""},
	}
	for _, tc := range cases {
		if got := isoImagePath(tc.is); got != tc.want {
			t.Errorf("isoImagePath(%+v) = %q，期望 %q", tc.is, got, tc.want)
		}
	}
}

func TestSanbootAndMemdisk(t *testing.T) {
	sb := model.ISO{
		ID: "tiny", RelPath: "tiny.iso", Name: "tiny.iso",
		Boot: model.BootSpec{Method: MethodSanboot},
	}
	script := Script(uefiClient(), []model.ISO{sb}, testSettings(), "http://10.0.0.1:16000")
	if !strings.Contains(script, "sanboot --no-describe ${base}/iso/tiny/tiny.iso") {
		t.Errorf("sanboot 段不正确：\n%s", script)
	}

	md := model.ISO{
		ID: "floppy", RelPath: "floppy.img", Name: "floppy.img",
		Boot: model.BootSpec{Method: MethodMemdisk, Kernel: "/floppy.img"},
	}
	script = Script(uefiClient(), []model.ISO{md}, testSettings(), "http://10.0.0.1:16000")
	for _, want := range []string{"kernel memdisk", "initrd ${base}/iso/floppy/floppy.img", "boot"} {
		if !strings.Contains(script, want) {
			t.Errorf("memdisk 段缺少 %q：\n%s", want, script)
		}
	}
}

func TestUnbootableISOShowsHint(t *testing.T) {
	is := model.ISO{ID: "broken", Name: "broken.iso", Bootable: false}
	script := Script(uefiClient(), []model.ISO{is}, testSettings(), "http://10.0.0.1:16000")
	for _, want := range []string{"echo NetBootX: 无法引导", "shell"} {
		if !strings.Contains(script, want) {
			t.Errorf("不可引导的 ISO 应给出提示 %q：\n%s", want, script)
		}
	}
}

func TestNoFormatResidue(t *testing.T) {
	// 脚本里不应出现 fmt 格式化残留（说明有人把 %% 用错了）。
	script := Script(uefiClient(), []model.ISO{ubuntuISO(), windowsISO()}, testSettings(), "http://10.0.0.1:16000")
	if strings.Contains(script, "%s") || strings.Contains(script, "%d") {
		t.Errorf("脚本里出现格式化残留：\n%s", script)
	}
}

// TestScriptFailureFallbacks 验证脚本的失败兜底：引导失败回菜单、取消进命令行。
// 没有兜底时客户机会停在 iPXE 命令行，对无人值守场景是致命的。
func TestScriptFailureFallbacks(t *testing.T) {
	script := Script(uefiClient(), []model.ISO{ubuntuISO()}, testSettings(), "http://192.168.123.10:16000")
	for _, want := range []string{
		"boot || goto netbootx_failed",
		":netbootx_failed",
		"chain http://192.168.123.10:16000/boot/menu.ipxe?mac=52:54:00:12:34:56",
		":netbootx_cancelled",
		"choose --timeout 30000 --default iso_ubuntu2404 target || goto netbootx_cancelled",
		"goto ${target}",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("脚本缺少失败兜底片段 %q：\n%s", want, script)
		}
	}
	// 每个引导段的 boot 都必须带兜底，不能有裸 boot。
	for _, line := range strings.Split(script, "\n") {
		if line == "boot" {
			t.Errorf("出现裸 boot 命令（缺失败兜底）：\n%s", script)
			break
		}
	}
}

// TestEntryFailureFallback 验证直达脚本引导失败时也能回菜单。
func TestEntryFailureFallback(t *testing.T) {
	entry := Entry(uefiClient(), ubuntuISO(), testSettings(), "http://192.168.123.10:16000")
	for _, want := range []string{
		"boot || goto netbootx_failed",
		":netbootx_failed",
		"chain ${base}/boot/menu.ipxe?mac=52:54:00:12:34:56",
	} {
		if !strings.Contains(entry, want) {
			t.Errorf("Entry 缺少失败兜底 %q：\n%s", want, entry)
		}
	}
}

// TestScriptWithoutMAC 验证没有 MAC 时也不会生成坏 URL。
func TestScriptWithoutMAC(t *testing.T) {
	script := Script(nil, []model.ISO{ubuntuISO()}, testSettings(), "http://10.0.0.1:16000")
	if !strings.HasPrefix(script, "#!ipxe") {
		t.Fatalf("没有客户机时也要生成合法脚本：%q", script)
	}
	if !strings.Contains(script, "chain http://10.0.0.1:16000/boot/menu.ipxe?mac=\n") {
		t.Errorf("无 MAC 时应生成空 mac 参数而不是坏 URL：\n%s", script)
	}
}
