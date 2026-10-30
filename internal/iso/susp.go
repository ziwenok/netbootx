package iso

import (
	"bytes"
	"encoding/binary"
	"io"
	"strings"
)

// Rock Ridge / SUSP 相关常量。
const (
	suspSP = "SP" // SUSP 存在标志（System Use Sharing Protocol）
	suspCE = "CE" // 续接块（Continuation Area）
	suspNM = "NM" // 文件名（POSIX 名字）
	suspSL = "SL" // 符号链接
	suspST = "ST" // SUSP 结束标记

	suspCheckBE = 0xBE
	suspCheckEF = 0xEF

	maxSUSPDepth = 8 // CE 续接最多递归层数
	maxSUSPLen   = 1 << 20
)

// detectRockRidge 检查根目录 "." 记录里是否有 SUSP "SP" 签名。
// 有则说明整张盘用 Rock Ridge 扩展，文件名应该用 NM 里的 POSIX 名字。
func (r *Reader) detectRockRidge() bool {
	if r.pvd == nil {
		return false
	}
	bs := r.BlockSize()
	buf := make([]byte, bs)
	if _, err := r.f.ReadAt(buf, r.pvd.rootLBA*bs); err != nil {
		return false
	}
	// 根目录记录：len(1) extAttrLen(1) ... nameLen(1) name(1) → SUSP 从偏移 34 开始。
	if len(buf) < 34+7 || int(buf[0]) < 34 {
		return false
	}
	if string(buf[34:36]) != suspSP {
		return false
	}
	// SP 数据区：len(7) version(1) checkByte1(0xBE) checkByte2(0xEF)
	return buf[36] == 7 && buf[38] == suspCheckBE && buf[39] == suspCheckEF
}

// parseSUSP 解析一段 System Use Area，返回 POSIX 文件名和符号链接目标。
// 自动跟随 CE 续接块。解析失败时返回空串，不影响目录项本身。
func (r *Reader) parseSUSP(su []byte) (name, link string) {
	var nameBuf strings.Builder
	var linkBuf strings.Builder
	r.parseSUSPInto(su, &nameBuf, &linkBuf, 0)
	name = strings.TrimSpace(nameBuf.String())
	link = strings.TrimSpace(linkBuf.String())
	return name, link
}

// parseSUSPInto 是 parseSUSP 的递归实现，depth 用于限制 CE 续接层数。
func (r *Reader) parseSUSPInto(su []byte, name, link *strings.Builder, depth int) {
	if depth > maxSUSPDepth || len(su) < 4 {
		return
	}
	for off := 0; off+4 <= len(su); {
		sig := string(su[off : off+2])
		length := int(su[off+2])
		// 长度非法（0 或越界）说明 SUSP 区域已结束。
		if length < 4 || off+length > len(su) {
			return
		}
		data := su[off+4 : off+length]
		switch sig {
		case suspST:
			return
		case suspNM:
			r.parseNM(data, name)
		case suspSL:
			r.parseSL(data, link)
		case suspCE:
			r.followCE(data, name, link, depth)
		}
		off += length
	}
}

// parseNM 解析 NM（Alternate Name）条目。
//
// 数据区：flags(1) name(...)
//
//	flags 0x01 = 名字在下一个 NM 条目继续
//	flags 0x02 = "."，0x04 = ".."，0x08 = 根目录
func (r *Reader) parseNM(data []byte, name *strings.Builder) {
	if len(data) < 1 {
		return
	}
	flags := data[0]
	// 名字按偶数字节对齐，尾部可能有 NUL 填充，必须去掉。
	body := bytes.TrimRight(data[1:], "\x00")
	switch {
	case flags&0x02 != 0:
		name.WriteString(".")
		return
	case flags&0x04 != 0:
		name.WriteString("..")
		return
	case flags&0x08 != 0:
		name.WriteString("/")
		return
	}
	name.WriteString(string(body))
}

// parseSL 解析 SL（Symbolic Link）条目。
//
// 数据区：flags(1) 然后若干组件，每个组件：flags(1) len(1) data
//
//	组件 flags 0x01 = 续接，0x02 = "."，0x04 = ".."，0x08 = "/"，
//	0x10 = 卷根，0x20 = 主机名（跳过）
func (r *Reader) parseSL(data []byte, link *strings.Builder) {
	if len(data) < 1 {
		return
	}
	for off := 1; off+2 <= len(data); {
		cflags := data[off]
		clen := int(data[off+1])
		if off+2+clen > len(data) {
			return
		}
		body := data[off+2 : off+2+clen]
		switch {
		case cflags&0x02 != 0:
			link.WriteString(".")
		case cflags&0x04 != 0:
			link.WriteString("..")
		case cflags&0x08 != 0 || cflags&0x10 != 0:
			link.WriteString("/")
		case cflags&0x20 != 0:
			// 主机名组件，对本地路径无意义。
		default:
			if link.Len() > 0 && !strings.HasSuffix(link.String(), "/") {
				link.WriteString("/")
			}
			link.WriteString(string(bytes.TrimRight(body, "\x00")))
		}
		off += 2 + clen
	}
}

// followCE 读取续接块（Continuation Area）并递归解析。
//
// CE 数据区：block(4 LE) offset(4 LE) length(4 LE)
func (r *Reader) followCE(data []byte, name, link *strings.Builder, depth int) {
	if len(data) < 12 {
		return
	}
	block := int64(binary.LittleEndian.Uint32(data[0:4]))
	offset := int64(binary.LittleEndian.Uint32(data[4:8]))
	length := int64(binary.LittleEndian.Uint32(data[8:12]))
	if block <= 0 || length <= 0 || length > maxSUSPLen {
		return
	}
	buf := make([]byte, length)
	n, err := r.f.ReadAt(buf, block*r.BlockSize()+offset)
	if err != nil && err != io.EOF {
		return
	}
	if int64(n) < length {
		buf = buf[:n]
	}
	r.parseSUSPInto(buf, name, link, depth+1)
}
