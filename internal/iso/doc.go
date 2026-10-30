// Package iso 实现纯 Go 的 ISO9660 光盘镜像读取与操作系统识别。
//
// NetBootX 的核心约束是：ISO 文件从不挂载、从不解压，所有引导文件都通过 HTTP
// 按字节偏移流式读取。因此本包只做「按扇区 seek」的随机访问解析，不依赖任何
// 外部库（纯标准库），也不会把整个 ISO 读进内存。
//
// # 支持的格式
//
//   - ISO9660 Primary Volume Descriptor（PVD）
//   - Joliet Supplementary Volume Descriptor（SVD，UCS-2 大端文件名）
//   - Rock Ridge（SUSP：SP / CE / NM / PX / SL），优先于 Joliet 提供 POSIX 文件名
//   - El Torito 引导目录（Boot Record Volume Descriptor + Boot Catalog），
//     用于判断是否可引导、是 Legacy BIOS 还是 UEFI
//   - 多段文件（multi-extent）、符号链接（Rock Ridge SL）、大小写不敏感回退查找
//
// # 对外 API
//
// 打开与探测：
//
//	Open(path string) (*Reader, error)           // 打开 ISO，解析卷描述符 / 目录树 / El Torito
//	Probe(path string) (*model.ISO, error)       // 探测单个 ISO，识别操作系统并生成引导描述
//	Scan(dir string) ([]model.ISO, error)        // 递归扫描目录下所有 *.iso，并发探测
//
// 读取（Reader 的所有方法都可并发调用）：
//
//	(*Reader).Lookup(p string) (*File, error)                    // p 为 ISO 内绝对路径，如 "/casper/vmlinuz"
//	(*Reader).FileSize(p string) (int64, error)                  // 文件字节数；目录返回 ErrIsDir
//	(*Reader).ReadAt(p string, buf []byte, off int64) (int, error) // 从文件 off 处读入 buf，语义同 io.ReaderAt
//	(*Reader).SectionReader(p string) (io.ReaderAt, int64, error)  // 给 httpd 用：把 ISO 内文件包成 io.ReaderAt
//	(*Reader).ReadFile(p string, limit int64) ([]byte, error)     // 读小文件（os-release 之类），limit<=0 不限
//	(*Reader).List(dir string) ([]FileEntry, error)              // 列目录（目录在前，按名字排序）
//	(*Reader).Exists(p string) bool
//	(*Reader).Root() *File
//	(*Reader).Close() error
//
// 元信息：
//
//	(*Reader).Path() string        // ISO 文件路径
//	(*Reader).Size() int64         // ISO 文件字节数
//	(*Reader).BlockSize() int64    // 逻辑块大小（通常 2048）
//	(*Reader).VolumeID() string    // PVD 卷标识
//	(*Reader).Joliet() bool        // 是否使用 Joliet 文件名
//	(*Reader).RockRidge() bool     // 是否使用 Rock Ridge 文件名
//	(*Reader).BootInfo() *BootInfo // El Torito 引导信息
//
// 辅助：
//
//	ArchCompatible(img, client model.Arch) bool // 镜像架构能否被该客户机架构引导
//
// # 路径约定
//
// 所有 p 参数都是 ISO 内绝对路径，以 "/" 开头，如 "/casper/vmlinuz"；
// 不以 "/" 开头也接受（会自动补上）。查找先精确匹配，失败再大小写不敏感回退，
// 这样 iPXE / 浏览器里写错大小写也能读到文件。
//
// # 错误约定
//
// 路径不存在返回包装了 ErrNotExist 的错误（可用 errors.Is 判断）；
// 对目录调用文件读取类方法返回 ErrIsDir；Reader 关闭后返回 ErrClosed。
// 任何解析异常都不会 panic。
//
// # 与 ARCHITECTURE.md 的关系
//
// 契约要求的最小集合是 Probe 和 Scan；Open / Reader 系列是 httpd 包流式读取
// ISO 内部文件所需的扩展接口（httpd 通过 SectionReader + Exists + Close 消费）。
package iso
