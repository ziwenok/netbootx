package iso

import (
	"encoding/binary"
	"fmt"
	"io"
	"strings"
)

// 卷描述符类型（ECMA-119 8.1）。
const (
	vdBootRecord = 0   // 引导记录（El Torito 用它）
	vdPrimary    = 1   // 主卷描述符 PVD
	vdSupplementary = 2 // 补充卷描述符（Joliet 用这个）
	vdTerminator = 255 // 卷描述符集合结束
)

// Joliet escape sequence 常量：UCS-2 level 1/2/3。
var jolietEscapes = [][]byte{
	{'%', '/', '@'}, // UCS-2 Level 1
	{'%', '/', 'C'}, // UCS-2 Level 2
	{'%', '/', 'E'}, // UCS-2 Level 3
}

// readVolumeDescriptors 从 LBA 16 开始扫描卷描述符集合，
// 找到 PVD、Joliet SVD 和 Boot Record，然后确定用哪套文件名。
func (r *Reader) readVolumeDescriptors() error {
	buf := make([]byte, SectorSize)
	var pvd, joliet *volumeDescriptor
	var boot *volumeDescriptor

	for i := 0; i < maxVolumeDescriptors; i++ {
		lba := int64(vdStartLBA + i)
		if _, err := r.f.ReadAt(buf, lba*SectorSize); err != nil {
			if i == 0 {
				return fmt.Errorf("读取卷描述符（LBA %d）: %w", lba, err)
			}
			break
		}
		typ := buf[0]
		if string(buf[1:6]) != "CD001" {
			if i == 0 {
				return fmt.Errorf("LBA %d 处缺少 \"CD001\" 签名（文件头=%q）: %w", lba, string(buf[1:6]), ErrInvalidImage)
			}
			break
		}
		// 版本号 b[6] 应为 1，损坏镜像忽略。
		vd := &volumeDescriptor{typ: typ}
		switch typ {
		case vdPrimary:
			if pvd == nil {
				parsePrimaryVD(vd, buf)
				pvd = vd
			}
		case vdSupplementary:
			if isJolietEscape(buf[88:91]) {
				parseSupplementaryVD(vd, buf)
				if joliet == nil {
					joliet = vd
				}
			}
		case vdBootRecord:
			if boot == nil {
				parseBootRecordVD(vd, buf)
				boot = vd
			}
		case vdTerminator:
			i = maxVolumeDescriptors
		}
		if typ == vdTerminator {
			break
		}
	}
	if pvd == nil {
		return fmt.Errorf("未找到 Primary Volume Descriptor（LBA 16 起）: %w", ErrInvalidImage)
	}
	r.pvd = pvd
	r.bs = pvd.bs
	if r.bs <= 0 || r.bs > 65536 {
		r.bs = SectorSize
	}
	r.joliet = joliet
	r.bootRecord = boot
	// Rock Ridge 只在 PVD 根目录上探测；有 RR 就不再用 Joliet 名字（RR 更准确）。
	r.rr = r.detectRockRidge()
	return nil
}

// parsePrimaryVD 解析 PVD 的关键字段。
//
// 布局（字节偏移，ECMA-119 8.4）：
//
//	0     类型
//	1-5   "CD001"
//	6     版本
//	80-87 卷空间大小（LE+BE）
//	120-123 逻辑块大小（LE）
//	156-189 根目录记录（34 字节）
//	190-317 卷标识符（128 字节，实际用前 32 字节）
func parsePrimaryVD(vd *volumeDescriptor, b []byte) {
	if len(b) < 190 {
		return
	}
	vd.bs = int64(binary.LittleEndian.Uint32(b[128:132]))
	if vd.bs <= 0 || vd.bs > 65536 {
		vd.bs = SectorSize
	}
	vd.volumeSpace = int64(binary.LittleEndian.Uint32(b[80:84]))
	vd.volumeID = cleanVolumeID(b[40:72])
	parseRootDirRecord(vd, b[156:190])
}

// parseSupplementaryVD 解析 Joliet SVD：除卷标识符编码不同外，布局与 PVD 相同。
func parseSupplementaryVD(vd *volumeDescriptor, b []byte) {
	if len(b) < 190 {
		return
	}
	vd.joliet = true
	vd.escape = string(b[88:91])
	vd.bs = int64(binary.LittleEndian.Uint32(b[128:132]))
	if vd.bs <= 0 || vd.bs > 65536 {
		vd.bs = SectorSize
	}
	vd.volumeSpace = int64(binary.LittleEndian.Uint32(b[80:84]))
	vd.volumeID = decodeUCS2(b[40:72])
	parseRootDirRecord(vd, b[156:190])
}

// parseBootRecordVD 解析引导记录卷描述符：El Torito 引导目录 LBA 在偏移 0x47（71）。
func parseBootRecordVD(vd *volumeDescriptor, b []byte) {
	if len(b) < 0x50 {
		return
	}
	vd.volumeID = strings.TrimSpace(string(b[7:39]))
	vd.rootLBA = int64(binary.LittleEndian.Uint32(b[0x47:0x4B]))
}

// parseRootDirRecord 从卷描述符内嵌的 34 字节根目录记录里取出 extent 与长度。
func parseRootDirRecord(vd *volumeDescriptor, b []byte) {
	if len(b) < 34 || b[0] < 34 {
		return
	}
	vd.rootLBA = int64(binary.LittleEndian.Uint32(b[2:6]))
	vd.rootSize = int64(binary.LittleEndian.Uint32(b[10:14]))
	if vd.rootLBA <= 0 {
		vd.rootLBA = int64(binary.BigEndian.Uint32(b[6:10]))
	}
	if vd.rootSize <= 0 {
		vd.rootSize = int64(binary.BigEndian.Uint32(b[14:18]))
	}
}

// isJolietEscape 判断 SVD 的 escape sequence 是否是 Joliet（%/@、%/C、%/E）。
func isJolietEscape(esc []byte) bool {
	for _, want := range jolietEscapes {
		if len(esc) >= len(want) && string(esc[:len(want)]) == string(want) {
			return true
		}
	}
	return false
}

// cleanVolumeID 清理 PVD 里的卷标识（去掉填充空格和末尾 NUL）。
func cleanVolumeID(b []byte) string {
	s := string(b)
	if i := strings.IndexByte(s, 0); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

// readExtents 从多段文件中读取数据，自动跨段拼接。
func (r *Reader) readExtents(f *File, p []byte, off int64) (int, error) {
	if len(f.extents) == 0 {
		return 0, io.EOF
	}
	bs := r.BlockSize()
	var done int
	pos := off // 相对文件开头的偏移
	for _, ex := range f.extents {
		if done >= len(p) {
			break
		}
		if pos >= ex.length {
			pos -= ex.length
			continue
		}
		// 本段内可读字节数
		n := int64(len(p) - done)
		if rem := ex.length - pos; n > rem {
			n = rem
		}
		if n <= 0 {
			continue
		}
		got, err := r.f.ReadAt(p[done:done+int(n)], ex.lba*bs+pos)
		done += got
		if err != nil && !isEOF(err) {
			return done, fmt.Errorf("读取 %q（LBA %d 偏移 %d）: %w", f.Path, ex.lba, pos, err)
		}
		if int64(got) < n {
			return done, io.EOF
		}
		pos = 0
	}
	return done, nil
}

// isEOF 判断错误是否只是「读到了文件末尾」。
func isEOF(err error) bool {
	return err == io.EOF || err == io.ErrUnexpectedEOF
}
