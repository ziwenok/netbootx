package iso

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/wupengcheng/netbootx/internal/model"
)

// realISOCandidates 是本地真实 ISO 的候选路径。
//
// 测试用真实发行版 ISO 验证解析器（自造镜像只能证明「理想镜像」正确）。
// 文件不存在时 t.Skip，不阻塞 CI；也不要把大文件提交进仓库。
var realISOCandidates = []string{
	os.Getenv("NBX_TEST_ISO"),
	"/tmp/nbx-test.iso",
	"/tmp/nbx-iso/alpine.iso",
}

// findRealISO 找一个可用的真实 ISO，找不到就跳过。
func findRealISO(t *testing.T) string {
	t.Helper()
	for _, p := range realISOCandidates {
		if p == "" {
			continue
		}
		st, err := os.Stat(p)
		if err != nil || !st.Mode().IsRegular() {
			continue
		}
		if st.Size() < 10*1024*1024 {
			continue
		}
		return p
	}
	t.Skip("没有本地真实 ISO，跳过（可设 NBX_TEST_ISO=/path/to/alpine.iso 启用）")
	return ""
}

// TestRealISOAlpine 用真实 Alpine ISO 验收：目录树、内核文件读取、Linux 识别。
func TestRealISOAlpine(t *testing.T) {
	p := findRealISO(t)
	r, err := Open(p)
	if err != nil {
		t.Fatalf("打开真实 ISO %q 失败: %v", p, err)
	}
	defer func() { _ = r.Close() }()

	t.Logf("ISO=%s 卷ID=%q 块大小=%d Joliet=%v RockRidge=%v 引导=%s",
		p, r.VolumeID(), r.BlockSize(), r.Joliet(), r.RockRidge(), r.BootInfo())

	if r.BlockSize() != SectorSize {
		t.Errorf("块大小 = %d，期望 %d", r.BlockSize(), SectorSize)
	}
	// 1. 能列根目录
	root, err := r.List("/")
	if err != nil {
		t.Fatalf("List(/) 失败: %v", err)
	}
	if len(root) == 0 {
		t.Fatal("根目录为空")
	}
	names := make([]string, 0, len(root))
	for _, e := range root {
		names = append(names, e.Name)
	}
	t.Logf("根目录 %d 项: %s", len(root), strings.Join(names, ", "))

	// 2. 真实发行版 ISO 一定有内核
	var kernel string
	for _, c := range []string{
		"/boot/vmlinuz-lts", "/boot/vmlinuz-virt", "/boot/vmlinuz",
		"/casper/vmlinuz", "/live/vmlinuz", "/isolinux/vmlinuz",
		"/images/pxeboot/vmlinuz", "/arch/boot/x86_64/vmlinuz", "/boot/Image",
	} {
		if r.Exists(c) {
			kernel = c
			break
		}
	}
	if kernel == "" {
		t.Fatalf("真实 ISO 里找不到内核，根目录: %v", names)
	}
	size, err := r.FileSize(kernel)
	if err != nil {
		t.Fatalf("FileSize(%q) 失败: %v", kernel, err)
	}
	if size < 100*1024 {
		t.Errorf("内核 %q 只有 %d 字节，太小了", kernel, size)
	}
	// 3. 能真正读到内核内容（Linux bzImage 以 MZ 开头）
	head := make([]byte, 512)
	n, err := r.ReadAt(kernel, head, 0)
	if err != nil && n == 0 {
		t.Fatalf("读取内核 %q 失败: %v", kernel, err)
	}
	if n < 2 {
		t.Fatalf("只读到 %d 字节", n)
	}
	if !bytes.Equal(head[:2], []byte{0x4d, 0x5a}) {
		t.Errorf("内核头不是 MZ（实际 %x），偏移可能算错了", head[:2])
	}
	t.Logf("内核 %s 大小 %d，头部 MZ ✓", kernel, size)

	// 4. 随机访问正确性：同一偏移读两次结果一致
	mid := size / 2
	b1 := make([]byte, 1024)
	b2 := make([]byte, 1024)
	if _, err := r.ReadAt(kernel, b1, mid); err != nil {
		t.Fatalf("从偏移 %d 读取失败: %v", mid, err)
	}
	if _, err := r.ReadAt(kernel, b2, mid); err != nil {
		t.Fatalf("从偏移 %d 二次读取失败: %v", mid, err)
	}
	if !bytes.Equal(b1, b2) {
		t.Error("同一偏移两次读取结果不一致")
	}

	// 5. SectionReader（httpd 用的接口）
	ra, srSize, err := r.SectionReader(kernel)
	if err != nil {
		t.Fatalf("SectionReader 失败: %v", err)
	}
	if srSize != size {
		t.Errorf("SectionReader 大小 = %d，期望 %d", srSize, size)
	}
	probe := make([]byte, 4)
	if _, err := ra.ReadAt(probe, 0); err != nil {
		t.Fatalf("SectionReader.ReadAt 失败: %v", err)
	}

	// 6. Probe 识别为 linux，且引导方式可执行
	img, err := Probe(p)
	if err != nil {
		t.Fatalf("Probe 失败: %v", err)
	}
	if img.Error != "" {
		t.Fatalf("Probe 报错: %s", img.Error)
	}
	if img.Family != model.FamilyLinux {
		t.Errorf("Family = %q，期望 linux", img.Family)
	}
	if !img.Bootable {
		t.Error("真实发行版 ISO 应可引导")
	}
	if img.Boot.Method != "kernel" {
		t.Errorf("Method = %q，期望 kernel", img.Boot.Method)
	}
	if img.Boot.Kernel == "" || len(img.Boot.Initrd) == 0 {
		t.Errorf("引导描述不完整: %+v", img.Boot)
	}
	// 声明的内核路径必须真能打开
	if !r.Exists(img.Boot.Kernel) {
		t.Errorf("Probe 给出的内核路径 %q 在 ISO 里不存在", img.Boot.Kernel)
	}
	for _, initrd := range img.Boot.Initrd {
		if !r.Exists(initrd) {
			t.Errorf("Probe 给出的 initrd %q 在 ISO 里不存在", initrd)
		}
	}
	t.Logf("识别结果: OSName=%q Family=%s Arch=%s Bootable=%v Method=%s Kernel=%s Initrd=%v",
		img.OSName, img.Family, img.OSArch, img.Bootable, img.Boot.Method, img.Boot.Kernel, img.Boot.Initrd)
}

// TestRealISODeepTree 遍历真实 ISO 的多级目录，验证 Rock Ridge / CE 续接块处理正确。
func TestRealISODeepTree(t *testing.T) {
	p := findRealISO(t)
	r, err := Open(p)
	if err != nil {
		t.Fatalf("Open 失败: %v", err)
	}
	defer func() { _ = r.Close() }()

	entries := 0
	var walk func(dir string, depth int)
	walk = func(dir string, depth int) {
		if depth > 6 {
			return
		}
		list, err := r.List(dir)
		if err != nil {
			return
		}
		for _, e := range list {
			entries++
			if e.IsDir {
				walk(e.Path, depth+1)
			} else if e.Size > 0 {
				// 抽查：能读到文件的第一个字节
				buf := make([]byte, 1)
				if _, err := r.ReadAt(e.Path, buf, 0); err != nil {
					t.Errorf("读取 %q 失败: %v", e.Path, err)
					return
				}
			}
		}
	}
	walk("/", 0)
	if entries < 10 {
		t.Errorf("只遍历到 %d 个条目，真实 ISO 不应该这么少", entries)
	}
	t.Logf("遍历到 %d 个条目", entries)
}
