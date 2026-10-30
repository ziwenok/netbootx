package iso

import (
	"strings"
	"testing"

	"github.com/wupengcheng/netbootx/internal/model"
)

// TestDetectMatrix 覆盖 ARCHITECTURE.md 3.3 列出的全部发行版路径特征。
// 每个用例造一个只含特征文件的最小镜像，断言家族 / 内核 / initrd / 内核参数。
func TestDetectMatrix(t *testing.T) {
	tests := []struct {
		name     string
		files    map[string]string // 路径 -> 内容
		family   model.ISOFamily
		arch     model.Arch
		method   string
		kernel   string
		initrd   []string
		args     []string // 必须包含的内核参数（精确匹配）
		argPrefx []string // 必须包含的内核参数（前缀匹配）
		osName   string   // 期望 OSName 包含这个子串
	}{
		{
			name: "Ubuntu Live (casper)",
			files: map[string]string{
				"/.disk/info":      "Ubuntu 24.04.2 LTS \"Noble Numbat\" - Release amd64 (20250214)\n",
				"/casper/vmlinuz":  "K",
				"/casper/initrd":   "I",
			},
			family: model.FamilyLinux, arch: model.ArchUEFI64, method: "kernel",
			kernel: "/casper/vmlinuz", initrd: []string{"/casper/initrd"},
			args: []string{"boot=casper"}, argPrefx: []string{"iso-scan/filename="},
			osName: "Ubuntu 24.04.2 LTS",
		},
		{
			name: "Debian Live",
			files: map[string]string{
				"/.disk/info":       "Debian GNU/Linux 12.5.0 \"Bookworm\" - Official amd64 NETINST (2024-02-10)\n",
				"/live/vmlinuz":     "K",
				"/live/initrd.img":  "I",
			},
			family: model.FamilyLinux, arch: model.ArchUEFI64, method: "kernel",
			kernel: "/live/vmlinuz", initrd: []string{"/live/initrd.img"},
			args: []string{"boot=live"}, osName: "Debian GNU/Linux 12.5.0",
		},
		{
			name: "RHEL 安装盘 (images/pxeboot)",
			files: map[string]string{
				"/.treeinfo":                   "[general]\nname = Red Hat Enterprise Linux\nversion = 9.4\n",
				"/images/pxeboot/vmlinuz":      "K",
				"/images/pxeboot/initrd.img":   "I",
			},
			family: model.FamilyLinux, arch: model.ArchUEFI64, method: "kernel",
			kernel: "/images/pxeboot/vmlinuz", initrd: []string{"/images/pxeboot/initrd.img"},
			argPrefx: []string{"inst.stage2="}, osName: "Red Hat Enterprise Linux 9.4",
		},
		{
			name: "Fedora 安装盘",
			files: map[string]string{
				"/.treeinfo":                 "[general]\nname = Fedora\nversion = 40\n",
				"/images/pxeboot/vmlinuz":    "K",
				"/images/pxeboot/initrd.img": "I",
			},
			family: model.FamilyLinux, arch: model.ArchUEFI64, method: "kernel",
			kernel: "/images/pxeboot/vmlinuz", osName: "Fedora 40",
		},
		{
			name: "CentOS/openEuler/Kylin (isolinux)",
			files: map[string]string{
				"/isolinux/vmlinuz":   "K",
				"/isolinux/initrd.img": "I",
			},
			family: model.FamilyLinux, arch: model.ArchUEFI64, method: "kernel",
			kernel: "/isolinux/vmlinuz", initrd: []string{"/isolinux/initrd.img"},
			argPrefx: []string{"inst.stage2="},
		},
		{
			name: "SUSE (boot/x86_64/loader)",
			files: map[string]string{
				"/boot/x86_64/loader/linux":  "K",
				"/boot/x86_64/loader/initrd": "I",
				"/content": "PRODUCT / SUSE Linux Enterprise Server 15 SP5\nVERSION 15.5\n",
			},
			family: model.FamilyLinux, arch: model.ArchUEFI64, method: "kernel",
			kernel: "/boot/x86_64/loader/linux", initrd: []string{"/boot/x86_64/loader/initrd"},
			argPrefx: []string{"install="}, osName: "SUSE Linux Enterprise Server 15 SP5",
		},
		{
			name: "Arch Linux (archiso)",
			files: map[string]string{
				"/arch/boot/x86_64/vmlinuz":    "K",
				"/arch/boot/x86_64/archiso.img": "I",
				"/arch/boot/intel_ucode.img":   "U",
				"/arch/version":                "2024.04.01\n",
			},
			family: model.FamilyLinux, arch: model.ArchUEFI64, method: "kernel",
			kernel: "/arch/boot/x86_64/vmlinuz",
			// 微码必须在主 initrd 之前
			initrd: []string{"/arch/boot/intel_ucode.img", "/arch/boot/x86_64/archiso.img"},
			argPrefx: []string{"archisolabel="}, osName: "Arch Linux 2024.04.01",
		},
		{
			name: "Alpine Linux",
			files: map[string]string{
				"/boot/vmlinuz-lts":    "K",
				"/boot/initramfs-lts":  "I",
				"/.alpine-release":     "3.20.10",
			},
			family: model.FamilyLinux, arch: model.ArchUEFI64, method: "kernel",
			kernel: "/boot/vmlinuz-lts", initrd: []string{"/boot/initramfs-lts"},
			args: []string{"alpine_dev=cdrom"}, osName: "Alpine Linux 3.20.10",
		},
		{
			name: "通用 Linux (/boot/vmlinuz)",
			files: map[string]string{
				"/boot/vmlinuz":   "K",
				"/boot/initrd.img": "I",
			},
			family: model.FamilyLinux, arch: model.ArchUEFI64, method: "kernel",
			kernel: "/boot/vmlinuz", initrd: []string{"/boot/initrd.img"},
		},
		{
			name: "arm64 Linux (/boot/Image)",
			files: map[string]string{
				"/boot/Image":     "K",
				"/boot/initrd.img": "I",
			},
			family: model.FamilyLinux, arch: model.ArchARM64, method: "kernel",
			kernel: "/boot/Image", initrd: []string{"/boot/initrd.img"},
		},
		{
			name: "Windows 安装盘",
			files: map[string]string{
				"/bootmgr":            "B",
				"/boot/bcd":           "B",
				"/boot/boot.sdi":      "S",
				"/sources/boot.wim":   "W",
				"/sources/install.wim": "W",
				"/sources/pid.txt":    "ProductName=Windows Server 2022 Standard\n",
			},
			family: model.FamilyWindows, arch: model.ArchUEFI64, method: "wimboot",
			osName: "Windows Server 2022 Standard",
		},
		{
			name: "Windows ARM64 安装盘",
			files: map[string]string{
				"/sources/boot.wim":         "W",
				"/sources/install.esd":      "W",
				"/efi/boot/bootaa64.efi":    "E",
			},
			family: model.FamilyWindows, arch: model.ArchARM64, method: "wimboot",
		},
		{
			name: "WinPE",
			files: map[string]string{
				"/bootmgr":          "B",
				"/boot/bcd":         "B",
				"/sources/boot.wim": "W",
			},
			family: model.FamilyWinPE, method: "wimboot",
		},
		{
			name: "ESXi",
			files: map[string]string{
				"/boot.cfg":                    "title=VMware ESXi 8.0 Update 2\nkernel=/tboot.b00\n",
				"/tboot.b00":                   "T",
				"/vmxboot/efi/bootx64.efi":     "E",
			},
			family: model.FamilyESXi, method: "sanboot", osName: "VMware ESXi 8.0 Update 2",
		},
		{
			name: "FreeBSD",
			files: map[string]string{
				"/boot/loader": "L",
				"/etc/rc":      "#!/bin/sh\n",
			},
			family: model.FamilyBSD, method: "sanboot", osName: "FreeBSD",
		},
		{
			name:   "非引导数据盘",
			files:  map[string]string{"/readme.txt": "hello"},
			family: model.FamilyOther,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			img := newTestImage("TEST-VOL")
			for p, content := range tc.files {
				img.addFileStr(p, content)
			}
			got, err := Probe(writeTempISO(t, img))
			if err != nil {
				t.Fatalf("Probe 失败: %v", err)
			}
			if got.Error != "" {
				t.Fatalf("Probe 报错: %s", got.Error)
			}
			if got.Family != tc.family {
				t.Errorf("Family = %q，期望 %q", got.Family, tc.family)
			}
			if tc.arch != "" && got.OSArch != tc.arch {
				t.Errorf("OSArch = %q，期望 %q", got.OSArch, tc.arch)
			}
			if tc.method != "" && got.Boot.Method != tc.method {
				t.Errorf("Method = %q，期望 %q", got.Boot.Method, tc.method)
			}
			if tc.kernel != "" && got.Boot.Kernel != tc.kernel {
				t.Errorf("Kernel = %q，期望 %q", got.Boot.Kernel, tc.kernel)
			}
			if len(tc.initrd) > 0 {
				if strings.Join(got.Boot.Initrd, ",") != strings.Join(tc.initrd, ",") {
					t.Errorf("Initrd = %v，期望 %v", got.Boot.Initrd, tc.initrd)
				}
			}
			for _, want := range tc.args {
				if !hasArg(got.Boot.KernelArgs, want) {
					t.Errorf("KernelArgs 缺少 %q: %v", want, got.Boot.KernelArgs)
				}
			}
			for _, prefix := range tc.argPrefx {
				if !hasArgPrefix(got.Boot.KernelArgs, prefix) {
					t.Errorf("KernelArgs 缺少前缀 %q: %v", prefix, got.Boot.KernelArgs)
				}
			}
			if tc.osName != "" && !strings.Contains(got.OSName, tc.osName) {
				t.Errorf("OSName = %q，期望包含 %q", got.OSName, tc.osName)
			}
			if got.OSName == "" {
				t.Error("OSName 不应为空")
			}
			// 声明的引导文件必须真的存在
			if got.Boot.Kernel != "" && !strings.HasPrefix(got.Boot.Kernel, "/") {
				t.Errorf("Kernel 必须是 ISO 内绝对路径: %q", got.Boot.Kernel)
			}
		})
	}
}

// TestDetectPriority 验证优先级：Windows 优先于 Linux，casper 优先于 isolinux。
func TestDetectPriority(t *testing.T) {
	// 同时有 Windows 和 Linux 特征时，应按 Windows 处理
	img := newTestImage("MIXED")
	img.addFile("/sources/boot.wim", []byte("W"))
	img.addFile("/sources/install.wim", []byte("W"))
	img.addFile("/bootmgr", []byte("B"))
	img.addFile("/casper/vmlinuz", []byte("K"))
	img.addFile("/casper/initrd", []byte("I"))
	got, err := Probe(writeTempISO(t, img))
	if err != nil {
		t.Fatal(err)
	}
	if got.Family != model.FamilyWindows {
		t.Errorf("Family = %q，Windows 特征应优先", got.Family)
	}

	// casper 与 isolinux 同时存在时，应该用 casper 的内核参数
	img2 := newTestImage("UBUNTU-ISO")
	img2.addFile("/casper/vmlinuz", []byte("K"))
	img2.addFile("/casper/initrd", []byte("I"))
	img2.addFile("/isolinux/vmlinuz", []byte("K2"))
	img2.addFile("/isolinux/initrd.img", []byte("I2"))
	got2, err := Probe(writeTempISO(t, img2))
	if err != nil {
		t.Fatal(err)
	}
	if got2.Boot.Kernel != "/casper/vmlinuz" {
		t.Errorf("Kernel = %q，casper 应优先于 isolinux", got2.Boot.Kernel)
	}
	if !hasArg(got2.Boot.KernelArgs, "boot=casper") {
		t.Errorf("KernelArgs = %v，应使用 casper 参数", got2.Boot.KernelArgs)
	}
}
