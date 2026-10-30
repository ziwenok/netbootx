package iso

import (
	"encoding/binary"
	"fmt"
	"strings"
)

// El Torito 相关常量（El Torito Bootable CD-ROM Format Specification 1.0）。
const (
	// 引导指示符
	bootIndicatorNoEmul   = 0x00 // 不模拟
	bootIndicatorHDEmul   = 0x01 // 1.44MB 软盘模拟
	bootIndicatorNoEmul2  = 0x02 // 不模拟（部分工具用）
	bootIndicatorHDEmul2  = 0x03 // 硬盘模拟
	bootIndicatorBootable = 0x88 // 可引导

	// 平台 ID
	platformX86   = 0x00
	platformPPC   = 0x01
	platformMac   = 0x02
	platformEFI   = 0xEF
	platformARM64 = 0xAA // 部分发行版自定义
	platformARM32 = 0xAB
	platformRISCV = 0xBC // 部分发行版自定义

	// 引导目录 entry 的媒体类型
	mediaNoEmul     = 0
	media12Floppy   = 1
	media144Floppy  = 2
	media288Floppy  = 3
	mediaHardDisk   = 4
)

// BootInfo 是 El Torito 引导信息：这张盘能不能引导、怎么引导。
type BootInfo struct {
	// Bootable 表示引导目录里至少有一个「可引导」条目。
	Bootable bool
	// BIOS 表示存在 x86 Legacy BIOS 引导条目。
	BIOS bool
	// UEFI 表示存在 EFI 引导条目。
	UEFI bool
	// Platform 是主引导条目的平台名（x86 / efi / ...）。
	Platform string
	// BootCatalogLBA 是引导目录所在逻辑块号。
	BootCatalogLBA int64
	// ImageLBA / ImageSize 是默认引导映像的位置和长度（字节）。
	ImageLBA  int64
	ImageSize int64
	// MediaType 是引导映像的模拟类型（noemul / floppy / harddisk）。
	MediaType string
	// Entries 是引导目录里的全部条目。
	Entries []BootEntry
}

// BootEntry 是引导目录里的一个引导条目。
type BootEntry struct {
	Platform  string // x86 / efi / arm64 / ...
	PlatformID byte
	Bootable  bool
	MediaType string
	LoadSeg   uint16 // 加载段（通常 0x7C0）
	SectorCount uint16
	ImageLBA  int64
	ImageSize int64
}

// String 返回引导能力的简短中文描述，便于日志和界面显示。
func (b *BootInfo) String() string {
	if b == nil || !b.Bootable {
		return "不可引导"
	}
	var caps []string
	if b.BIOS {
		caps = append(caps, "Legacy BIOS")
	}
	if b.UEFI {
		caps = append(caps, "UEFI")
	}
	if len(caps) == 0 {
		caps = append(caps, "未知平台")
	}
	return "可引导（" + strings.Join(caps, " + ") + "）"
}

// readBootInfo 解析 El Torito 引导目录。
//
// 流程：
//  1. 在卷描述符里找 type=0 的 Boot Record（其 0x47 处是引导目录 LBA）
//  2. 引导目录第一个 32 字节是「验证条目」（validation entry），
//     第二个 32 字节是「默认条目」（initial/default entry）
//  3. 之后可能有多个 32 字节的「分区条目」，每个对应一种平台（BIOS/UEFI）
func (r *Reader) readBootInfo() (*BootInfo, error) {
	br := r.bootRecord
	if br == nil || br.rootLBA <= 0 {
		return nil, nil
	}
	lba := br.rootLBA
	bs := r.BlockSize()
	if lba <= 0 {
		return nil, fmt.Errorf("引导记录里的引导目录 LBA 非法: %w", ErrInvalidImage)
	}
	buf := make([]byte, bs)
	if _, err := r.f.ReadAt(buf, lba*bs); err != nil {
		return nil, fmt.Errorf("读取 El Torito 引导目录（LBA %d）: %w", lba, err)
	}
	bi := &BootInfo{BootCatalogLBA: lba}
	// 校验条目：byte0 应为 0x01，byte30-31 应为 0x55 0xAA。
	if buf[0] != 0x01 || buf[30] != 0x55 || buf[31] != 0xAA {
		return nil, fmt.Errorf("El Torito 验证条目标识非法（LBA %d）: %w", lba, ErrInvalidImage)
	}
	// 默认引导条目：平台由校验条目的 byte1 决定。
	validationPlatform := buf[1]
	def := parseBootEntry(buf[32:64], validationPlatform)
	if def != nil {
		bi.Entries = append(bi.Entries, *def)
		if def.Bootable {
			bi.Bootable = true
			bi.ImageLBA = def.ImageLBA
			bi.ImageSize = def.ImageSize
			bi.MediaType = def.MediaType
			bi.Platform = def.Platform
		}
	}
	// 后续条目：可能是「分区条目」（直接跟随），也可能是「分区头」
	// （0x90 = 非最后一段，0x91 = 最后一段），分区头后面跟它声明数量的条目，
	// 这些条目的平台取自分区头而不是条目自己的 byte4。
	curPlatform := validationPlatform
	remaining := 0 // 当前分区段里还没读的条目数
	maxEntries := len(buf)/32 - 2
	for i := 0; i < maxEntries; i++ {
		off := (2 + i) * 32
		raw := buf[off : off+32]
		if isZero(raw) {
			break
		}
		// 分区头
		if raw[0] == sectionHeaderMore || raw[0] == sectionHeaderLast {
			curPlatform = raw[1]
			remaining = int(binary.LittleEndian.Uint16(raw[2:4]))
			continue
		}
		e := parseBootEntry(raw, curPlatform)
		if e == nil {
			continue
		}
		bi.Entries = append(bi.Entries, *e)
		if remaining > 0 {
			remaining--
		}
	}
	// 汇总平台能力。
	for _, e := range bi.Entries {
		if !e.Bootable {
			continue
		}
		bi.Bootable = true
		switch e.PlatformID {
		case platformX86:
			bi.BIOS = true
		case platformEFI:
			bi.UEFI = true
		}
	}
	return bi, nil
}

// 分区头（section header entry）的引导指示符取值。
const (
	sectionHeaderMore = 0x90 // 还有后续分区段
	sectionHeaderLast = 0x91 // 最后一个分区段
)

// parseBootEntry 解析一个 32 字节引导条目，platform 是该条目所属分区的平台 ID。
//
// 条目布局（El Torito 2.0）：
//
//	0    引导指示符（0x88 = 可引导）
//	1    媒体类型（0 = 不模拟，1 = 1.2MB 软盘，2 = 1.44MB 软盘，3 = 2.88MB 软盘，4 = 硬盘）
//	2-3  加载段（通常 0）
//	4    系统类型
//	5    未使用
//	6-7  扇区数（虚拟 512 字节扇区）
//	8-11 引导映像 LBA（4 字节小端）
//	12-31 未使用
func parseBootEntry(b []byte, platform byte) *BootEntry {
	if len(b) < 32 {
		return nil
	}
	indicator := b[0]
	media := b[1]
	// 引导指示符只接受 0x00 / 0x88；其它值说明不是合法条目。
	if indicator != bootIndicatorNoEmul && indicator != bootIndicatorBootable &&
		indicator != bootIndicatorHDEmul && indicator != bootIndicatorNoEmul2 &&
		indicator != bootIndicatorHDEmul2 {
		return nil
	}
	// 全 0 条目不合法（平台也是 0，但 LBA 为 0）。
	imgLBA := int64(binary.LittleEndian.Uint32(b[8:12]))
	if imgLBA <= 0 && indicator == 0 && media == 0 {
		return nil
	}
	e := &BootEntry{
		PlatformID:  platform,
		Bootable:    indicator == bootIndicatorBootable,
		MediaType:   mediaName(media),
		LoadSeg:     binary.LittleEndian.Uint16(b[2:4]),
		SectorCount: binary.LittleEndian.Uint16(b[6:8]),
		ImageLBA:    imgLBA,
		ImageSize:   int64(binary.LittleEndian.Uint16(b[6:8])) * 512,
	}
	e.Platform = platformName(e.PlatformID)
	return e
}

// platformName 把平台 ID 翻译成可读名字。
func platformName(id byte) string {
	switch id {
	case platformX86:
		return "x86"
	case platformPPC:
		return "ppc"
	case platformMac:
		return "mac"
	case platformEFI:
		return "efi"
	case platformARM64:
		return "arm64"
	case platformARM32:
		return "arm32"
	case platformRISCV:
		return "riscv"
	}
	return fmt.Sprintf("unknown(0x%02X)", id)
}

// mediaName 把媒体类型翻译成可读名字。
func mediaName(m byte) string {
	switch m {
	case mediaNoEmul:
		return "noemul"
	case media12Floppy:
		return "floppy120"
	case media144Floppy:
		return "floppy144"
	case media288Floppy:
		return "floppy288"
	case mediaHardDisk:
		return "harddisk"
	}
	return fmt.Sprintf("unknown(%d)", m)
}

// isZero 判断字节切片是否全 0。
func isZero(b []byte) bool {
	for _, v := range b {
		if v != 0 {
			return false
		}
	}
	return true
}
