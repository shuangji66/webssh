package secret

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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
	c, err := Load(t.TempDir(), base64.StdEncoding.EncodeToString(key))
	if err != nil {
		t.Fatalf("初始化失败：%v", err)
	}
	if sec, err := c.Encrypt(FieldPassword, "sid", ""); err != nil || sec != nil {
		t.Errorf("空明文不该产生密文：%v %v", sec, err)
	}
	if plain, err := c.Decrypt(FieldPassword, "sid", nil); err != nil || plain != "" {
		t.Errorf("nil 密文应当解出空串：%q %v", plain, err)
	}
	// 密文里的 base64 不是合法 base64 时也要报错
	if _, err := c.Decrypt(FieldPassword, "sid", &Secret{V: 1, CT: "!!!"}); err == nil {
		t.Error("非法 base64 应当报错")
	}
}

// TestMasterKeyFile 校验 master.key 的生成、长度与权限。
func TestMasterKeyFile(t *testing.T) {
	dir := t.TempDir()
	c1, err := Load(dir, "")
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
	c2, err := Load(dir, "")
	if err != nil {
		t.Fatalf("再次加载失败：%v", err)
	}
	sec, err := c1.Encrypt(FieldPassword, "sid", "hello")
	if err != nil {
		t.Fatalf("加密失败：%v", err)
	}
	if plain, err := c2.Decrypt(FieldPassword, "sid", sec); err != nil || plain != "hello" {
		t.Fatalf("同一密钥应当能解密：%q %v", plain, err)
	}
	// 长度不对的密钥文件必须让启动失败（不能悄悄重新生成：那会把用户的密码全变成废密文）
	if err := os.WriteFile(path, []byte("short"), 0o600); err != nil {
		t.Fatalf("写坏文件失败：%v", err)
	}
	if _, err := Load(dir, ""); err == nil {
		t.Fatal("坏的 master.key 应当被拒绝")
	}
}
