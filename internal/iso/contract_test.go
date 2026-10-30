package iso

import (
	"io"
	"testing"

	"github.com/wupengcheng/netbootx/internal/model"
)

// 编译期断言：确保 *Reader 满足 httpd 侧定义的解耦接口。
//
// httpd 会有多台客户机并发 Range 读同一个 ISO，因此 SectionReader 返回的
// io.ReaderAt 必须并发安全（*File 只做 ReadAt，无共享可变状态）；
// Exists / Close 也必须并发安全（Reader 用 sync.RWMutex 保护关闭状态）。
type isoReaderContract interface {
	SectionReader(p string) (io.ReaderAt, int64, error)
	Exists(p string) bool
	Close() error
}

var _ isoReaderContract = (*Reader)(nil)

// TestContractSignatures 锁死对外 API 的签名，避免无意改动破坏 httpd/boot 包。
func TestContractSignatures(t *testing.T) {
	// Open
	var _ func(string) (*Reader, error) = Open
	// Probe / Scan
	var _ func(string) (*model.ISO, error) = Probe
	var _ func(string) ([]model.ISO, error) = Scan
	// Reader 方法
	var r *Reader
	var _ func(string) (*File, error) = r.Lookup
	var _ func(string) (int64, error) = r.FileSize
	var _ func(string, []byte, int64) (int, error) = r.ReadAt
	var _ func(string) ([]FileEntry, error) = r.List
	var _ func(string, int64) ([]byte, error) = r.ReadFile
	var _ func(string) bool = r.Exists
	var _ func() error = r.Close
	var _ func() *File = r.Root
	var _ func() *BootInfo = r.BootInfo
	var _ func() string = r.Path
	var _ func() int64 = r.Size
	var _ func() int64 = r.BlockSize
	var _ func() string = r.VolumeID
	var _ func() bool = r.Joliet
	var _ func() bool = r.RockRidge
	// File 实现 io.ReaderAt，httpd 可直接包 io.NewSectionReader
	var _ io.ReaderAt = (*File)(nil)
	// 辅助函数
	var _ func(model.Arch, model.Arch) bool = ArchCompatible
	var _ func(model.ISO) *model.AutoInstall = AutoInstallFor
	var _ func(model.ISO) string = Info
}
