// Package apierr 是 HTTP API 的错误码与 JSON 响应层（契约第 4.1 节）。
//
// 成功：{"ok": true, ...}
// 失败：HTTP 4xx/5xx + {"ok": false, "code": "<裸码>", "error": "<人类可读，含技术细节>"}
//
// code 是**裸码**（不带 err_ 前缀）：前端把它映射成 i18n 的 err_<code>，映射不到就原样
// 显示 error（frontend/src/serverapi/index.ts 的 describeError 就是这么做的）。
// 因此码值是前后端契约：改名要同时改两套 i18n 字典。
package apierr

import (
	"encoding/json"
	"fmt"
	"net/http"

	"webssh/backend/internal/logx"
)

// 连接类裸码（契约第 4.1 节）：这些码既用于 HTTP 响应，也用于 connectError.Code，
// 所以放在本包作为唯一来源，避免两处各写一份字符串。
const (
	CodeNoCredential    = "no_credential"
	CodeKeyParse        = "key_parse"
	CodeShellFailed     = "shell_failed"
	CodeSFTPUnavailable = "sftp_unavailable"
)

// Error 是一个 API 错误：裸码 + HTTP 状态码 + 给人看的消息。
type Error struct {
	Code    string
	Status  int
	Message string
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

// Fail 是底层构造器（状态码 + 码值都自己给）。
func Fail(status int, code, format string, args ...any) *Error {
	return &Error{Code: code, Status: status, Message: fmt.Sprintf(format, args...)}
}

// 错误码构造器：状态码按语义分——
// 参数错 400、不存在 404、状态冲突 / 重复 409、权限 403、超限 413、依赖不可用 503、
// 远端（SSH/SFTP）失败 502、其余 500。前端只用 code 做 i18n，状态码是给日志与反代看的。
func Invalid(format string, args ...any) *Error {
	return Fail(400, "invalid", format, args...)
}
func NotFound(format string, args ...any) *Error {
	return Fail(404, "not_found", format, args...)
}
func Conflict(format string, args ...any) *Error {
	return Fail(409, "conflict", format, args...)
}
func Duplicate(format string, args ...any) *Error {
	return Fail(409, "duplicate", format, args...)
}
func Permission(format string, args ...any) *Error {
	return Fail(403, "permission", format, args...)
}
func TooLarge(format string, args ...any) *Error {
	return Fail(413, "too_large", format, args...)
}
func Unavailable(format string, args ...any) *Error {
	return Fail(503, "unavailable", format, args...)
}
func Internal(format string, args ...any) *Error {
	return Fail(500, "internal", format, args...)
}
func NoCredential(format string, args ...any) *Error {
	return Fail(400, CodeNoCredential, format, args...)
}
func KeyParse(format string, args ...any) *Error {
	return Fail(400, CodeKeyParse, format, args...)
}
func ShellFailed(format string, args ...any) *Error {
	return Fail(502, CodeShellFailed, format, args...)
}
func SFTPUnavailable(format string, args ...any) *Error {
	return Fail(502, CodeSFTPUnavailable, format, args...)
}

// CredentialRequired 用于「该会话不保存凭据，且这次请求没带凭据」（契约 11.1）。
//
// 状态码取 409 而不是 400：请求本身没写错，缺的是一个**状态**（本进程内还没有人给这个
// 会话输入过凭据）。前端拿到这个裸码就去弹凭据输入框，填完再调一次 open。
func CredentialRequired(format string, args ...any) *Error {
	return Fail(409, "credential_required", format, args...)
}

// NoSharePath 用于「没有配置飞牛共享目录」（TRIM_DATA_SHARE_PATHS 为空）。
// 503 而不是 400：参数没问题，是这个部署形态下确实没有可写的共享目录（依赖不可用）。
func NoSharePath(format string, args ...any) *Error {
	return Fail(503, "no_share_path", format, args...)
}

// NoArchiveTool 用于「远端主机上没有这个归档工具」（命令退出码 127）。
// 502：本服务正常，远端环境缺东西（与 shell_failed 同类）。
func NoArchiveTool(format string, args ...any) *Error {
	return Fail(502, "no_archive_tool", format, args...)
}

// ArchiveUnsupported 用于「扩展名不认识，不知道拿什么工具解」。
// 400：这是请求本身的问题（用户选错了文件），改一下就能过。
func ArchiveUnsupported(format string, args ...any) *Error {
	return Fail(400, "archive_unsupported", format, args...)
}

// Aborted 用于「客户端取消」：前端有 err_aborted 文案（「已取消」）。
// 状态码 408：请求超时 / 中断，中间件据此不会再重试。
func Aborted(format string, args ...any) *Error {
	return Fail(408, "aborted", format, args...)
}

// Method 用于方法不对：这不是「参数不合法」而是用错了接口，405 更准确。
func Method(format string, args ...any) *Error { return Fail(405, "invalid", format, args...) }

// ---------------------------------------------------------------------------
// JSON 助手
// ---------------------------------------------------------------------------

// WriteJSON 写一个 JSON 响应。
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		// 响应已经写出去了，再回错误也没用；记一条日志，否则「前端偶尔拿到半截 JSON」
		// 这类问题在服务端毫无痕迹。
		logx.Warnf("写 JSON 响应失败：%v", err)
	}
}

// WriteErr 写失败响应。body 里的 ok/code/error 三个字段是前端的唯一判据。
func WriteErr(w http.ResponseWriter, e *Error) {
	if e.Status == 0 {
		e.Status = 500
	}
	if e.Code == "" {
		e.Code = "internal"
	}
	WriteJSON(w, e.Status, map[string]any{"ok": false, "code": e.Code, "error": e.Message})
}

// Respond 写成功响应并返回 nil，让 handler 能写成 `return apierr.Respond(w, …)`。
func Respond(w http.ResponseWriter, v any) *Error {
	WriteJSON(w, http.StatusOK, v)
	return nil
}
