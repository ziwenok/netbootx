package iso

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
	"unicode/utf16"
)

// 本文件是一个「最小 ISO9660 镜像生成器」，只给测试用。
//
// 它按照 ECMA-119 手工拼出一个合法镜像：PVD + 可选的 Joliet SVD（独立的 UCS-2 目录树）
// + 可选的 Rock Ridge（SP/NM/SL/PX）+ El Torito 引导目录 + 目录记录 + 文件数据。
// 这样测试就不依赖任何外部工具（mkisofs / genisoimage 在 CI 上不一定有），
// 也不需要提交二进制镜像进仓库。

// testFile 是待写入镜像的一个文件或目录。
type testFile struct {
	name     string
	data     []byte // 文件内容；目录为 nil
	dir      bool
	link     string // 非空表示 Rock Ridge 符号链接（目标为镜像内绝对路径）
	children []*testFile
	parent   *testFile

	lba  int64 // PVD 目录树里目录/文件数据的起始块
	jlba int64 // Joliet 目录树里目录的起始块
}

// isoImage 描述一个待生成的测试镜像。
type isoImage struct {
	volumeID  string
	root      *testFile
	useJoliet bool
	useRR     bool
	boot      *testBoot
}

// testBoot 描述测试镜像的 El Torito 配置。
type testBoot struct {
	biosImage bool // 写一个 x86 可引导条目
	efiImage  bool // 写一个 EFI 可引导条目
	imageData []byte
}

// newTestImage 创建一个带根目录的测试镜像（默认同时开 Joliet 和 Rock Ridge）。
func newTestImage(volumeID string) *isoImage {
	return &isoImage{
		volumeID:  volumeID,
		root:      &testFile{name: "", dir: true},
		useJoliet: true,
		useRR:     true,
	}
}

// addFile 在镜像里加一个文件，p 是镜像内绝对路径（如 "/boot/vmlinuz"）。
func (img *isoImage) addFile(p string, data []byte) *testFile {
	return img.add(p, &testFile{name: path.Base(p), data: data})
}

// addFileStr 加一个文本文件。
func (img *isoImage) addFileStr(p, s string) *testFile {
	return img.addFile(p, []byte(s))
}

// addDir 在镜像里加一个空目录。
func (img *isoImage) addDir(p string) *testFile {
	return img.add(p, &testFile{name: path.Base(p), dir: true})
}

// addSymlink 在镜像里加一个 Rock Ridge 符号链接。
func (img *isoImage) addSymlink(p, target string) *testFile {
	return img.add(p, &testFile{name: path.Base(p), link: target})
}

// add 把节点挂到目录树上（自动创建中间目录）。
func (img *isoImage) add(p string, node *testFile) *testFile {
	clean := strings.Trim(p, "/")
	parts := strings.Split(clean, "/")
	cur := img.root
	for i, part := range parts {
		if i == len(parts)-1 {
			node.name = part
			node.parent = cur
			cur.children = append(cur.children, node)
			return node
		}
		next := findChild(cur, part)
		if next == nil {
			next = &testFile{name: part, dir: true, parent: cur}
			cur.children = append(cur.children, next)
		}
		cur = next
	}
	return node
}

// findChild 在目录里找指定名字的子项。
func findChild(dir *testFile, name string) *testFile {
	for _, c := range dir.children {
		if c.name == name {
			return c
		}
	}
	return nil
}

// allDirs 返回所有目录（深度优先，根在前）。
func allDirs(root *testFile) []*testFile {
	out := []*testFile{root}
	var walk func(n *testFile)
	walk = func(n *testFile) {
		kids := append([]*testFile(nil), n.children...)
		sort.Slice(kids, func(i, j int) bool { return kids[i].name < kids[j].name })
		for _, c := range kids {
			if c.dir {
				out = append(out, c)
				walk(c)
			}
		}
	}
	walk(root)
	return out
}

// build 生成完整镜像字节。
func (img *isoImage) build() ([]byte, error) {
	if img.useRR && !img.useJoliet {
		return nil, fmt.Errorf("测试镜像生成器：Rock Ridge 必须和 Joliet 一起开启")
	}
	dirs := allDirs(img.root)
	next := int64(vdStartLBA + 3) // LBA 16/17/18 留给 PVD/SVD/BootRecord

	// 1. 给 PVD 目录树的每个目录分配一个块。
	for _, d := range dirs {
		d.lba = next
		next++
	}
	// 2. 给 Joliet 目录树的每个目录分配一个块（独立树，名字用 UCS-2）。
	if img.useJoliet {
		for _, d := range dirs {
			d.jlba = next
			next++
		}
	}
	// 3. 文件数据
	var files []*testFile
	var collect func(n *testFile)
	collect = func(n *testFile) {
		for _, c := range n.children {
			if c.dir {
				collect(c)
				continue
			}
			c.lba = next
			c.size()
			sectors := int64((len(c.data) + SectorSize - 1) / SectorSize)
			if sectors == 0 {
				sectors = 1
			}
			next += sectors
			files = append(files, c)
		}
	}
	collect(img.root)

	// 4. El Torito 引导映像 + 引导目录
	bootImageLBA := next
	if img.boot != nil && len(img.boot.imageData) > 0 {
		next += int64((len(img.boot.imageData) + SectorSize - 1) / SectorSize)
	}
	bootCatalogLBA := next
	if img.boot != nil {
		next++
	}
	buf := make([]byte, next*SectorSize)

	// 5. 卷描述符
	writePVD(buf, img, next)
	if img.useJoliet {
		writeSVD(buf, img, next)
	}
	if img.boot != nil {
		writeBootRecord(buf, bootCatalogLBA)
		writeBootCatalog(buf, bootCatalogLBA, img.boot, bootImageLBA)
		copy(buf[bootImageLBA*SectorSize:], img.boot.imageData)
	}

	// 6. 目录记录（两套树）+ 文件数据
	for _, d := range dirs {
		copy(buf[d.lba*SectorSize:], img.dirRecords(d, false))
		if img.useJoliet {
			copy(buf[d.jlba*SectorSize:], img.dirRecords(d, true))
		}
	}
	for _, f := range files {
		copy(buf[f.lba*SectorSize:], f.data)
	}
	return buf, nil
}

// size 返回文件长度。
func (f *testFile) size() int64 { return int64(len(f.data)) }

// dirRecords 生成某个目录的完整目录记录字节（含 "." 和 ".."）。
//
// ECMA-119 要求一条目录记录不能跨扇区；跨扇区时该扇区剩余部分填 0，
// 解析器看到 len==0 就跳到下一个扇区。
func (img *isoImage) dirRecords(dir *testFile, joliet bool) []byte {
	var buf bytes.Buffer
	write := func(rec []byte) {
		rem := buf.Len() % SectorSize
		if rem != 0 && rem+len(rec) > SectorSize {
			buf.Write(make([]byte, SectorSize-rem))
		}
		buf.Write(rec)
	}
	selfLBA := dir.lba
	parentLBA := dir.lba
	parent := dir.parent
	if joliet {
		selfLBA = dir.jlba
		if parent != nil {
			parentLBA = parent.jlba
		}
	} else if parent != nil {
		parentLBA = parent.lba
	}
	// "." 指向自己，父目录指向 parent（根目录的父指向自己）。
	write(img.dirRecord(".", dir, selfLBA, SectorSize, true, joliet))
	write(img.dirRecord("..", nil, parentLBA, SectorSize, true, joliet))
	kids := append([]*testFile(nil), dir.children...)
	sort.Slice(kids, func(i, j int) bool { return kids[i].name < kids[j].name })
	for _, c := range kids {
		lba := c.lba
		size := c.size()
		if c.dir {
			if joliet {
				lba = c.jlba
			} else {
				lba = c.lba
			}
			size = SectorSize
		}
		write(img.dirRecord(c.name, c, lba, size, c.dir, joliet))
	}
	rem := buf.Len() % SectorSize
	if rem != 0 {
		buf.Write(make([]byte, SectorSize-rem))
	}
	return buf.Bytes()
}

// dirRecord 生成一条 ISO9660 目录记录。
func (img *isoImage) dirRecord(name string, node *testFile, lba, size int64, isDir, joliet bool) []byte {
	var nameBytes []byte
	switch {
	case name == ".":
		nameBytes = []byte{0}
	case name == "..":
		nameBytes = []byte{1}
	case joliet:
		nameBytes = encodeUCS2(name)
	default:
		iso := strings.ToUpper(name)
		if !isDir {
			iso += ";1"
		}
		nameBytes = []byte(iso)
	}

	// Rock Ridge SUSP 只写在 PVD 树上：根目录 "." 放 SP，其它项放 NM/SL + PX。
	var su []byte
	if img.useRR && !joliet {
		if name == "." && node != nil && node.parent == nil {
			su = append(su, 'S', 'P', 7, 1, suspCheckBE, suspCheckEF, 0)
		}
		if name != "." && name != ".." && node != nil {
			su = append(su, encodeNM(name)...)
			if node.link != "" {
				su = append(su, encodeSL(node.link)...)
			}
			su = append(su, encodePX(isDir)...)
		}
	}

	base := 33 + len(nameBytes)
	if base%2 == 1 {
		base++
	}
	recLen := base + len(su)
	if recLen%2 == 1 {
		recLen++
	}
	if recLen > 255 {
		recLen = 255
	}
	rec := make([]byte, recLen)
	rec[0] = byte(recLen)
	rec[1] = 0
	binary.LittleEndian.PutUint32(rec[2:6], uint32(lba))
	binary.BigEndian.PutUint32(rec[6:10], uint32(lba))
	binary.LittleEndian.PutUint32(rec[10:14], uint32(size))
	binary.BigEndian.PutUint32(rec[14:18], uint32(size))
	copy(rec[18:25], encodeRecordingDate(time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)))
	if isDir {
		rec[25] = 0x02
	}
	binary.LittleEndian.PutUint16(rec[28:30], 1)
	binary.BigEndian.PutUint16(rec[30:32], 1)
	rec[32] = byte(len(nameBytes))
	copy(rec[33:], nameBytes)
	suOff := 33 + len(nameBytes)
	if suOff%2 == 1 {
		suOff++
	}
	if suOff+len(su) <= recLen {
		copy(rec[suOff:], su)
	}
	return rec
}

// encodeNM 生成 Rock Ridge NM 条目（文件名）。
func encodeNM(name string) []byte {
	length := 5 + len(name)
	if length%2 == 1 {
		length++
	}
	b := make([]byte, length)
	b[0], b[1], b[2], b[3] = 'N', 'M', byte(length), 1
	b[4] = 0
	copy(b[5:], name)
	return b
}

// encodeSL 生成 Rock Ridge SL 条目（符号链接）。
func encodeSL(target string) []byte {
	var comps []byte
	if strings.HasPrefix(target, "/") {
		comps = append(comps, 0x08, 0)
	}
	for _, part := range strings.Split(strings.Trim(target, "/"), "/") {
		if part == "" {
			continue
		}
		comps = append(comps, 0, byte(len(part)))
		comps = append(comps, part...)
	}
	length := 5 + len(comps)
	if length%2 == 1 {
		length++
	}
	b := make([]byte, length)
	b[0], b[1], b[2], b[3] = 'S', 'L', byte(length), 1
	b[4] = 0
	copy(b[5:], comps)
	return b
}

// encodePX 生成 Rock Ridge PX 条目（POSIX 属性占位）。
func encodePX(isDir bool) []byte {
	b := make([]byte, 44)
	b[0], b[1], b[2], b[3] = 'P', 'X', 44, 1
	mode := uint32(0o100644)
	if isDir {
		mode = 0o040755
	}
	binary.LittleEndian.PutUint32(b[4:8], mode)
	binary.BigEndian.PutUint32(b[8:12], mode)
	return b
}

// encodeUCS2 把字符串编码成 UCS-2 大端字节。
func encodeUCS2(s string) []byte {
	units := utf16.Encode([]rune(s))
	b := make([]byte, len(units)*2)
	for i, u := range units {
		binary.BigEndian.PutUint16(b[i*2:], u)
	}
	return b
}

// encodeRecordingDate 把时间编码成 7 字节目录记录日期。
func encodeRecordingDate(t time.Time) []byte {
	b := make([]byte, 7)
	b[0] = byte(t.Year() - 1900)
	b[1] = byte(t.Month())
	b[2] = byte(t.Day())
	b[3] = byte(t.Hour())
	b[4] = byte(t.Minute())
	b[5] = byte(t.Second())
	b[6] = 0
	return b
}

// writePVD 写主卷描述符（LBA 16）。
func writePVD(buf []byte, img *isoImage, totalSectors int64) {
	off := int64(vdStartLBA) * SectorSize
	b := buf[off : off+SectorSize]
	b[0] = vdPrimary
	copy(b[1:6], "CD001")
	b[6] = 1
	fillSpace(b[8:40])
	fillSpace(b[40:72])
	vol := []byte(img.volumeID)
	if len(vol) > 32 {
		vol = vol[:32]
	}
	copy(b[40:72], vol)
	binary.LittleEndian.PutUint32(b[80:84], uint32(totalSectors))
	binary.BigEndian.PutUint32(b[84:88], uint32(totalSectors))
	binary.LittleEndian.PutUint16(b[120:122], 1)
	binary.BigEndian.PutUint16(b[122:124], 1)
	binary.LittleEndian.PutUint16(b[124:126], 1)
	binary.BigEndian.PutUint16(b[126:128], 1)
	binary.LittleEndian.PutUint16(b[128:130], SectorSize)
	binary.BigEndian.PutUint16(b[130:132], SectorSize)
	binary.LittleEndian.PutUint32(b[132:136], SectorSize)
	binary.BigEndian.PutUint32(b[136:140], SectorSize)
	writeRootRecord(b[156:190], img.root.lba, SectorSize)
	b[881] = 1
}

// writeSVD 写 Joliet 补充卷描述符（LBA 17）。
func writeSVD(buf []byte, img *isoImage, totalSectors int64) {
	off := int64(vdStartLBA+1) * SectorSize
	b := buf[off : off+SectorSize]
	b[0] = vdSupplementary
	copy(b[1:6], "CD001")
	b[6] = 1
	fillSpace(b[8:40])
	fillSpace(b[40:72])
	enc := encodeUCS2(img.volumeID)
	if len(enc) > 32 {
		enc = enc[:32]
		// 不能把一个 UCS-2 字符劈成两半
		if len(enc)%2 == 1 {
			enc = enc[:len(enc)-1]
		}
	}
	copy(b[40:72], enc)
	binary.LittleEndian.PutUint32(b[80:84], uint32(totalSectors))
	binary.BigEndian.PutUint32(b[84:88], uint32(totalSectors))
	binary.LittleEndian.PutUint16(b[120:122], 1)
	binary.BigEndian.PutUint16(b[122:124], 1)
	binary.LittleEndian.PutUint16(b[124:126], 1)
	binary.BigEndian.PutUint16(b[126:128], 1)
	binary.LittleEndian.PutUint16(b[128:130], SectorSize)
	binary.BigEndian.PutUint16(b[130:132], SectorSize)
	binary.LittleEndian.PutUint32(b[132:136], SectorSize)
	binary.BigEndian.PutUint32(b[136:140], SectorSize)
	writeRootRecord(b[156:190], img.root.jlba, SectorSize)
	copy(b[88:91], "%/@")
	b[881] = 1
}

// writeBootRecord 写 El Torito 引导记录卷描述符（LBA 18，type 0）。
func writeBootRecord(buf []byte, catalogLBA int64) {
	off := int64(vdStartLBA+2) * SectorSize
	b := buf[off : off+SectorSize]
	b[0] = vdBootRecord
	copy(b[1:6], "CD001")
	b[6] = 1
	copy(b[7:39], "EL TORITO SPECIFICATION")
	binary.LittleEndian.PutUint32(b[0x47:0x4B], uint32(catalogLBA))
}

// writeBootCatalog 写 El Torito 引导目录：验证条目 + 默认条目 + 可选 EFI 分区段。
func writeBootCatalog(buf []byte, catalogLBA int64, boot *testBoot, imageLBA int64) {
	off := catalogLBA * SectorSize
	b := buf[off : off+SectorSize]
	b[0] = 0x01
	if boot.biosImage {
		b[1] = platformX86
	} else {
		b[1] = platformEFI
	}
	b[30], b[31] = 0x55, 0xAA
	e := b[32:64]
	e[0] = bootIndicatorBootable
	e[1] = mediaNoEmul
	binary.LittleEndian.PutUint16(e[2:4], 0x7C0)
	binary.LittleEndian.PutUint16(e[6:8], 4)
	binary.LittleEndian.PutUint32(e[8:12], uint32(imageLBA))

	if boot.efiImage && boot.biosImage {
		hdr := b[64:96]
		hdr[0] = sectionHeaderLast
		hdr[1] = platformEFI
		binary.LittleEndian.PutUint16(hdr[2:4], 1)
		ue := b[96:128]
		ue[0] = bootIndicatorBootable
		ue[1] = mediaNoEmul
		binary.LittleEndian.PutUint16(ue[2:4], 0x7C0)
		binary.LittleEndian.PutUint16(ue[6:8], 4)
		binary.LittleEndian.PutUint32(ue[8:12], uint32(imageLBA))
	}
}

// writeRootRecord 把根目录记录写进卷描述符。
func writeRootRecord(dst []byte, lba, size int64) {
	dst[0] = 34
	binary.LittleEndian.PutUint32(dst[2:6], uint32(lba))
	binary.BigEndian.PutUint32(dst[6:10], uint32(lba))
	binary.LittleEndian.PutUint32(dst[10:14], uint32(size))
	binary.BigEndian.PutUint32(dst[14:18], uint32(size))
	copy(dst[18:25], encodeRecordingDate(time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)))
	dst[25] = 0x02
	binary.LittleEndian.PutUint16(dst[28:30], 1)
	binary.BigEndian.PutUint16(dst[30:32], 1)
	dst[32] = 1
	dst[33] = 0
}

// fillSpace 用空格填充字节切片（ISO9660 的字符串字段填充约定）。
func fillSpace(b []byte) {
	for i := range b {
		b[i] = ' '
	}
}

// writeTempISO 把测试镜像写到临时目录，返回文件路径。
func writeTempISO(t *testing.T, img *isoImage) string {
	t.Helper()
	data, err := img.build()
	if err != nil {
		t.Fatalf("生成测试镜像失败: %v", err)
	}
	p := filepath.Join(t.TempDir(), "test.iso")
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatalf("写入测试镜像失败: %v", err)
	}
	return p
}
