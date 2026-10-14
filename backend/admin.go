package main

import (
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"os"
	"strings"
	"webssh/backend/internal/apierr"
	"webssh/backend/internal/secret"
	"webssh/backend/internal/store"
	"webssh/backend/internal/trimapi"
)

// maxJSONBytes 是 JSON 请求体的上限（契约第 2 节）：整表提交的快捷指令也只有几 KB，
// 1 MiB 足够，而没有上限时一个客户端就能用超大 body 把进程内存打爆。
const maxJSONBytes = 1 << 20

// decodeJSON 读一个 POST 的 JSON body。
//
// 刻意**不**用 DisallowUnknownFields：前端将来多带一个字段（例如新增的开关）不该让
// 老后端直接返回 400——那会把一次向前兼容的升级变成一次线上故障。
func (m *AdminMux) decodeJSON(w http.ResponseWriter, r *http.Request, out any) *apierr.Error {
	if r.Method != http.MethodPost {
		return apierr.Method("该接口只支持 POST（带参数的接口一律 POST + JSON body）")
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxJSONBytes)
	err := json.NewDecoder(r.Body).Decode(out)
	switch {
	case err == nil:
		return nil
	case tooLargeBody(err):
		return apierr.TooLarge("请求体超过上限 %s", humanSize(maxJSONBytes))
	case errors.Is(err, io.EOF):
		return apierr.Invalid("缺少请求体（需要 JSON）")
	default:
		return apierr.Invalid("请求体不是合法 JSON：%v", err)
	}
}

// requireMethod 校验无参数读取接口的方法。
func requireMethod(r *http.Request, want string) *apierr.Error {
	if r.Method != want {
		return apierr.Method("该接口只支持 %s", want)
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
	st       *store.Store
	crypt    *secret.Cryptor
	hks      *hostKeyStore
	sessions *sessionManager
	trim     *trimapi.Client
	spa      http.Handler
}

func newAdminMux(renv *RuntimeEnv) (*AdminMux, error) {
	st, err := store.New(renv.DataDir)
	if err != nil {
		return nil, err
	}
	crypt, err := secret.Load(renv.DataDir, renv.MasterKeyEnv)
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
		trim:     trimapi.NewClient(renv.TrimSocket, renv.TrimApp),
	}, nil
}

// SetSPA 挂上内嵌的前端资源（main.go 把 //go:embed 的文件系统传进来）。
func (m *AdminMux) SetSPA(fsys fs.FS) {
	m.spa = newSPAHandler(fsys, m.renv.AdminBaseURL)
}

// apiHandlerFunc 返回 nil 表示响应已经写好；返回 apierr.Error 则由 route 统一写错误响应。
type apiHandlerFunc func(http.ResponseWriter, *http.Request) *apierr.Error

func (m *AdminMux) route(h apiHandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if apiErr := h(w, r); apiErr != nil {
			apierr.WriteErr(w, apiErr)
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
func (m *AdminMux) handleInfo(w http.ResponseWriter, r *http.Request) *apierr.Error {
	if apiErr := requireMethod(r, http.MethodGet); apiErr != nil {
		return apiErr
	}
	hostname, err := os.Hostname()
	if err != nil {
		hostname = ""
	}
	return apierr.Respond(w, map[string]any{
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
			"trim":       m.trim.Probe(r.Context()),
			"gateway":    gatewayUserOf(r),
		},
	})
}

// handleSessions 返回会话配置 + 已打开会话的实时状态。
func (m *AdminMux) handleSessions(w http.ResponseWriter, r *http.Request) *apierr.Error {
	if apiErr := requireMethod(r, http.MethodGet); apiErr != nil {
		return apiErr
	}
	return apierr.Respond(w, map[string]any{
		"ok":       true,
		"sessions": m.sessions.views(),
		"open":     m.sessions.states(),
	})
}

func (m *AdminMux) handleSessionsSave(w http.ResponseWriter, r *http.Request) *apierr.Error {
	var in store.SessionInput
	if apiErr := m.decodeJSON(w, r, &in); apiErr != nil {
		return apiErr
	}
	view, apiErr := m.sessions.save(in)
	if apiErr != nil {
		return apiErr
	}
	return apierr.Respond(w, map[string]any{"ok": true, "session": view})
}

func (m *AdminMux) handleSessionsDelete(w http.ResponseWriter, r *http.Request) *apierr.Error {
	var req struct {
		ID string `json:"id"`
	}
	if apiErr := m.decodeJSON(w, r, &req); apiErr != nil {
		return apiErr
	}
	if apiErr := m.sessions.delete(req.ID); apiErr != nil {
		return apiErr
	}
	return apierr.Respond(w, map[string]any{"ok": true})
}

func (m *AdminMux) handleSessionsTest(w http.ResponseWriter, r *http.Request) *apierr.Error {
	var in store.SessionInput
	if apiErr := m.decodeJSON(w, r, &in); apiErr != nil {
		return apiErr
	}
	out, apiErr := m.sessions.test(r.Context(), in)
	if apiErr != nil {
		return apiErr
	}
	return apierr.Respond(w, map[string]any{"ok": true, "result": out})
}

func (m *AdminMux) handleSessionsTrust(w http.ResponseWriter, r *http.Request) *apierr.Error {
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
	return apierr.Respond(w, map[string]any{"ok": true})
}

// handleSessionsOpen 打开会话。请求体里的明文凭据是「不保存凭据」的会话唯一的凭据来源
// （契约 11.1 / 11.5）；**响应里不回任何凭据**，只回实时状态。
func (m *AdminMux) handleSessionsOpen(w http.ResponseWriter, r *http.Request) *apierr.Error {
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
	return apierr.Respond(w, map[string]any{"ok": true, "state": rt.state()})
}

func (m *AdminMux) handleSessionsClose(w http.ResponseWriter, r *http.Request) *apierr.Error {
	var req struct {
		ID string `json:"id"`
	}
	if apiErr := m.decodeJSON(w, r, &req); apiErr != nil {
		return apiErr
	}
	if apiErr := m.sessions.closeRuntime(req.ID); apiErr != nil {
		return apiErr
	}
	return apierr.Respond(w, map[string]any{"ok": true})
}

// handleSessionsStats 取「已打开会话」所在远端主机的系统信息（CPU / 负载 / 内存 / 磁盘）。
//
// 只有打开着的会话才有：没打开就没有连接可问（回 409 err_not_open，前端此时也不会来问——
// 它只对已打开的会话轮询）。取数失败（远端读不到 /proc）**不是**错误，是 stats.available=false。
func (m *AdminMux) handleSessionsStats(w http.ResponseWriter, r *http.Request) *apierr.Error {
	var req struct {
		ID string `json:"id"`
	}
	if apiErr := m.decodeJSON(w, r, &req); apiErr != nil {
		return apiErr
	}
	rt, apiErr := m.sessions.runtimeFor(req.ID)
	if apiErr != nil {
		return apiErr
	}
	stats, apiErr := rt.stats(r.Context())
	if apiErr != nil {
		return apiErr
	}
	return apierr.Respond(w, map[string]any{"ok": true, "stats": stats})
}

func (m *AdminMux) handleHostKeys(w http.ResponseWriter, r *http.Request) *apierr.Error {
	if apiErr := requireMethod(r, http.MethodGet); apiErr != nil {
		return apiErr
	}
	return apierr.Respond(w, map[string]any{"ok": true, "hosts": m.hks.list()})
}

func (m *AdminMux) handleHostKeysForget(w http.ResponseWriter, r *http.Request) *apierr.Error {
	var req struct {
		Host string `json:"host"`
		Port int    `json:"port"`
	}
	if apiErr := m.decodeJSON(w, r, &req); apiErr != nil {
		return apiErr
	}
	if strings.TrimSpace(req.Host) == "" {
		return apierr.Invalid("缺少 host")
	}
	if err := m.hks.forget(req.Host, req.Port); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return apierr.NotFound("没有记录 %s 的主机密钥", hostKeyID(req.Host, req.Port))
		}
		return apierr.Internal("移除信任失败：%v", err)
	}
	return apierr.Respond(w, map[string]any{"ok": true})
}

// terminalOf 取出 (会话运行时, 终端)：终端类接口统一走它做存在性校验。
func (m *AdminMux) terminalOf(sid, tid string) (*sessionRuntime, *terminal, *apierr.Error) {
	if !store.ValidID(tid) {
		return nil, nil, apierr.NotFound("终端不存在")
	}
	rt, apiErr := m.sessions.runtimeFor(sid)
	if apiErr != nil {
		return nil, nil, apiErr
	}
	t, ok := rt.terminal(tid)
	if !ok {
		return nil, nil, apierr.NotFound("终端不存在：%s", tid)
	}
	return rt, t, nil
}

func (m *AdminMux) handleTerminalHistory(w http.ResponseWriter, r *http.Request) *apierr.Error {
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
		return apierr.Internal("读取终端历史失败：%v", err)
	}
	return apierr.Respond(w, map[string]any{"ok": true, "content": string(content), "size": len(content)})
}

// handleTerminalClear 清空历史（前端「清屏」同步清掉回放内容，刷新后不会又冒出来）。
func (m *AdminMux) handleTerminalClear(w http.ResponseWriter, r *http.Request) *apierr.Error {
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
		return apierr.Internal("清空终端历史失败：%v", err)
	}
	return apierr.Respond(w, map[string]any{"ok": true})
}

// handleTerminalClose 关闭一个终端（终止它的 shell 与连接）。
func (m *AdminMux) handleTerminalClose(w http.ResponseWriter, r *http.Request) *apierr.Error {
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
	return apierr.Respond(w, map[string]any{"ok": true})
}

// handleTerminalRename 改终端标题（只用于界面展示；空标题 = 恢复成「终端 N」）。
func (m *AdminMux) handleTerminalRename(w http.ResponseWriter, r *http.Request) *apierr.Error {
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
		return apierr.Invalid("标题过长（最多 128 字符）")
	}
	t.setTitle(title)
	return apierr.Respond(w, map[string]any{"ok": true})
}

// ---------------------------------------------------------------------------
// 路由
// ---------------------------------------------------------------------------

func isWebSocket(r *http.Request) bool {
	return r.Method == http.MethodGet && strings.EqualFold(r.Header.Get("Upgrade"), "websocket")
}
