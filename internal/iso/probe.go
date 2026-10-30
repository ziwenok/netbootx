package iso

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"

	"github.com/wupengcheng/netbootx/internal/model"
)

// Scan 的并发度上限：1 万个文件也不能把机器打爆（每个探测只读几 MB）。
const maxScanWorkers = 8

// Probe 读一个 ISO 文件，识别它的类型并生成引导描述。
//
// 返回的 *model.ISO 一定会带上 ID / Path / Name / Size / ModTime / RelPath；
// 解析失败时不会返回 error 给调用方中断整个扫描，而是返回带 Error 字段的对象。
// 只有「文件都打不开」这种完全无法继续的情况才返回 error。
func Probe(path string) (*model.ISO, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("解析 ISO 路径 %q: %w", path, err)
	}
	st, err := os.Stat(abs)
	if err != nil {
		return nil, fmt.Errorf("读取 ISO 状态 %q: %w", abs, err)
	}
	img := &model.ISO{
		ID:      makeID(abs),
		Path:    abs,
		Name:    filepath.Base(abs),
		Size:    st.Size(),
		ModTime: st.ModTime(),
		Family:  model.FamilyOther,
		OSArch:  model.ArchUEFI64,
	}
	rd, err := Open(abs)
	if err != nil {
		// 不是有效 ISO：返回带 Error 的对象，让界面能显示「解析失败」。
		img.Error = err.Error()
		img.OSName = strings.TrimSuffix(img.Name, filepath.Ext(img.Name))
		return img, nil
	}
	defer func() { _ = rd.Close() }()

	// 探测过程绝不能因为镜像内容怪异而崩溃：兜底 recover。
	func() {
		defer func() {
			if rec := recover(); rec != nil {
				img.Error = fmt.Sprintf("探测 %s 时发生内部错误: %v", img.Name, rec)
			}
		}()
		rd.detect(img)
	}()
	if img.OSName == "" {
		img.OSName = strings.TrimSuffix(img.Name, filepath.Ext(img.Name))
	}
	if img.Family == "" {
		img.Family = model.FamilyOther
	}
	if img.OSArch == "" {
		img.OSArch = model.ArchUEFI64
	}
	return img, nil
}

// Scan 扫描目录下所有 *.iso（递归），并发探测。
//
// 行为：
//   - 目录不存在或不是目录：返回错误
//   - 单个 ISO 探测失败：记进结果的 Error 字段，不影响其它文件
//   - 结果按相对路径排序，保证界面顺序稳定
//   - 并发度固定上限（maxScanWorkers），不会因为文件多而耗尽句柄
func Scan(dir string) ([]model.ISO, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("解析 ISO 目录 %q: %w", dir, err)
	}
	st, err := os.Stat(abs)
	if err != nil {
		return nil, fmt.Errorf("访问 ISO 目录 %q: %w", abs, err)
	}
	if !st.IsDir() {
		return nil, fmt.Errorf("ISO 路径 %q 不是目录", abs)
	}
	paths, err := collectISOs(abs)
	if err != nil {
		return nil, err
	}
	if len(paths) == 0 {
		return []model.ISO{}, nil
	}
	return scanFiles(abs, paths), nil
}

// collectISOs 递归收集目录下的 *.iso 文件（不跟随符号链接目录，避免环路）。
func collectISOs(root string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			// 单个子目录不可读不致命，跳过即可。
			if d != nil && d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			// 跳过隐藏目录（.git、.tools、缓存目录），避免误扫。
			name := d.Name()
			if p != root && strings.HasPrefix(name, ".") {
				return filepath.SkipDir
			}
			return nil
		}
		// 跟随指向普通文件的符号链接（用户常用软链接把 ISO 挂进 iso/ 目录）；
		// 不跟随目录软链接，避免环路。
		if !d.Type().IsRegular() {
			if d.Type()&os.ModeSymlink == 0 {
				return nil
			}
			st, serr := os.Stat(p)
			if serr != nil || !st.Mode().IsRegular() {
				return nil
			}
		}
		if !strings.EqualFold(filepath.Ext(p), ".iso") {
			return nil
		}
		out = append(out, p)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("遍历 ISO 目录 %q: %w", root, err)
	}
	return out, nil
}

// scanFiles 并发探测一批 ISO 文件，保持输入顺序。
func scanFiles(root string, paths []string) []model.ISO {
	workers := runtime.NumCPU()
	if workers > maxScanWorkers {
		workers = maxScanWorkers
	}
	if workers < 1 {
		workers = 1
	}
	if workers > len(paths) {
		workers = len(paths)
	}
	results := make([]model.ISO, len(paths))
	var wg sync.WaitGroup
	jobs := make(chan int)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				img := probeOne(paths[i])
				img.RelPath = relPath(root, paths[i])
				results[i] = img
			}
		}()
	}
	for i := range paths {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	sort.SliceStable(results, func(i, j int) bool {
		return results[i].RelPath < results[j].RelPath
	})
	return results
}

// probeOne 是 scanFiles 里单个文件的探测，保证不 panic、不返回 error。
func probeOne(path string) (img model.ISO) {
	defer func() {
		if rec := recover(); rec != nil {
			img = model.ISO{
				Path:   path,
				Name:   filepath.Base(path),
				Family: model.FamilyOther,
				OSArch: model.ArchUEFI64,
				Error:  fmt.Sprintf("探测时发生内部错误: %v", rec),
			}
		}
	}()
	res, err := Probe(path)
	if err != nil {
		return model.ISO{
			Path:   path,
			Name:   filepath.Base(path),
			Family: model.FamilyOther,
			OSArch: model.ArchUEFI64,
			Error:  err.Error(),
		}
	}
	return *res
}

// relPath 计算文件相对扫描根目录的路径（统一用 / 分隔，界面显示用）。
func relPath(root, p string) string {
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return filepath.Base(p)
	}
	return filepath.ToSlash(rel)
}

// makeID 由 ISO 的绝对路径生成稳定的短 ID（重启后不变，界面链接不会失效）。
func makeID(abs string) string {
	sum := sha1.Sum([]byte(filepath.ToSlash(abs)))
	return hex.EncodeToString(sum[:8])
}

// Info 返回该镜像的一行摘要，方便日志和调试输出。
func Info(img model.ISO) string {
	if img.Error != "" {
		return fmt.Sprintf("%s（%s，解析失败: %s）", img.Name, humanSize(img.Size), img.Error)
	}
	return fmt.Sprintf("%s（%s，%s，%s，%s）", img.Name, humanSize(img.Size),
		familyString(img.Family), img.OSName, img.Boot.Method)
}

// humanSize 把字节数格式化成人类可读的大小。
func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}
