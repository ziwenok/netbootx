package iso

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/wupengcheng/netbootx/internal/model"
)

// sampleImage 造一个结构接近真实发行版的测试镜像：
//   /boot/vmlinuz-lts, /boot/initramfs-lts, /.alpine-release, /apks/x86_64/...
//   /EFI/BOOT/bootx64.efi（El Torito BIOS+UEFI 双引导）
func sampleImage(t *testing.T) string {
	t.Helper()
	img := newTestImage("ALPINE-TEST")
	img.boot = &testBoot{
		biosImage: true,
		efiImage:  true,
		imageData: bytes.Repeat([]byte{0xCD}, 2048),
	}
	img.addFileStr("/.alpine-release", "3.20.10")
	img.addFile("/boot/vmlinuz-lts", bytes.Repeat([]byte("K"), 5000))
	img.addFile("/boot/initramfs-lts", bytes.Repeat([]byte("I"), 9000))
	img.addFile("/EFI/BOOT/bootx64.efi", []byte("EFI-BINARY"))
	img.addFile("/apks/x86_64/APKINDEX.tar.gz", []byte("apkindex"))
	img.addFile("/README.txt", []byte("hello world"))
	return writeTempISO(t, img)
}

func TestOpenAndVolumeInfo(t *testing.T) {
	p := sampleImage(t)
	r, err := Open(p)
	if err != nil {
		t.Fatalf("Open 失败: %v", err)
	}
	defer func() { _ = r.Close() }()

	if got := r.VolumeID(); got != "ALPINE-TEST" {
		t.Errorf("VolumeID = %q，期望 %q", got, "ALPINE-TEST")
	}
	if r.BlockSize() != SectorSize {
		t.Errorf("BlockSize = %d，期望 %d", r.BlockSize(), SectorSize)
	}
	if !r.Joliet() {
		t.Error("期望识别出 Joliet SVD")
	}
	if !r.RockRidge() {
		t.Error("期望识别出 Rock Ridge")
	}
	if r.Size() <= 0 {
		t.Errorf("Size = %d，期望 > 0", r.Size())
	}
	if r.Path() != p {
		t.Errorf("Path = %q，期望 %q", r.Path(), p)
	}
	if r.Root() == nil || !r.Root().IsDir {
		t.Fatal("Root() 应为目录")
	}
}

func TestLookupAndReadFile(t *testing.T) {
	r, err := Open(sampleImage(t))
	if err != nil {
		t.Fatalf("Open 失败: %v", err)
	}
	defer func() { _ = r.Close() }()

	tests := []struct {
		path string
		want string
	}{
		{"/.alpine-release", "3.20.10"},
		{"/README.txt", "hello world"},
		{"/EFI/BOOT/bootx64.efi", "EFI-BINARY"},
		{"/apks/x86_64/APKINDEX.tar.gz", "apkindex"},
	}
	for _, tc := range tests {
		got, err := r.ReadFile(tc.path, 0)
		if err != nil {
			t.Errorf("ReadFile(%q) 失败: %v", tc.path, err)
			continue
		}
		if string(got) != tc.want {
			t.Errorf("ReadFile(%q) = %q，期望 %q", tc.path, got, tc.want)
		}
	}
	// 相对路径也应接受
	if !r.Exists("README.txt") {
		t.Error("相对路径 README.txt 应存在")
	}
	// 大小写不敏感回退
	if !r.Exists("/readme.TXT") {
		t.Error("大小写不敏感查找应命中 /readme.TXT")
	}
	// 不存在的路径
	if r.Exists("/nope/nothere") {
		t.Error("不存在的路径不应返回 true")
	}
	if _, err := r.Lookup("/nope"); !errors.Is(err, ErrNotExist) {
		t.Errorf("Lookup 不存在的路径应返回 ErrNotExist，实际 %v", err)
	}
}

func TestFileSizeAndReadAt(t *testing.T) {
	r, err := Open(sampleImage(t))
	if err != nil {
		t.Fatalf("Open 失败: %v", err)
	}
	defer func() { _ = r.Close() }()

	size, err := r.FileSize("/boot/vmlinuz-lts")
	if err != nil {
		t.Fatalf("FileSize 失败: %v", err)
	}
	if size != 5000 {
		t.Errorf("FileSize = %d，期望 5000", size)
	}

	// 从中间读
	buf := make([]byte, 100)
	n, err := r.ReadAt("/boot/vmlinuz-lts", buf, 1000)
	if err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("ReadAt 失败: %v", err)
	}
	if n != 100 {
		t.Errorf("ReadAt 读到 %d 字节，期望 100", n)
	}
	for _, b := range buf {
		if b != 'K' {
			t.Fatalf("ReadAt 内容不对: %q", buf)
		}
	}

	// 越过文件末尾
	n, err = r.ReadAt("/boot/vmlinuz-lts", buf, 4990)
	if !errors.Is(err, io.EOF) {
		t.Errorf("越界读应返回 io.EOF，实际 %v", err)
	}
	if n != 10 {
		t.Errorf("越界读应返回 10 字节，实际 %d", n)
	}

	// 负偏移
	if _, err := r.ReadAt("/boot/vmlinuz-lts", buf, -1); !errors.Is(err, ErrInvalidOffset) {
		t.Errorf("负偏移应返回 ErrInvalidOffset，实际 %v", err)
	}

	// 对目录读
	if _, err := r.FileSize("/boot"); !errors.Is(err, ErrIsDir) {
		t.Errorf("FileSize 对目录应返回 ErrIsDir，实际 %v", err)
	}
}

func TestSectionReader(t *testing.T) {
	r, err := Open(sampleImage(t))
	if err != nil {
		t.Fatalf("Open 失败: %v", err)
	}
	defer func() { _ = r.Close() }()

	ra, size, err := r.SectionReader("/boot/initramfs-lts")
	if err != nil {
		t.Fatalf("SectionReader 失败: %v", err)
	}
	if size != 9000 {
		t.Errorf("SectionReader 大小 = %d，期望 9000", size)
	}
	// 模拟 httpd 的 Range：从偏移 500 读 32 字节
	sr := io.NewSectionReader(ra, 500, 32)
	buf := make([]byte, 32)
	if _, err := io.ReadFull(sr, buf); err != nil {
		t.Fatalf("SectionReader 读取失败: %v", err)
	}
	for _, b := range buf {
		if b != 'I' {
			t.Fatalf("SectionReader 内容不对: %q", buf)
		}
	}
}

func TestList(t *testing.T) {
	r, err := Open(sampleImage(t))
	if err != nil {
		t.Fatalf("Open 失败: %v", err)
	}
	defer func() { _ = r.Close() }()

	root, err := r.List("/")
	if err != nil {
		t.Fatalf("List(/) 失败: %v", err)
	}
	names := make([]string, 0, len(root))
	for _, e := range root {
		names = append(names, e.Name)
	}
	// 目录应该排在文件前面
	if len(root) < 4 {
		t.Fatalf("根目录项太少: %v", names)
	}
	firstFile := -1
	for i, e := range root {
		if !e.IsDir && firstFile < 0 {
			firstFile = i
		}
		if e.IsDir && firstFile >= 0 {
			t.Errorf("目录项 %q 排在文件后面了: %v", e.Name, names)
		}
	}
	// 根目录必须包含 boot / EFI / apks / .alpine-release
	want := map[string]bool{"boot": true, "EFI": true, "apks": true, ".alpine-release": true}
	for _, e := range root {
		delete(want, e.Name)
	}
	if len(want) != 0 {
		t.Errorf("根目录缺少项: %v（实际 %v）", want, names)
	}

	boot, err := r.List("/boot")
	if err != nil {
		t.Fatalf("List(/boot) 失败: %v", err)
	}
	if len(boot) != 2 {
		t.Fatalf("/boot 应有 2 项，实际 %d: %+v", len(boot), boot)
	}
	if boot[0].Name != "initramfs-lts" || boot[1].Name != "vmlinuz-lts" {
		t.Errorf("/boot 排序不对: %s, %s", boot[0].Name, boot[1].Name)
	}
	if boot[1].Size != 5000 {
		t.Errorf("vmlinuz 大小 = %d，期望 5000", boot[1].Size)
	}

	// 对文件 List
	if _, err := r.List("/README.txt"); !errors.Is(err, ErrNotDir) {
		t.Errorf("对文件 List 应返回 ErrNotDir，实际 %v", err)
	}
	// 空目录
	img := newTestImage("EMPTY")
	img.addDir("/empty")
	r2, err := Open(writeTempISO(t, img))
	if err != nil {
		t.Fatalf("Open 空目录镜像失败: %v", err)
	}
	defer func() { _ = r2.Close() }()
	list, err := r2.List("/empty")
	if err != nil {
		t.Fatalf("List(/empty) 失败: %v", err)
	}
	if len(list) != 0 {
		t.Errorf("空目录应返回 0 项，实际 %d", len(list))
	}
}

func TestRockRidgeSymlink(t *testing.T) {
	img := newTestImage("RR-TEST")
	img.addFile("/boot/vmlinuz-6.1.0", []byte("KERNEL"))
	img.addSymlink("/vmlinuz", "/boot/vmlinuz-6.1.0")
	r, err := Open(writeTempISO(t, img))
	if err != nil {
		t.Fatalf("Open 失败: %v", err)
	}
	defer func() { _ = r.Close() }()

	if !r.RockRidge() {
		t.Fatal("应识别出 Rock Ridge")
	}
	// 通过符号链接应能读到目标内容
	got, err := r.ReadFile("/vmlinuz", 0)
	if err != nil {
		t.Fatalf("通过符号链接读取失败: %v", err)
	}
	if string(got) != "KERNEL" {
		t.Errorf("符号链接读取内容 = %q，期望 KERNEL", got)
	}
	// 符号链接的原始条目应带 Link 字段
	raw, err := r.findRaw("/vmlinuz")
	if err != nil {
		t.Fatalf("查找原始符号链接失败: %v", err)
	}
	if raw.Link != "/boot/vmlinuz-6.1.0" {
		t.Errorf("Link = %q，期望 /boot/vmlinuz-6.1.0", raw.Link)
	}
}

// findRaw 查未解引用的目录项（测试辅助）。
func (r *Reader) findRaw(p string) (*File, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if f, ok := r.entries[p]; ok {
		return f, nil
	}
	return nil, ErrNotExist
}

func TestJolietFilenames(t *testing.T) {
	img := newTestImage("JOLIET-TEST")
	img.addFileStr("/中文目录/说明文件.txt", "joliet content")
	img.addFileStr("/MixedCase.txt", "case")
	r, err := Open(writeTempISO(t, img))
	if err != nil {
		t.Fatalf("Open 失败: %v", err)
	}
	defer func() { _ = r.Close() }()

	if !r.Joliet() {
		t.Fatal("应识别出 Joliet")
	}
	// Rock Ridge 优先，名字应保持原样（含大小写）
	got, err := r.ReadFile("/MixedCase.txt", 0)
	if err != nil {
		t.Fatalf("读取 Joliet 文件名失败: %v", err)
	}
	if string(got) != "case" {
		t.Errorf("内容 = %q，期望 case", got)
	}
	// UTF-16 中文名（Rock Ridge 的 NM 里是 UTF-8，这里验证整条链路）
	if !r.Exists("/中文目录/说明文件.txt") {
		entries, _ := r.List("/中文目录")
		t.Errorf("中文路径不存在，/中文目录 下的项: %+v", entries)
	}
}

func TestJolietOnly(t *testing.T) {
	// 关掉 Rock Ridge，强制走 Joliet 的 UCS-2 解码路径。
	img := newTestImage("JOLIET-ONLY")
	img.useRR = false
	img.addFileStr("/boot/Initrd.Img", "initrd-joliet")
	img.addFileStr("/Readme.Txt", "readme")
	r, err := Open(writeTempISO(t, img))
	if err != nil {
		t.Fatalf("Open 失败: %v", err)
	}
	defer func() { _ = r.Close() }()

	if !r.Joliet() {
		t.Fatal("应识别出 Joliet")
	}
	if r.RockRidge() {
		t.Fatal("不应识别出 Rock Ridge")
	}
	got, err := r.ReadFile("/boot/Initrd.Img", 0)
	if err != nil {
		t.Fatalf("Joliet 文件名读取失败: %v", err)
	}
	if string(got) != "initrd-joliet" {
		t.Errorf("内容 = %q，期望 initrd-joliet", got)
	}
}

func TestElTorito(t *testing.T) {
	// 双引导（BIOS + UEFI）
	r, err := Open(sampleImage(t))
	if err != nil {
		t.Fatalf("Open 失败: %v", err)
	}
	defer func() { _ = r.Close() }()
	bi := r.BootInfo()
	if bi == nil {
		t.Fatal("应解析出 El Torito 引导信息")
	}
	if !bi.Bootable {
		t.Error("期望 Bootable = true")
	}
	if !bi.BIOS {
		t.Error("期望识别出 Legacy BIOS 引导")
	}
	if !bi.UEFI {
		t.Error("期望识别出 UEFI 引导")
	}
	if len(bi.Entries) < 2 {
		t.Errorf("期望至少 2 个引导条目，实际 %d", len(bi.Entries))
	}
	if !strings.Contains(bi.String(), "可引导") {
		t.Errorf("String() = %q，应包含「可引导」", bi.String())
	}

	// 只有 BIOS 引导
	img := newTestImage("BIOS-ONLY")
	img.boot = &testBoot{biosImage: true, imageData: []byte{0xCD}}
	rb, err := Open(writeTempISO(t, img))
	if err != nil {
		t.Fatalf("Open 失败: %v", err)
	}
	defer func() { _ = rb.Close() }()
	bb := rb.BootInfo()
	if bb == nil || !bb.Bootable || !bb.BIOS || bb.UEFI {
		t.Errorf("BIOS-only 引导信息不对: %+v", bb)
	}

	// 完全没有 El Torito 的可引导目录
	img2 := newTestImage("NO-BOOT")
	img2.addFileStr("/readme", "x")
	rn, err := Open(writeTempISO(t, img2))
	if err != nil {
		t.Fatalf("Open 失败: %v", err)
	}
	defer func() { _ = rn.Close() }()
	if rn.BootInfo() != nil {
		t.Errorf("无引导目录时 BootInfo 应为 nil，实际 %+v", rn.BootInfo())
	}
}

func TestProbeLinux(t *testing.T) {
	img := newTestImage("ALPINE-TEST")
	img.boot = &testBoot{biosImage: true, efiImage: true, imageData: []byte{0xCD}}
	img.addFileStr("/.alpine-release", "3.20.10")
	img.addFile("/boot/vmlinuz-lts", bytes.Repeat([]byte("K"), 4096))
	img.addFile("/boot/initramfs-lts", bytes.Repeat([]byte("I"), 8192))
	img.addFileStr("/etc/os-release", "NAME=\"Alpine Linux\"\nPRETTY_NAME=\"Alpine Linux v3.20\"\n")
	p := writeTempISO(t, img)

	got, err := Probe(p)
	if err != nil {
		t.Fatalf("Probe 失败: %v", err)
	}
	if got.Error != "" {
		t.Fatalf("Probe 返回错误: %s", got.Error)
	}
	if got.Family != model.FamilyLinux {
		t.Errorf("Family = %q，期望 linux", got.Family)
	}
	if !strings.Contains(got.OSName, "Alpine") {
		t.Errorf("OSName = %q，应包含 Alpine", got.OSName)
	}
	if got.Boot.Method != "kernel" {
		t.Errorf("Method = %q，期望 kernel", got.Boot.Method)
	}
	if got.Boot.Kernel != "/boot/vmlinuz-lts" {
		t.Errorf("Kernel = %q，期望 /boot/vmlinuz-lts", got.Boot.Kernel)
	}
	if len(got.Boot.Initrd) != 1 || got.Boot.Initrd[0] != "/boot/initramfs-lts" {
		t.Errorf("Initrd = %v，期望 [/boot/initramfs-lts]", got.Boot.Initrd)
	}
	if !got.Bootable {
		t.Error("期望 Bootable = true")
	}
	if got.Size <= 0 || got.ID == "" || got.Name == "" {
		t.Errorf("元信息不全: %+v", got)
	}
}

func TestProbeUbuntuCasper(t *testing.T) {
	img := newTestImage("Ubuntu 24.04.2 LTS amd64")
	img.boot = &testBoot{biosImage: true, efiImage: true, imageData: []byte{0xCD}}
	img.addFileStr("/.disk/info", "Ubuntu 24.04.2 LTS \"Noble Numbat\" - Release amd64 (20250214)\n")
	img.addFile("/casper/vmlinuz", []byte("KERNEL"))
	img.addFile("/casper/initrd", []byte("INITRD"))
	p := writeTempISO(t, img)

	got, err := Probe(p)
	if err != nil {
		t.Fatalf("Probe 失败: %v", err)
	}
	if got.Family != model.FamilyLinux {
		t.Errorf("Family = %q，期望 linux", got.Family)
	}
	if got.OSName != "Ubuntu 24.04.2 LTS" {
		t.Errorf("OSName = %q，期望 \"Ubuntu 24.04.2 LTS\"", got.OSName)
	}
	if got.Boot.Kernel != "/casper/vmlinuz" || got.Boot.Initrd[0] != "/casper/initrd" {
		t.Errorf("引导文件不对: %+v", got.Boot)
	}
	if !hasArg(got.Boot.KernelArgs, "boot=casper") {
		t.Errorf("KernelArgs 缺少 boot=casper: %v", got.Boot.KernelArgs)
	}
	if !hasArgPrefix(got.Boot.KernelArgs, "iso-scan/filename=") {
		t.Errorf("KernelArgs 缺少 iso-scan/filename: %v", got.Boot.KernelArgs)
	}
}

func TestProbeRHEL(t *testing.T) {
	img := newTestImage("RHEL-9-4-0-BaseOS-x86_64")
	img.addFileStr("/.treeinfo", "[general]\nname = Rocky Linux\nversion = 9.4\narch = x86_64\n")
	img.addFile("/images/pxeboot/vmlinuz", []byte("K"))
	img.addFile("/images/pxeboot/initrd.img", []byte("I"))
	img.addFile("/isolinux/vmlinuz", []byte("K2")) // 老结构也在，应优先 pxeboot
	p := writeTempISO(t, img)

	got, err := Probe(p)
	if err != nil {
		t.Fatalf("Probe 失败: %v", err)
	}
	if got.Family != model.FamilyLinux {
		t.Errorf("Family = %q，期望 linux", got.Family)
	}
	if got.OSName != "Rocky Linux 9.4" {
		t.Errorf("OSName = %q，期望 Rocky Linux 9.4", got.OSName)
	}
	if got.Boot.Kernel != "/images/pxeboot/vmlinuz" {
		t.Errorf("Kernel = %q，期望 /images/pxeboot/vmlinuz", got.Boot.Kernel)
	}
	if !hasArgPrefix(got.Boot.KernelArgs, "inst.stage2=") {
		t.Errorf("KernelArgs 缺少 inst.stage2: %v", got.Boot.KernelArgs)
	}
}

func TestProbeDebianLive(t *testing.T) {
	img := newTestImage("d-live 12.5.0 amd64")
	img.addFileStr("/.disk/info", "Debian GNU/Linux 12.5.0 \"Bookworm\" - Official amd64 NETINST (2024-02-10)")
	img.addFile("/live/vmlinuz", []byte("K"))
	img.addFile("/live/initrd.img", []byte("I"))
	got, err := Probe(writeTempISO(t, img))
	if err != nil {
		t.Fatalf("Probe 失败: %v", err)
	}
	if got.Family != model.FamilyLinux {
		t.Errorf("Family = %q，期望 linux", got.Family)
	}
	if !strings.Contains(got.OSName, "Debian") {
		t.Errorf("OSName = %q，应包含 Debian", got.OSName)
	}
	if !hasArg(got.Boot.KernelArgs, "boot=live") {
		t.Errorf("KernelArgs 缺少 boot=live: %v", got.Boot.KernelArgs)
	}
}

func TestProbeWindows(t *testing.T) {
	img := newTestImage("CCCOMA_X64FRE_ZH-CN_DV9")
	img.boot = &testBoot{biosImage: true, efiImage: true, imageData: []byte{0xCD}}
	img.addFile("/bootmgr", []byte("BOOTMGR"))
	img.addFile("/boot/bcd", []byte("BCD"))
	img.addFile("/boot/boot.sdi", []byte("SDI"))
	img.addFile("/sources/boot.wim", []byte("BOOTWIM"))
	img.addFile("/sources/install.wim", []byte("INSTALLWIM"))
	img.addFileStr("/sources/pid.txt", "\n[PID]\nValue=XXXXX\nProductName=Windows 11 Pro\n")
	p := writeTempISO(t, img)

	got, err := Probe(p)
	if err != nil {
		t.Fatalf("Probe 失败: %v", err)
	}
	if got.Family != model.FamilyWindows {
		t.Errorf("Family = %q，期望 windows", got.Family)
	}
	if got.Boot.Method != "wimboot" {
		t.Errorf("Method = %q，期望 wimboot", got.Boot.Method)
	}
	if got.Boot.BootMGR != "/bootmgr" {
		t.Errorf("BootMGR = %q，期望 /bootmgr", got.Boot.BootMGR)
	}
	if got.Boot.BCD != "/boot/bcd" {
		t.Errorf("BCD = %q，期望 /boot/bcd", got.Boot.BCD)
	}
	if got.Boot.BootSDI != "/boot/boot.sdi" {
		t.Errorf("BootSDI = %q，期望 /boot/boot.sdi", got.Boot.BootSDI)
	}
	if got.Boot.BootWIM != "/sources/boot.wim" {
		t.Errorf("BootWIM = %q，期望 /sources/boot.wim", got.Boot.BootWIM)
	}
	if got.OSName != "Windows 11 Pro" {
		t.Errorf("OSName = %q，期望 Windows 11 Pro", got.OSName)
	}
	if got.OSArch != model.ArchUEFI64 {
		t.Errorf("OSArch = %q，期望 uefi64", got.OSArch)
	}
}

func TestProbeWinPE(t *testing.T) {
	img := newTestImage("WINPE")
	img.addFile("/bootmgr", []byte("BOOTMGR"))
	img.addFile("/boot/bcd", []byte("BCD"))
	img.addFile("/boot/boot.sdi", []byte("SDI"))
	img.addFile("/sources/boot.wim", []byte("BOOTWIM"))
	got, err := Probe(writeTempISO(t, img))
	if err != nil {
		t.Fatalf("Probe 失败: %v", err)
	}
	if got.Family != model.FamilyWinPE {
		t.Errorf("Family = %q，期望 winpe", got.Family)
	}
	if got.Boot.Method != "wimboot" {
		t.Errorf("Method = %q，期望 wimboot", got.Boot.Method)
	}
}

func TestProbeESXi(t *testing.T) {
	img := newTestImage("VMware-VMvisor-Installer-8.0")
	img.addFileStr("/boot.cfg", "bootstate=0\ntitle=Loading VMware ESXi 8.0\nkernel=/tboot.b00\n")
	img.addFile("/tboot.b00", []byte("TBOOT"))
	img.addFile("/vmxboot/efi/bootx64.efi", []byte("EFI"))
	got, err := Probe(writeTempISO(t, img))
	if err != nil {
		t.Fatalf("Probe 失败: %v", err)
	}
	if got.Family != model.FamilyESXi {
		t.Errorf("Family = %q，期望 esxi", got.Family)
	}
	if got.Boot.Method != "sanboot" {
		t.Errorf("Method = %q，期望 sanboot", got.Boot.Method)
	}
	if !strings.Contains(got.OSName, "VMware ESXi") {
		t.Errorf("OSName = %q，应包含 VMware ESXi", got.OSName)
	}
}

func TestProbeBSD(t *testing.T) {
	img := newTestImage("FreeBSD-14.0-RELEASE-amd64")
	img.addFile("/boot/loader", []byte("LOADER"))
	img.addFileStr("/etc/rc", "#!/bin/sh\n")
	got, err := Probe(writeTempISO(t, img))
	if err != nil {
		t.Fatalf("Probe 失败: %v", err)
	}
	if got.Family != model.FamilyBSD {
		t.Errorf("Family = %q，期望 bsd", got.Family)
	}
	if !strings.Contains(got.OSName, "FreeBSD") {
		t.Errorf("OSName = %q，应包含 FreeBSD", got.OSName)
	}
}

func TestProbeArch(t *testing.T) {
	img := newTestImage("ARCH_202404")
	img.addFile("/arch/boot/x86_64/vmlinuz", []byte("K"))
	img.addFile("/arch/boot/x86_64/archiso.img", []byte("I"))
	img.addFileStr("/arch/version", "2024.04.01\n")
	got, err := Probe(writeTempISO(t, img))
	if err != nil {
		t.Fatalf("Probe 失败: %v", err)
	}
	if got.Family != model.FamilyLinux {
		t.Errorf("Family = %q，期望 linux", got.Family)
	}
	if got.OSName != "Arch Linux 2024.04.01" {
		t.Errorf("OSName = %q，期望 Arch Linux 2024.04.01", got.OSName)
	}
	if got.Boot.Kernel != "/arch/boot/x86_64/vmlinuz" {
		t.Errorf("Kernel = %q", got.Boot.Kernel)
	}
	if !hasArgPrefix(got.Boot.KernelArgs, "archisolabel=") {
		t.Errorf("KernelArgs 缺少 archisolabel: %v", got.Boot.KernelArgs)
	}
}

func TestProbeArm64(t *testing.T) {
	img := newTestImage("UBUNTU-ARM64")
	img.addFile("/casper/vmlinuz", []byte("K"))
	img.addFile("/casper/initrd", []byte("I"))
	img.addFile("/arch/boot/aarch64/vmlinuz", []byte("K2"))
	got, err := Probe(writeTempISO(t, img))
	if err != nil {
		t.Fatalf("Probe 失败: %v", err)
	}
	if got.OSArch != model.ArchARM64 {
		t.Errorf("OSArch = %q，期望 arm64", got.OSArch)
	}
}

func TestProbeOther(t *testing.T) {
	img := newTestImage("SOME-TOOL-1.0")
	img.addFileStr("/readme.txt", "not bootable")
	got, err := Probe(writeTempISO(t, img))
	if err != nil {
		t.Fatalf("Probe 失败: %v", err)
	}
	if got.Family != model.FamilyOther {
		t.Errorf("Family = %q，期望 other", got.Family)
	}
	if got.OSName == "" {
		t.Error("OSName 不应为空")
	}
}

func TestProbeInvalidFile(t *testing.T) {
	dir := t.TempDir()
	// 不是 ISO 的文件
	p := filepath.Join(dir, "fake.iso")
	if err := os.WriteFile(p, bytes.Repeat([]byte("x"), 100000), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := Probe(p)
	if err != nil {
		t.Fatalf("Probe 对非 ISO 文件不应返回 error（应写进 Error 字段）: %v", err)
	}
	if got.Error == "" {
		t.Error("非 ISO 文件的 Error 字段不应为空")
	}
	if got.Name != "fake.iso" {
		t.Errorf("Name = %q，期望 fake.iso", got.Name)
	}

	// 完全不存在的文件：返回 error
	if _, err := Probe(filepath.Join(dir, "nope.iso")); err == nil {
		t.Error("不存在的文件应返回 error")
	}

	// 空文件
	empty := filepath.Join(dir, "empty.iso")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	got2, err := Probe(empty)
	if err != nil {
		t.Fatalf("Probe 空文件不应返回 error: %v", err)
	}
	if got2.Error == "" {
		t.Error("空文件的 Error 字段不应为空")
	}
}

func TestOpenErrors(t *testing.T) {
	if _, err := Open("/definitely/not/here.iso"); err == nil {
		t.Error("打开不存在的文件应返回 error")
	}
	dir := t.TempDir()
	if _, err := Open(dir); err == nil {
		t.Error("打开目录应返回 error")
	}
}

func TestClosedReader(t *testing.T) {
	r, err := Open(sampleImage(t))
	if err != nil {
		t.Fatalf("Open 失败: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("Close 失败: %v", err)
	}
	// 幂等
	if err := r.Close(); err != nil {
		t.Errorf("重复 Close 应返回 nil，实际 %v", err)
	}
	if _, err := r.Lookup("/README.txt"); !errors.Is(err, ErrClosed) {
		t.Errorf("关闭后 Lookup 应返回 ErrClosed，实际 %v", err)
	}
	if r.Exists("/README.txt") {
		t.Error("关闭后 Exists 应返回 false")
	}
}

func TestCleanPath(t *testing.T) {
	tests := []struct {
		in   string
		want string
		ok   bool
	}{
		{"", "/", true},
		{"/", "/", true},
		{"/a/b", "/a/b", true},
		{"a/b", "/a/b", true},
		{"/a//b/", "/a/b", true},
		{"/a/./b", "/a/b", true},
		{"/a/../b", "/b", true},
		{"../../etc/passwd", "", false},
		{"/a/../../b", "", false},
	}
	for _, tc := range tests {
		got, ok := cleanPath(tc.in)
		if ok != tc.ok || (ok && got != tc.want) {
			t.Errorf("cleanPath(%q) = (%q, %v)，期望 (%q, %v)", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

func TestDecodeISOName(t *testing.T) {
	tests := []struct {
		raw    []byte
		joliet bool
		want   string
	}{
		{[]byte("VMLINUZ.;1"), false, "VMLINUZ"},
		{[]byte("README.TXT;1"), false, "README.TXT"},
		{[]byte("BOOT"), false, "BOOT"},
		{[]byte("CDROM"), false, "CDROM"},
		{[]byte("DIR.;1"), false, "DIR"},
		{encodeUCS2("MixedCase.txt"), true, "MixedCase.txt"},
		{encodeUCS2("说明.txt"), true, "说明.txt"},
		{encodeUCS2("file.txt;1"), true, "file.txt"},
	}
	for _, tc := range tests {
		if got := decodeISOName(tc.raw, tc.joliet); got != tc.want {
			t.Errorf("decodeISOName(%q, joliet=%v) = %q，期望 %q", tc.raw, tc.joliet, got, tc.want)
		}
	}
}

func TestParseRecordingDate(t *testing.T) {
	got := parseRecordingDate([]byte{124, 1, 2, 3, 4, 5, 0})
	want := "2024-01-02T03:04:05Z"
	if got.UTC().Format("2006-01-02T15:04:05Z") != want {
		t.Errorf("parseRecordingDate = %v，期望 %s", got, want)
	}
	// 时区偏移：+8 小时 = 32 个 15 分钟
	got2 := parseRecordingDate([]byte{124, 1, 2, 3, 4, 5, 32})
	if _, off := got2.Zone(); off != 8*3600 {
		t.Errorf("时区偏移 = %d 秒，期望 %d", off, 8*3600)
	}
	// 非法月份/日期要能兜底不 panic
	_ = parseRecordingDate([]byte{124, 99, 99, 99, 99, 99, 99})
}

func TestParseOSRelease(t *testing.T) {
	s := "NAME=\"Rocky Linux\"\nVERSION=\"9.4 (Blue Onyx)\"\nVERSION_ID=\"9.4\"\nPRETTY_NAME=\"Rocky Linux 9.4 (Blue Onyx)\"\n"
	if got := parseOSRelease(s); got != "Rocky Linux 9.4 (Blue Onyx)" {
		t.Errorf("parseOSRelease = %q", got)
	}
	// 只有 NAME + VERSION_ID
	s2 := "NAME=Ubuntu\nVERSION_ID=\"24.04\"\n"
	if got := parseOSRelease(s2); got != "Ubuntu 24.04" {
		t.Errorf("parseOSRelease = %q，期望 Ubuntu 24.04", got)
	}
	if got := parseOSRelease(""); got != "" {
		t.Errorf("空输入应返回空串，实际 %q", got)
	}
}

func TestParseDiskInfo(t *testing.T) {
	tests := []struct{ in, want string }{
		{"Ubuntu 24.04.2 LTS \"Noble Numbat\" - Release amd64 (20250214)", "Ubuntu 24.04.2 LTS"},
		{"Debian GNU/Linux 12.5.0 \"Bookworm\" - Official amd64 NETINST (2024-02-10)", "Debian GNU/Linux 12.5.0"},
		{"Kubuntu 24.04 LTS", "Kubuntu 24.04 LTS"},
		{"", ""},
	}
	for _, tc := range tests {
		if got := parseDiskInfo(tc.in); got != tc.want {
			t.Errorf("parseDiskInfo(%q) = %q，期望 %q", tc.in, got, tc.want)
		}
	}
}

func TestParseWIMName(t *testing.T) {
	// 构造一个 UTF-16LE 编码的 WIM 头片段
	xml := "<WIM><IMAGE INDEX=\"1\"><DISPLAYNAME>Windows 11 Pro</DISPLAYNAME></IMAGE></WIM>"
	le := encodeUTF16LE(xml)
	if got := parseWIMName(string(le)); got != "Windows 11 Pro" {
		t.Errorf("parseWIMName = %q，期望 Windows 11 Pro", got)
	}
	// ASCII 版本
	if got := parseWIMName("<IMAGE><NAME>Windows 10 Enterprise</NAME></IMAGE>"); got != "Windows 10 Enterprise" {
		t.Errorf("parseWIMName(ascii) = %q", got)
	}
	if got := parseWIMName("no xml here"); got != "" {
		t.Errorf("无 XML 应返回空串，实际 %q", got)
	}
}

// encodeUTF16LE 把字符串编码成 UTF-16LE 字节（测试辅助）。
func encodeUTF16LE(s string) []byte {
	var b []byte
	for _, r := range s {
		if r < 0x10000 {
			b = append(b, byte(r), byte(r>>8))
		}
	}
	return b
}

func TestArchCompatible(t *testing.T) {
	tests := []struct {
		img, client model.Arch
		want        bool
	}{
		{model.ArchUEFI64, model.ArchBIOS, true},
		{model.ArchUEFI64, model.ArchUEFI64, true},
		{model.ArchUEFI64, model.ArchUEFI64HTTP, true},
		{model.ArchUEFI64, model.ArchARM64, false},
		{model.ArchARM64, model.ArchARM64, true},
		{model.ArchARM64, model.ArchARM64HTTP, true},
		{model.ArchARM64, model.ArchUEFI64, false},
		{model.ArchUEFI32, model.ArchUEFI32, true},
		{model.ArchUEFI32, model.ArchUEFI64, false},
		{model.ArchLoongArch64, model.ArchLoongArch64, true},
		{model.ArchUnknown, model.ArchBIOS, true}, // 未知一律显示
	}
	for _, tc := range tests {
		if got := ArchCompatible(tc.img, tc.client); got != tc.want {
			t.Errorf("ArchCompatible(%q, %q) = %v，期望 %v", tc.img, tc.client, got, tc.want)
		}
	}
}

func TestScan(t *testing.T) {
	dir := t.TempDir()
	mk := func(name, vol string, linux bool) {
		img := newTestImage(vol)
		img.boot = &testBoot{biosImage: true, efiImage: true, imageData: []byte{0xCD}}
		if linux {
			img.addFile("/boot/vmlinuz-lts", []byte("K"))
			img.addFile("/boot/initramfs-lts", []byte("I"))
			img.addFileStr("/.alpine-release", "3.20.10")
		}
		data, err := img.build()
		if err != nil {
			t.Fatalf("build: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	mk("alpine-a.iso", "A", true)
	mk("alpine-b.ISO", "B", true) // 大写扩展名也要认
	mk("other.iso", "C", false)
	// 子目录里的 ISO
	sub := filepath.Join(dir, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	data, _ := newTestImage("D").build()
	if err := os.WriteFile(filepath.Join(sub, "nested.iso"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	// 非 ISO 文件应被忽略
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	// 坏 ISO：不能 panic，要带 Error
	if err := os.WriteFile(filepath.Join(dir, "broken.iso"), []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := Scan(dir)
	if err != nil {
		t.Fatalf("Scan 失败: %v", err)
	}
	if len(got) != 5 {
		t.Fatalf("Scan 返回 %d 个 ISO，期望 5", len(got))
	}
	// 结果按 RelPath 排序，顺序稳定
	for i := 1; i < len(got); i++ {
		if got[i-1].RelPath >= got[i].RelPath {
			t.Errorf("结果未按 RelPath 排序: %q >= %q", got[i-1].RelPath, got[i].RelPath)
		}
	}
	var broken, nested int
	for _, is := range got {
		if is.Error != "" {
			broken++
		}
		if is.RelPath == "sub/nested.iso" {
			nested++
		}
		if is.ID == "" {
			t.Errorf("%q 的 ID 为空", is.RelPath)
		}
		if is.Path == "" || !filepath.IsAbs(is.Path) {
			t.Errorf("%q 的 Path 不是绝对路径: %q", is.RelPath, is.Path)
		}
	}
	if broken != 1 {
		t.Errorf("期望 1 个坏 ISO，实际 %d", broken)
	}
	if nested != 1 {
		t.Errorf("期望找到 sub/nested.iso，实际 %d", nested)
	}

	// 空目录
	emptyDir := t.TempDir()
	got2, err := Scan(emptyDir)
	if err != nil {
		t.Fatalf("Scan 空目录失败: %v", err)
	}
	if len(got2) != 0 {
		t.Errorf("空目录应返回 0 个 ISO，实际 %d", len(got2))
	}

	// 不存在的目录
	if _, err := Scan(filepath.Join(dir, "nope")); err == nil {
		t.Error("扫描不存在的目录应返回 error")
	}
	// 传文件而不是目录
	if _, err := Scan(filepath.Join(dir, "other.iso")); err == nil {
		t.Error("扫描文件应返回 error")
	}
}

func TestScanStableIDs(t *testing.T) {
	p := sampleImage(t)
	a, err := Probe(p)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Probe(p)
	if err != nil {
		t.Fatal(err)
	}
	if a.ID != b.ID {
		t.Errorf("同一文件的 ID 应稳定: %q != %q", a.ID, b.ID)
	}
	// 不同路径的 ID 不同
	p2 := sampleImage(t)
	c, err := Probe(p2)
	if err != nil {
		t.Fatal(err)
	}
	if a.ID == c.ID {
		t.Error("不同路径的 ID 不应相同")
	}
}

func TestConcurrentReads(t *testing.T) {
	r, err := Open(sampleImage(t))
	if err != nil {
		t.Fatalf("Open 失败: %v", err)
	}
	defer func() { _ = r.Close() }()

	// 模拟多台客户机同时 Range 读同一个 ISO
	var wg sync.WaitGroup
	errs := make(chan error, 32)
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ra, size, err := r.SectionReader("/boot/initramfs-lts")
			if err != nil {
				errs <- err
				return
			}
			if size != 9000 {
				errs <- errors.New("大小不对")
				return
			}
			off := int64(i * 100)
			buf := make([]byte, 64)
			if _, err := ra.ReadAt(buf, off); err != nil && !errors.Is(err, io.EOF) {
				errs <- err
				return
			}
			if !r.Exists("/boot/vmlinuz-lts") {
				errs <- errors.New("Exists 失败")
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("并发读取失败: %v", err)
	}
}

func TestHumanSize(t *testing.T) {
	tests := []struct {
		in   int64
		want string
	}{
		{512, "512 B"},
		{2048, "2.0 KB"},
		{1024 * 1024, "1.0 MB"},
		{3 * 1024 * 1024 * 1024, "3.0 GB"},
	}
	for _, tc := range tests {
		if got := humanSize(tc.in); got != tc.want {
			t.Errorf("humanSize(%d) = %q，期望 %q", tc.in, got, tc.want)
		}
	}
}

func TestInfoAndDescribe(t *testing.T) {
	img := model.ISO{
		Name:   "test.iso",
		Size:   2048,
		Family: model.FamilyLinux,
		OSName: "Test Linux",
		Boot:   model.BootSpec{Method: "kernel"},
	}
	if s := Info(img); !strings.Contains(s, "Test Linux") {
		t.Errorf("Info = %q", s)
	}
	if s := describe(&img); !strings.Contains(s, "Linux") {
		t.Errorf("describe = %q", s)
	}
	img.Error = "boom"
	if s := Info(img); !strings.Contains(s, "解析失败") {
		t.Errorf("Info(错误) = %q", s)
	}
	if s := describe(&img); !strings.Contains(s, "解析失败") {
		t.Errorf("describe(错误) = %q", s)
	}
}

func TestAutoInstallFor(t *testing.T) {
	tests := []struct {
		family model.ISOFamily
		name   string
		kind   string
	}{
		{model.FamilyWindows, "Windows 11", "autounattend"},
		{model.FamilyWinPE, "WinPE", "autounattend"},
		{model.FamilyLinux, "Ubuntu 24.04", "preseed"},
		{model.FamilyLinux, "Rocky Linux 9.4", "kickstart"},
		{model.FamilyLinux, "SUSE Linux Enterprise", "autoyast"},
		{model.FamilyLinux, "Alpine Linux 3.20", "cloudinit"},
	}
	for _, tc := range tests {
		got := AutoInstallFor(model.ISO{Family: tc.family, OSName: tc.name})
		if got == nil {
			t.Errorf("AutoInstallFor(%s, %s) = nil", tc.family, tc.name)
			continue
		}
		if got.Kind != tc.kind {
			t.Errorf("AutoInstallFor(%s, %s).Kind = %q，期望 %q", tc.family, tc.name, got.Kind, tc.kind)
		}
		if got.Template == "" {
			t.Errorf("AutoInstallFor(%s, %s).Template 为空", tc.family, tc.name)
		}
	}
	if AutoInstallFor(model.ISO{Family: model.FamilyOther}) != nil {
		t.Error("其它家族不应有自动安装")
	}
}

// 测试辅助：参数列表里是否有某个精确值
func hasArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

// 测试辅助：参数列表里是否有某个前缀
func hasArgPrefix(args []string, prefix string) bool {
	for _, a := range args {
		if strings.HasPrefix(a, prefix) {
			return true
		}
	}
	return false
}
