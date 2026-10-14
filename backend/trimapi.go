package main

import (
	"net/http"

	"webssh/backend/internal/apierr"
	"webssh/backend/internal/trimapi"
)

// ---------------------------------------------------------------------------
// /api/trim/*
//
// 客户端本身在 internal/trimapi 包里，这里只做「HTTP 入口 + 身份取用」。
// ---------------------------------------------------------------------------

// handleTrimStatus 返回飞牛宿主是否可用（设置弹窗里要显示原因）。
//
// 这是条只读接口（契约 4.2 标的是 GET）：显式校验方法，与 /api/info、/api/sessions、
// /api/hostkeys 那三条 GET 一致——不校验时 POST 也能进来，文档与实现就对不上了。
func (m *AdminMux) handleTrimStatus(w http.ResponseWriter, r *http.Request) *apierr.Error {
	if apiErr := requireMethod(r, http.MethodGet); apiErr != nil {
		return apiErr
	}
	return apierr.Respond(w, map[string]any{"ok": true, "trim": m.trim.Probe(r.Context())})
}

// handleTrimACL 检查当前用户对若干本机路径的权限。
//
// 身份取网关转发的 X-Trim-Userid（没有网关时退化成进程自己的 uid）：请求体里带的
// uid 一律不认——那是客户端可控的输入。
func (m *AdminMux) handleTrimACL(w http.ResponseWriter, r *http.Request) *apierr.Error {
	var req struct {
		Paths []string `json:"paths"`
	}
	if apiErr := m.decodeJSON(w, r, &req); apiErr != nil {
		return apiErr
	}
	u := gatewayUserOf(r)
	list, err := m.trim.CheckUserACL(r.Context(), u.UID, req.Paths)
	if err != nil {
		return trimapi.ToHTTPError(err)
	}
	return apierr.Respond(w, map[string]any{"ok": true, "list": list})
}
