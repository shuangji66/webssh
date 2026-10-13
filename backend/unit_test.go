package main

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestWSHandshakeAndFrames 校验 WebSocket 编解码：握手向量、掩码解包、长度上限、关闭帧。
func TestWSHandshakeAndFrames(t *testing.T) {
	// RFC 6455 第 1.3 节的例子
	if got := wsAcceptKey("dGhlIHNhbXBsZSBub25jZQ=="); got != "s3pPLMBiTxaQ9kYGzzhZRbK+xOo=" {
		t.Fatalf("Sec-WebSocket-Accept = %q", got)
	}

	// 服务端帧 → 客户端（无掩码）能被 wsReadFrame 读回
	for _, payload := range []string{"", "hi", strings.Repeat("x", 200), strings.Repeat("y", 70000)} {
		frame := wsFrame(opText, []byte(payload))
		op, fin, got, err := wsReadFrame(bytes.NewReader(frame))
		if err != nil {
			t.Fatalf("读帧失败（len=%d）：%v", len(payload), err)
		}
		if op != opText || !fin || string(got) != payload {
			t.Fatalf("帧编解码不一致：op=%d fin=%v len=%d", op, fin, len(got))
		}
	}

	// 客户端帧带掩码：解出来的必须是原文
	masked := maskFrame(opBin, []byte("masked-payload"))
	op, _, got, err := wsReadFrame(bytes.NewReader(masked))
	if err != nil || op != opBin || string(got) != "masked-payload" {
		t.Fatalf("掩码帧解包失败：op=%d payload=%q err=%v", op, got, err)
	}

	// 超过单帧上限：**先拒绝再分配**（长度字段由客户端给，直接 make 就是打爆内存的入口）
	over := wsFrame(opText, make([]byte, maxFrameBytes+1))
	if _, _, _, err := wsReadFrame(bytes.NewReader(over)); err == nil {
		t.Fatal("超过单帧上限的帧竟然被接受了")
	}

	// close 帧：2 字节状态码 + reason
	closeFrame := wsCloseFrame(wsCloseDetached, "detached")
	op, _, payload, err := wsReadFrame(bytes.NewReader(closeFrame))
	if err != nil || op != opClose || len(payload) < 2 {
		t.Fatalf("close 帧不对：op=%d payload=%v err=%v", op, payload, err)
	}
}

// TestCryptoHelpers 校验主密钥解析与空凭据语义。
func TestCryptoHelpers(t *testing.T) {
	key := make([]byte, masterKeyLen)
	for i := range key {
		key[i] = byte(i * 7)
	}
	for _, encoded := range []string{
		base64.StdEncoding.EncodeToString(key),
		base64.RawStdEncoding.EncodeToString(key),
		base64.URLEncoding.EncodeToString(key),
		hex.EncodeToString(key),
	} {
		got, err := decodeMasterKey(encoded)
		if err != nil || !bytes.Equal(got, key) {
			t.Errorf("解析 %q 失败：%v", encoded, err)
		}
	}
	for _, bad := range []string{"", "abc", base64.StdEncoding.EncodeToString(key[:8]), strings.Repeat("z", 64)} {
		if _, err := decodeMasterKey(bad); err == nil {
			t.Errorf("%q 不该被接受", bad)
		}
	}

	// 空明文 = 没有这个凭据（不写密文），nil 密文解出空串
	c := &cryptor{}
	var err error
	if c, err = loadCryptor(t.TempDir(), base64.StdEncoding.EncodeToString(key)); err != nil {
		t.Fatalf("初始化失败：%v", err)
	}
	if sec, err := c.encrypt(fieldPassword, "sid", ""); err != nil || sec != nil {
		t.Errorf("空明文不该产生密文：%v %v", sec, err)
	}
	if plain, err := c.decrypt(fieldPassword, "sid", nil); err != nil || plain != "" {
		t.Errorf("nil 密文应当解出空串：%q %v", plain, err)
	}
	// 密文里的 base64 不是合法 base64 时也要报错
	if _, err := c.decrypt(fieldPassword, "sid", &secret{V: 1, CT: "!!!"}); err == nil {
		t.Error("非法 base64 应当报错")
	}
}

// TestMasterKeyFile 校验 master.key 的生成、长度与权限。
func TestMasterKeyFile(t *testing.T) {
	dir := t.TempDir()
	c1, err := loadCryptor(dir, "")
	if err != nil {
		t.Fatalf("首次生成 master.key 失败：%v", err)
	}
	path := filepath.Join(dir, masterKeyFile)
	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("master.key 不存在：%v", err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Errorf("权限 = %o", st.Mode().Perm())
	}
	key, _ := os.ReadFile(path)
	if len(key) != masterKeyLen {
		t.Fatalf("长度 = %d", len(key))
	}
	// 第二次加载必须拿到同一把密钥（否则已存的密文全部解不开）
	c2, err := loadCryptor(dir, "")
	if err != nil {
		t.Fatalf("再次加载失败：%v", err)
	}
	sec, err := c1.encrypt(fieldPassword, "sid", "hello")
	if err != nil {
		t.Fatalf("加密失败：%v", err)
	}
	if plain, err := c2.decrypt(fieldPassword, "sid", sec); err != nil || plain != "hello" {
		t.Fatalf("同一密钥应当能解密：%q %v", plain, err)
	}
	// 长度不对的密钥文件必须让启动失败（不能悄悄重新生成：那会把用户的密码全变成废密文）
	if err := os.WriteFile(path, []byte("short"), 0o600); err != nil {
		t.Fatalf("写坏文件失败：%v", err)
	}
	if _, err := loadCryptor(dir, ""); err == nil {
		t.Fatal("坏的 master.key 应当被拒绝")
	}
}

// TestStoreHelpers 校验 id 校验、原子写与目录布局。
func TestStoreHelpers(t *testing.T) {
	if id := newID(); len(id) != idLen {
		t.Errorf("newID 长度 = %d", len(id))
	}
	for _, ok := range []string{"abc", "a1-b2_C3", strings.Repeat("a", 64)} {
		if !validID(ok) {
			t.Errorf("validID(%q) 应当为真", ok)
		}
	}
	for _, bad := range []string{"", "../x", "a/b", "a.b", strings.Repeat("a", 65), "a b", "汉字"} {
		if validID(bad) {
			t.Errorf("validID(%q) 应当为假（这是路径安全的边界）", bad)
		}
	}

	st, err := newStore(t.TempDir())
	if err != nil {
		t.Fatalf("初始化数据目录失败：%v", err)
	}
	sc := &sessionConfig{ID: newID(), Name: "n", Host: "h", Port: 22, User: "u", AuthType: "password",
		CreatedAt: time.Now().Format(time.RFC3339), UpdatedAt: time.Now().Format(time.RFC3339)}
	if err := st.saveSession(sc); err != nil {
		t.Fatalf("保存会话失败：%v", err)
	}
	// 权限 0600（凭据密文和 master.key 一样敏感）
	info, err := os.Stat(st.sessionPath(sc.ID))
	if err != nil {
		t.Fatalf("会话文件不存在：%v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("会话文件权限 = %o", info.Mode().Perm())
	}
	// 原子写：不能留下临时文件
	entries, _ := os.ReadDir(st.sessionsDir())
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Errorf("残留临时文件：%s", e.Name())
		}
	}
	got, err := st.loadSession(sc.ID)
	if err != nil || got.Host != "h" || got.Port != 22 {
		t.Fatalf("读回失败：%+v %v", got, err)
	}
	// 非法 id 一律当不存在（不能拼出 data/sessions/../../x.json）
	if _, err := st.loadSession("../../etc/passwd"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("非法 id 应当当成不存在：%v", err)
	}
	// 历史文件必须可读写（O_RDWR|O_APPEND：只写打开会让回放/历史接口 EBADF）
	f, err := st.createHistoryFile(sc.ID, "tid1")
	if err != nil {
		t.Fatalf("创建历史文件失败：%v", err)
	}
	if _, err := f.WriteString("hello"); err != nil {
		t.Fatalf("写历史失败：%v", err)
	}
	if _, err := f.Seek(0, 0); err != nil {
		t.Fatalf("历史文件不可读（应当以 O_RDWR 打开）：%v", err)
	}
	data := make([]byte, 5)
	if _, err := f.Read(data); err != nil {
		t.Fatalf("读历史失败：%v", err)
	}
	f.Close()
	st.removeHistoryDir(sc.ID)
	if _, err := os.Stat(st.historyDir(sc.ID)); !os.IsNotExist(err) {
		t.Errorf("历史目录没删掉：%v", err)
	}
}

// TestHostKeyStoreAndPending 校验 known_hosts 的键格式与「待确认」缓存。
func TestHostKeyStoreAndPending(t *testing.T) {
	if got := hostKeyID("10.0.0.1", 22); got != "10.0.0.1:22" {
		t.Errorf("hostKeyID = %q", got)
	}
	if got := hostKeyID("10.0.0.1", 0); got != "10.0.0.1:22" {
		t.Errorf("缺省端口应当按 22 处理：%q", got)
	}
	if got := hostKeyID("fe80::1", 2222); got != "[fe80::1]:2222" {
		t.Errorf("IPv6 应当带方括号：%q", got)
	}
	host, port := splitHostKeyID("[fe80::1]:2222")
	if host != "fe80::1" || port != 2222 {
		t.Errorf("splitHostKeyID = %q %d", host, port)
	}
	host, port = splitHostKeyID("10.0.0.1:2222")
	if host != "10.0.0.1" || port != 2222 {
		t.Errorf("splitHostKeyID = %q %d", host, port)
	}

	st, err := newStore(t.TempDir())
	if err != nil {
		t.Fatalf("初始化失败：%v", err)
	}
	hks, err := newHostKeyStore(st)
	if err != nil {
		t.Fatalf("初始化 known_hosts 失败：%v", err)
	}
	rec := hostKeyRecord{KeyType: "ssh-ed25519", Fingerprint: "SHA256:abc", PublicKey: "AAAA", AddedAt: time.Now().Format(time.RFC3339)}
	if err := hks.add("10.0.0.1:22", rec); err != nil {
		t.Fatalf("写入失败：%v", err)
	}
	if got, ok := hks.lookup("10.0.0.1:22"); !ok || got.Fingerprint != "SHA256:abc" {
		t.Fatalf("读回失败：%+v", got)
	}
	list := hks.list()
	if len(list) != 1 || list[0].Host != "10.0.0.1" || list[0].Port != 22 {
		t.Fatalf("列表 = %+v", list)
	}
	if err := hks.forget("10.0.0.1", 22); err != nil {
		t.Fatalf("移除失败：%v", err)
	}
	if _, ok := hks.lookup("10.0.0.1:22"); ok {
		t.Error("移除后不该还查得到")
	}
	if err := hks.forget("10.0.0.1", 22); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("移除不存在的记录应当报 not exist：%v", err)
	}
	// known_hosts 文件本身也要 0600
	if info, err := os.Stat(st.knownHostsPath()); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("known_hosts 权限不对：%v %v", info, err)
	}
	// 损坏的 known_hosts 必须让启动失败（否则会把手动确认过的信任降级成「未知」）
	if err := os.WriteFile(st.knownHostsPath(), []byte("{not json"), 0o600); err != nil {
		t.Fatalf("写坏文件失败：%v", err)
	}
	if _, err := newHostKeyStore(st); err == nil {
		t.Error("损坏的 known_hosts 应当报错")
	}

	// pendingKeys：指纹必须一致、认领一次即失效
	p := newPendingKeys()
	p.put("h:22", hostKeyRecord{Fingerprint: "SHA256:abc"})
	if _, ok := p.take("h:22", "SHA256:wrong"); ok {
		t.Error("指纹不一致不该被认领")
	}
	if rec, ok := p.take("h:22", "SHA256:abc"); !ok || rec.Fingerprint != "SHA256:abc" {
		t.Error("指纹一致应当被认领")
	}
	if _, ok := p.take("h:22", "SHA256:abc"); ok {
		t.Error("认领过的条目不该还能再认领")
	}
	// 过期条目
	p2 := newPendingKeys()
	p2.ttl = time.Millisecond
	p2.put("h:22", hostKeyRecord{Fingerprint: "SHA256:abc"})
	time.Sleep(5 * time.Millisecond)
	if _, ok := p2.take("h:22", "SHA256:abc"); ok {
		t.Error("过期条目不该被认领")
	}
}

// TestPathValidation 校验远程路径与上传文件名的校验规则。
func TestPathValidation(t *testing.T) {
	ok := []string{"/", "/a", "/a/b", "a/b", "./x", ""}
	for _, p := range ok {
		if err := validateRemotePath(p); err != nil {
			t.Errorf("validateRemotePath(%q) 不该报错：%v", p, err)
		}
	}
	for _, p := range []string{"/a/../b", "../x", "a/..", "/..", ".."} {
		if err := validateRemotePath(p); err == nil {
			t.Errorf("validateRemotePath(%q) 应当报错", p)
		}
	}

	segs, err := validateUploadName("dir/sub/child.txt")
	if err != nil || len(segs) != 3 || segs[2] != "child.txt" {
		t.Errorf("相对路径上传应当被切成段：%v %v", segs, err)
	}
	if segs, err := validateUploadName(`win\dir\a.txt`); err != nil || len(segs) != 3 {
		t.Errorf("反斜杠应当被归一：%v %v", segs, err)
	}
	for _, bad := range []string{"", "/abs.txt", "../up.txt", "a/../b", "a//b", "./a", "a/./b", "a\nb"} {
		if _, err := validateUploadName(bad); err == nil {
			t.Errorf("validateUploadName(%q) 应当报错", bad)
		}
	}
	// HTML 规范里的 filename 转义
	if got := decodeFormFilenameEscapes("a%22b%0Ac.txt"); got != "a\"b\nc.txt" {
		t.Errorf("转义还原 = %q", got)
	}
}

// TestConfigEnv 校验环境变量解析与默认值。
func TestConfigEnv(t *testing.T) {
	t.Setenv("WEBSSH_ADMIN_SOCK", "")
	t.Setenv("WEBSSH_DIAL_TIMEOUT", "")
	renv := loadRuntimeEnv()
	if !strings.HasSuffix(renv.AdminSock, filepath.Join("webssh", "app.sock")) {
		t.Errorf("默认 socket = %q", renv.AdminSock)
	}
	if !strings.HasSuffix(renv.DataDir, filepath.Join("webssh", "data")) {
		t.Errorf("默认数据目录 = %q", renv.DataDir)
	}
	if renv.DialTimeout != 15*time.Second {
		t.Errorf("默认超时 = %v", renv.DialTimeout)
	}
	if renv.TrimSocket != trimDefaultSocket || renv.TrimApp != "WebSSH" {
		t.Errorf("飞牛默认值不对：%v %v", renv.TrimSocket, renv.TrimApp)
	}
	if renv.Version != appVersion {
		t.Errorf("版本 = %q", renv.Version)
	}

	t.Setenv("WEBSSH_ADMIN_SOCK", "/tmp/x.sock")
	t.Setenv("WEBSSH_ADMIN_BASEURL", "/app/WebSSH")
	t.Setenv("WEBSSH_DATA_DIR", "/tmp/d")
	t.Setenv("WEBSSH_DIAL_TIMEOUT", "3s")
	t.Setenv("WEBSSH_TRIM_SOCKET", "/tmp/trim.sock")
	t.Setenv("WEBSSH_TRIM_APP", "Other")
	t.Setenv("WEBSSH_MASTER_KEY", "abc")
	renv = loadRuntimeEnv()
	if renv.AdminSock != "/tmp/x.sock" || renv.DataDir != "/tmp/d" || renv.DialTimeout != 3*time.Second ||
		renv.TrimSocket != "/tmp/trim.sock" || renv.TrimApp != "Other" || renv.MasterKeyEnv != "abc" ||
		renv.AdminBaseURL != "/app/WebSSH" {
		t.Fatalf("环境变量没有生效：%+v", renv)
	}
	// 非法时长回退默认值（不拒绝启动，但会记一条日志）
	t.Setenv("WEBSSH_DIAL_TIMEOUT", "abc")
	if got := dialTimeoutFromEnv(); got != 15*time.Second {
		t.Errorf("非法时长应当回退默认值，实际 %v", got)
	}
	t.Setenv("WEBSSH_DIAL_TIMEOUT", "-5s")
	if got := dialTimeoutFromEnv(); got != 15*time.Second {
		t.Errorf("非正时长应当回退默认值，实际 %v", got)
	}
}

// TestConnectErrorStatusMapping 校验连接类错误到 HTTP 状态码的映射。
func TestConnectErrorStatusMapping(t *testing.T) {
	cases := map[string]int{
		codeNoCredential:    400,
		codeKeyParse:        400,
		codeHostKeyUnknown:  409,
		codeHostKeyMismatch: 409,
		codeAuthFailed:      502,
		codeDNS:             502,
		codeDialTimeout:     502,
		codeSFTPUnavailable: 502,
	}
	for code, want := range cases {
		got := errFromConnect(&connectError{Code: code, Detail: "x"})
		if got.Status != want || got.Code != code {
			t.Errorf("%s → %d，期望 %d", code, got.Status, want)
		}
	}
}

// TestSmallHelpers 覆盖几个纯函数（体积文案、文件名清洗、错误码构造）。
func TestSmallHelpers(t *testing.T) {
	cases := map[int64]string{
		0:       "0 B",
		512:     "512 B",
		1024:    "1.0 KB",
		1 << 20: "1.0 MB",
		1 << 30: "1.0 GB",
		1536:    "1.5 KB",
	}
	for in, want := range cases {
		if got := humanSize(in); got != want {
			t.Errorf("humanSize(%d) = %q，期望 %q", in, got, want)
		}
	}
	if got := sanitizeHeader("a\"b\\c.txt"); got != "a_b_c.txt" {
		t.Errorf("sanitizeHeader = %q", got)
	}
	if kindOf(true) != "目录" || kindOf(false) != "文件" {
		t.Error("kindOf 文案不对")
	}
	if e := errNotFound("会话 %s 不存在", "abc"); e.Status != 404 || e.Code != "not_found" ||
		!strings.Contains(e.Message, "abc") {
		t.Errorf("errNotFound = %+v", e)
	}
	if e := errTooLarge("超过 %s", "1 GiB"); e.Status != 413 {
		t.Errorf("errTooLarge = %+v", e)
	}
	if got := defaultSessionName("root", "10.0.0.1"); got != "root@10.0.0.1" {
		t.Errorf("defaultSessionName = %q", got)
	}
	if got := derefString(nil); got != "" {
		t.Errorf("derefString(nil) = %q", got)
	}
	s := "  x  "
	if got := derefString(&s); got != "x" {
		t.Errorf("derefString = %q", got)
	}
}

// TestSessionIdentityDefaultPort 校验「端口默认 22 参与身份比较」。
func TestSessionIdentityDefaultPort(t *testing.T) {
	a := &sessionConfig{User: "root", Host: "h", Port: 22}
	b := &sessionConfig{User: "root", Host: "h", Port: 2222}
	if a.identity() == b.identity() {
		t.Error("端口不同应当是不同的身份")
	}
	if a.identity() != "root@h:22" {
		t.Errorf("identity = %q", a.identity())
	}
}

// TestStaleHistoryPurgedOnStartup 校验上次运行留下的历史目录在启动时被清掉。
func TestStaleHistoryPurgedOnStartup(t *testing.T) {
	dir := t.TempDir()
	stale := filepath.Join(dir, "history", "0123456789abcdef0123456789abcdef")
	if err := os.MkdirAll(stale, 0o700); err != nil {
		t.Fatalf("准备目录失败：%v", err)
	}
	if err := os.WriteFile(filepath.Join(stale, "tid.log"), []byte("old"), 0o600); err != nil {
		t.Fatalf("写文件失败：%v", err)
	}
	renv := &RuntimeEnv{
		AdminSock: filepath.Join(dir, "app.sock"), DataDir: dir,
		DialTimeout: time.Second, TrimSocket: filepath.Join(dir, "no-trim.sock"), TrimApp: "WebSSH",
	}
	if _, err := newAdminMux(renv); err != nil {
		t.Fatalf("初始化失败：%v", err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("重启后留着的历史目录再也没有挂载者，应当被清理：%v", err)
	}
}
