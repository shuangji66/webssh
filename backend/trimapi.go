package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// 飞牛开放 API 客户端（trim.file.*）
//
// 三条来自官方文档 / 参考实现的硬约束：
//   - **只能服务端调**：token 不下发到浏览器，前端要什么经 /api/trim/* 转一手。
//   - **token 每次从环境变量读**（TRIM_API_TOKEN）：系统重新拉起应用时会换新值，
//     持久化下来只会拿到过期的那个。
//   - **固定走 unix socket**（/var/run/trim_open_gateway_apiscope.socket），
//     允许环境变量覆盖，是为了在没有飞牛的环境里也能跑通链路。
// ---------------------------------------------------------------------------

const (
	// trimEnvToken 是系统注入的访问令牌所在的环境变量。
	trimEnvToken = "TRIM_API_TOKEN"
	// trimReqCheckUserACL 是我们要用的那个能力。
	trimReqCheckUserACL = "trim.file.checkUserACL"
	// trimTimeout 单次调用的上限：socket 在本机，正常是毫秒级；给足是为了网关卡住时
	// 不要把前端的弹窗一直吊着。
	trimTimeout = 15 * time.Second
)

// errTrimUnavailable 表示当前进程没跑在飞牛的网关环境里（缺 socket 或缺 token）。
var errTrimUnavailable = errors.New("当前应用不在飞牛系统的网关环境中运行")

// trimAPIError 是开放 API 返回的业务错误（code 非 0）。
type trimAPIError struct {
	Code int
	Msg  string
	HTTP int
}

func (e *trimAPIError) Error() string {
	msg := e.Msg
	if msg == "" {
		msg = "未提供错误信息"
	}
	if e.HTTP != 0 && e.HTTP != http.StatusOK {
		return fmt.Sprintf("飞牛接口调用失败（HTTP %d，code %d）：%s", e.HTTP, e.Code, msg)
	}
	return fmt.Sprintf("飞牛接口调用失败（code %d）：%s", e.Code, msg)
}

// trimStatus 是 /api/trim/status 与 RuntimeInfo.trim 的形态（契约第 4.3 节 TrimStatus）。
type trimStatus struct {
	Available bool   `json:"available"`
	Message   string `json:"message"`
	App       string `json:"app"`
}

// aclEntry 是 AclEntry（契约第 4.3 节）。
type aclEntry struct {
	Path      string `json:"path"`
	Readable  bool   `json:"readable"`
	Writable  bool   `json:"writable"`
	Deletable bool   `json:"deletable"`
}

type trimClient struct {
	renv *RuntimeEnv
	http *http.Client
}

func newTrimClient(renv *RuntimeEnv) *trimClient {
	return &trimClient{
		renv: renv,
		http: &http.Client{
			Timeout: trimTimeout,
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					var d net.Dialer
					return d.DialContext(ctx, "unix", renv.TrimSocket)
				},
			},
		},
	}
}

// probe 探一次可用性：socket 连得上、token 又读得到，才算可用。
//
// 不可用时**如实返回原因**（后端与前端都要显示给用户看），绝不假装成功：
// 「文件管理里选不了飞牛文件」的原因必须能从界面上一眼看出来。
func (c *trimClient) probe(ctx context.Context) trimStatus {
	st := trimStatus{App: c.renv.TrimApp}
	if _, err := os.Stat(c.renv.TrimSocket); err != nil {
		st.Message = "找不到飞牛开放 API 的 Socket（" + c.renv.TrimSocket + "），当前不在飞牛应用里运行"
		return st
	}
	conn, err := (&net.Dialer{Timeout: 2 * time.Second}).DialContext(ctx, "unix", c.renv.TrimSocket)
	if err != nil {
		st.Message = "连不上飞牛开放 API 的 Socket：" + err.Error()
		return st
	}
	_ = conn.Close()
	if strings.TrimSpace(os.Getenv(trimEnvToken)) == "" {
		st.Message = "没有拿到 " + trimEnvToken + "，请从飞牛应用中心启动本应用"
		return st
	}
	st.Available = true
	return st
}

func (c *trimClient) available(ctx context.Context) bool {
	return c.probe(ctx).Available
}

type trimRequest struct {
	ReqID   string `json:"reqId"`
	Req     string `json:"req"`
	AppName string `json:"appName"`
	Data    any    `json:"data"`
}

type trimResponse struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

// checkUserACL 查询当前用户对若干本机路径的权限。
func (c *trimClient) checkUserACL(ctx context.Context, uid int, paths []string) ([]aclEntry, error) {
	if len(paths) == 0 {
		return []aclEntry{}, nil
	}
	raw, err := c.call(ctx, trimReqCheckUserACL, map[string]any{"uid": uid, "path": paths})
	if err != nil {
		return nil, err
	}
	var out []aclEntry
	if len(bytes.TrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, &out); err != nil {
			return nil, &trimAPIError{Msg: "ACL 响应格式不符合预期：" + err.Error()}
		}
	}
	if out == nil {
		out = []aclEntry{}
	}
	return out, nil
}

// call 调一次开放 API，返回 data 原文（不同系统版本的返回结构可能不一样，
// 先拿到原文再自己解比钉死一个结构稳）。
func (c *trimClient) call(ctx context.Context, req string, data any) (json.RawMessage, error) {
	token := strings.TrimSpace(os.Getenv(trimEnvToken))
	if token == "" {
		return nil, fmt.Errorf("%w：没有读到环境变量 %s", errTrimUnavailable, trimEnvToken)
	}
	if data == nil {
		data = map[string]any{}
	}
	body, err := json.Marshal(trimRequest{
		ReqID:   fmt.Sprintf("%d", time.Now().UnixNano()),
		Req:     req,
		AppName: c.renv.TrimApp,
		Data:    data,
	})
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://localhost/api/v1/trimapp", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+token)

	res, err := c.http.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("%w（%s）：%v", errTrimUnavailable, c.renv.TrimSocket, err)
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, 8<<20))
	if err != nil {
		return nil, fmt.Errorf("读取飞牛接口响应失败：%w", err)
	}
	var resp trimResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, &trimAPIError{HTTP: res.StatusCode, Msg: "响应不是合法 JSON：" + trimSnippet(raw)}
	}
	if resp.Code != 0 || res.StatusCode != http.StatusOK {
		return nil, &trimAPIError{Code: resp.Code, Msg: resp.Msg, HTTP: res.StatusCode}
	}
	return resp.Data, nil
}

func trimSnippet(b []byte) string {
	s := strings.Join(strings.Fields(string(b)), " ")
	if len(s) > 200 {
		return s[:200] + "…"
	}
	return s
}

// trimError 把开放 API 的失败翻成 HTTP 错误响应。
func trimError(err error) *apiError {
	if err == nil {
		return nil
	}
	if errors.Is(err, errTrimUnavailable) {
		return errUnavailable("%v", err)
	}
	var apiErr *trimAPIError
	if errors.As(err, &apiErr) {
		// code 1000002 是「管理员权限或 API Scope 不足」：写成明确的一句话，
		// 否则前端只能看到一个数字，排查方向完全没有。
		if apiErr.Code == 1000002 {
			return errPermission("%v（请确认应用包已在 config/resource 声明对应 api-scope）", err)
		}
		return errInvalid("%v", err)
	}
	return errInternal("调用飞牛接口失败：%v", err)
}

// ---------------------------------------------------------------------------
// /api/trim/*
// ---------------------------------------------------------------------------

// handleTrimStatus 返回飞牛宿主是否可用（设置弹窗里要显示原因）。
func (m *AdminMux) handleTrimStatus(w http.ResponseWriter, r *http.Request) *apiError {
	return respond(w, map[string]any{"ok": true, "trim": m.trim.probe(r.Context())})
}

// handleTrimACL 检查当前用户对若干本机路径的权限。
//
// 身份取网关转发的 X-Trim-Userid（没有网关时退化成进程自己的 uid）：请求体里带的
// uid 一律不认——那是客户端可控的输入。
func (m *AdminMux) handleTrimACL(w http.ResponseWriter, r *http.Request) *apiError {
	var req struct {
		Paths []string `json:"paths"`
	}
	if apiErr := m.decodeJSON(w, r, &req); apiErr != nil {
		return apiErr
	}
	u := gatewayUserOf(r)
	list, err := m.trim.checkUserACL(r.Context(), u.UID, req.Paths)
	if err != nil {
		return trimError(err)
	}
	return respond(w, map[string]any{"ok": true, "list": list})
}
