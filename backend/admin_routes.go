package main

import (
	"net/http"
	"path"
	"strings"
	"webssh/backend/internal/apierr"
)

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
				apierr.WriteErr(w, apierr.Invalid("终端 WebSocket 需要 Upgrade 头"))
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
		case p == "/api/sessions/stats":
			m.route(m.handleSessionsStats)(w, r)
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
			apierr.WriteErr(w, apierr.NotFound("接口不存在：%s", p))

		default:
			if r.Method != http.MethodGet && r.Method != http.MethodHead {
				apierr.WriteErr(w, apierr.Method("静态资源只支持 GET"))
				return
			}
			// 其余路径回退到 index.html（SPA 前端路由）。
			if m.spa == nil {
				apierr.WriteErr(w, apierr.Internal("前端资源未内嵌"))
				return
			}
			m.spa.ServeHTTP(w, r)
		}
	})
}

// ---------------------------------------------------------------------------
// 前端静态资源：base href 注入 + 三档缓存头
// ---------------------------------------------------------------------------
