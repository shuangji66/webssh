package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/user"
	"path"
	"path/filepath"
	"strconv"
	"strings"
)

// maxJSONBytes 是 JSON 请求体的上限（契约第 2 节）：整表提交的快捷指令也只有几 KB，
// 1 MiB 足够，而没有上限时一个客户端就能用超大 body 把进程内存打爆。
const maxJSONBytes = 1 << 20

// ---------------------------------------------------------------------------
// 统一错误响应
//
// 成功：{"ok": true, ...}
// 失败：HTTP 4xx/5xx + {"ok": false, "code": "<裸码>", "error": "<人类可读，含技术细节>"}
//
// code 是**裸码**（不带 err_ 前缀）：前端把它映射成 i18n 的 err_<code>，映射不到就原样
// 显示 error（frontend/src/serverapi/index.ts 的 describeError 就是这么做的）。
// ---------------------------------------------------------------------------

type apiError struct {
	Code    string
	Status  int
	Message string
}

func (e *apiError) Error() string { return e.Code + ": " + e.Message }

func failErr(status int, code, format string, args ...any) *apiError {
	return &apiError{Code: code, Status: status, Message: fmt.Sprintf(format, args...)}
}

// 错误码构造器：码表见契约第 4.1 节。状态码按语义分——
// 参数错 400、不存在 404、状态冲突 / 重复 409、权限 403、超限 413、依赖不可用 503、
// 远端（SSH/SFTP）失败 502、其余 500。前端只用 code 做 i18n，状态码是给日志与反代看的。
func errInvalid(format string, args ...any) *apiError {
	return failErr(400, "invalid", format, args...)
}
func errNotFound(format string, args ...any) *apiError {
	return failErr(404, "not_found", format, args...)
}
func errConflict(format string, args ...any) *apiError {
	return failErr(409, "conflict", format, args...)
}
func errDuplicate(format string, args ...any) *apiError {
	return failErr(409, "duplicate", format, args...)
}
func errPermission(format string, args ...any) *apiError {
	return failErr(403, "permission", format, args...)
}
func errTooLarge(format string, args ...any) *apiError {
	return failErr(413, "too_large", format, args...)
}
func errUnavailable(format string, args ...any) *apiError {
	return failErr(503, "unavailable", format, args...)
}
func errInternal(format string, args ...any) *apiError {
	return failErr(500, "internal", format, args...)
}
func errNoCredential(format string, args ...any) *apiError {
	return failErr(400, codeNoCredential, format, args...)
}
func errKeyParse(format string, args ...any) *apiError {
	return failErr(400, codeKeyParse, format, args...)
}
func errShellFailed(format string, args ...any) *apiError {
	return failErr(502, codeShellFailed, format, args...)
}
func errSFTPUnavailable(format string, args ...any) *apiError {
	return failErr(502, codeSFTPUnavailable, format, args...)
}

// errCredentialRequired 用于「该会话不保存凭据，且这次请求没带凭据」（契约 11.1）。
//
// 状态码取 409 而不是 400：请求本身没写错，缺的是一个**状态**（本进程内还没有人给这个
// 会话输入过凭据）。前端拿到这个裸码就去弹凭据输入框，填完再调一次 open。
func errCredentialRequired(format string, args ...any) *apiError {
	return failErr(409, "credential_required", format, args...)
}

// errNoSharePath 用于「没有配置飞牛共享目录」（TRIM_DATA_SHARE_PATHS 为空）。
// 503 而不是 400：参数没问题，是这个部署形态下确实没有可写的共享目录（依赖不可用）。
func errNoSharePath(format string, args ...any) *apiError {
	return failErr(503, "no_share_path", format, args...)
}

// errNoArchiveTool 用于「远端主机上没有这个归档工具」（命令退出码 127）。
// 502：本服务正常，远端环境缺东西（与 shell_failed 同类）。
func errNoArchiveTool(format string, args ...any) *apiError {
	return failErr(502, "no_archive_tool", format, args...)
}

// errArchiveUnsupported 用于「扩展名不认识，不知道拿什么工具解」。
// 400：这是请求本身的问题（用户选错了文件），改一下就能过。
func errArchiveUnsupported(format string, args ...any) *apiError {
	return failErr(400, "archive_unsupported", format, args...)
}

// errAborted 用于「客户端取消」：前端有 err_aborted 文案（「已取消」）。
// 状态码 408：请求超时 / 中断，中间件据此不会再重试。
func errAborted(format string, args ...any) *apiError {
	return failErr(408, "aborted", format, args...)
}

// errMethod 用于方法不对：这不是「参数不合法」而是用错了接口，405 更准确。
func errMethod(format string, args ...any) *apiError { return failErr(405, "invalid", format, args...) }

// ---------------------------------------------------------------------------
// JSON 助手
// ---------------------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		// 响应已经写出去了，再回错误也没用；记一条日志，否则「前端偶尔拿到半截 JSON」
		// 这类问题在服务端毫无痕迹。
		warnf("写 JSON 响应失败：%v", err)
	}
}

// writeErr 写失败响应。body 里的 ok/code/error 三个字段是前端的唯一判据。
func writeErr(w http.ResponseWriter, e *apiError) {
	if e.Status == 0 {
		e.Status = 500
	}
	if e.Code == "" {
		e.Code = "internal"
	}
	writeJSON(w, e.Status, map[string]any{"ok": false, "code": e.Code, "error": e.Message})
}

// respond 写成功响应并返回 nil，让 handler 能写成 `return respond(w, …)`。
func respond(w http.ResponseWriter, v any) *apiError {
	writeJSON(w, http.StatusOK, v)
	return nil
}

// decodeJSON 读一个 POST 的 JSON body。
//
// 刻意**不**用 DisallowUnknownFields：前端将来多带一个字段（例如新增的开关）不该让
// 老后端直接返回 400——那会把一次向前兼容的升级变成一次线上故障。
func (m *AdminMux) decodeJSON(w http.ResponseWriter, r *http.Request, out any) *apiError {
	if r.Method != http.MethodPost {
		return errMethod("该接口只支持 POST（带参数的接口一律 POST + JSON body）")
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxJSONBytes)
	err := json.NewDecoder(r.Body).Decode(out)
	switch {
	case err == nil:
		return nil
	case tooLargeBody(err):
		return errTooLarge("请求体超过上限 %s", humanSize(maxJSONBytes))
	case errors.Is(err, io.EOF):
		return errInvalid("缺少请求体（需要 JSON）")
	default:
		return errInvalid("请求体不是合法 JSON：%v", err)
	}
}

// requireMethod 校验无参数读取接口的方法。
func requireMethod(r *http.Request, want string) *apiError {
	if r.Method != want {
		return errMethod("该接口只支持 %s", want)
	}
	return nil
}

// ---------------------------------------------------------------------------
// AdminMux
// ---------------------------------------------------------------------------

// AdminMux 在 unix socket 上提供 SPA、JSON API 与终端 WebSocket。
// baseurl 前缀由 nginx / 飞牛统一网关带进来，这里先剥掉再匹配路由。
type AdminMux struct {
	renv     *RuntimeEnv
	st       *store
	crypt    *cryptor
	hks      *hostKeyStore
	sessions *sessionManager
	trim     *trimClient
	spa      http.Handler
}

func newAdminMux(renv *RuntimeEnv) (*AdminMux, error) {
	st, err := newStore(renv.DataDir)
	if err != nil {
		return nil, err
	}
	crypt, err := loadCryptor(renv.DataDir, renv.MasterKeyEnv)
	if err != nil {
		return nil, err
	}
	hks, err := newHostKeyStore(st)
	if err != nil {
		return nil, err
	}
	sessions, err := newSessionManager(renv, st, crypt, hks)
	if err != nil {
		return nil, err
	}
	return &AdminMux{
		renv:     renv,
		st:       st,
		crypt:    crypt,
		hks:      hks,
		sessions: sessions,
		trim:     newTrimClient(renv),
	}, nil
}

// SetSPA 挂上内嵌的前端资源（main.go 把 //go:embed 的文件系统传进来）。
func (m *AdminMux) SetSPA(fsys fs.FS) {
	m.spa = newSPAHandler(fsys, m.renv.AdminBaseURL)
}

// apiHandlerFunc 返回 nil 表示响应已经写好；返回 apiError 则由 route 统一写错误响应。
type apiHandlerFunc func(http.ResponseWriter, *http.Request) *apiError

func (m *AdminMux) route(h apiHandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if apiErr := h(w, r); apiErr != nil {
			writeErr(w, apiErr)
		}
	}
}

// ---------------------------------------------------------------------------
// 各接口
// ---------------------------------------------------------------------------

// handleInfo 返回运行期信息（版本 / bootId / 数据目录 / 飞牛可用性 / 网关身份）。
//
// bootId 每次进程启动都不一样：前端据此判断「后端换进程了」——换了就说明所有内存态会话
// 都没了（配置仍在），必须清空会话标签，而不是拿旧 id 去挂载一个不存在的终端。
func (m *AdminMux) handleInfo(w http.ResponseWriter, r *http.Request) *apiError {
	if apiErr := requireMethod(r, http.MethodGet); apiErr != nil {
		return apiErr
	}
	hostname, err := os.Hostname()
	if err != nil {
		hostname = ""
	}
	return respond(w, map[string]any{
		"ok": true,
		"runtime": map[string]any{
			"version":      m.renv.Version,
			"bootId":       bootID,
			"dataDir":      m.renv.DataDir,
			"adminSock":    m.renv.AdminSock,
			"adminBaseURL": m.renv.AdminBaseURL,
			"hostname":     hostname,
			// archiveDir 是**远端**压缩包的落点（契约 11.4：前端压缩成功后要跳到这个目录）；
			// exportRoot 是「导出至飞牛」的本机目标目录，空串 = 该功能不可用（前端据此禁用菜单项）。
			"archiveDir": m.renv.ArchiveDir,
			"exportRoot": m.renv.ExportRoot,
			"trim":       m.trim.probe(r.Context()),
			"gateway":    gatewayUserOf(r),
		},
	})
}

// handleSessions 返回会话配置 + 已打开会话的实时状态。
func (m *AdminMux) handleSessions(w http.ResponseWriter, r *http.Request) *apiError {
	if apiErr := requireMethod(r, http.MethodGet); apiErr != nil {
		return apiErr
	}
	return respond(w, map[string]any{
		"ok":       true,
		"sessions": m.sessions.views(),
		"open":     m.sessions.states(),
	})
}

func (m *AdminMux) handleSessionsSave(w http.ResponseWriter, r *http.Request) *apiError {
	var in sessionInput
	if apiErr := m.decodeJSON(w, r, &in); apiErr != nil {
		return apiErr
	}
	view, apiErr := m.sessions.save(in)
	if apiErr != nil {
		return apiErr
	}
	return respond(w, map[string]any{"ok": true, "session": view})
}

func (m *AdminMux) handleSessionsDelete(w http.ResponseWriter, r *http.Request) *apiError {
	var req struct {
		ID string `json:"id"`
	}
	if apiErr := m.decodeJSON(w, r, &req); apiErr != nil {
		return apiErr
	}
	if apiErr := m.sessions.delete(req.ID); apiErr != nil {
		return apiErr
	}
	return respond(w, map[string]any{"ok": true})
}

func (m *AdminMux) handleSessionsTest(w http.ResponseWriter, r *http.Request) *apiError {
	var in sessionInput
	if apiErr := m.decodeJSON(w, r, &in); apiErr != nil {
		return apiErr
	}
	out, apiErr := m.sessions.test(r.Context(), in)
	if apiErr != nil {
		return apiErr
	}
	return respond(w, map[string]any{"ok": true, "result": out})
}

func (m *AdminMux) handleSessionsTrust(w http.ResponseWriter, r *http.Request) *apiError {
	var req struct {
		Host        string `json:"host"`
		Port        int    `json:"port"`
		Fingerprint string `json:"fingerprint"`
	}
	if apiErr := m.decodeJSON(w, r, &req); apiErr != nil {
		return apiErr
	}
	if apiErr := m.sessions.trust(req.Host, req.Port, req.Fingerprint); apiErr != nil {
		return apiErr
	}
	return respond(w, map[string]any{"ok": true})
}

// handleSessionsOpen 打开会话。请求体里的明文凭据是「不保存凭据」的会话唯一的凭据来源
// （契约 11.1 / 11.5）；**响应里不回任何凭据**，只回实时状态。
func (m *AdminMux) handleSessionsOpen(w http.ResponseWriter, r *http.Request) *apiError {
	var req struct {
		ID         string  `json:"id"`
		Password   *string `json:"password"`
		PrivateKey *string `json:"privateKey"`
		Passphrase *string `json:"passphrase"`
	}
	if apiErr := m.decodeJSON(w, r, &req); apiErr != nil {
		return apiErr
	}
	creds := openCredentials{Password: req.Password, PrivateKey: req.PrivateKey, Passphrase: req.Passphrase}
	rt, apiErr := m.sessions.open(r.Context(), req.ID, creds)
	if apiErr != nil {
		return apiErr
	}
	return respond(w, map[string]any{"ok": true, "state": rt.state()})
}

func (m *AdminMux) handleSessionsClose(w http.ResponseWriter, r *http.Request) *apiError {
	var req struct {
		ID string `json:"id"`
	}
	if apiErr := m.decodeJSON(w, r, &req); apiErr != nil {
		return apiErr
	}
	if apiErr := m.sessions.closeRuntime(req.ID); apiErr != nil {
		return apiErr
	}
	return respond(w, map[string]any{"ok": true})
}

func (m *AdminMux) handleHostKeys(w http.ResponseWriter, r *http.Request) *apiError {
	if apiErr := requireMethod(r, http.MethodGet); apiErr != nil {
		return apiErr
	}
	return respond(w, map[string]any{"ok": true, "hosts": m.hks.list()})
}

func (m *AdminMux) handleHostKeysForget(w http.ResponseWriter, r *http.Request) *apiError {
	var req struct {
		Host string `json:"host"`
		Port int    `json:"port"`
	}
	if apiErr := m.decodeJSON(w, r, &req); apiErr != nil {
		return apiErr
	}
	if strings.TrimSpace(req.Host) == "" {
		return errInvalid("缺少 host")
	}
	if err := m.hks.forget(req.Host, req.Port); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return errNotFound("没有记录 %s 的主机密钥", hostKeyID(req.Host, req.Port))
		}
		return errInternal("移除信任失败：%v", err)
	}
	return respond(w, map[string]any{"ok": true})
}

// terminalOf 取出 (会话运行时, 终端)：终端类接口统一走它做存在性校验。
func (m *AdminMux) terminalOf(sid, tid string) (*sessionRuntime, *terminal, *apiError) {
	if !validID(tid) {
		return nil, nil, errNotFound("终端不存在")
	}
	rt, apiErr := m.sessions.runtimeFor(sid)
	if apiErr != nil {
		return nil, nil, apiErr
	}
	t, ok := rt.terminal(tid)
	if !ok {
		return nil, nil, errNotFound("终端不存在：%s", tid)
	}
	return rt, t, nil
}

func (m *AdminMux) handleTerminalHistory(w http.ResponseWriter, r *http.Request) *apiError {
	var req struct {
		SID string `json:"sid"`
		TID string `json:"tid"`
	}
	if apiErr := m.decodeJSON(w, r, &req); apiErr != nil {
		return apiErr
	}
	_, t, apiErr := m.terminalOf(req.SID, req.TID)
	if apiErr != nil {
		return apiErr
	}
	content, err := t.history()
	if err != nil {
		return errInternal("读取终端历史失败：%v", err)
	}
	return respond(w, map[string]any{"ok": true, "content": string(content), "size": len(content)})
}

// handleTerminalClear 清空历史（前端「清屏」同步清掉回放内容，刷新后不会又冒出来）。
func (m *AdminMux) handleTerminalClear(w http.ResponseWriter, r *http.Request) *apiError {
	var req struct {
		SID string `json:"sid"`
		TID string `json:"tid"`
	}
	if apiErr := m.decodeJSON(w, r, &req); apiErr != nil {
		return apiErr
	}
	_, t, apiErr := m.terminalOf(req.SID, req.TID)
	if apiErr != nil {
		return apiErr
	}
	if err := t.clearHistory(); err != nil {
		return errInternal("清空终端历史失败：%v", err)
	}
	return respond(w, map[string]any{"ok": true})
}

// handleTerminalClose 关闭一个终端（终止它的 shell 与连接）。
func (m *AdminMux) handleTerminalClose(w http.ResponseWriter, r *http.Request) *apiError {
	var req struct {
		SID string `json:"sid"`
		TID string `json:"tid"`
	}
	if apiErr := m.decodeJSON(w, r, &req); apiErr != nil {
		return apiErr
	}
	rt, t, apiErr := m.terminalOf(req.SID, req.TID)
	if apiErr != nil {
		return apiErr
	}
	t.close()
	rt.forgetTerminal(t.id)
	return respond(w, map[string]any{"ok": true})
}

// handleTerminalRename 改终端标题（只用于界面展示；空标题 = 恢复成「终端 N」）。
func (m *AdminMux) handleTerminalRename(w http.ResponseWriter, r *http.Request) *apiError {
	var req struct {
		SID   string `json:"sid"`
		TID   string `json:"tid"`
		Title string `json:"title"`
	}
	if apiErr := m.decodeJSON(w, r, &req); apiErr != nil {
		return apiErr
	}
	_, t, apiErr := m.terminalOf(req.SID, req.TID)
	if apiErr != nil {
		return apiErr
	}
	title := strings.TrimSpace(req.Title)
	if len(title) > 128 {
		return errInvalid("标题过长（最多 128 字符）")
	}
	t.setTitle(title)
	return respond(w, map[string]any{"ok": true})
}

// ---------------------------------------------------------------------------
// 路由
// ---------------------------------------------------------------------------

func isWebSocket(r *http.Request) bool {
	return r.Method == http.MethodGet && strings.EqualFold(r.Header.Get("Upgrade"), "websocket")
}

func (m *AdminMux) buildHandler() http.Handler {
	base := strings.TrimRight(m.renv.AdminBaseURL, "/")
	term := &terminalHandler{mux: m}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if base != "" && strings.HasPrefix(p, base) {
			p = strings.TrimPrefix(p, base)
			if p == "" {
				p = "/"
			}
		}
		// 反代可能送进来的重复斜杠（"//api/info"）：规整掉，否则路由匹配不上，
		// 用户看到的是「前端路由页面」而不是接口错误。
		if strings.Contains(p, "//") {
			p = path.Clean(p)
		}
		r.URL.Path = p

		switch {
		case p == "/api/terminal/ws":
			if !isWebSocket(r) {
				writeErr(w, errInvalid("终端 WebSocket 需要 Upgrade 头"))
				return
			}
			term.serve(w, r)
			return

		case p == "/api/info":
			m.route(m.handleInfo)(w, r)
		case p == "/api/sessions":
			m.route(m.handleSessions)(w, r)
		case p == "/api/sessions/save":
			m.route(m.handleSessionsSave)(w, r)
		case p == "/api/sessions/delete":
			m.route(m.handleSessionsDelete)(w, r)
		case p == "/api/sessions/test":
			m.route(m.handleSessionsTest)(w, r)
		case p == "/api/sessions/trust":
			m.route(m.handleSessionsTrust)(w, r)
		case p == "/api/sessions/open":
			m.route(m.handleSessionsOpen)(w, r)
		case p == "/api/sessions/close":
			m.route(m.handleSessionsClose)(w, r)
		case p == "/api/hostkeys":
			m.route(m.handleHostKeys)(w, r)
		case p == "/api/hostkeys/forget":
			m.route(m.handleHostKeysForget)(w, r)
		case p == "/api/terminals/history":
			m.route(m.handleTerminalHistory)(w, r)
		case p == "/api/terminals/clear":
			m.route(m.handleTerminalClear)(w, r)
		case p == "/api/terminals/close":
			m.route(m.handleTerminalClose)(w, r)
		case p == "/api/terminals/rename":
			m.route(m.handleTerminalRename)(w, r)
		case p == "/api/fs/list":
			m.route(m.handleFsList)(w, r)
		case p == "/api/fs/info":
			m.route(m.handleFsInfo)(w, r)
		case p == "/api/fs/mkdir":
			m.route(m.handleFsMkdir)(w, r)
		case p == "/api/fs/create":
			m.route(m.handleFsCreate)(w, r)
		case p == "/api/fs/rename":
			m.route(m.handleFsRename)(w, r)
		case p == "/api/fs/remove":
			m.route(m.handleFsRemove)(w, r)
		case p == "/api/fs/copy":
			m.route(m.handleFsCopy)(w, r)
		case p == "/api/fs/move":
			m.route(m.handleFsMove)(w, r)
		case p == "/api/fs/compress":
			m.route(m.handleFsCompress)(w, r)
		case p == "/api/fs/extract":
			m.route(m.handleFsExtract)(w, r)
		case p == "/api/fs/export-to-nas":
			m.route(m.handleFsExportToNas)(w, r)
		case p == "/api/fs/upload":
			m.route(m.handleFsUpload)(w, r)
		case p == "/api/fs/upload-from-nas":
			m.route(m.handleFsUploadFromNas)(w, r)
		case p == "/api/fs/download":
			m.route(m.handleFsDownload)(w, r)
		case p == "/api/fs/read":
			m.route(m.handleFsRead)(w, r)
		case p == "/api/quickcmds/list":
			m.route(m.handleQuickCmdsList)(w, r)
		case p == "/api/quickcmds/save":
			m.route(m.handleQuickCmdsSave)(w, r)
		case p == "/api/trim/status":
			m.route(m.handleTrimStatus)(w, r)
		case p == "/api/trim/acl":
			m.route(m.handleTrimACL)(w, r)

		case strings.HasPrefix(p, "/api/"):
			// /api/* 未命中**必须**回 404 JSON：退化成 index.html 会让前端拿到一坨 HTML
			// 去 JSON.parse，报出来的是「Unexpected token <」——排查方向完全被带偏。
			writeErr(w, errNotFound("接口不存在：%s", p))

		default:
			if r.Method != http.MethodGet && r.Method != http.MethodHead {
				writeErr(w, errMethod("静态资源只支持 GET"))
				return
			}
			// 其余路径回退到 index.html（SPA 前端路由）。
			if m.spa == nil {
				writeErr(w, errInternal("前端资源未内嵌"))
				return
			}
			m.spa.ServeHTTP(w, r)
		}
	})
}

// ---------------------------------------------------------------------------
// 前端静态资源：base href 注入 + 三档缓存头
// ---------------------------------------------------------------------------

type spaHandler struct {
	fsys fs.FS
	base string // 已去掉尾部斜杠
}

func newSPAHandler(fsys fs.FS, baseurl string) http.Handler {
	// 内嵌的是 embed/ 目录本身，取子目录后内部的路径就是 "index.html" / "assets/…"。
	if sub, err := fs.Sub(fsys, "embed"); err == nil {
		fsys = sub
	}
	return &spaHandler{fsys: fsys, base: strings.TrimRight(baseurl, "/")}
}

func (s *spaHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	reqPath := strings.TrimPrefix(r.URL.Path, "/")
	if reqPath == "" {
		reqPath = "index.html"
	}
	if b, err := fs.ReadFile(s.fsys, reqPath); err == nil {
		s.serve(w, reqPath, b)
		return
	}
	// 不存在的路径回退到 index.html（SPA 前端路由：/session/xxx 这类地址刷新后要能打开）。
	b, err := fs.ReadFile(s.fsys, "index.html")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	s.serve(w, "index.html", b)
}

func (s *spaHandler) serve(w http.ResponseWriter, name string, b []byte) {
	if strings.HasSuffix(name, ".html") && s.base != "" {
		b = rewriteIndexBase(b, s.base)
	}
	w.Header().Set("Cache-Control", cacheControlFor(name))
	w.Header().Set("Content-Type", contentTypeFor(name))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(b)
}

// rewriteIndexBase 注入运行期 baseurl。
//
// baseurl 构建期未知（部署在哪个前缀由网关决定），前端产物用相对 base（./assets/…）
// 构建；这里把 <base href> 塞进 <head> 并把 ./assets/ 改写成 <base>/assets/。
// 注意 <base href> 必须以 `/` 结尾：否则相对路径会基于上一级目录解析。
func rewriteIndexBase(body []byte, base string) []byte {
	s := string(body)
	s = strings.ReplaceAll(s, `src="./assets/`, `src="`+base+`/assets/`)
	s = strings.ReplaceAll(s, `href="./assets/`, `href="`+base+`/assets/`)
	tag := `<base href="` + base + `/">`
	if idx := strings.Index(strings.ToLower(s), "<head"); idx != -1 {
		if closeIdx := strings.Index(s[idx:], ">"); closeIdx != -1 {
			pos := idx + closeIdx + 1
			return []byte(s[:pos] + tag + s[pos:])
		}
	}
	return []byte(tag + s)
}

// cacheControlFor 返回静态资源的 Cache-Control：
//   - `*.html` → no-cache：后端每次响应都要注入运行期 <base href>，缓存住会把旧页面钉死
//     （用户拿到的还是上一个 baseurl，接口全 404）。
//   - `assets/*` → immutable：Vite 产物文件名带内容哈希，内容变了文件名就变。
//   - 其余根级文件（图标 / 字体 / manifest）→ 一天：文件名无哈希，给个折中值。
func cacheControlFor(name string) string {
	switch {
	case strings.HasSuffix(name, ".html"):
		return "no-cache, must-revalidate"
	case strings.HasPrefix(path.Clean(name), "assets/"):
		return "public, max-age=31536000, immutable"
	default:
		return "public, max-age=86400"
	}
}

func contentTypeFor(name string) string {
	switch {
	case strings.HasSuffix(name, ".html"):
		return "text/html; charset=utf-8"
	case strings.HasSuffix(name, ".js"):
		return "application/javascript; charset=utf-8"
	case strings.HasSuffix(name, ".css"):
		return "text/css; charset=utf-8"
	case strings.HasSuffix(name, ".json"):
		return "application/json; charset=utf-8"
	case strings.HasSuffix(name, ".svg"):
		return "image/svg+xml"
	case strings.HasSuffix(name, ".png"):
		return "image/png"
	case strings.HasSuffix(name, ".jpg"), strings.HasSuffix(name, ".jpeg"):
		return "image/jpeg"
	case strings.HasSuffix(name, ".ico"):
		return "image/x-icon"
	case strings.HasSuffix(name, ".woff2"):
		return "font/woff2"
	case strings.HasSuffix(name, ".woff"):
		return "font/woff"
	case strings.HasSuffix(name, ".map"):
		return "application/json; charset=utf-8"
	default:
		return "application/octet-stream"
	}
}

// ---------------------------------------------------------------------------
// unix socket 入口
// ---------------------------------------------------------------------------

// listenAdminSocket 绑定唯一访问入口。
//
// 绑定必须**成功**才进信号等待：早先的实现（参考项目）在 goroutine 里 bind、失败只打
// 日志，于是进程会「活着但没有任何 socket」——supervisor 看不出异常，用户访问不了，
// 也查不出原因。
func listenAdminSocket(renv *RuntimeEnv) (net.Listener, error) {
	if dir := filepath.Dir(renv.AdminSock); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
	}
	// 上一次异常退出留下的 socket 文件会让 bind 直接失败（EADDRINUSE）。调用方已经
	// 确认过没有活着的监听者，这里删掉残留文件再绑。
	_ = os.Remove(renv.AdminSock)
	ln, err := net.Listen("unix", renv.AdminSock)
	if err != nil {
		return nil, err
	}
	// 0660：nginx / 网关与进程同组即可访问；同组之外任何用户都进不来。
	if err := os.Chmod(renv.AdminSock, 0o660); err != nil {
		warnf("设置 socket 权限失败：%v", err)
	}
	return ln, nil
}

// ---------------------------------------------------------------------------
// 统一网关转发的当前用户
// ---------------------------------------------------------------------------

// gatewayUser 是 RuntimeInfo.gateway（契约第 4.3 节）。
type gatewayUser struct {
	UID         int    `json:"uid"`
	Username    string `json:"username"`
	IsAdmin     bool   `json:"isAdmin"`
	FromGateway bool   `json:"fromGateway"`
}

// gatewayUserOf 解析网关转发的 X-Trim-* 头。
//
// 网关只在校验过登录态之后才转发这组头，所以它可以作为**可信身份**——注意是「可信身份」
// 而不是「已授权」：能不能读某个本机路径仍然要用 uid 去问 trim.file.checkUserACL。
// 没有网关时（直接调 socket / 开发）退化成进程自己的 uid。
func gatewayUserOf(r *http.Request) gatewayUser {
	u := gatewayUser{UID: os.Getuid()}
	if cur, err := user.Current(); err == nil {
		u.Username = cur.Username
	}
	raw := strings.TrimSpace(r.Header.Get("X-Trim-Userid"))
	if raw == "" {
		return u
	}
	uid, err := strconv.Atoi(raw)
	if err != nil || uid < 0 {
		return u
	}
	u.UID = uid
	u.Username = strings.TrimSpace(r.Header.Get("X-Trim-Username"))
	if u.Username == "" {
		if looked, lerr := user.LookupId(raw); lerr == nil {
			u.Username = looked.Username
		}
	}
	u.IsAdmin = strings.EqualFold(strings.TrimSpace(r.Header.Get("X-Trim-Isadmin")), "true")
	u.FromGateway = true
	return u
}
