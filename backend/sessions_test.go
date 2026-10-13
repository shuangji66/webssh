package main

import (
	"encoding/base64"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"webssh/backend/internal/secret"
	"webssh/backend/internal/store"
	"webssh/backend/internal/testssh"
)

// TestSessionSaveValidation 覆盖保存会话的参数校验与身份去重。
func TestSessionSaveValidation(t *testing.T) {
	a := newTestApp(t, appOptions{noSSH: true})

	out := a.ok(t, "/api/sessions/save", map[string]any{
		"name": "nas", "host": "10.0.0.1", "port": 2222, "user": "root",
		"authType": "password", "password": "pw", "note": "机房",
	})
	view, _ := out["session"].(map[string]any)
	sid := strOf(view, "id")
	if len(sid) != 32 {
		t.Fatalf("id = %q，期望 %d 位十六进制", sid, 32)
	}
	if strOf(view, "name") != "nas" || strOf(view, "host") != "10.0.0.1" || numOf(view, "port") != 2222 ||
		strOf(view, "user") != "root" || strOf(view, "authType") != "password" ||
		view["hasPassword"] != true || view["hasPrivateKey"] != false {
		t.Fatalf("SessionView 字段不对：%v", view)
	}
	if _, ok := view["lastUsedAt"]; ok {
		t.Errorf("从未连接过的会话不该有 lastUsedAt：%v", view)
	}
	if _, err := time.Parse(time.RFC3339, strOf(view, "createdAt")); err != nil {
		t.Errorf("createdAt 不是 RFC3339：%v", view["createdAt"])
	}
	if _, err := time.Parse(time.RFC3339, strOf(view, "updatedAt")); err != nil {
		t.Errorf("updatedAt 不是 RFC3339：%v", view["updatedAt"])
	}

	// 同一身份（user@host:port）只允许一个会话。
	a.fail(t, "/api/sessions/save", map[string]any{
		"host": "10.0.0.1", "port": 2222, "user": "root", "authType": "password", "password": "pw",
	}, "duplicate", http.StatusConflict)
	// 端口不同 = 不同身份。
	a.ok(t, "/api/sessions/save", map[string]any{
		"host": "10.0.0.1", "port": 22, "user": "root", "authType": "password", "password": "pw",
	})
	// 登录名不同 = 不同身份。
	a.ok(t, "/api/sessions/save", map[string]any{
		"host": "10.0.0.1", "port": 2222, "user": "admin", "authType": "password", "password": "pw",
	})

	// 参数校验
	a.fail(t, "/api/sessions/save", map[string]any{"host": "", "user": "root", "authType": "password", "password": "x"}, "invalid", http.StatusBadRequest)
	a.fail(t, "/api/sessions/save", map[string]any{"host": "h", "user": "", "authType": "password", "password": "x"}, "invalid", 0)
	a.fail(t, "/api/sessions/save", map[string]any{"host": "h", "user": "u", "port": 70000, "authType": "password", "password": "x"}, "invalid", 0)
	a.fail(t, "/api/sessions/save", map[string]any{"host": "h", "user": "u", "authType": "agent", "password": "x"}, "invalid", 0)
	a.fail(t, "/api/sessions/save", map[string]any{"host": "a/b", "user": "u", "authType": "password", "password": "x"}, "invalid", 0)
	a.fail(t, "/api/sessions/save", map[string]any{"host": "h", "user": "u@x", "authType": "password", "password": "x"}, "invalid", 0)
	// 新建会话必须给凭据（否则存下来就是一个永远打不开的卡片）。
	a.fail(t, "/api/sessions/save", map[string]any{"host": "h2", "user": "u", "authType": "password"}, "no_credential", http.StatusBadRequest)
	a.fail(t, "/api/sessions/save", map[string]any{"host": "h3", "user": "u", "authType": "key"}, "no_credential", http.StatusBadRequest)
	// 私钥解析不了要当场拒绝（存下来只会得到「永远连不上」的会话）。
	a.fail(t, "/api/sessions/save", map[string]any{"host": "h4", "user": "u", "authType": "key", "privateKey": "not-a-key"}, "key_parse", http.StatusBadRequest)

	// 端口缺省 = 22
	out = a.ok(t, "/api/sessions/save", map[string]any{"host": "h5", "user": "u", "authType": "password", "password": "x"})
	if numOf(out["session"], "port") != 22 {
		t.Errorf("端口缺省应为 22：%v", out["session"])
	}
	// 名称缺省 = user@host
	if name := strOf(out["session"], "name"); name != "u@h5" {
		t.Errorf("名称缺省应为 u@h5，实际 %q", name)
	}

	// 更新：同 id 改名字不算重复
	upd := a.ok(t, "/api/sessions/save", map[string]any{
		"id": sid, "name": "nas2", "host": "10.0.0.1", "port": 2222, "user": "root", "authType": "password",
	})
	if strOf(upd["session"], "name") != "nas2" {
		t.Errorf("更新后名称 = %v", upd["session"])
	}
	if strOf(upd["session"], "createdAt") != strOf(view, "createdAt") {
		t.Errorf("更新不该改 createdAt")
	}
	// 更新撞身份
	other := a.ok(t, "/api/sessions/save", map[string]any{"host": "10.0.0.9", "user": "root", "authType": "password", "password": "x"})
	a.fail(t, "/api/sessions/save", map[string]any{
		"id": strOf(other["session"], "id"), "host": "10.0.0.1", "port": 2222, "user": "root",
		"authType": "password", "password": "x",
	}, "duplicate", 0)
	a.fail(t, "/api/sessions/save", map[string]any{"id": "0123456789abcdef0123456789abcdef", "host": "z", "user": "u", "authType": "password", "password": "x"}, "not_found", 0)

	// 删除
	a.ok(t, "/api/sessions/delete", map[string]any{"id": sid})
	a.fail(t, "/api/sessions/delete", map[string]any{"id": sid}, "not_found", http.StatusNotFound)

	_, list := a.get(t, "/api/sessions")
	sessions, _ := list["sessions"].([]any)
	if len(sessions) != 4 {
		t.Fatalf("会话数 = %d，期望 4：%v", len(sessions), sessions)
	}
}

// TestCredentialEncryptionAtRest 校验凭据以密文落盘、解密可往返、AAD 绑定字段与会话。
func TestCredentialEncryptionAtRest(t *testing.T) {
	a := newTestApp(t, appOptions{noSSH: true})
	kp, err := testssh.NewKeyPair()
	if err != nil {
		t.Fatalf("生成密钥失败：%v", err)
	}
	pemText, err := kp.PEM()
	if err != nil {
		t.Fatalf("导出私钥失败：%v", err)
	}

	const plainPassword = "s3cret-password-9f3a"
	const plainPassphrase = "passphrase-c0ffee"
	out := a.ok(t, "/api/sessions/save", map[string]any{
		"host": "10.0.0.1", "user": "root", "authType": "key",
		"privateKey": pemText, "passphrase": plainPassphrase, "password": plainPassword,
	})
	view, _ := out["session"].(map[string]any)
	sid := strOf(view, "id")
	if view["hasPrivateKey"] != true {
		t.Fatalf("hasPrivateKey 应为 true：%v", view)
	}
	wantFP := ssh.FingerprintSHA256(kp.PublicKey())
	if got := strOf(view, "keyFingerprint"); got != wantFP {
		t.Errorf("keyFingerprint = %q，期望 %q", got, wantFP)
	}

	// master.key：32 字节、0600。
	keyPath := filepath.Join(a.renv.DataDir, "master.key")
	st, err := os.Stat(keyPath)
	if err != nil {
		t.Fatalf("master.key 不存在：%v", err)
	}
	if perm := st.Mode().Perm(); perm != 0o600 {
		t.Errorf("master.key 权限 = %o，期望 600", perm)
	}
	key, err := os.ReadFile(keyPath)
	if err != nil || len(key) != 32 {
		t.Fatalf("master.key 长度 = %d（err=%v），期望 %d", len(key), err, 32)
	}

	// 落盘内容里不能有明文。
	raw, err := os.ReadFile(filepath.Join(a.renv.DataDir, "sessions", sid+".json"))
	if err != nil {
		t.Fatalf("读取会话文件失败：%v", err)
	}
	for _, plain := range []string{plainPassword, plainPassphrase, "OPENSSH PRIVATE KEY", pemText} {
		if strings.Contains(string(raw), plain) {
			t.Errorf("会话文件里出现了明文凭据 %q：%s", plain, snippetOf(raw))
		}
	}
	var sc store.Session
	if err := json.Unmarshal(raw, &sc); err != nil {
		t.Fatalf("会话文件不是合法 JSON：%v", err)
	}
	if sc.Password == nil || sc.Password.V != 1 || sc.Password.CT == "" {
		t.Fatalf("password 字段形态不对：%+v", sc.Password)
	}

	// 解密往返
	password, privateKey, passphrase, apiErr := a.mux.sessions.credentials(&sc)
	if apiErr != nil {
		t.Fatalf("解密失败：%v", apiErr)
	}
	if password != plainPassword || privateKey != pemText || passphrase != plainPassphrase {
		t.Fatalf("解密结果不对：%q / %q / %q", password, privateKey, passphrase)
	}
	// AAD 绑定：把密文搬到另一个字段 / 另一个会话必须解不开（不能悄悄用一个错误的凭据）。
	if _, err := a.mux.crypt.Decrypt(secret.FieldPrivateKey, sid, sc.Password); err == nil {
		t.Error("密码密文搬到 privateKey 字段竟然能解开")
	}
	if _, err := a.mux.crypt.Decrypt(secret.FieldPassword, "another-session", sc.Password); err == nil {
		t.Error("密码密文搬到别的会话竟然能解开")
	}
	if _, err := a.mux.crypt.Decrypt(secret.FieldPassword, sid, &secret.Secret{V: 1, CT: "AAAA"}); err == nil {
		t.Error("乱码密文竟然能解开")
	}
	// 信封版本不支持时也要报错
	if _, err := a.mux.crypt.Decrypt(secret.FieldPassword, sid, &secret.Secret{V: 99, CT: sc.Password.CT}); err == nil {
		t.Error("不支持的版本号应当报错")
	}
}

// TestMasterKeyFromEnv 校验 WEBSSH_MASTER_KEY 外部提供主密钥（数据目录里不落密钥文件）。
func TestMasterKeyFromEnv(t *testing.T) {
	raw := make([]byte, 32)
	for i := range raw {
		raw[i] = byte(i)
	}
	a := newTestApp(t, appOptions{noSSH: true, masterKeyEnv: base64.StdEncoding.EncodeToString(raw)})
	if _, err := os.Stat(filepath.Join(a.renv.DataDir, "master.key")); err == nil {
		t.Fatal("用环境变量提供主密钥时不该再生成 master.key")
	}
	out := a.ok(t, "/api/sessions/save", map[string]any{"host": "h", "user": "u", "authType": "password", "password": "pw"})
	sid := strOf(out["session"], "id")
	sc, err := a.mux.st.LoadSession(sid)
	if err != nil {
		t.Fatalf("读取会话失败：%v", err)
	}
	password, _, _, apiErr := a.mux.sessions.credentials(sc)
	if apiErr != nil || password != "pw" {
		t.Fatalf("用环境变量密钥解密失败：%v %q", apiErr, password)
	}

	// 解不出来的密钥必须让启动失败（静默回退到本地文件会表现成「密码莫名失效」）。
	renv := &RuntimeEnv{AdminSock: filepath.Join(t.TempDir(), "s.sock"), DataDir: t.TempDir(), MasterKeyEnv: "not-a-key"}
	if _, err := newAdminMux(renv); err == nil {
		t.Fatal("WEBSSH_MASTER_KEY 不合法时应当初始化失败")
	}
	// 长度不对的密钥同样拒绝
	renv.MasterKeyEnv = base64.StdEncoding.EncodeToString([]byte("short"))
	if _, err := newAdminMux(renv); err == nil {
		t.Fatal("长度不对的 WEBSSH_MASTER_KEY 应当被拒绝")
	}
}

// TestSessionInputSemantics 校验「字段缺省 = 不修改、空字符串 = 清除」。
func TestSessionInputSemantics(t *testing.T) {
	a := newTestApp(t, appOptions{noSSH: true})
	out := a.ok(t, "/api/sessions/save", map[string]any{
		"host": "h", "user": "u", "authType": "password", "password": "first", "note": "n1",
	})
	sid := strOf(out["session"], "id")

	// 不带 password 字段 → 保留原密码
	upd := a.ok(t, "/api/sessions/save", map[string]any{
		"id": sid, "host": "h", "user": "u", "authType": "password", "name": "x", "note": "",
	})
	if upd["session"].(map[string]any)["hasPassword"] != true {
		t.Fatalf("字段缺省时应当保留已存密码：%v", upd["session"])
	}
	// note 给了空字符串 → 清除
	if strOf(upd["session"], "note") != "" {
		t.Fatalf("note 传空串应清空：%v", upd["session"])
	}
	// name 给了空字符串 → 清除自定义名（前端会退回 user@host 展示）。
	// 第六轮修：原来那版把空串当成「保持原值」，用户清空名称保存后界面上还是旧名字。
	upd = a.ok(t, "/api/sessions/save", map[string]any{"id": sid, "name": ""})
	if strOf(upd["session"], "name") != "" {
		t.Fatalf("name 传空串应清空：%v", upd["session"])
	}
	// 字段缺省（压根不发 name）→ 保持原值（上面刚清空，这里不该被塞回默认名）
	upd = a.ok(t, "/api/sessions/save", map[string]any{"id": sid, "note": "n2"})
	if strOf(upd["session"], "name") != "" {
		t.Fatalf("缺省 name 不该改名字：%v", upd["session"])
	}
	// password 给了空字符串 → 清除（契约第 4.3 节：空串 = 清除该字段）
	upd = a.ok(t, "/api/sessions/save", map[string]any{
		"id": sid, "host": "h", "user": "u", "authType": "password", "password": "",
	})
	if upd["session"].(map[string]any)["hasPassword"] != false {
		t.Fatalf("password 传空串应清除：%v", upd["session"])
	}
	sc, err := a.mux.st.LoadSession(sid)
	if err != nil {
		t.Fatalf("读取会话失败：%v", err)
	}
	password, _, _, _ := a.mux.sessions.credentials(sc)
	if password != "" {
		t.Fatalf("清除后不该还能解出密码：%q", password)
	}
}

// TestSessionPartialUpdateWhileOpen 是「部分字段更新已打开会话」的回归测试。
//
// 修复前的症状（端口不是 22 的会话必现）：`POST /api/sessions/save` 只发
// `{id, name, host, user, authType}`（不带 port）时，缺省的 port=0 被归一成 22 再与现值
// 比较，被判成「改了身份」→ 已打开的会话直接回 409 conflict「请先关闭再修改地址 / 端口 /
// 登录名」。端口恰好是 22 的会话会蒙对，所以这个坑藏得很深。
//
// 现在更新语义是：空串 / 0 / 缺省 = 保留原值（见 resolveSessionFields）。
func TestSessionPartialUpdateWhileOpen(t *testing.T) {
	a := newTestApp(t, appOptions{})
	sid := a.newSSHSession(t)
	port := a.ssh.Port()
	if port == 22 {
		t.Fatalf("内嵌 SSH 服务器不该占用 22 端口（本用例要验非 22 端口）：%d", port)
	}

	// 1) 只发 id + name：必须成功，且身份字段一字不差
	out := a.ok(t, "/api/sessions/save", map[string]any{"id": sid, "name": "改名后的会话"})
	view, _ := out["session"].(map[string]any)
	if strOf(view, "name") != "改名后的会话" {
		t.Fatalf("改名没生效：%v", view)
	}
	checkIdentity := func(where string, view map[string]any) {
		t.Helper()
		if strOf(view, "host") != a.ssh.Host() || numOf(view, "port") != float64(port) ||
			strOf(view, "user") != a.sshUser || strOf(view, "authType") != "password" {
			t.Fatalf("%s：身份字段被部分更新改掉了：%v", where, view)
		}
		if view["hasPassword"] != true || view["credentialSaved"] != true {
			t.Fatalf("%s：凭据状态不该变：%v", where, view)
		}
	}
	checkIdentity("改名后", view)

	// 2) 会话必须还是开着的（身份没变，不该把运行时丢掉）
	_, sessions := a.get(t, "/api/sessions")
	open := false
	for _, item := range listOf(sessions, "open") {
		m, _ := item.(map[string]any)
		if strOf(m, "id") == sid && m["open"] == true {
			open = true
		}
	}
	if !open {
		t.Fatalf("只改名字不该影响已打开的会话：%v", sessions["open"])
	}

	// 3) 落盘的配置也必须是原身份（并入内存态是同一份值）
	sc, err := a.mux.st.LoadSession(sid)
	if err != nil {
		t.Fatalf("读取会话失败：%v", err)
	}
	if sc.Host != a.ssh.Host() || sc.Port != port || sc.User != a.sshUser {
		t.Fatalf("落盘的身份不对：%s@%s:%d", sc.User, sc.Host, sc.Port)
	}

	// 4) 老调用方发空串（host/user 空、port 0）同样按「保留」处理
	view, _ = a.ok(t, "/api/sessions/save", map[string]any{
		"id": sid, "name": "再改一次", "host": "", "user": "", "port": 0, "authType": "",
	})["session"].(map[string]any)
	checkIdentity("空字段更新后", view)

	// 5) 显式改了端口 → 仍然是 409（会话开着时不许换连接目标）
	a.fail(t, "/api/sessions/save", map[string]any{
		"id": sid, "name": "x", "host": a.ssh.Host(), "port": port + 1,
		"user": a.sshUser, "authType": "password",
	}, "conflict", http.StatusConflict)
	// 显式改了地址 / 登录名同理
	a.fail(t, "/api/sessions/save", map[string]any{
		"id": sid, "name": "x", "host": "192.0.2.1", "port": port, "user": a.sshUser, "authType": "password",
	}, "conflict", http.StatusConflict)
	a.fail(t, "/api/sessions/save", map[string]any{
		"id": sid, "name": "x", "host": a.ssh.Host(), "port": port, "user": "other", "authType": "password",
	}, "conflict", http.StatusConflict)

	// 6) 关闭之后显式改端口就能改（顺序：先关再改，改完还是同一个会话）
	a.ok(t, "/api/sessions/close", map[string]any{"id": sid})
	view, _ = a.ok(t, "/api/sessions/save", map[string]any{
		"id": sid, "name": "x", "host": a.ssh.Host(), "port": port + 1, "user": a.sshUser, "authType": "password",
	})["session"].(map[string]any)
	if numOf(view, "port") != float64(port+1) {
		t.Fatalf("关闭后改端口没生效：%v", view)
	}
	// 新建会话时 port 缺省仍然是 22
	out = a.ok(t, "/api/sessions/save", map[string]any{
		"host": "example.invalid", "user": "u", "authType": "password", "password": "p",
	})
	if numOf(out["session"], "port") != 22 {
		t.Fatalf("新建会话的缺省端口应当是 22：%v", out["session"])
	}
}

// TestOpenCloseLifecycle 校验打开/关闭的幂等性与语义。
func TestOpenCloseLifecycle(t *testing.T) {
	requirePTY(t)
	a := newTestApp(t, appOptions{})
	sid := a.newSSHSession(t)

	// 打开后：配置里有、实时状态里有。
	_, list := a.get(t, "/api/sessions")
	if len(listOf(list, "sessions")) != 1 {
		t.Fatalf("会话列表 = %v", list["sessions"])
	}
	open := listOf(list, "open")
	if len(open) != 1 {
		t.Fatalf("已打开会话 = %v", list["open"])
	}
	state, _ := open[0].(map[string]any)
	if strOf(state, "id") != sid || state["open"] != true {
		t.Fatalf("SessionState 不对：%v", state)
	}
	if _, err := time.Parse(time.RFC3339, strOf(state, "openedAt")); err != nil {
		t.Errorf("openedAt 不是 RFC3339：%v", state["openedAt"])
	}
	if state["sftpOpen"] != true {
		t.Errorf("打开会话时 SFTP 通道应当已建立：%v", state)
	}
	if n := len(listOf(state, "terminals")); n != 0 {
		t.Errorf("新打开的会话不该有终端：%v", state)
	}
	if used := strOf(listOf(list, "sessions")[0].(map[string]any), "lastUsedAt"); used == "" {
		t.Error("打开会话后应写 lastUsedAt")
	}

	// 幂等：再打开一次不会重建（openedAt 不变）
	out := a.ok(t, "/api/sessions/open", map[string]any{"id": sid})
	state2, _ := out["state"].(map[string]any)
	if strOf(state2, "openedAt") != strOf(state, "openedAt") {
		t.Errorf("重复打开不应重建会话：%v vs %v", state2["openedAt"], state["openedAt"])
	}

	// 建一个终端后再 open，终端列表必须还在（幂等 = 不重建终端）。
	ws := a.dialWS(t, sid, "")
	tid := waitForTID(t, ws)
	ws.mustCollectUntil("$ ", testTimeout)
	a.ok(t, "/api/sessions/open", map[string]any{"id": sid})
	_, list = a.get(t, "/api/sessions")
	state3, _ := listOf(list, "open")[0].(map[string]any)
	terms := listOf(state3, "terminals")
	if len(terms) != 1 || strOf(terms[0].(map[string]any), "id") != tid {
		t.Fatalf("重复打开后终端丢了：%v", state3)
	}
	ws.close()

	// 关闭：实时状态清空，配置保留。
	a.ok(t, "/api/sessions/close", map[string]any{"id": sid})
	_, list = a.get(t, "/api/sessions")
	if len(listOf(list, "open")) != 0 {
		t.Fatalf("关闭后不该还有已打开会话：%v", list["open"])
	}
	if len(listOf(list, "sessions")) != 1 {
		t.Fatalf("关闭不该删除配置：%v", list["sessions"])
	}
	// 关闭后再做文件操作 → not_open（i18n 里有 err_not_open）
	a.fail(t, "/api/fs/list", map[string]any{"sid": sid, "path": ""}, "not_open", http.StatusConflict)
	// 关闭未知会话 → not_found；重复关闭是幂等的
	a.fail(t, "/api/sessions/close", map[string]any{"id": "0123456789abcdef"}, "not_found", 0)
	a.ok(t, "/api/sessions/close", map[string]any{"id": sid})
	// 所有终端的历史目录必须清掉（契约：会话关闭时删除整目录）
	if _, err := os.Stat(filepath.Join(a.renv.DataDir, "history", sid)); !os.IsNotExist(err) {
		t.Errorf("会话关闭后历史目录应当被删除：%v", err)
	}
	// 配置还在，可以再次打开
	a.ok(t, "/api/sessions/open", map[string]any{"id": sid})
	a.ok(t, "/api/sessions/close", map[string]any{"id": sid})
	// 删除会话：配置、专属快捷指令、历史都不留
	a.ok(t, "/api/quickcmds/save", map[string]any{"sid": sid, "commands": []any{map[string]any{"name": "n", "content": "c", "auto": false}}})
	a.ok(t, "/api/sessions/delete", map[string]any{"id": sid})
	if _, err := os.Stat(filepath.Join(a.renv.DataDir, "quickcmds", sid+".json")); !os.IsNotExist(err) {
		t.Errorf("删除会话应同时删掉专属快捷指令：%v", err)
	}
}

// TestSessionsSurviveRestart 校验「配置活过重启」（内存态会话则消失）。
func TestSessionsSurviveRestart(t *testing.T) {
	a := newTestApp(t, appOptions{})
	sid := a.newSSHSession(t)

	_, before := a.get(t, "/api/info")
	bootBefore := strOf(before["runtime"], "bootId")

	b := a.restart(t)
	_, after := b.get(t, "/api/info")
	if strOf(after["runtime"], "bootId") == bootBefore {
		t.Error("重启后 bootId 必须变（前端靠它清掉失效的会话标签）")
	}
	if len(strOf(after["runtime"], "bootId")) != 32 {
		t.Errorf("重启用例里的 bootId 形态不对：%v", after["runtime"])
	}
	_, list := b.get(t, "/api/sessions")
	sessions := listOf(list, "sessions")
	if len(sessions) != 1 {
		t.Fatalf("重启后配置应当还在：%v", list)
	}
	if sessions[0].(map[string]any)["hasPassword"] != true {
		t.Fatalf("重启后密码应当仍可解密：%v", sessions[0])
	}
	if len(listOf(list, "open")) != 0 {
		t.Errorf("重启后不该有「已打开」的会话（内存态）：%v", list["open"])
	}
	// 重新打开：主机密钥已在 known_hosts 里（不需要再信任）
	out := b.ok(t, "/api/sessions/open", map[string]any{"id": sid})
	if out["state"].(map[string]any)["open"] != true {
		t.Fatalf("重启后重新打开失败：%v", out)
	}
}

// TestConnectTestOutcomes 覆盖连接测试的全部结局（契约第 5.8 条）。
func TestConnectTestOutcomes(t *testing.T) {
	a := newTestApp(t, appOptions{})
	target := func(extra map[string]any) map[string]any {
		body := map[string]any{
			"host": a.ssh.Host(), "port": a.ssh.Port(), "user": "demo",
			"authType": "password", "password": "demo",
		}
		for k, v := range extra {
			body[k] = v
		}
		return body
	}
	test := func(body map[string]any) map[string]any {
		t.Helper()
		res, out := a.post(t, "/api/sessions/test", body)
		if res.StatusCode != http.StatusOK {
			t.Fatalf("连接测试应当始终返回 200 + outcome（实际 %d：%v）", res.StatusCode, out)
		}
		if out["ok"] != true {
			t.Fatalf("连接测试的 ok 应为 true：%v", out)
		}
		result, _ := out["result"].(map[string]any)
		if result == nil {
			t.Fatalf("缺少 result：%v", out)
		}
		return result
	}

	// 1) 首次连接：未知主机密钥，必须带上指纹与类型（前端要展示给用户确认）
	r := test(target(nil))
	if r["success"] != false || r["code"] != "hostkey_unknown" {
		t.Fatalf("首次连接结果 = %v", r)
	}
	if strOf(r, "fingerprint") != a.ssh.Fingerprint() || strOf(r, "keyType") != a.ssh.KeyType() {
		t.Fatalf("首次连接必须带指纹与密钥类型：%v", r)
	}
	if strOf(r, "stage") != "hostkey" {
		t.Errorf("stage = %v，期望 hostkey", r["stage"])
	}

	// 指纹对不上不能信任
	a.fail(t, "/api/sessions/trust", map[string]any{"host": a.ssh.Host(), "port": a.ssh.Port(), "fingerprint": "SHA256:wrong"}, "not_found", http.StatusNotFound)
	a.fail(t, "/api/sessions/trust", map[string]any{"host": "", "port": a.ssh.Port(), "fingerprint": "SHA256:x"}, "invalid", 0)

	// 2) 信任后：成功
	a.ok(t, "/api/sessions/trust", map[string]any{"host": a.ssh.Host(), "port": a.ssh.Port(), "fingerprint": a.ssh.Fingerprint()})
	r = test(target(nil))
	if r["success"] != true || r["code"] != "ok" {
		t.Fatalf("信任后连接测试应成功：%v", r)
	}
	if r["trusted"] != true || numOf(r, "latencyMs") <= 0 {
		t.Errorf("成功结果应带 trusted 与 latencyMs：%v", r)
	}
	// 信任是一次性动作：重复点击不该再次写入
	a.fail(t, "/api/sessions/trust", map[string]any{"host": a.ssh.Host(), "port": a.ssh.Port(), "fingerprint": a.ssh.Fingerprint()}, "not_found", 0)

	// 3) 认证失败
	r = test(target(map[string]any{"password": "wrong"}))
	if r["success"] != false || r["code"] != "auth_failed" || strOf(r, "stage") != "auth" {
		t.Fatalf("密码错误的结果 = %v", r)
	}
	// 4) 没有凭据
	r = test(map[string]any{"host": a.ssh.Host(), "port": a.ssh.Port(), "user": "demo", "authType": "password"})
	if r["code"] != "no_credential" {
		t.Fatalf("缺凭据的结果 = %v", r)
	}
	// 5) 私钥解析失败
	r = test(target(map[string]any{"authType": "key", "password": nil, "privateKey": "-----BEGIN OPENSSH PRIVATE KEY-----\nbad\n-----END OPENSSH PRIVATE KEY-----"}))
	if r["code"] != "key_parse" {
		t.Fatalf("坏私钥的结果 = %v", r)
	}
	// 6) 参数不合法仍然是 4xx
	a.fail(t, "/api/sessions/test", map[string]any{"host": "", "user": "demo", "authType": "password", "password": "x"}, "invalid", http.StatusBadRequest)

	// 7) 端口没人监听 → dial_refused
	closedPort := freeClosedPort(t)
	r = test(map[string]any{"host": "127.0.0.1", "port": closedPort, "user": "demo", "authType": "password", "password": "x"})
	if r["code"] != "dial_refused" {
		t.Fatalf("关闭端口的结果 = %v", r)
	}
	// 8) 域名解析失败
	r = test(map[string]any{"host": "webssh-does-not-exist.invalid", "port": 22, "user": "demo", "authType": "password", "password": "x"})
	if r["code"] != "dns" {
		t.Fatalf("域名解析失败的结果 = %v", r)
	}
}

// TestConnectAuthUnsupported 校验「服务器不接受该认证方式」与 auth_failed 区分开。
func TestConnectAuthUnsupported(t *testing.T) {
	kp, err := testssh.NewKeyPair()
	if err != nil {
		t.Fatalf("生成密钥失败：%v", err)
	}
	// 只允许公钥认证的服务器：用密码去连必然被拒，而且客户端连一个可用的方式都没有。
	a := newTestApp(t, appOptions{sshOptions: &testssh.Options{
		User: "demo", AuthorizedKeys: []ssh.PublicKey{kp.PublicKey()},
	}})
	body := map[string]any{
		"host": a.ssh.Host(), "port": a.ssh.Port(), "user": "demo", "authType": "password", "password": "demo",
	}
	// 主机密钥回调在认证**之前**跑，所以要先信任主机密钥才能看到认证阶段的结论。
	res, out := a.post(t, "/api/sessions/test", body)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d", res.StatusCode)
	}
	r, _ := out["result"].(map[string]any)
	if r["code"] != "hostkey_unknown" {
		t.Fatalf("第一次连接应当是「未知主机密钥」：%v", r)
	}
	a.ok(t, "/api/sessions/trust", map[string]any{"host": a.ssh.Host(), "port": a.ssh.Port(), "fingerprint": r["fingerprint"]})

	_, out = a.post(t, "/api/sessions/test", body)
	r, _ = out["result"].(map[string]any)
	if r["code"] != "auth_unsupported" {
		t.Fatalf("结果 = %v，期望 auth_unsupported（服务器只允许公钥）", r)
	}
}

// TestKeyAuthVariants 覆盖密钥认证（含带口令的私钥）。
func TestKeyAuthVariants(t *testing.T) {
	kp, err := testssh.NewKeyPair()
	if err != nil {
		t.Fatalf("生成密钥失败：%v", err)
	}
	plain, err := kp.PEM()
	if err != nil {
		t.Fatalf("导出私钥失败：%v", err)
	}
	encrypted, err := kp.PEMWithPassphrase("secret")
	if err != nil {
		t.Fatalf("导出带口令私钥失败：%v", err)
	}
	a := newTestApp(t, appOptions{sshOptions: &testssh.Options{
		User: "demo", AuthorizedKeys: []ssh.PublicKey{kp.PublicKey()},
	}})

	target := func(key, passphrase string) map[string]any {
		body := map[string]any{
			"host": a.ssh.Host(), "port": a.ssh.Port(), "user": "demo", "authType": "key", "privateKey": key,
		}
		if passphrase != "" {
			body["passphrase"] = passphrase
		}
		return body
	}
	// 第一次测试：未知主机密钥（先拿到指纹与密钥类型，再信任）
	_, out := a.post(t, "/api/sessions/test", target(plain, ""))
	first, _ := out["result"].(map[string]any)
	if first["code"] != "hostkey_unknown" || strOf(first, "fingerprint") != a.ssh.Fingerprint() {
		t.Fatalf("首次测试应当要求信任主机密钥：%v", first)
	}
	a.ok(t, "/api/sessions/trust", map[string]any{"host": a.ssh.Host(), "port": a.ssh.Port(), "fingerprint": first["fingerprint"]})

	// 未加密私钥
	_, out = a.post(t, "/api/sessions/test", target(plain, ""))
	if r, _ := out["result"].(map[string]any); r["success"] != true {
		t.Fatalf("私钥认证应成功：%v", r)
	}
	// 带口令私钥：口令对 → 成功；口令错 → key_parse
	_, out = a.post(t, "/api/sessions/test", target(encrypted, "secret"))
	if r, _ := out["result"].(map[string]any); r["success"] != true {
		t.Fatalf("带口令私钥认证应成功：%v", r)
	}
	_, out = a.post(t, "/api/sessions/test", target(encrypted, "wrong"))
	if r, _ := out["result"].(map[string]any); r["code"] != "key_parse" {
		t.Fatalf("口令错误应报 key_parse：%v", r)
	}
	// 存会话后打开：密钥指纹要回填
	saved := a.ok(t, "/api/sessions/save", map[string]any{
		"name": "keyed", "host": a.ssh.Host(), "port": a.ssh.Port(), "user": "demo",
		"authType": "key", "privateKey": plain,
	})
	view, _ := saved["session"].(map[string]any)
	if strOf(view, "keyFingerprint") != ssh.FingerprintSHA256(kp.PublicKey()) {
		t.Errorf("keyFingerprint = %v", view["keyFingerprint"])
	}
	a.ok(t, "/api/sessions/open", map[string]any{"id": strOf(view, "id")})
}

// TestHostKeyMismatch 校验主机密钥变了只报错、绝不覆盖（可能是中间人）。
func TestHostKeyMismatch(t *testing.T) {
	a := newTestApp(t, appOptions{})
	sid := a.newSSHSession(t) // 信任并打开
	host, port := a.ssh.Host(), a.ssh.Port()
	fpA := a.ssh.Fingerprint()
	a.ok(t, "/api/sessions/close", map[string]any{"id": sid})

	// 在同一个 host:port 上换一把主机密钥（模拟「服务器重装 / 中间人」）。
	other, err := testssh.NewKeyPair()
	if err != nil {
		t.Fatalf("生成主机密钥失败：%v", err)
	}
	a.ssh.Close()
	a.ssh = testssh.StartServer(t, testssh.Options{
		Addr: net.JoinHostPort(host, strconv.Itoa(port)), User: "demo", Password: "demo",
		HostKey: other.Signer, Root: a.sshRoot,
	})
	fpB := ssh.FingerprintSHA256(other.Signer.PublicKey())

	_, out := a.post(t, "/api/sessions/test", map[string]any{
		"host": host, "port": port, "user": "demo", "authType": "password", "password": "demo",
	})
	r, _ := out["result"].(map[string]any)
	if r["code"] != "hostkey_mismatch" || r["mismatch"] != true {
		t.Fatalf("换个主机密钥后应当报 hostkey_mismatch：%v", r)
	}
	if strOf(r, "fingerprint") != fpB {
		t.Errorf("mismatch 结果要带上本次看到的指纹（实际 %v，期望 %v）", r["fingerprint"], fpB)
	}
	// known_hosts 里必须还是旧指纹（绝不覆盖）。
	rec, ok := a.mux.hks.lookup(hostKeyID(host, port))
	if !ok {
		t.Fatal("known_hosts 记录丢了")
	}
	if rec.Fingerprint != fpA {
		t.Fatalf("known_hosts 被覆盖了：%s → %s", fpA, rec.Fingerprint)
	}
	// 打开会话同样要失败（不能带着不匹配的密钥连上去）。
	a.fail(t, "/api/sessions/open", map[string]any{"id": sid}, "hostkey_mismatch", http.StatusConflict)
}

// TestHostKeysListForget 校验已信任主机的列表与移除。
func TestHostKeysListForget(t *testing.T) {
	a := newTestApp(t, appOptions{})
	a.newSSHSession(t)
	_, hosts := a.get(t, "/api/hostkeys")
	list := listOf(hosts, "hosts")
	if len(list) != 1 {
		t.Fatalf("known_hosts = %v", hosts)
	}
	rec, _ := list[0].(map[string]any)
	if strOf(rec, "host") != a.ssh.Host() || numOf(rec, "port") != float64(a.ssh.Port()) ||
		strOf(rec, "keyType") != a.ssh.KeyType() || strOf(rec, "fingerprint") != a.ssh.Fingerprint() {
		t.Fatalf("HostKeyRecord 字段不对：%v", rec)
	}
	if _, err := time.Parse(time.RFC3339, strOf(rec, "addedAt")); err != nil {
		t.Errorf("addedAt 不是 RFC3339：%v", rec["addedAt"])
	}
	// 移除后重新连接需要再次信任
	a.ok(t, "/api/hostkeys/forget", map[string]any{"host": a.ssh.Host(), "port": a.ssh.Port()})
	a.fail(t, "/api/hostkeys/forget", map[string]any{"host": a.ssh.Host(), "port": a.ssh.Port()}, "not_found", 0)
	_, out := a.post(t, "/api/sessions/test", map[string]any{
		"host": a.ssh.Host(), "port": a.ssh.Port(), "user": "demo", "authType": "password", "password": "demo",
	})
	if r, _ := out["result"].(map[string]any); r["code"] != "hostkey_unknown" {
		t.Fatalf("移除信任后应重新要求确认：%v", r)
	}
}

// TestOpenErrors 校验打开会话失败时返回带码的错误（而不是一个「打开了但没终端」的假状态）。
func TestOpenErrors(t *testing.T) {
	a := newTestApp(t, appOptions{})
	// 未知主机密钥时会话还没信任 → 打开必须失败并带 hostkey_unknown。
	closed := freeClosedPort(t)
	out := a.ok(t, "/api/sessions/save", map[string]any{
		"host": "127.0.0.1", "port": closed, "user": "demo", "authType": "password", "password": "x",
	})
	sid := strOf(out["session"], "id")
	a.fail(t, "/api/sessions/open", map[string]any{"id": sid}, "dial_refused", http.StatusBadGateway)
	// 打开失败后不应留下半开状态
	_, list := a.get(t, "/api/sessions")
	if len(listOf(list, "open")) != 0 {
		t.Fatalf("打开失败不该留下「已打开」状态：%v", list["open"])
	}
	a.fail(t, "/api/sessions/open", map[string]any{"id": "0123456789abcdef0123456789abcdef"}, "not_found", 0)
}

// ---------------------------------------------------------------------------
// 小工具
// ---------------------------------------------------------------------------

func freeClosedPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("取空闲端口失败：%v", err)
	}
	_, portStr, _ := net.SplitHostPort(ln.Addr().String())
	_ = ln.Close()
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("解析端口失败：%v", err)
	}
	return port
}
