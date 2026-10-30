package iso

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestMultiExtentFile 验证 >4GB 文件用的「多段目录记录」能被合并成一个逻辑文件。
//
// 造法：把同一个文件拆成两条同名记录，第一条 flags 带 0x80（还有续段）。
// 这里直接在生成的镜像上手工改目录记录，比扩展生成器更贴近真实镜像的形态。
func TestMultiExtentFile(t *testing.T) {
	img := newTestImage("MULTI-EXTENT")
	img.useRR = false
	img.useJoliet = false
	// 两个各 2048 字节的块，逻辑上拼成一个 4096 字节的文件。
	img.addFile("/big.bin", bytes.Repeat([]byte("A"), 2048))
	img.addFile("/tail.bin", bytes.Repeat([]byte("B"), 2048))
	data, err := img.build()
	if err != nil {
		t.Fatalf("生成镜像失败: %v", err)
	}

	// 找到根目录记录区，把 tail.bin 那条改成 big.bin 的续段：
	// 同名 + flags |= 0x80 在前一条上。
	rootLBA := int64(16 + 3) // 生成器把根目录放在卷描述符之后的第一块
	off := int(rootLBA * SectorSize)
	patched := false
	for off < len(data) {
		rl := int(data[off])
		if rl == 0 {
			off = (off/SectorSize + 1) * SectorSize
			continue
		}
		if rl < 33 || off+rl > len(data) {
			break
		}
		nameLen := int(data[off+32])
		name := string(data[off+33 : off+33+nameLen])
		switch name {
		case "BIG.BIN;1":
			data[off+25] |= 0x80 // 标记：还有续段
			patched = true
		case "TAIL.BIN;1":
			// 改名成 BIG.BIN;1（长度必须一致）
			copy(data[off+33:off+33+nameLen], "BIG.BIN;1")
		}
		off += rl
	}
	if !patched {
		t.Fatal("没找到 BIG.BIN 目录记录，测试镜像布局变了")
	}

	p := filepath.Join(t.TempDir(), "multi.iso")
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	r, err := Open(p)
	if err != nil {
		t.Fatalf("Open 失败: %v", err)
	}
	defer func() { _ = r.Close() }()

	f, err := r.Lookup("/big.bin")
	if err != nil {
		t.Fatalf("多段文件应能查到: %v", err)
	}
	if f.Size != 4096 {
		t.Errorf("多段文件大小 = %d，期望 4096", f.Size)
	}
	if len(f.extents) != 2 {
		t.Errorf("多段文件应有 2 个 extent，实际 %d: %+v", len(f.extents), f.extents)
	}
	// 跨段读取：从第一段末尾读 4 字节，应跨到第二段拿到 "BBBB"
	buf := make([]byte, 4)
	if _, err := f.ReadAt(buf, 2046); err != nil {
		t.Fatalf("跨段读取失败: %v", err)
	}
	if string(buf) != "AABB" {
		t.Errorf("跨段读取 = %q，期望 AABB", buf)
	}
	// 只出现在 / 一次（续段被合并，不重复出现）
	root, err := r.List("/")
	if err != nil {
		t.Fatal(err)
	}
	// 关掉 Rock Ridge / Joliet 时名字是 ISO9660 大写形式（BIG.BIN），
	// 这里不区分大小写比较，只验证「续段被合并、没有重复出现」。
	count := 0
	for _, e := range root {
		if strings.EqualFold(e.Name, "big.bin") {
			count++
		}
	}
	if count != 1 {
		t.Errorf("多段文件在目录里应只出现 1 次，实际 %d", count)
	}
}

// TestScanFollowsFileSymlink 验证 Scan 会跟随指向文件的软链接（README 推荐用法），
// 但不会进入目录软链接（避免环路）。
func TestScanFollowsFileSymlink(t *testing.T) {
	dir := t.TempDir()
	// 真实 ISO 放在别处，iso 目录里只放软链接
	realDir := t.TempDir()
	img := newTestImage("LINKED")
	img.addFile("/boot/vmlinuz-lts", []byte("K"))
	img.addFile("/boot/initramfs-lts", []byte("I"))
	img.addFileStr("/.alpine-release", "3.20.10")
	realPath := filepath.Join(realDir, "alpine.iso")
	data, err := img.build()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(realPath, data, 0o600); err != nil {
		t.Fatal(err)
	}

	link := filepath.Join(dir, "alpine.iso")
	if err := os.Symlink(realPath, link); err != nil {
		t.Skipf("无法创建软链接: %v", err)
	}
	// 悬空软链接：应被忽略而不是报错
	if err := os.Symlink(filepath.Join(realDir, "missing.iso"), filepath.Join(dir, "dangling.iso")); err != nil {
		t.Fatal(err)
	}
	// 目录软链接：不应进入（否则可能成环）
	sub := filepath.Join(dir, "subdir")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realDir, filepath.Join(sub, "loop")); err != nil {
		t.Fatal(err)
	}

	got, err := Scan(dir)
	if err != nil {
		t.Fatalf("Scan 失败: %v", err)
	}
	if len(got) != 1 {
		names := make([]string, 0, len(got))
		for _, is := range got {
			names = append(names, is.RelPath)
		}
		t.Fatalf("扫到 %d 个 ISO（%v），期望 1 个（只认文件软链接）", len(got), names)
	}
	if got[0].RelPath != "alpine.iso" {
		t.Errorf("RelPath = %q，期望 alpine.iso", got[0].RelPath)
	}
	if got[0].Family != "linux" {
		t.Errorf("Family = %q，期望 linux", got[0].Family)
	}
}

// TestScanHiddenDirsSkipped 验证隐藏目录（.git / 缓存）不会被扫描。
func TestScanHiddenDirsSkipped(t *testing.T) {
	dir := t.TempDir()
	hidden := filepath.Join(dir, ".cache")
	if err := os.MkdirAll(hidden, 0o755); err != nil {
		t.Fatal(err)
	}
	data, _ := newTestImage("HIDDEN").build()
	if err := os.WriteFile(filepath.Join(hidden, "a.iso"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := Scan(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("隐藏目录里的 ISO 不应被扫到，实际 %d 个", len(got))
	}
}

// TestTruncatedISO 验证截断的 ISO 不会 panic。
func TestTruncatedISO(t *testing.T) {
	img := newTestImage("TRUNC")
	img.addFile("/boot/vmlinuz-lts", bytes.Repeat([]byte("K"), 10000))
	data, err := img.build()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	// 分别截断到不同长度，都要能「不 panic」地处理
	for _, cut := range []int{0, 100, SectorSize * 16, SectorSize * 17, len(data) / 2} {
		if cut > len(data) {
			continue
		}
		p := filepath.Join(dir, "trunc.iso")
		if err := os.WriteFile(p, data[:cut], 0o600); err != nil {
			t.Fatal(err)
		}
		func() {
			defer func() {
				if rec := recover(); rec != nil {
					t.Errorf("截断到 %d 字节时 panic 了: %v", cut, rec)
				}
			}()
			_, _ = Probe(p) // 允许返回 error，但绝不能 panic
		}()
	}
}
