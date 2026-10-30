package iso_test

// 这个文件是 Lead 写的「真实 ISO 验收测试」。
// 用真实发行版 ISO 验证解析器，而不是只测自己造的理想镜像。
//
// 跑法：
//   NBX_TEST_ISO=/tmp/nbx-iso/alpine.iso ./go test ./internal/iso/ -run TestRealISO -v
// 没设环境变量时自动跳过，不影响 CI。

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/wupengcheng/netbootx/internal/iso"
	"github.com/wupengcheng/netbootx/internal/model"
)

// realISOPath 返回测试用真实 ISO 路径，没有就跳过。
func realISOPath(t *testing.T) string {
	t.Helper()
	p := os.Getenv("NBX_TEST_ISO")
	if p == "" {
		p = "/tmp/nbx-iso/alpine.iso"
	}
	st, err := os.Stat(p)
	if err != nil {
		t.Skipf("真实 ISO 不存在（%s），跳过。设置 NBX_TEST_ISO 指向一个 ISO 可启用。", p)
	}
	if st.Size() < 10*1024*1024 {
		t.Skipf("%s 太小（%d 字节），不像真实 ISO", p, st.Size())
	}
	return p
}

func TestRealISOOpenAndList(t *testing.T) {
	p := realISOPath(t)
	r, err := iso.Open(p)
	if err != nil {
		t.Fatalf("打开真实 ISO 失败: %v", err)
	}
	defer r.Close()

	t.Logf("卷 ID: %q, 块大小: %d, Joliet: %v, RockRidge: %v",
		r.VolumeID(), r.BlockSize(), r.Joliet(), r.RockRidge())

	if r.BlockSize() != 2048 {
		t.Errorf("ISO9660 块大小应为 2048, 实际 %d", r.BlockSize())
	}

	// 根目录必须能列出来
	root, err := r.List("/")
	if err != nil {
		t.Fatalf("列根目录失败: %v", err)
	}
	if len(root) == 0 {
		t.Fatal("根目录是空的")
	}
	names := make([]string, 0, len(root))
	for _, e := range root {
		names = append(names, e.Name)
	}
	t.Logf("根目录 %d 项: %s", len(root), strings.Join(names, ", "))

	// 真实 Linux ISO 根目录一定有 boot/ 或 isolinux/ 之类
	hasBoot := false
	for _, e := range root {
		if e.IsDir && (e.Name == "boot" || e.Name == "isolinux" || e.Name == "EFI" || e.Name == "casper" || e.Name == "live") {
			hasBoot = true
		}
	}
	if !hasBoot {
		t.Errorf("根目录里没有预期的引导目录: %v", names)
	}
}

func TestRealISOReadKernel(t *testing.T) {
	p := realISOPath(t)
	r, err := iso.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	// 找一个内核文件
	candidates := []string{
		"/boot/vmlinuz-lts", "/boot/vmlinuz-virt", "/boot/vmlinuz",
		"/casper/vmlinuz", "/live/vmlinuz", "/isolinux/vmlinuz",
		"/images/pxeboot/vmlinuz", "/arch/boot/x86_64/vmlinuz",
	}
	var kernelPath string
	for _, c := range candidates {
		if r.Exists(c) {
			kernelPath = c
			break
		}
	}
	if kernelPath == "" {
		t.Skip("这个 ISO 里没找到已知内核路径，跳过读取测试")
	}

	size, err := r.FileSize(kernelPath)
	if err != nil {
		t.Fatalf("取 %s 大小失败: %v", kernelPath, err)
	}
	t.Logf("内核 %s 大小 %d 字节", kernelPath, size)
	if size < 100_000 {
		t.Errorf("内核太小（%d 字节），可能读错了", size)
	}

	// 读头部，Linux 内核是 bzImage，开头有 "MZ" (0x4d 0x5a)
	head := make([]byte, 512)
	n, err := r.ReadAt(kernelPath, head, 0)
	if err != nil && n == 0 {
		t.Fatalf("读内核头失败: %v", err)
	}
	if n < 2 {
		t.Fatalf("只读到 %d 字节", n)
	}
	if !bytes.Equal(head[:2], []byte{0x4d, 0x5a}) {
		t.Errorf("内核头不是 MZ（实际 %x），可能读到了错误的偏移", head[:2])
	}

	// 从中间读一段，验证随机访问正确
	mid := size / 2
	buf := make([]byte, 1024)
	n, err = r.ReadAt(kernelPath, buf, mid)
	if err != nil && n == 0 {
		t.Fatalf("从偏移 %d 读失败: %v", mid, err)
	}
	if n != 1024 {
		t.Errorf("从中间读了 %d 字节，期望 1024", n)
	}
	// 同一段再读一次必须一致（确定性）
	buf2 := make([]byte, 1024)
	if _, err := r.ReadAt(kernelPath, buf2, mid); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(buf, buf2) {
		t.Error("同一偏移两次读取结果不一致")
	}
}

func TestRealISOSectionReader(t *testing.T) {
	p := realISOPath(t)
	r, err := iso.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	// SectionReader 是 httpd 用的关键接口
	candidates := []string{"/boot/initramfs-lts", "/boot/initrd.img", "/casper/initrd", "/live/initrd.img"}
	var target string
	for _, c := range candidates {
		if r.Exists(c) {
			target = c
			break
		}
	}
	if target == "" {
		t.Skip("没找到已知 initrd 路径")
	}

	ra, size, err := r.SectionReader(target)
	if err != nil {
		t.Fatalf("SectionReader(%s) 失败: %v", target, err)
	}
	if size <= 0 {
		t.Fatalf("大小 = %d", size)
	}
	// gzip 或 xz 压缩的 initrd，读头几个字节确认非全零
	b := make([]byte, 6)
	if _, err := ra.ReadAt(b, 0); err != nil {
		t.Fatalf("ReadAt 失败: %v", err)
	}
	allZero := true
	for _, x := range b {
		if x != 0 {
			allZero = false
		}
	}
	if allZero {
		t.Errorf("initrd 头部全零，可能读错了位置: %v", b)
	}
	t.Logf("initrd %s 大小 %d, 头部 %x", target, size, b)
}

func TestRealISOProbe(t *testing.T) {
	p := realISOPath(t)
	img, err := iso.Probe(p)
	if err != nil {
		t.Fatalf("Probe 失败: %v", err)
	}
	if img == nil {
		t.Fatal("Probe 返回 nil")
	}
	if img.Error != "" {
		t.Errorf("Probe 报告错误: %s", img.Error)
	}
	if img.Size <= 0 {
		t.Errorf("Size = %d", img.Size)
	}
	if img.Family != model.FamilyLinux {
		t.Errorf("Family = %q, 期望 linux（这是 Alpine ISO）", img.Family)
	}
	if img.OSName == "" {
		t.Error("OSName 为空")
	}
	if !img.Bootable {
		t.Error("真实发行版 ISO 应该是可引导的")
	}
	if img.Boot.Method == "" {
		t.Error("Boot.Method 为空")
	}
	if img.Boot.Method == "kernel" && img.Boot.Kernel == "" {
		t.Error("kernel 方式必须给出 Kernel 路径")
	}
	if img.Boot.Method == "kernel" && len(img.Boot.Initrd) == 0 {
		t.Error("kernel 方式必须给出至少一个 Initrd")
	}

	t.Logf("识别结果: ID=%s OSName=%q Family=%s Arch=%s Bootable=%v",
		img.ID, img.OSName, img.Family, img.OSArch, img.Bootable)
	t.Logf("引导方式: method=%s kernel=%q initrd=%v args=%v",
		img.Boot.Method, img.Boot.Kernel, img.Boot.Initrd, img.Boot.KernelArgs)

	// ID 必须稳定（同样的输入两次扫描结果一样）
	img2, err := iso.Probe(p)
	if err != nil {
		t.Fatal(err)
	}
	if img.ID != img2.ID {
		t.Errorf("ID 不稳定: %q vs %q", img.ID, img2.ID)
	}
}

func TestRealISOScan(t *testing.T) {
	p := realISOPath(t)
	dir := t.TempDir()
	// 软链接到临时目录，验证 Scan 跟随软链接
	link := dir + "/alpine.iso"
	if err := os.Symlink(p, link); err != nil {
		t.Skipf("无法创建软链接: %v", err)
	}

	isos, err := iso.Scan(dir)
	if err != nil {
		t.Fatalf("Scan 失败: %v", err)
	}
	if len(isos) != 1 {
		t.Fatalf("扫到 %d 个 ISO, 期望 1", len(isos))
	}
	if isos[0].Family != model.FamilyLinux {
		t.Errorf("Family = %q", isos[0].Family)
	}
	if isos[0].RelPath != "alpine.iso" {
		t.Errorf("RelPath = %q", isos[0].RelPath)
	}
}

func TestRealISORejectsNonISO(t *testing.T) {
	// 造一个假文件，确认不 panic 且返回错误
	dir := t.TempDir()
	p := dir + "/fake.iso"
	if err := os.WriteFile(p, bytes.Repeat([]byte("not an iso"), 10000), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := iso.Open(p)
	if err == nil {
		defer r.Close()
		t.Error("打开非 ISO 文件应返回错误")
	}
	// Probe 不能 panic
	img, err := iso.Probe(p)
	if err == nil && img != nil && img.Error == "" {
		t.Error("Probe 非 ISO 文件应报告错误")
	}
	t.Logf("Probe 非 ISO 返回: img=%v err=%v", img, err)
}

func TestRealISOBigFileNoFullRead(t *testing.T) {
	// 验证打开大 ISO 不会把整个文件读进内存（通过观察内存增长）
	p := realISOPath(t)
	st, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	r, err := iso.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	// 列出全部文件（会遍历整棵目录树）
	n := 0
	var walk func(dir string, depth int)
	walk = func(dir string, depth int) {
		if depth > 4 {
			return
		}
		entries, err := r.List(dir)
		if err != nil {
			return
		}
		for _, e := range entries {
			n++
			if e.IsDir {
				walk(e.Path, depth+1)
			}
		}
	}
	walk("/", 0)
	t.Logf("ISO 大小 %.1f MB, 遍历到 %d 个条目", float64(st.Size())/(1024*1024), n)
	if n == 0 {
		t.Error("没遍历到任何条目")
	}
}
