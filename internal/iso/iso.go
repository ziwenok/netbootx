package iso

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf16"
)

// 常量集中定义，便于测试和复用。
const (
	// SectorSize 是 ISO9660 的逻辑扇区大小（标准固定 2048 字节）。
	SectorSize = 2048

	// vdStartLBA 是卷描述符集合的起始逻辑块号（标准规定从第 16 块开始）。
	vdStartLBA = 16
	// maxVolumeDescriptors 限制卷描述符扫描数量，防止损坏镜像导致死循环。
	maxVolumeDescriptors = 64

	// 目录遍历保护：防止损坏镜像造成无限递归 / 内存爆炸。
	maxDirDepth      = 64
	maxDirBytes      = 64 << 20 // 单个目录最多 64MB 记录
	maxDirRecords    = 200000
	maxExtentsPerFile = 64

	// 符号链接最大解引用深度。
	maxSymlinkDepth = 16
)

// 本包对外的哨兵错误，调用方可用 errors.Is 判断。
var (
	// ErrNotExist 表示 ISO 内路径不存在。
	ErrNotExist = errors.New("iso: 路径不存在")
	// ErrIsDir 表示对目录做了文件读取操作。
	ErrIsDir = errors.New("iso: 目标是目录")
	// ErrNotDir 表示对文件做了目录列举操作。
	ErrNotDir = errors.New("iso: 目标不是目录")
	// ErrClosed 表示 Reader 已关闭。
	ErrClosed = errors.New("iso: 读取器已关闭")
	// ErrInvalidOffset 表示偏移量为负数。
	ErrInvalidOffset = errors.New("iso: 偏移量非法")
	// ErrInvalidImage 表示文件不是有效的 ISO9660 镜像。
	ErrInvalidImage = errors.New("iso: 不是有效的 ISO9660 镜像")
)

// File 是 ISO9660 里的一个文件或目录。
// 它自带读能力（ReadAt），因此 httpd 可以直接用 io.NewSectionReader 包住它。
type File struct {
	Path    string    // ISO 内绝对路径，如 "/casper/vmlinuz"
	Name    string    // 名字（不含路径）
	Size    int64     // 字节数；目录为其目录记录总长度
	IsDir   bool      // 是否目录
	LBA     int64     // 起始逻辑块号
	ModTime time.Time // 记录时间（可能为零值）
	// Link 非空表示这是一个 Rock Ridge 符号链接，内容是 ISO 内绝对目标路径。
	Link string

	rd          *Reader
	extents     []extent // 多段文件（>4GB 或跨段）由多个 extent 组成
	multiExtent bool     // 还有续段未合并（仅构建目录树时使用）
}

// extent 是一段连续的磁盘数据。
type extent struct {
	lba    int64 // 逻辑块号
	length int64 // 字节数
}

// ReadAt 实现 io.ReaderAt，从文件内部偏移 off 处读取。
// 文件由多段（multi-extent）组成时自动跨段拼接；符号链接由 Reader 解析。
func (f *File) ReadAt(p []byte, off int64) (int, error) {
	if f == nil || f.rd == nil {
		return 0, fmt.Errorf("iso: 文件未绑定读取器: %w", ErrNotExist)
	}
	if f.IsDir {
		return 0, ErrIsDir
	}
	if off < 0 {
		return 0, fmt.Errorf("iso: 偏移 %d: %w", off, ErrInvalidOffset)
	}
	if len(p) == 0 {
		return 0, nil
	}
	if off >= f.Size {
		return 0, io.EOF
	}
	want := len(p)
	if avail := f.Size - off; int64(want) > avail {
		want = int(avail)
	}
	n, err := f.rd.readExtents(f, p[:want], off)
	if err != nil {
		return n, err
	}
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

// FileEntry 是 List 返回的目录项快照（值类型，脱离 Reader 生命周期后仍可用）。
type FileEntry struct {
	Name    string    // 名字
	Path    string    // ISO 内绝对路径
	Size    int64     // 字节数
	IsDir   bool      // 是否目录
	Link    string    // 符号链接目标（ISO 内绝对路径），空表示普通文件
	ModTime time.Time // 记录时间
}

// Reader 是一个已打开的 ISO9660 镜像。
//
// 所有方法都可被多个 goroutine 并发调用；目录树在首次访问时一次性建好并缓存。
type Reader struct {
	f    *os.File
	path string
	size int64
	bs   int64 // 逻辑块大小（通常 2048）

	mu     sync.RWMutex
	closed bool

	pvd    *volumeDescriptor
	joliet *volumeDescriptor
	rr     bool // 是否启用 Rock Ridge

	entries  map[string]*File   // 绝对路径 -> 文件
	children map[string][]*File // 父目录路径 -> 子项

	treeOnce sync.Once
	treeErr  error

	root *File
	boot *BootInfo

	// bootRecord 是 El Torito 的 Boot Record Volume Descriptor（type 0）。
	bootRecord *volumeDescriptor
}

// volumeDescriptor 是解析后的卷描述符（PVD 或 Joliet SVD）。
type volumeDescriptor struct {
	typ         byte
	joliet      bool
	volumeID    string
	bs          int64
	volumeSpace int64
	rootLBA     int64
	rootSize    int64
	escape      string
}

// Open 打开一个 ISO 文件并解析卷描述符、目录树和 El Torito 引导信息。
//
// 返回的 Reader 必须调用 Close 释放文件句柄。任何解析失败都返回错误而不是 panic。
func Open(path string) (*Reader, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("解析 ISO 路径 %q: %w", path, err)
	}
	f, err := os.Open(abs)
	if err != nil {
		return nil, fmt.Errorf("打开 ISO %q: %w", abs, err)
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("读取 ISO 状态 %q: %w", abs, err)
	}
	if st.IsDir() {
		_ = f.Close()
		return nil, fmt.Errorf("ISO 路径 %q 是目录而不是文件: %w", abs, ErrInvalidImage)
	}
	r := &Reader{f: f, path: abs, size: st.Size()}
	if err := r.readVolumeDescriptors(); err != nil {
		_ = f.Close()
		return nil, err
	}
	if err := r.ensureTree(); err != nil {
		_ = f.Close()
		return nil, err
	}
	// El Torito 解析失败不影响读取，只影响「是否可引导」的判断。
	if bi, err := r.readBootInfo(); err == nil && bi != nil {
		r.boot = bi
	}
	return r, nil
}

// Path 返回 ISO 文件在磁盘上的绝对路径。
func (r *Reader) Path() string { return r.path }

// Size 返回 ISO 文件字节数。
func (r *Reader) Size() int64 { return r.size }

// BlockSize 返回逻辑块大小（通常是 2048）。
func (r *Reader) BlockSize() int64 {
	if r.bs > 0 {
		return r.bs
	}
	return SectorSize
}

// VolumeID 返回 PVD 里的卷标识（通常是发行版名字，最多 32 字符）。
func (r *Reader) VolumeID() string {
	if r.pvd == nil {
		return ""
	}
	return r.pvd.volumeID
}

// Joliet 报告是否使用了 Joliet 文件名（UCS-2）。
func (r *Reader) Joliet() bool { return r.joliet != nil }

// RockRidge 报告是否使用了 Rock Ridge 扩展（POSIX 文件名 / 符号链接）。
func (r *Reader) RockRidge() bool { return r.rr }

// BootInfo 返回 El Torito 引导信息，不可引导时返回 nil。
func (r *Reader) BootInfo() *BootInfo { return r.boot }

// Root 返回根目录。
func (r *Reader) Root() *File { return r.root }

// Close 关闭底层文件句柄，可重复调用。
func (r *Reader) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil
	}
	r.closed = true
	if r.f == nil {
		return nil
	}
	if err := r.f.Close(); err != nil {
		return fmt.Errorf("关闭 ISO %q: %w", r.path, err)
	}
	return nil
}

// check 在每次操作前确认 Reader 仍然可用。
func (r *Reader) check() error {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.closed {
		return ErrClosed
	}
	return nil
}

// Lookup 在 ISO 内查找路径 p（如 "/casper/vmlinuz"），返回文件对象。
// 路径可以是相对形式（"casper/vmlinuz"），会自动补前导 "/"。
// 精确匹配失败时会做大小写不敏感回退。
func (r *Reader) Lookup(p string) (*File, error) {
	if err := r.check(); err != nil {
		return nil, err
	}
	if err := r.ensureTree(); err != nil {
		return nil, err
	}
	clean, ok := cleanPath(p)
	if !ok {
		return nil, fmt.Errorf("ISO 内路径 %q 非法: %w", p, ErrNotExist)
	}
	f := r.find(clean)
	if f == nil {
		return nil, fmt.Errorf("ISO 内路径 %q 不存在: %w", clean, ErrNotExist)
	}
	// 解引用 Rock Ridge 符号链接（如 Debian Live 的 /vmlinuz -> /boot/vmlinuz-xxx）。
	for i := 0; i < maxSymlinkDepth && f.Link != ""; i++ {
		next := r.find(f.Link)
		if next == nil {
			return nil, fmt.Errorf("ISO 内符号链接 %q 指向不存在的 %q: %w", f.Path, f.Link, ErrNotExist)
		}
		f = next
	}
	if f.Link != "" {
		return nil, fmt.Errorf("ISO 内符号链接 %q 解引用层数过深: %w", clean, ErrNotExist)
	}
	return f, nil
}

// Exists 报告 ISO 内路径是否存在（目录也算存在）。
func (r *Reader) Exists(p string) bool {
	_, err := r.Lookup(p)
	return err == nil
}

// FileSize 返回 ISO 内文件的字节数；目录返回 ErrIsDir。
func (r *Reader) FileSize(p string) (int64, error) {
	f, err := r.Lookup(p)
	if err != nil {
		return 0, err
	}
	if f.IsDir {
		return 0, fmt.Errorf("ISO 内路径 %q: %w", f.Path, ErrIsDir)
	}
	return f.Size, nil
}

// ReadAt 从 ISO 内文件 p 的偏移 off 处读取数据，语义与 io.ReaderAt 一致。
func (r *Reader) ReadAt(p string, buf []byte, off int64) (int, error) {
	f, err := r.Lookup(p)
	if err != nil {
		return 0, err
	}
	return f.ReadAt(buf, off)
}

// SectionReader 把 ISO 内文件包成 io.ReaderAt，并返回文件大小。
// httpd 用它配合 http.ServeContent 实现 Range 请求，不落地、不整读。
func (r *Reader) SectionReader(p string) (io.ReaderAt, int64, error) {
	f, err := r.Lookup(p)
	if err != nil {
		return nil, 0, err
	}
	if f.IsDir {
		return nil, 0, fmt.Errorf("ISO 内路径 %q: %w", f.Path, ErrIsDir)
	}
	return io.NewSectionReader(f, 0, f.Size), f.Size, nil
}

// ReadFile 读取 ISO 内小文件的全部内容（如 .disk/info、etc/os-release）。
// limit > 0 时最多读 limit 字节，避免误读巨型文件耗尽内存。
func (r *Reader) ReadFile(p string, limit int64) ([]byte, error) {
	f, err := r.Lookup(p)
	if err != nil {
		return nil, err
	}
	if f.IsDir {
		return nil, fmt.Errorf("ISO 内路径 %q: %w", f.Path, ErrIsDir)
	}
	n := f.Size
	if limit > 0 && n > limit {
		n = limit
	}
	buf := make([]byte, n)
	m, err := f.ReadAt(buf, 0)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return buf[:m], nil
}

// List 列出目录 dir 的直接子项：目录在前，然后按名字（不区分大小写）排序。
func (r *Reader) List(dir string) ([]FileEntry, error) {
	f, err := r.Lookup(dir)
	if err != nil {
		return nil, err
	}
	if !f.IsDir {
		return nil, fmt.Errorf("ISO 内路径 %q: %w", f.Path, ErrNotDir)
	}
	r.mu.RLock()
	kids := r.children[f.Path]
	out := make([]FileEntry, 0, len(kids))
	for _, c := range kids {
		out = append(out, FileEntry{
			Name:    c.Name,
			Path:    c.Path,
			Size:    c.Size,
			IsDir:   c.IsDir,
			Link:    c.Link,
			ModTime: c.ModTime,
		})
	}
	r.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool {
		if out[i].IsDir != out[j].IsDir {
			return out[i].IsDir
		}
		li, lj := strings.ToLower(out[i].Name), strings.ToLower(out[j].Name)
		if li != lj {
			return li < lj
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

// find 在已建好的目录树里查路径：先精确匹配，再大小写不敏感回退。
func (r *Reader) find(p string) *File {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.entries == nil {
		return nil
	}
	if f, ok := r.entries[p]; ok {
		return f
	}
	var best *File
	var bestKey string
	for k, f := range r.entries {
		if strings.EqualFold(k, p) {
			if best == nil || k < bestKey {
				best, bestKey = f, k
			}
		}
	}
	return best
}

// ensureTree 惰性构建目录树，只会执行一次；并发调用会等待构建完成。
func (r *Reader) ensureTree() error {
	r.treeOnce.Do(func() {
		r.treeErr = r.buildTree()
	})
	return r.treeErr
}

// buildTree 从根目录开始递归建立「绝对路径 -> File」索引。
func (r *Reader) buildTree() error {
	vd := r.pvd
	useJoliet := false
	if vd == nil {
		return fmt.Errorf("%w: 缺少 Primary Volume Descriptor", ErrInvalidImage)
	}
	// Rock Ridge 优先：它能提供大小写正确的 POSIX 文件名和符号链接。
	if r.joliet != nil && !r.rr {
		vd = r.joliet
		useJoliet = true
	}
	root, err := r.readRootRecord(vd, useJoliet)
	if err != nil {
		return err
	}
	root.Name = "/"
	root.Path = "/"
	root.IsDir = true
	r.root = root
	r.entries = map[string]*File{"/": root}
	r.children = make(map[string][]*File)
	if err := r.walk(root, 0); err != nil {
		return err
	}
	return nil
}

// readRootRecord 读取卷描述符里内嵌的根目录记录。
func (r *Reader) readRootRecord(vd *volumeDescriptor, useJoliet bool) (*File, error) {
	sector := make([]byte, r.BlockSize())
	if _, err := r.f.ReadAt(sector, vd.rootLBA*r.BlockSize()); err != nil {
		return nil, fmt.Errorf("读取根目录记录（LBA %d）: %w", vd.rootLBA, err)
	}
	rec, ok := r.parseDirRecord(sector, useJoliet)
	if !ok {
		return nil, fmt.Errorf("根目录记录格式非法（LBA %d）: %w", vd.rootLBA, ErrInvalidImage)
	}
	f := &File{
		Size:    rec.size,
		IsDir:   true,
		LBA:     rec.lba,
		ModTime: rec.modTime,
		rd:      r,
	}
	if len(rec.extents) > 0 {
		f.extents = rec.extents
		f.LBA = rec.extents[0].lba
	} else {
		f.extents = []extent{{lba: rec.lba, length: rec.size}}
	}
	return f, nil
}

// walk 递归遍历目录。depth 用于防止损坏镜像造成的无限递归。
func (r *Reader) walk(dir *File, depth int) error {
	if depth > maxDirDepth {
		return nil
	}
	recs, err := r.readDirRecords(dir)
	if err != nil {
		// 子目录读失败不致命：保留已解析的部分，继续其它分支。
		return nil
	}
	for _, rec := range recs {
		if rec.name == "" || rec.name == "." || rec.name == ".." {
			continue
		}
		path := joinPath(dir.Path, rec.name)
		child := &File{
			Path:    path,
			Name:    rec.name,
			Size:    rec.size,
			IsDir:   rec.isDir,
			LBA:     rec.lba,
			ModTime: rec.modTime,
			rd:      r,
		}
		if len(rec.extents) > 0 {
			child.extents = rec.extents
			child.LBA = rec.extents[0].lba
		} else {
			child.extents = []extent{{lba: rec.lba, length: rec.size}}
		}
		child.multiExtent = rec.multiExtent
		if rec.link != "" {
			if target, ok := cleanPath(joinPath(dir.Path, rec.link)); ok {
				child.Link = target
			}
		}
		if prev, dup := r.entries[path]; dup {
			// 同名项：只有「多段文件」的续段才合并，其它情况保留第一个。
			if prev.multiExtent && !prev.IsDir && !child.IsDir {
				prev.extents = append(prev.extents, child.extents...)
				prev.Size += child.Size
				prev.multiExtent = child.multiExtent
			}
			continue
		}
		r.entries[path] = child
		r.children[dir.Path] = append(r.children[dir.Path], child)
		if child.IsDir {
			if err := r.walk(child, depth+1); err != nil {
				return err
			}
		}
	}
	return nil
}

// dirRecord 是从目录记录里解析出来的中间结构。
type dirRecord struct {
	name        string
	lba         int64
	size        int64
	isDir       bool
	modTime     time.Time
	extents     []extent // 多段文件
	link        string   // Rock Ridge SL 目标（相对路径）
	multiExtent bool     // 标志位 0x80：同名记录还有下一段（>4GB 文件用）
}

// readDirRecords 读取目录 dir 的全部目录记录。
func (r *Reader) readDirRecords(dir *File) ([]dirRecord, error) {
	var out []dirRecord
	for i, ex := range dir.extents {
		if i >= maxExtentsPerFile {
			break
		}
		if ex.length <= 0 {
			continue
		}
		if ex.length > maxDirBytes {
			return out, fmt.Errorf("目录 %q 过大（%d 字节）: %w", dir.Path, ex.length, ErrInvalidImage)
		}
		buf := make([]byte, ex.length)
		if _, err := r.f.ReadAt(buf, ex.lba*r.BlockSize()); err != nil && !errors.Is(err, io.EOF) {
			return out, fmt.Errorf("读取目录 %q（LBA %d）: %w", dir.Path, ex.lba, err)
		}
		recs, err := r.parseDirBlock(buf)
		if err != nil {
			return out, err
		}
		out = append(out, recs...)
		if len(out) > maxDirRecords {
			return out[:maxDirRecords], nil
		}
	}
	return out, nil
}

// parseDirBlock 解析一段目录记录缓冲区。
// 记录长度为 0 表示本扇区剩余全是填充，直接跳到下一扇区。
func (r *Reader) parseDirBlock(buf []byte) ([]dirRecord, error) {
	bs := r.BlockSize()
	var out []dirRecord
	useJoliet := r.useJolietNames()
	for off := int64(0); off < int64(len(buf)); {
		recLen := int(buf[off])
		if recLen == 0 {
			next := (off/bs + 1) * bs
			if next <= off {
				break
			}
			off = next
			continue
		}
		if recLen < 33 || off+int64(recLen) > int64(len(buf)) {
			break
		}
		rec, ok := r.parseDirRecord(buf[off:off+int64(recLen)], useJoliet)
		if ok {
			out = append(out, rec)
		}
		off += int64(recLen)
	}
	return out, nil
}

// useJolietNames 报告目录树是否用 Joliet 名字解码。
func (r *Reader) useJolietNames() bool {
	if r.pvd == nil || r.joliet == nil {
		return false
	}
	return !r.rr
}

// parseDirRecord 解析单条 ISO9660 目录记录。
//
// 记录格式（ECMA-119 9.1）：
//
//	len(1) extAttrLen(1) extent_LBA_LE(4) extent_LBA_BE(4)
//	dataLen_LE(4) dataLen_BE(4) date(7) flags(1) fileUnit(1)
//	interleave(1) volSeq(4) nameLen(1) name(nameLen) pad(0/1) systemUse(...)
func (r *Reader) parseDirRecord(b []byte, useJoliet bool) (dirRecord, bool) {
	if len(b) < 33 {
		return dirRecord{}, false
	}
	extAttrLen := int64(b[1])
	lbaLE := int64(binary.LittleEndian.Uint32(b[2:6]))
	lbaBE := int64(binary.BigEndian.Uint32(b[6:10]))
	sizeLE := int64(binary.LittleEndian.Uint32(b[10:14]))
	sizeBE := int64(binary.BigEndian.Uint32(b[14:18]))
	flags := b[25]
	nameLen := int(b[32])
	if 33+nameLen > len(b) {
		return dirRecord{}, false
	}
	// 双端序字段一致性：正常镜像两者相同；若小端明显异常而大端合理，用大端兜底。
	if lbaLE <= 0 && lbaBE > 0 {
		lbaLE = lbaBE
	}
	if sizeLE <= 0 && sizeBE > 0 {
		sizeLE = sizeBE
	}
	if lbaLE < 0 || sizeLE < 0 {
		return dirRecord{}, false
	}
	rec := dirRecord{
		lba:         lbaLE,
		size:        sizeLE,
		isDir:       flags&0x02 != 0,
		modTime:     parseRecordingDate(b[18:25]),
		multiExtent: flags&0x80 != 0,
	}
	// 扩展属性记录位于数据区之前，需要跳过。
	if extAttrLen > 0 {
		bs := r.BlockSize()
		rec.lba += (extAttrLen + bs - 1) / bs
		rec.size -= extAttrLen
		if rec.size < 0 {
			rec.size = 0
		}
	}
	nameRaw := b[33 : 33+nameLen]
	switch {
	case nameLen == 1 && nameRaw[0] == 0:
		rec.name = "."
	case nameLen == 1 && nameRaw[0] == 1:
		rec.name = ".."
	default:
		rec.name = decodeISOName(nameRaw, useJoliet)
	}
	// System Use Area（SUSP / Rock Ridge）紧跟在名字之后，按偶数字节对齐。
	suOff := 33 + nameLen
	if suOff%2 == 1 {
		suOff++
	}
	if r.rr && suOff <= len(b) {
		nm, link := r.parseSUSP(b[suOff:])
		if nm != "" {
			rec.name = nm
		}
		rec.link = link
	}
	if rec.name == "" {
		return dirRecord{}, false
	}
	rec.extents = []extent{{lba: rec.lba, length: rec.size}}
	return rec, true
}

// decodeISOName 把原始文件名字节解码成字符串。
// Joliet 用 UCS-2 大端；普通 ISO9660 用 ASCII，并去掉 ";版本号" 和末尾的点。
func decodeISOName(raw []byte, joliet bool) string {
	if joliet {
		return decodeUCS2(raw)
	}
	s := string(raw)
	// ISO9660 level 1/2 名字形如 "VMLINUZ.;1" / "README.TXT;1"。
	if i := strings.IndexByte(s, ';'); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimRight(s, ".")
	return s
}

// decodeUCS2 把 UCS-2 大端字节解码成 UTF-8 字符串。
func decodeUCS2(raw []byte) string {
	if len(raw) == 0 {
		return ""
	}
	units := make([]uint16, 0, (len(raw)+1)/2)
	for i := 0; i+1 < len(raw); i += 2 {
		units = append(units, binary.BigEndian.Uint16(raw[i:i+2]))
	}
	// 去掉尾部 NUL（部分写入器会补 0）。
	for len(units) > 0 && units[len(units)-1] == 0 {
		units = units[:len(units)-1]
	}
	s := string(utf16.Decode(units))
	// Joliet 名字同样可能带 ";1" 版本号。
	if i := strings.IndexByte(s, ';'); i >= 0 {
		s = s[:i]
	}
	return strings.TrimRight(s, ".")
}

// parseRecordingDate 解析 ISO9660 的 7 字节日期（含时区偏移，单位 15 分钟）。
func parseRecordingDate(b []byte) time.Time {
	if len(b) < 7 {
		return time.Time{}
	}
	year := int(b[0]) + 1900
	month := int(b[1])
	if month < 1 || month > 12 {
		month = 1
	}
	day := int(b[2])
	if day < 1 || day > 31 {
		day = 1
	}
	hour, minute, sec := int(b[3]), int(b[4]), int(b[5])
	if hour > 23 {
		hour = 0
	}
	if minute > 59 {
		minute = 0
	}
	if sec > 59 {
		sec = 0
	}
	loc := time.UTC
	if off := int8(b[6]); off != 0 {
		loc = time.FixedZone("", int(off)*15*60)
	}
	return time.Date(year, time.Month(month), day, hour, minute, sec, 0, loc)
}

// cleanPath 规范化 ISO 内路径：统一分隔符、去掉 "." / 空段、折叠 ".."。
// 返回 false 表示路径试图越界（".." 逃出根）。
func cleanPath(p string) (string, bool) {
	p = strings.TrimSpace(strings.ReplaceAll(p, "\\", "/"))
	if p == "" {
		return "/", true
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	parts := strings.Split(p, "/")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		switch part {
		case "", ".":
			continue
		case "..":
			if len(out) == 0 {
				return "", false
			}
			out = out[:len(out)-1]
		default:
			out = append(out, part)
		}
	}
	return "/" + strings.Join(out, "/"), true
}

// joinPath 拼接目录路径和子项名字。
func joinPath(parent, name string) string {
	if parent == "" || parent == "/" {
		return "/" + name
	}
	return parent + "/" + name
}
