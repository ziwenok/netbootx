package pxe

import (
	"github.com/wupengcheng/netbootx/internal/boot"
	"github.com/wupengcheng/netbootx/internal/model"
)

// menuScript 生成客户机的 iPXE 主菜单。
// 真正的脚本拼装逻辑在 internal/boot 包里（纯函数，好测），这里只做数据准备。
func (s *Server) menuScript(c model.Client, isos []model.ISO, baseURL string) string {
	cfg := s.store.Settings()
	return boot.Script(&c, isos, cfg, baseURL)
}

// entryScript 生成「直接启动某个 ISO」的脚本（跳过菜单）。
func (s *Server) entryScript(c model.Client, is model.ISO, baseURL string) string {
	cfg := s.store.Settings()
	return boot.Entry(&c, is, cfg, baseURL)
}
