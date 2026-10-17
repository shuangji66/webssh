package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"webssh/backend/internal/secret"
	"webssh/backend/internal/testssh"
)

// ---------------------------------------------------------------------------
// 「不保存凭据」的会话（契约 11.1）的集成测试
//
// 一条贯穿的判据：**数据目录里不能出现任何凭据**（明文或密文都不行）。所以这里的会话用
// 一把不会在别处出现的密码（sshPassword），这样「扫一遍数据目录找这个串」是有意义的
// ——用 demo 当密码的话，用户名 demo 到处都是，断言等于什么都没验。
// ---------------------------------------------------------------------------

const secretPassword = "s3cret-pw-4f1c9a"

// newCredApp 起一个后端 + 内嵌 SSH 服务器（密码是 secretPassword），并确认主机密钥。
// 不建会话：几个用例要自己决定会话怎么建（保存 / 不保存凭据）。
func newCredApp(t *testing.T) *testApp {
	t.Helper()
	a := newTestApp(t, appOptions{
		sshOptions: &testssh.Options{User: "demo", Password: secretPassword},
	})
	a.trustHost(t, a.ssh)
	return a
}

// saveUnsavedSession 建一个「不保存凭据」的会话，返回 sid 与 SessionView。
func saveUnsavedSession(t *testing.T, a *testApp) (string, map[string]any) {
	t.Helper()
	out := a.ok(t, "/api/sessions/save", map[string]any{
		"name":           "nosave",
		"host":           a.ssh.Host(),
		"port":           a.ssh.Port(),
		"user":           a.sshUser,
		"authType":       "password",
		"password":       secretPassword,
		"saveCredential": false,
	})
	view, _ := out["session"].(map[string]any)
	sid := strOf(out["session"], "id")
	if sid == "" {
		t.Fatalf("保存会话没有返回 id：%v", out)
	}
	return sid, view
}

// TestCredentialSavedDefaultsToTrue 是回归用例：不带 saveCredential 的老用法不变。
func TestCredentialSavedDefaultsToTrue(t *testing.T) {
	a := newCredApp(t)
	out := a.ok(t, "/api/sessions/save", map[string]any{
		"host": a.ssh.Host(), "port": a.ssh.Port(), "user": a.sshUser,
		"authType": "password", "password": secretPassword,
	})
	view, _ := out["session"].(map[string]any)
	if view["credentialSaved"] != true {
		t.Fatalf("缺省应当保存凭据：%v", view)
	}
	if view["hasPassword"] != true {
		t.Fatalf("hasPassword 应当为 true：%v", view)
	}
	sid := strOf(out["session"], "id")
	// 保存了凭据的会话：open 不带凭据也能开（与第一轮一致）
	a.ok(t, "/api/sessions/open", map[string]any{"id": sid})
}

// TestCredentialNotSaved 覆盖 saveCredential=false 的全链路。
func TestCredentialNotSaved(t *testing.T) {
	a := newCredApp(t)
	sid, view := saveUnsavedSession(t, a)

	// 1) SessionView 必须下发 credentialSaved=false，且三个凭据字段都是空的
	if view["credentialSaved"] != false {
		t.Fatalf("credentialSaved 应当为 false：%v", view)
	}
	if view["hasPassword"] != false || view["hasPrivateKey"] != false {
		t.Fatalf("不保存凭据时不该有凭据：%v", view)
	}

	// 2) 配置落盘：没有任何凭据字段，也没有那把密码（明文/密文都不许有）
	raw := readLocal(t, filepath.Join(a.renv.DataDir, "sessions", sid+".json"))
	if strings.Contains(raw, secretPassword) {
		t.Fatalf("会话配置里出现了明文密码：%s", raw)
	}
	if !strings.Contains(raw, `"credentialSaved": false`) {
		t.Fatalf("配置里应当记下 credentialSaved=false：%s", raw)
	}
	for _, field := range []string{`"password"`, `"privateKey"`, `"passphrase"`} {
		if !strings.Contains(raw, field+": null") {
			t.Errorf("配置里的 %s 应当是 null：%s", field, raw)
		}
	}
	assertNoCredentialOnDisk(t, a)

	// 3) open 不带凭据 → credential_required（409）
	a.fail(t, "/api/sessions/open", map[string]any{"id": sid}, "credential_required", http.StatusConflict)

	// 4) 带凭据 open → 成功；响应里不能出现凭据
	res, out := a.post(t, "/api/sessions/open", map[string]any{"id": sid, "password": secretPassword})
	if res.StatusCode != http.StatusOK || out["ok"] != true {
		t.Fatalf("带凭据 open 失败：%d %v", res.StatusCode, out)
	}
	if strings.Contains(stringify(out), secretPassword) {
		t.Fatalf("open 响应里出现了明文凭据：%v", out)
	}
	// 4b) 连接测试：表单里没重填凭据时，内存里那份可以被复用（否则用户会看到一句莫名其妙的
	//     「没有可用凭据」，而会话明明刚打开成功）。
	_, out = a.post(t, "/api/sessions/test", map[string]any{
		"id": sid, "host": a.ssh.Host(), "port": a.ssh.Port(), "user": a.sshUser, "authType": "password",
	})
	result, _ := out["result"].(map[string]any)
	if result == nil || result["success"] != true {
		t.Fatalf("有内存凭据时连接测试应当成功：%v", out)
	}

	// 5) 会话列表（前端拿的就是它）同样不许出现凭据
	_, out = a.get(t, "/api/sessions")
	if strings.Contains(stringify(out), secretPassword) {
		t.Fatalf("/api/sessions 响应里出现了明文凭据：%v", out)
	}
	if view := findView(t, out, sid); view["credentialSaved"] != false {
		t.Fatalf("列表里的 credentialSaved 应当为 false：%v", view)
	}

	// 6) 内存凭据真被后续连接复用：把 SFTP 通道丢掉（模拟对端踢连接），下一次文件操作
	//    必须能重新拨号成功——它拿到的凭据只可能来自内存。
	rt, apiErr := a.mux.sessions.runtimeFor(sid)
	if apiErr != nil {
		t.Fatalf("取运行时失败：%v", apiErr)
	}
	rt.sftpMu.Lock()
	rt.dropSFTPLocked()
	rt.sftpMu.Unlock()
	a.ok(t, "/api/fs/list", map[string]any{"sid": sid, "path": ""})

	// 7) close 之后内存凭据失效：再 open 又要凭据
	a.ok(t, "/api/sessions/close", map[string]any{"id": sid})
	a.fail(t, "/api/sessions/open", map[string]any{"id": sid}, "credential_required", http.StatusConflict)

	// 8) close 之后再带凭据还是能打开（凭据随时可再输入）
	a.ok(t, "/api/sessions/open", map[string]any{"id": sid, "password": secretPassword})
	a.ok(t, "/api/sessions/close", map[string]any{"id": sid})

	// 9) 进程重启：内存凭据一并消失
	next := a.restart(t)
	next.fail(t, "/api/sessions/open", map[string]any{"id": sid}, "credential_required", http.StatusConflict)
	next.ok(t, "/api/sessions/open", map[string]any{"id": sid, "password": secretPassword})
}

// TestCredentialNotSavedWithKey 覆盖密钥认证 + 不保存凭据（私钥与口令都不落盘）。
func TestCredentialNotSavedWithKey(t *testing.T) {
	kp, err := testssh.NewKeyPair()
	if err != nil {
		t.Fatalf("生成测试密钥失败：%v", err)
	}
	pem, err := kp.PEM()
	if err != nil {
		t.Fatalf("导出私钥失败：%v", err)
	}
	a := newTestApp(t, appOptions{sshOptions: &testssh.Options{AuthorizedKeys: []ssh.PublicKey{kp.PublicKey()}}})
	// 用密钥认证先探一次，拿到主机密钥指纹并信任
	res, out := a.post(t, "/api/sessions/test", map[string]any{
		"host": a.ssh.Host(), "port": a.ssh.Port(), "user": "demo", "authType": "key", "privateKey": pem,
	})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("连接测试请求失败：%d %v", res.StatusCode, out)
	}
	result, _ := out["result"].(map[string]any)
	if result["success"] != true {
		if result["code"] != "hostkey_unknown" {
			t.Fatalf("密钥认证的连接测试失败：%v", result)
		}
		a.ok(t, "/api/sessions/trust", map[string]any{
			"host": a.ssh.Host(), "port": a.ssh.Port(), "fingerprint": result["fingerprint"],
		})
	}

	saved := a.ok(t, "/api/sessions/save", map[string]any{
		"name": "nosave-key", "host": a.ssh.Host(), "port": a.ssh.Port(), "user": "demo",
		"authType": "key", "privateKey": pem, "saveCredential": false,
	})
	sid := strOf(saved["session"], "id")
	view, _ := saved["session"].(map[string]any)
	if view["credentialSaved"] != false || view["hasPrivateKey"] != false {
		t.Fatalf("不保存凭据时不该落私钥：%v", view)
	}
	raw := readLocal(t, filepath.Join(a.renv.DataDir, "sessions", sid+".json"))
	if strings.Contains(raw, "PRIVATE KEY") {
		t.Fatalf("私钥落盘了：%s", raw)
	}
	if fp := strOf(view, "keyFingerprint"); fp != "" {
		t.Errorf("凭据没保存时不该留下由私钥算出的指纹：%v", fp)
	}
	a.fail(t, "/api/sessions/open", map[string]any{"id": sid}, "credential_required", http.StatusConflict)
	// 私钥（PEM 里有换行）经 JSON 往返后必须仍然可用
	a.ok(t, "/api/sessions/open", map[string]any{"id": sid, "privateKey": pem})
}

// TestCredentialSaveSwitchEdits 覆盖编辑时的开关语义。
func TestCredentialSaveSwitchEdits(t *testing.T) {
	a := newCredApp(t)
	sid, _ := saveUnsavedSession(t, a)

	// 1) 改名（前端 home.renameSession 就发这几个字段，不带 saveCredential/凭据）：
	//    不能把 credentialSaved 翻成 true，否则下次 open 会从 credential_required 变成
	//    no_credential，前端的凭据弹窗再也不弹。
	renamed := a.ok(t, "/api/sessions/save", map[string]any{
		"id": sid, "name": "renamed", "host": a.ssh.Host(), "port": a.ssh.Port(),
		"user": a.sshUser, "authType": "password",
	})
	view, _ := renamed["session"].(map[string]any)
	if view["credentialSaved"] != false {
		t.Fatalf("改名不该改变凭据保存状态：%v", view)
	}
	if strOf(view, "name") != "renamed" {
		t.Fatalf("改名没生效：%v", view)
	}
	a.fail(t, "/api/sessions/open", map[string]any{"id": sid}, "credential_required", http.StatusConflict)

	// 2) 打开会话留下内存凭据
	a.ok(t, "/api/sessions/open", map[string]any{"id": sid, "password": secretPassword})

	// 3) 打开开关但没给凭据：必须当场报 no_credential（而不是存下一个「声称保存、实际空」的会话）
	a.fail(t, "/api/sessions/save", map[string]any{
		"id": sid, "name": "renamed", "host": a.ssh.Host(), "port": a.ssh.Port(),
		"user": a.sshUser, "authType": "password", "saveCredential": true,
	}, "no_credential", http.StatusBadRequest)

	// 4) 打开开关 + 给凭据：落盘，之后 open 不再需要凭据
	saved := a.ok(t, "/api/sessions/save", map[string]any{
		"id": sid, "name": "renamed", "host": a.ssh.Host(), "port": a.ssh.Port(),
		"user": a.sshUser, "authType": "password", "password": secretPassword, "saveCredential": true,
	})
	view, _ = saved["session"].(map[string]any)
	if view["credentialSaved"] != true || view["hasPassword"] != true {
		t.Fatalf("开启保存后应当把凭据存下来：%v", view)
	}
	a.ok(t, "/api/sessions/close", map[string]any{"id": sid})
	a.ok(t, "/api/sessions/open", map[string]any{"id": sid})

	// 5) 反向：关掉开关 → 三个字段被清掉，只剩内存里的那份（close 后即失效）
	off := a.ok(t, "/api/sessions/save", map[string]any{
		"id": sid, "name": "renamed", "host": a.ssh.Host(), "port": a.ssh.Port(),
		"user": a.sshUser, "authType": "password", "saveCredential": false,
	})
	view, _ = off["session"].(map[string]any)
	if view["credentialSaved"] != false || view["hasPassword"] != false {
		t.Fatalf("关掉开关后应当清掉落盘凭据：%v", view)
	}
	raw := readLocal(t, filepath.Join(a.renv.DataDir, "sessions", sid+".json"))
	if strings.Contains(raw, "ct\":") {
		t.Fatalf("关掉开关后配置里不该还有密文：%s", raw)
	}
	// 5b) 会话**正开着**：刚才清掉落盘的那份凭据必须被留在内存里。用户只是关掉了
	//     「保存」这个持久化开关，不是要断开当前会话——新建子终端 / SFTP 断线重连
	//     仍然要能用它拨号（原实现这里会回 no_credential，前端还会误报「终端不存在」）。
	if _, ok := a.mux.sessions.cachedCredential(sid); !ok {
		t.Fatal("会话开着时关掉保存开关，当前凭据应当留在内存里")
	}
	rt, apiErr := a.mux.sessions.runtimeFor(sid)
	if apiErr != nil {
		t.Fatalf("取运行时失败：%v", apiErr)
	}
	rt.sftpMu.Lock()
	rt.dropSFTPLocked()
	rt.sftpMu.Unlock()
	a.ok(t, "/api/fs/list", map[string]any{"sid": sid, "path": ""})

	// 5c) close 之后内存里的那份失效（与「不保存凭据」的整体语义一致）
	a.ok(t, "/api/sessions/close", map[string]any{"id": sid})
	if _, ok := a.mux.sessions.cachedCredential(sid); ok {
		t.Error("关闭会话后不该还留着内存凭据")
	}
	a.fail(t, "/api/sessions/open", map[string]any{"id": sid}, "credential_required", http.StatusConflict)
}

// TestCredentialSaveOffKeepsVerifiedMemoryCredential 锁住 carry-over 的来源优先级与空值
// 防覆盖。
//
// 场景：配置里存着旧密码，这次 open 时用户当场输入了新密码（open 成功后写进 memCreds）。
// 此时把「保存凭据」关掉，**不能**拿磁盘上的旧密码覆盖内存里这份真正拨通了的凭据——否则
// 新终端会用旧密码去连，报 auth_failed。没有内存凭据时才回落到解密旧配置。
func TestCredentialSaveOffKeepsVerifiedMemoryCredential(t *testing.T) {
	a := newCredApp(t)
	saved := a.ok(t, "/api/sessions/save", map[string]any{
		"host": a.ssh.Host(), "port": a.ssh.Port(), "user": a.sshUser,
		"authType": "password", "password": secretPassword,
	})
	sid := strOf(saved["session"], "id")
	a.ok(t, "/api/sessions/open", map[string]any{"id": sid})

	// 模拟「这次 open 带了一份当场输入、验证成功的凭据」：它比磁盘上的旧密文更可信。
	a.mux.sessions.setCachedCredential(sid, memoryCredential{password: "verified-at-open"})
	a.ok(t, "/api/sessions/save", map[string]any{
		"id": sid, "host": a.ssh.Host(), "port": a.ssh.Port(), "user": a.sshUser,
		"authType": "password", "saveCredential": false,
	})
	mc, ok := a.mux.sessions.cachedCredential(sid)
	if !ok || mc.password != "verified-at-open" {
		t.Fatalf("carry over 不该用磁盘旧密码覆盖已验证的内存凭据：%+v", mc)
	}

	// API 调用方传空密码（前端表单不会发）也不能把内存里那份清成空：会话开着时那等于
	// 让之后每个新终端都报 no_credential。
	a.ok(t, "/api/sessions/save", map[string]any{
		"id": sid, "host": a.ssh.Host(), "port": a.ssh.Port(), "user": a.sshUser,
		"authType": "password", "password": "", "saveCredential": false,
	})
	mc, ok = a.mux.sessions.cachedCredential(sid)
	if !ok || mc.password != "verified-at-open" {
		t.Fatalf("空密码不该覆盖内存里能用的凭据：%+v", mc)
	}
}

// TestCredentialSaveOffWithUndecryptableStored 锁住「旧密文解不开时关掉保存凭据」的处置。
//
// 场景：master.key 换过 / 密文损坏。会话已经开着（现有连接不受影响），用户来关「保存凭据」。
// 这次保存本来就要把那份密文清掉——如果因为「读不出一个马上要被丢弃的字段」返回 500，
// 用户就被锁死在死角里：开关关不掉，之后每次 open 也都会在同一处解密失败（前端只能拿到一句
// internal），唯一出路是删掉会话重建。
//
// 正确做法：记一条日志、跳过 carry over，保存照常完成；内存里没有可用凭据（新终端会报
// no_credential，而不是拿空密码去连），落盘的那份密文被清掉。
func TestCredentialSaveOffWithUndecryptableStored(t *testing.T) {
	a := newCredApp(t)
	saved := a.ok(t, "/api/sessions/save", map[string]any{
		"host": a.ssh.Host(), "port": a.ssh.Port(), "user": a.sshUser,
		"authType": "password", "password": secretPassword,
	})
	sid := strOf(saved["session"], "id")
	a.ok(t, "/api/sessions/open", map[string]any{"id": sid})

	// 换成一把解不开的密文（AAD 绑定 sid 与字段，换了主密钥就是这个效果）。
	a.mux.sessions.mu.Lock()
	a.mux.sessions.configs[sid].Password = &secret.Secret{V: 1, CT: "bm90LWEtcmVhbC1jaXBoZXJ0ZXh0"}
	a.mux.sessions.mu.Unlock()

	out := a.ok(t, "/api/sessions/save", map[string]any{
		"id": sid, "host": a.ssh.Host(), "port": a.ssh.Port(), "user": a.sshUser,
		"authType": "password", "saveCredential": false,
	})
	view, _ := out["session"].(map[string]any)
	if view["credentialSaved"] != false || view["hasPassword"] != false {
		t.Fatalf("关掉保存凭据后应当没有任何凭据：%v", view)
	}
	// 解不开的那份必须被这次保存清掉，不能留在配置里继续毒下一次 open。
	raw := readLocal(t, filepath.Join(a.renv.DataDir, "sessions", sid+".json"))
	if strings.Contains(raw, `"password": {`) {
		t.Fatalf("解不开的旧密文应当被清掉：%s", raw)
	}
	assertNoCredentialOnDisk(t, a)
	// 没有可 carry over 的来源：内存里也不该凭空多出（或留下一份空的）凭据。
	if mc, ok := a.mux.sessions.cachedCredential(sid); ok {
		t.Fatalf("解不开的凭据不 carry over，内存里不该有凭据：%+v", mc)
	}
}

// TestCredentialCacheOnAuthTypeSwitch 钉住「改认证方式要清内存凭据、不改就不能清」。
//
// 背景：会话「不保存凭据」+ 已打开时，内存里留着一份明文凭据（只由一次成功的 open 写入）。
// 把认证方式从 password 改成 key 之后，那份密码已经和配置不匹配：留着它，下一次 open 会
// 拿密码去走密钥认证分支，报出「该会话使用密钥认证，但没有可用的私钥」——错误信息与真实
// 原因不符（真正的原因是缓存里有一份类型不匹配的旧凭据），用户完全无法自查。
// 正确行为：认证方式变了就清缓存 → open 回到 credential_required（前端据此弹凭据输入框）。
//
// 这条路径只有直接打 API 才走得到（前端的表单在切换认证方式时要求重填凭据），所以用例
// 刻意不带 password / privateKey。
//
// 反向同样重要：认证方式没变时**不能**清缓存，否则「打开过一次、进程内复用」的承诺就没了。
func TestCredentialCacheOnAuthTypeSwitch(t *testing.T) {
	a := newCredApp(t)
	sid, _ := saveUnsavedSession(t, a)

	// 打开一次：内存里留下密码（只有成功的 open 才会写缓存）
	a.ok(t, "/api/sessions/open", map[string]any{"id": sid, "password": secretPassword})
	if _, ok := a.mux.sessions.cachedCredential(sid); !ok {
		t.Fatal("open 成功后内存里应当有凭据")
	}

	// ① 只改名（认证方式不变、不带凭据）：缓存必须留着——否则「打开过一次、进程内复用」
	//    的承诺就没了（用户会被莫名其妙地要求重新输入密码）
	a.ok(t, "/api/sessions/save", map[string]any{
		"id": sid, "name": "renamed", "host": a.ssh.Host(), "port": a.ssh.Port(),
		"user": a.sshUser, "authType": "password",
	})
	if _, ok := a.mux.sessions.cachedCredential(sid); !ok {
		t.Error("认证方式没变，内存凭据不该被清掉")
	}

	// ② 改成密钥认证（不带凭据）：那份密码已经和配置不匹配，必须清掉
	a.ok(t, "/api/sessions/save", map[string]any{
		"id": sid, "name": "renamed", "host": a.ssh.Host(), "port": a.ssh.Port(),
		"user": a.sshUser, "authType": "key",
	})
	if _, ok := a.mux.sessions.cachedCredential(sid); ok {
		t.Error("改了认证方式之后内存里还留着旧凭据（下一次 open 会报出与原因不符的 no_credential）")
	}

	// 行为侧：关掉再打开（不带凭据）要回 409 credential_required（前端据此弹凭据输入框），
	// 而不是拿着残留密码去连、报 400 no_credential。
	a.ok(t, "/api/sessions/close", map[string]any{"id": sid})
	a.fail(t, "/api/sessions/open", map[string]any{"id": sid}, "credential_required", http.StatusConflict)
}

// TestOpenWithBadCredentialKeepsNothing 校验「失败的凭据不会被留在内存里」。
func TestOpenWithBadCredentialKeepsNothing(t *testing.T) {
	a := newCredApp(t)
	sid, _ := saveUnsavedSession(t, a)

	// 认证失败属于连接类错误（502），带 auth_failed 裸码
	a.fail(t, "/api/sessions/open", map[string]any{"id": sid, "password": "wrong-password"}, "auth_failed", http.StatusBadGateway)
	// 失败的那份没有被缓存：再来一次仍然要求输入凭据
	a.fail(t, "/api/sessions/open", map[string]any{"id": sid}, "credential_required", http.StatusConflict)
	// 带一个空凭据也属于「没给够」
	a.fail(t, "/api/sessions/open", map[string]any{"id": sid, "password": ""}, "credential_required", http.StatusConflict)
	// 正主还是能打开
	a.ok(t, "/api/sessions/open", map[string]any{"id": sid, "password": secretPassword})
}

// TestCredentialCacheDroppedOnDelete 删除会话要把内存凭据一并丢掉（否则同 id 复用时
// 会拿着上一个会话的密码去连——虽然 id 是随机的，但这条纪律必须锁住）。
func TestCredentialCacheDroppedOnDelete(t *testing.T) {
	a := newCredApp(t)
	sid, _ := saveUnsavedSession(t, a)
	a.ok(t, "/api/sessions/open", map[string]any{"id": sid, "password": secretPassword})
	if _, ok := a.mux.sessions.cachedCredential(sid); !ok {
		t.Fatal("打开后应当有内存凭据")
	}
	a.ok(t, "/api/sessions/delete", map[string]any{"id": sid})
	if _, ok := a.mux.sessions.cachedCredential(sid); ok {
		t.Error("删除会话后不该还留着内存凭据")
	}
}

// TestInfoReportsArchiveAndExportRoot 校验 /api/info 的 runtime 新字段与前端契约一致。
func TestInfoReportsArchiveAndExportRoot(t *testing.T) {
	nas := t.TempDir()
	a := newTestApp(t, appOptions{exportRoot: nas, archiveDir: "/tmp/x-archive"})
	_, info := a.get(t, "/api/info")
	runtime, _ := info["runtime"].(map[string]any)
	if strOf(runtime, "archiveDir") != "/tmp/x-archive" {
		t.Errorf("archiveDir = %v", runtime["archiveDir"])
	}
	if strOf(runtime, "exportRoot") != nas {
		t.Errorf("exportRoot = %v", runtime["exportRoot"])
	}
}

// assertNoCredentialOnDisk 扫一遍数据目录：任何文件里都不许出现那把密码。
//
// 这是「不落盘」这条纪律最直接的验证方式：不看字段名（改个名字就绕过去了），
// 直接看字节。
func assertNoCredentialOnDisk(t *testing.T, a *testApp) {
	t.Helper()
	if err := filepath.WalkDir(a.renv.DataDir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		if strings.Contains(string(data), secretPassword) {
			t.Errorf("数据目录里的 %s 出现了明文凭据", path)
		}
		return nil
	}); err != nil {
		t.Fatalf("遍历数据目录失败：%v", err)
	}
}

// stringify 把响应体序列化，用于「响应里有没有出现某个串」的断言。
func stringify(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}

// findView 从 /api/sessions 的响应里取出某个会话的 SessionView。
func findView(t *testing.T, out map[string]any, sid string) map[string]any {
	t.Helper()
	for _, item := range listOf(out, "sessions") {
		m, _ := item.(map[string]any)
		if strOf(m, "id") == sid {
			return m
		}
	}
	t.Fatalf("会话列表里没有 %s：%v", sid, out)
	return nil
}

// TestTerminalCreateFailureIsNotGone 锁住「新建终端失败」与「终端不存在」的分流。
//
// 新建终端时没有可用凭据是在**拨号之前**就失败的，不分配 PTY，所以这条用例在无 PTY 的
// 环境也会跑。它验证两件事：
//  1. 真实原因留给人看（不能只发一个 gone 让前端说「终端不存在」）；
//  2. 关闭码是 wsCloseCreateFailed 而不是 gone——前端据此保留标签并标红，用户修正凭据后
//     可以点「重连」重试。
func TestTerminalCreateFailureIsNotGone(t *testing.T) {
	a := newCredApp(t)
	sid, _ := saveUnsavedSession(t, a)
	a.ok(t, "/api/sessions/open", map[string]any{"id": sid, "password": secretPassword})
	// 抹掉内存凭据：之后新建终端只能从落盘或内存取，而两者都没有。
	a.mux.sessions.dropCachedCredential(sid)

	ws := a.dialWS(t, sid, "")
	got, ok := ws.collectUntil("请重新打开会话", testTimeout)
	if !ok {
		t.Fatalf("没有内存凭据时新建终端应当给出明确提示，实际：%q", got)
	}
	if !strings.Contains(got, "没有可用的凭据") {
		t.Errorf("错误文案没说清原因：%q", got)
	}
	if strings.Contains(got, oscGone) {
		t.Fatalf("创建失败不能附带 gone 控制帧（前端会误判成终端不存在并删标签）：%q", got)
	}
	if code := ws.closeCode(testTimeout); code != wsCloseCreateFailed {
		t.Errorf("创建失败的关闭码应当是 %d，实际 %d", wsCloseCreateFailed, code)
	}
}

// TestCredentialNotSavedTerminalReuse 覆盖「内存凭据也供**新终端**复用」。
//
// 终端是另一条独立的 SSH 连接（见 sessions.go 的 newTerminal），拨号时同样只能从内存里
// 拿到凭据——这条路径与 SFTP 重连（TestCredentialNotSaved 第 6 步）不同，值得单独锁住。
// 需要真的分配 PTY，环境不支持时按仓库约定跳过。失败路径（没有凭据）由
// TestTerminalCreateFailureIsNotGone 覆盖。
func TestCredentialNotSavedTerminalReuse(t *testing.T) {
	requirePTY(t)
	a := newCredApp(t)
	sid, _ := saveUnsavedSession(t, a)

	// 打开会话（凭据只进内存）
	a.ok(t, "/api/sessions/open", map[string]any{"id": sid, "password": secretPassword})

	// 把凭据放回内存（相当于用户重新输入一次），新终端就能连上：这条连接与打开会话时的
	// 那条完全无关，凭据只可能来自内存。
	a.mux.sessions.setCachedCredential(sid, memoryCredential{password: secretPassword})
	ws2 := a.dialWS(t, sid, "")
	waitForTID(t, ws2)
	ws2.mustCollectUntil(shellPrompt, testTimeout)
	ws2.writeText("echo cred-marker\r")
	ws2.mustCollectCount("cred-marker", 2, testTimeout)
}
