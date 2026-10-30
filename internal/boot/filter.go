package boot

import "github.com/wupengcheng/netbootx/internal/model"

// archGroup 是「架构大类」，用于把客户机与 ISO 配对。
// iPXE 二进制本身也按这个粒度分发：x86 一套、arm64 一套、loong64 一套。
type archGroup int

const (
	groupX86     archGroup = iota // Legacy BIOS / IA32 UEFI / x86_64 UEFI 都归入 x86
	groupARM64                    // ARM64 UEFI
	groupLoong64                  // LoongArch64 UEFI
)

// clientGroup 推导客户机属于哪个架构大类。
func clientGroup(c *model.Client) (archGroup, bool) {
	if c == nil {
		return groupX86, false
	}
	switch c.Arch {
	case model.ArchARM64, model.ArchARM64HTTP:
		return groupARM64, true
	case model.ArchLoongArch64:
		return groupLoong64, true
	case model.ArchBIOS, model.ArchUEFI32, model.ArchUEFI64, model.ArchUEFI64HTTP:
		return groupX86, true
	}
	return groupX86, false
}

// isoGroup 推导 ISO 支持的架构大类。
// OSArch 为空或未知时按 x86 处理：绝大多数可引导 ISO 是 x86 的，
// 而 arm64 镜像一定会被 iso 包识别出 OSArch=arm64（存在 boot/Image 或 arch/boot/aarch64）。
func isoGroup(is model.ISO) archGroup {
	switch is.OSArch {
	case model.ArchARM64, model.ArchARM64HTTP:
		return groupARM64
	case model.ArchLoongArch64:
		return groupLoong64
	}
	return groupX86
}

// Compatible 报告某个 ISO 能否被这台客户机引导。
//
// 客户机架构未知（c 为 nil 或 Arch 为 unknown）时一律返回 true，
// 这样管理员在 Web 界面预览时能看到全部镜像。
func Compatible(c *model.Client, is model.ISO) bool {
	g, known := clientGroup(c)
	if !known {
		return true
	}
	return isoGroup(is) == g
}

// Filter 返回适用于这台客户机的 ISO 子集，保持输入顺序。
func Filter(c *model.Client, isos []model.ISO) []model.ISO {
	if _, known := clientGroup(c); !known {
		out := make([]model.ISO, len(isos))
		copy(out, isos)
		return out
	}
	out := make([]model.ISO, 0, len(isos))
	for _, is := range isos {
		if Compatible(c, is) {
			out = append(out, is)
		}
	}
	return out
}
