package boot

import (
	"strconv"
	"strings"

	"github.com/wupengcheng/netbootx/internal/model"
)

// normalizeBase 计算脚本里使用的服务端基地址（不带结尾斜杠）。
//
// 优先使用调用方传入的 baseURL；为空时用 Settings.ServerIP + Settings.HTTPPort 推导。
func normalizeBase(baseURL string, s model.Settings) string {
	if base := strings.TrimRight(strings.TrimSpace(baseURL), "/"); base != "" {
		return base
	}
	host := strings.TrimSpace(s.ServerIP)
	if host == "" {
		host = "127.0.0.1"
	}
	// IPv6 地址需要方括号，否则 "http://::1:16000" 无法解析。
	if strings.Contains(host, ":") && !strings.HasPrefix(host, "[") {
		host = "[" + host + "]"
	}
	port := s.HTTPPort
	if port <= 0 {
		port = DefaultHTTPPort
	}
	return "http://" + host + ":" + strconv.Itoa(port)
}

// escapePath 对 URL 路径逐段做百分号编码，保留 "/" 分隔符。
//
// 不做整段编码是必须的：kernel 路径里常见的 "+"、"~"、空格都要正确处理，
// 而 "/" 本身必须保留，否则 httpd 侧的路由就匹配不到了。
func escapePath(p string) string {
	p = strings.Trim(p, "/")
	if p == "" {
		return ""
	}
	segs := strings.Split(p, "/")
	for i, seg := range segs {
		segs[i] = escapeSegment(seg)
	}
	return strings.Join(segs, "/")
}

// escapeSegment 对一个路径段做百分号编码。
// 只保留 RFC 3986 里 path segment 的 unreserved + 少量安全字符。
func escapeSegment(s string) string {
	const hex = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
			b.WriteByte(c)
		case c == '-' || c == '_' || c == '.' || c == '~' || c == '+' || c == '@':
			b.WriteByte(c)
		default:
			b.WriteByte('%')
			b.WriteByte(hex[c>>4])
			b.WriteByte(hex[c&0x0f])
		}
	}
	return b.String()
}

// isoFileURL 拼出 ISO 内部某个文件的下载地址。
// 例如 base="http://10.0.0.1:16000", id="abc123", p="/casper/vmlinuz"
// → "http://10.0.0.1:16000/iso/abc123/casper/vmlinuz"
func isoFileURL(base, id, p string) string {
	u := base + "/iso/" + escapeSegment(id)
	if rel := escapePath(p); rel != "" {
		u += "/" + rel
	}
	return u
}

// userFileURL 拼出 user 目录下某个文件的下载地址（自动安装模板用）。
func userFileURL(base, p string) string {
	u := base + "/user"
	if rel := escapePath(p); rel != "" {
		u += "/" + rel
	}
	return u
}

// templateURL 返回自动安装模板的完整下载地址。
func templateURL(base string, ai *model.AutoInstall) string {
	if ai == nil || strings.TrimSpace(ai.Template) == "" {
		return ""
	}
	return userFileURL(base, ai.Template)
}
