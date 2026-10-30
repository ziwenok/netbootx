package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/wupengcheng/netbootx/internal/model"
)

const (
	// defaultEventLimit 是 /api/events 不传 limit 时返回的条数。
	defaultEventLimit = 200
	// maxEventLimit 是 /api/events 允许请求的最大条数。
	maxEventLimit = 2000
)

// apiResponse 是所有 REST 接口统一的 JSON 包装。
type apiResponse struct {
	OK    bool   `json:"ok"`
	Data  any    `json:"data,omitempty"`
	Error string `json:"error,omitempty"`
}

// writeJSON 以 JSON 形式输出任意值（healthz 等简单接口用）。
func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("web: 写 JSON 响应失败: %v", err)
	}
}

// writeOK 输出成功包装：{"ok":true,"data":...}。
func writeOK(w http.ResponseWriter, data any) {
	writeJSON(w, http.StatusOK, apiResponse{OK: true, Data: data})
}

// writeErr 输出失败包装：{"ok":false,"error":"..."}。
func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, apiResponse{OK: false, Error: msg})
}

// methodNotAllowed 统一处理不支持的方法，并回 Allow 头。
func methodNotAllowed(w http.ResponseWriter, r *http.Request, allow string) {
	w.Header().Set("Allow", allow)
	writeErr(w, http.StatusMethodNotAllowed, fmt.Sprintf("接口 %s 不支持 %s 方法，只允许 %s", r.URL.Path, r.Method, allow))
}

// requireMethod 检查请求方法，不匹配时返回 false 由调用方写 405。
func requireMethod(w http.ResponseWriter, r *http.Request, methods ...string) bool {
	for _, m := range methods {
		if r.Method == m {
			return true
		}
	}
	return false
}

// handleAPI 是 /api/ 前缀下的总路由。
func (s *Server) handleAPI(w http.ResponseWriter, r *http.Request) {
	// 去掉末尾斜杠，让 /api/status 与 /api/status/ 等价。
	path := strings.TrimRight(r.URL.Path, "/")
	if path == "" {
		path = "/"
	}

	switch path {
	case "/api/status":
		s.handleStatus(w, r)
	case "/api/start":
		s.handleStart(w, r)
	case "/api/stop":
		s.handleStop(w, r)
	case "/api/restart":
		s.handleRestart(w, r)
	case "/api/isos":
		s.handleISOs(w, r)
	case "/api/isos/rescan":
		s.handleRescan(w, r)
	case "/api/isos/upload":
		s.handleUpload(w, r)
	case "/api/clients":
		s.handleClients(w, r)
	case "/api/events":
		s.handleEvents(w, r)
	case "/api/events/stream":
		s.handleStream(w, r)
	case "/api/settings":
		s.handleSettings(w, r)
	case "/api/interfaces":
		s.handleInterfaces(w, r)
	case "/api/version":
		s.handleVersion(w, r)
	default:
		if id, ok := strings.CutPrefix(path, "/api/preview/"); ok && id != "" {
			s.handlePreview(w, r, id)
			return
		}
		writeErr(w, http.StatusNotFound, fmt.Sprintf("未知接口 %s", r.URL.Path))
	}
}

// ---------------------------------------------------------------------------
// 状态与控制
// ---------------------------------------------------------------------------

// handleStatus 返回服务器实时状态。
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		methodNotAllowed(w, r, http.MethodGet)
		return
	}
	st := s.cfg.PXE.Status()
	if st.Version == "" {
		st.Version = s.version
	}
	// 兜底：控制器没有填充主机信息时，用本进程的信息补上。
	if st.Hostname == "" {
		if h, err := os.Hostname(); err == nil {
			st.Hostname = h
		}
	}
	if st.OS == "" {
		st.OS = runtime.GOOS
	}
	if st.Arch == "" {
		st.Arch = runtime.GOARCH
	}
	if st.ClientCount == 0 {
		st.ClientCount = len(s.cfg.PXE.Clients())
	}
	if st.ISOCount == 0 {
		st.ISOCount = len(s.cfg.PXE.ISOs())
	}

	settings := s.cfg.PXE.Settings()
	writeOK(w, map[string]any{
		"status":     st,
		"defaultISO": s.cfg.PXE.DefaultISO(),
		"settings": map[string]any{
			"serverIP":   settings.ServerIP,
			"interface":  settings.Interface,
			"rangeStart": settings.RangeStart,
			"rangeEnd":   settings.RangeEnd,
			"netmask":    settings.Netmask,
			"gateway":    settings.Gateway,
			"dhcpMode":   settings.DHCPMode,
		},
		"version": s.version,
	})
}

// handleStart 启动 PXE 服务。
func (s *Server) handleStart(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		methodNotAllowed(w, r, http.MethodPost)
		return
	}
	if err := s.cfg.PXE.Start(); err != nil {
		s.logEvent("error", "system", fmt.Sprintf("启动 PXE 服务失败: %v", err))
		writeErr(w, http.StatusInternalServerError, fmt.Sprintf("启动失败: %v", err))
		return
	}
	s.logEvent("info", "system", "PXE 服务已启动")
	writeOK(w, s.statusPayload())
}

// handleStop 停止 PXE 服务。
func (s *Server) handleStop(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		methodNotAllowed(w, r, http.MethodPost)
		return
	}
	if err := s.cfg.PXE.Stop(); err != nil {
		s.logEvent("error", "system", fmt.Sprintf("停止 PXE 服务失败: %v", err))
		writeErr(w, http.StatusInternalServerError, fmt.Sprintf("停止失败: %v", err))
		return
	}
	s.logEvent("warn", "system", "PXE 服务已停止")
	writeOK(w, s.statusPayload())
}

// handleRestart 重启 PXE 服务。
func (s *Server) handleRestart(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		methodNotAllowed(w, r, http.MethodPost)
		return
	}
	if err := s.cfg.PXE.Restart(); err != nil {
		s.logEvent("error", "system", fmt.Sprintf("重启 PXE 服务失败: %v", err))
		writeErr(w, http.StatusInternalServerError, fmt.Sprintf("重启失败: %v", err))
		return
	}
	s.logEvent("info", "system", "PXE 服务已重启")
	writeOK(w, s.statusPayload())
}

// statusPayload 是控制类接口统一返回的数据：最新状态 + 默认 ISO。
func (s *Server) statusPayload() map[string]any {
	st := s.cfg.PXE.Status()
	if st.Version == "" {
		st.Version = s.version
	}
	return map[string]any{
		"status":     st,
		"defaultISO": s.cfg.PXE.DefaultISO(),
	}
}

// ---------------------------------------------------------------------------
// ISO
// ---------------------------------------------------------------------------

// isoItem 是 ISO 列表里的一项，附带前端需要的派生信息。
type isoItem struct {
	model.ISO
	Default  bool   `json:"default"`
	SizeText string `json:"sizeText"`
	ArchText string `json:"archText"`
}

// handleISOs 返回当前 ISO 列表。
func (s *Server) handleISOs(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		methodNotAllowed(w, r, http.MethodGet)
		return
	}
	writeOK(w, s.isoPayload())
}

// isoPayload 组装 ISO 列表响应。
func (s *Server) isoPayload() map[string]any {
	isos := s.cfg.PXE.ISOs()
	def := s.cfg.PXE.DefaultISO()
	items := make([]isoItem, 0, len(isos))
	for _, is := range isos {
		items = append(items, isoItem{
			ISO:      is,
			Default:  is.ID != "" && is.ID == def,
			SizeText: humanSize(is.Size),
			ArchText: is.OSArch.String(),
		})
	}
	return map[string]any{
		"isos":       items,
		"count":      len(items),
		"defaultISO": def,
	}
}

// handleRescan 重新扫描 ISO 目录。
func (s *Server) handleRescan(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		methodNotAllowed(w, r, http.MethodPost)
		return
	}
	start := time.Now()
	count, err := s.cfg.PXE.RescanISOs()
	cost := time.Since(start)
	if err != nil {
		s.logEvent("error", "system", fmt.Sprintf("重新扫描 ISO 失败: %v", err))
		writeErr(w, http.StatusInternalServerError, fmt.Sprintf("扫描失败: %v", err))
		return
	}
	s.logEvent("info", "system", fmt.Sprintf("ISO 扫描完成，共 %d 个镜像（耗时 %s）", count, cost.Round(time.Millisecond)))
	payload := s.isoPayload()
	payload["rescanCount"] = count
	payload["elapsedMs"] = cost.Milliseconds()
	writeOK(w, payload)
}

// handleUpload 接收浏览器上传的 ISO 文件（multipart/form-data 或裸 body）。
func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		methodNotAllowed(w, r, http.MethodPost)
		return
	}
	if r.ContentLength > maxUploadBytes {
		writeErr(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("文件过大，单个 ISO 上限 %s", humanSize(maxUploadBytes)))
		return
	}

	dir, err := s.isoDir()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	saved := make([]map[string]any, 0, 1)
	mediaType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	switch {
	case strings.HasPrefix(mediaType, "multipart/"):
		mr, err := r.MultipartReader()
		if err != nil {
			writeErr(w, http.StatusBadRequest, fmt.Sprintf("解析上传表单失败: %v", err))
			return
		}
		for {
			part, err := mr.NextPart()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				writeErr(w, http.StatusBadRequest, fmt.Sprintf("读取上传内容失败: %v", err))
				return
			}
			if part.FileName() == "" {
				_ = part.Close()
				continue
			}
			name, err := safeISOName(part.FileName())
			if err != nil {
				_ = part.Close()
				writeErr(w, http.StatusBadRequest, err.Error())
				return
			}
			item, err := s.saveUpload(dir, name, part)
			_ = part.Close()
			if err != nil {
				writeErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			saved = append(saved, item)
		}
	default:
		// 裸 body：文件名从 ?name= 或 Content-Disposition 里取。
		name := r.URL.Query().Get("name")
		if name == "" {
			if _, params, err := mime.ParseMediaType(r.Header.Get("Content-Disposition")); err == nil {
				name = params["filename"]
			}
		}
		if name == "" {
			writeErr(w, http.StatusBadRequest, "缺少文件名：请用 multipart 上传，或在 URL 上带 ?name=xxx.iso")
			return
		}
		name, err = safeISOName(name)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		item, err := s.saveUpload(dir, name, http.MaxBytesReader(w, r.Body, maxUploadBytes))
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		saved = append(saved, item)
	}

	if len(saved) == 0 {
		writeErr(w, http.StatusBadRequest, "没有收到任何文件")
		return
	}

	// 上传完成后立即重新扫描，让新 ISO 出现在列表里。
	count, rescanErr := s.cfg.PXE.RescanISOs()
	for _, item := range saved {
		s.logEvent("info", "system", fmt.Sprintf("已上传 ISO %s（%v）", item["name"], item["sizeText"]))
	}
	if rescanErr != nil {
		s.logEvent("warn", "system", fmt.Sprintf("上传后自动扫描失败: %v", rescanErr))
	}

	payload := s.isoPayload()
	payload["uploaded"] = saved
	payload["rescanCount"] = count
	writeOK(w, payload)
}

// isoDir 返回 ISO 目录的绝对路径（不存在则创建）。
func (s *Server) isoDir() (string, error) {
	dir := strings.TrimSpace(s.cfg.PXE.Settings().ISODir)
	if dir == "" {
		dir = "iso"
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("无法解析 ISO 目录 %q: %w", dir, err)
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return "", fmt.Errorf("创建 ISO 目录 %s 失败: %w", abs, err)
	}
	return abs, nil
}

// safeISOName 校验并规范化上传的文件名，拒绝路径穿越。
func safeISOName(name string) (string, error) {
	name = strings.TrimSpace(name)
	// 浏览器可能带上完整路径（老 IE 用反斜杠），统一成正斜杠后只取最后一段。
	name = strings.ReplaceAll(name, "\\", "/")
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	if name == "" || name == "." || name == ".." {
		return "", errors.New("非法的文件名")
	}
	if strings.ContainsRune(name, 0) || strings.ContainsAny(name, "/") {
		return "", errors.New("文件名包含非法字符")
	}
	if !strings.EqualFold(filepath.Ext(name), ".iso") {
		return "", fmt.Errorf("只接受 .iso 文件（收到 %q）", name)
	}
	return name, nil
}

// saveUpload 把上传流落盘（先写临时文件再原子改名），返回给前端展示的信息。
func (s *Server) saveUpload(dir, name string, src io.Reader) (map[string]any, error) {
	tmp, err := os.CreateTemp(dir, ".upload-*.part")
	if err != nil {
		return nil, fmt.Errorf("创建临时文件失败: %w", err)
	}
	tmpName := tmp.Name()
	written, copyErr := io.Copy(tmp, io.LimitReader(src, maxUploadBytes))
	closeErr := tmp.Close()
	if copyErr != nil || closeErr != nil {
		_ = os.Remove(tmpName)
		if copyErr != nil {
			return nil, fmt.Errorf("写入上传文件失败: %w", copyErr)
		}
		return nil, fmt.Errorf("关闭上传文件失败: %w", closeErr)
	}
	if written == 0 {
		_ = os.Remove(tmpName)
		return nil, fmt.Errorf("上传的文件 %q 是空的", name)
	}
	final := filepath.Join(dir, name)
	if err := os.Rename(tmpName, final); err != nil {
		_ = os.Remove(tmpName)
		return nil, fmt.Errorf("保存 %s 失败: %w", name, err)
	}
	return map[string]any{
		"name":     name,
		"path":     final,
		"size":     written,
		"sizeText": humanSize(written),
	}, nil
}

// handlePreview 返回某个 ISO 生成的 iPXE 脚本（调试神器）。
func (s *Server) handlePreview(w http.ResponseWriter, r *http.Request, rawID string) {
	if !requireMethod(w, r, http.MethodGet) {
		methodNotAllowed(w, r, http.MethodGet)
		return
	}
	id, err := url.PathUnescape(rawID)
	if err != nil || id == "" {
		writeErr(w, http.StatusBadRequest, "非法的 ISO ID")
		return
	}
	mac := model.NormalizeMAC(r.URL.Query().Get("mac"))
	if mac == "" {
		mac = "00:00:00:00:00:00"
	}

	is, ok := s.findISO(id)
	if !ok {
		writeErr(w, http.StatusNotFound, fmt.Sprintf("找不到 ISO: %s", id))
		return
	}

	script, source := s.previewScript(is, mac)

	writeOK(w, map[string]any{
		"isoID":      is.ID,
		"isoName":    is.Name,
		"osName":     is.OSName,
		"family":     string(is.Family),
		"mac":        mac,
		"bootMethod": is.Boot.Method,
		"source":     source, // "pxe"=真实生成器，"web"=web 层近似预览
		"script":     script,
	})
}

// findISO 按 ID 查找 ISO；找不到时允许用文件名 / 相对路径做一次宽松匹配。
func (s *Server) findISO(id string) (model.ISO, bool) {
	isos := s.cfg.PXE.ISOs()
	for _, is := range isos {
		if is.ID == id {
			return is, true
		}
	}
	for _, is := range isos {
		if is.Name == id || is.RelPath == id {
			return is, true
		}
	}
	return model.ISO{}, false
}

// previewScript 优先交给 pxe 层生成（与真实下发完全一致），否则退回本地近似实现。
func (s *Server) previewScript(is model.ISO, mac string) (script, source string) {
	if p, ok := s.cfg.PXE.(Previewer); ok {
		if out, err := p.Preview(is.ID, mac); err == nil && strings.TrimSpace(out) != "" {
			return out, "pxe"
		}
	}
	st := s.cfg.PXE.Status()
	return buildPreviewScript(is, mac, s.cfg.PXE.Settings(), st.ServerIP, st.HTTPPort), "web"
}

// handleVersion 返回版本信息。
func (s *Server) handleVersion(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		methodNotAllowed(w, r, http.MethodGet)
		return
	}
	writeOK(w, map[string]any{
		"version":   s.version,
		"name":      "NetBootX",
		"goVersion": runtime.Version(),
		"os":        runtime.GOOS,
		"arch":      runtime.GOARCH,
	})
}

// ---------------------------------------------------------------------------
// 客户机
// ---------------------------------------------------------------------------

// handleClients 返回客户机列表，或清空它。
func (s *Server) handleClients(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		clients := s.cfg.PXE.Clients()
		if clients == nil {
			clients = []model.Client{}
		}
		// 按最近活跃时间倒序，最近启动的机器排在最上面。
		sort.SliceStable(clients, func(i, j int) bool {
			return clients[i].LastSeen.After(clients[j].LastSeen)
		})
		writeOK(w, map[string]any{
			"clients": clients,
			"count":   len(clients),
		})
	case http.MethodDelete:
		clearer, ok := s.cfg.PXE.(ClientClearer)
		if !ok {
			writeErr(w, http.StatusNotImplemented, "当前 PXE 后端不支持清空客户机列表")
			return
		}
		if err := clearer.ClearClients(); err != nil {
			s.logEvent("error", "system", fmt.Sprintf("清空客户机列表失败: %v", err))
			writeErr(w, http.StatusInternalServerError, fmt.Sprintf("清空失败: %v", err))
			return
		}
		s.logEvent("warn", "system", "客户机列表已清空")
		writeOK(w, map[string]any{"cleared": true})
	default:
		methodNotAllowed(w, r, "GET, DELETE")
	}
}

// ---------------------------------------------------------------------------
// 事件日志
// ---------------------------------------------------------------------------

// handleEvents 返回最近的事件日志。
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		methodNotAllowed(w, r, http.MethodGet)
		return
	}
	limit, err := parseLimit(r.URL.Query().Get("limit"), defaultEventLimit, maxEventLimit)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	events := s.cfg.PXE.Events(limit)
	if events == nil {
		events = []model.Event{}
	}
	writeOK(w, map[string]any{
		"events": events,
		"count":  len(events),
		"limit":  limit,
	})
}

// parseLimit 解析 ?limit=，缺省用 def，超过 max 时截断。
func parseLimit(raw string, def, max int) (int, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return def, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("limit 参数必须是整数（收到 %q）", raw)
	}
	if n <= 0 {
		return 0, fmt.Errorf("limit 参数必须大于 0（收到 %d）", n)
	}
	if n > max {
		n = max
	}
	return n, nil
}

// ---------------------------------------------------------------------------
// 配置
// ---------------------------------------------------------------------------

// handleSettings 读取或保存配置。
func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeOK(w, s.cfg.PXE.Settings())
	case http.MethodPost:
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
		if err != nil {
			writeErr(w, http.StatusBadRequest, fmt.Sprintf("读取配置请求体失败: %v", err))
			return
		}
		merged, err := mergeSettings(s.cfg.PXE.Settings(), body)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := validateSettings(merged); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := s.cfg.PXE.SaveSettings(merged); err != nil {
			s.logEvent("error", "system", fmt.Sprintf("保存配置失败: %v", err))
			writeErr(w, http.StatusInternalServerError, fmt.Sprintf("保存配置失败: %v", err))
			return
		}
		s.logEvent("info", "system", "配置已保存")
		writeOK(w, map[string]any{
			"settings": s.cfg.PXE.Settings(),
			"saved":    true,
		})
	default:
		methodNotAllowed(w, r, "GET, POST")
	}
}

// mergeSettings 把请求体里「显式出现」的字段合并到当前配置上。
// 这样前端可以只提交改动的字段，而不必回传整份配置；
// 也避免了 bool 字段（如 autoStart=false）被误判成「未提供」。
func mergeSettings(cur model.Settings, body []byte) (model.Settings, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		// 数组 / 字符串 / 数字等「是合法 JSON 但不是对象」的情况给出更明确的提示。
		var probe any
		if json.Unmarshal(body, &probe) == nil {
			return model.Settings{}, errors.New("配置请求体必须是一个 JSON 对象")
		}
		return model.Settings{}, fmt.Errorf("解析配置 JSON 失败: %w", err)
	}
	if raw == nil {
		return model.Settings{}, errors.New("配置请求体必须是一个 JSON 对象")
	}

	out := cur // 以当前配置为底，再逐字段覆盖
	fields := []struct {
		key string
		dst any
	}{
		{"serverIP", &out.ServerIP},
		{"interface", &out.Interface},
		{"rangeStart", &out.RangeStart},
		{"rangeEnd", &out.RangeEnd},
		{"netmask", &out.Netmask},
		{"gateway", &out.Gateway},
		{"dns", &out.DNS},
		{"domain", &out.Domain},
		{"leaseTime", &out.LeaseTime},
		{"dhcpMode", &out.DHCPMode},
		{"dhcpPort", &out.DHCPPort},
		{"httpPort", &out.HTTPPort},
		{"tftpPort", &out.TFTPPort},
		{"webPort", &out.WebPort},
		{"isoDir", &out.ISODir},
		{"userDir", &out.UserDir},
		{"dataDir", &out.DataDir},
		{"logDir", &out.LogDir},
		{"bootMenuTimeout", &out.BootMenuTimeout},
		{"defaultISO", &out.DefaultISO},
		{"tftpBlockSize", &out.TFTPBlockSize},
		{"secureBoot", &out.SecureBoot},
		{"autoStart", &out.AutoStart},
		{"language", &out.Language},
	}
	for _, f := range fields {
		v, ok := raw[f.key]
		if !ok {
			continue
		}
		if err := json.Unmarshal(v, f.dst); err != nil {
			return model.Settings{}, fmt.Errorf("字段 %s 的值非法: %w", f.key, err)
		}
	}
	return out, nil
}

// validateSettings 做最基本的合法性检查，避免把明显错误的配置写进磁盘。
func validateSettings(s model.Settings) error {
	switch s.DHCPMode {
	case "full", "proxy", "external":
	default:
		return fmt.Errorf("dhcpMode 必须是 full / proxy / external 之一（收到 %q）", s.DHCPMode)
	}
	if s.DHCPMode == "full" {
		if net.ParseIP(s.RangeStart) == nil {
			return fmt.Errorf("full 模式下 rangeStart 必须是合法 IPv4（收到 %q）", s.RangeStart)
		}
		if net.ParseIP(s.RangeEnd) == nil {
			return fmt.Errorf("full 模式下 rangeEnd 必须是合法 IPv4（收到 %q）", s.RangeEnd)
		}
	}
	if s.ServerIP != "" && net.ParseIP(s.ServerIP) == nil {
		return fmt.Errorf("serverIP 不是合法 IP（收到 %q）", s.ServerIP)
	}
	for _, item := range []struct {
		name string
		port int
	}{
		{"dhcpPort", s.DHCPPort},
		{"httpPort", s.HTTPPort},
		{"tftpPort", s.TFTPPort},
		{"webPort", s.WebPort},
	} {
		if item.port < 0 || item.port > 65535 {
			return fmt.Errorf("%s 超出范围 0-65535（收到 %d）", item.name, item.port)
		}
	}
	if s.LeaseTime < 0 {
		return fmt.Errorf("leaseTime 不能为负数（收到 %d）", s.LeaseTime)
	}
	if s.BootMenuTimeout < 0 {
		return fmt.Errorf("bootMenuTimeout 不能为负数（收到 %d）", s.BootMenuTimeout)
	}
	return nil
}

// ---------------------------------------------------------------------------
// 网卡
// ---------------------------------------------------------------------------

// handleInterfaces 返回本机网卡列表（界面顶部的 IP 下拉框用）。
func (s *Server) handleInterfaces(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		methodNotAllowed(w, r, http.MethodGet)
		return
	}
	ifaces := s.cfg.PXE.Interfaces()
	if ifaces == nil {
		ifaces = []NetInterface{}
	}
	writeOK(w, map[string]any{
		"interfaces": ifaces,
		"count":      len(ifaces),
	})
}

// ---------------------------------------------------------------------------
// 工具函数
// ---------------------------------------------------------------------------

// logEvent 记录一条 web 层产生的操作事件（写进 store，界面日志面板可见）。
func (s *Server) logEvent(level, kind, msg string) {
	ev := model.Event{
		Time:  time.Now(),
		Level: level,
		Kind:  kind,
		Msg:   msg,
	}
	if s.eventSink != nil {
		s.eventSink.AddEvent(ev)
	}
	if level == "error" {
		log.Printf("web: %s", msg)
	}
}

// humanSize 把字节数转成人类可读的大小。
func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	units := []string{"KB", "MB", "GB", "TB", "PB"}
	value := float64(n)
	for _, u := range units {
		value /= unit
		if value < unit {
			return fmt.Sprintf("%.1f %s", value, u)
		}
	}
	return fmt.Sprintf("%.1f EB", value)
}
