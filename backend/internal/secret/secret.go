// Package secret 负责凭据的落盘加密（AES-256-GCM）与主密钥管理。
//
// **威胁模型（如实写清，别夸大）**：这一层加密防的是「拿到磁盘快照 / 备份 / 卷拷贝的人
// 直接读到明文密码」。它**不防**「已经能以同权限读取数据目录的攻击者」——master.key 就
// 躺在密文旁边，能读密文的人多半也能读密钥。
// 想避免密钥与密文同放，用 WEBSSH_MASTER_KEY 从外部注入（由启动脚本 / 容器 secret 提供），
// 这样数据目录的备份里就没有可用密钥。
//
// AAD 绑定 `webssh:v1:<field>:<sid>`：把某个字段的密文剪到另一个字段或另一个会话上，
// 解密会直接失败（GCM 的认证标签覆盖 AAD），而不是悄悄用一个错误的凭据去连别的机器。
package secret

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	// masterKeyFile 是主密钥文件名（数据目录根下）。
	masterKeyFile = "master.key"
	// masterKeyLen 是 AES-256 的密钥长度。
	masterKeyLen = 32
	// secretVersion 是密文信封的版本号，为空时按 1 处理。
	secretVersion = 1
)

// 凭据字段名：既是落盘字段名，也是 AAD 的第二个成分。**改这里会让已存的密文全部解不开**
// （AAD 不匹配 → GCM 认证失败），所以只在协议版本升级（secretVersion）时一起动。
const (
	FieldPassword   = "password"
	FieldPrivateKey = "privateKey"
	FieldPassphrase = "passphrase"
)

// Secret 是凭据字段的落盘形态：{"v":1,"ct":"<base64(nonce||ciphertext)>"}。
type Secret struct {
	V  int    `json:"v"`
	CT string `json:"ct"`
}

// Cryptor 是加密器：只持有 AEAD，无状态，可并发使用（cipher.AEAD 本身并发安全）。
type Cryptor struct {
	aead cipher.AEAD
}

// Load 取得主密钥并构造 AEAD。
//
// 密钥来源二选一：WEBSSH_MASTER_KEY（外部提供，优先）或数据目录里的 master.key。
// 环境变量给了但**解不出来必须直接失败**，绝不「回退到本地文件」：那种静默回退的表现是
// 用户换了密钥后旧会话全部解密失败，而进程却正常启动、只是每个会话都报「密码失效」。
func Load(dataDir, envKey string) (*Cryptor, error) {
	var key []byte
	if strings.TrimSpace(envKey) != "" {
		k, err := decodeMasterKey(envKey)
		if err != nil {
			return nil, fmt.Errorf("WEBSSH_MASTER_KEY 不可用：%w", err)
		}
		key = k
	} else {
		k, err := loadOrCreateMasterKey(filepath.Join(dataDir, masterKeyFile))
		if err != nil {
			return nil, err
		}
		key = k
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("初始化 AES 失败：%w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("初始化 AES-GCM 失败：%w", err)
	}
	return &Cryptor{aead: aead}, nil
}

// decodeMasterKey 解析外部提供的主密钥：32 字节的 base64（标准/URL 编码都认）或 hex。
func decodeMasterKey(raw string) ([]byte, error) {
	s := strings.TrimSpace(raw)
	if len(s) == hex.EncodedLen(masterKeyLen) {
		if b, err := hex.DecodeString(s); err == nil {
			return b, nil
		}
	}
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if b, err := enc.DecodeString(s); err == nil && len(b) == masterKeyLen {
			return b, nil
		}
	}
	return nil, fmt.Errorf("需要 %d 字节的 base64 或 hex 编码密钥", masterKeyLen)
}

// loadOrCreateMasterKey 读取或生成 master.key（0600）。
func loadOrCreateMasterKey(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err == nil {
		if len(data) != masterKeyLen {
			return nil, fmt.Errorf("%s 长度不是 %d 字节（疑似被截断或损坏），请恢复备份或删除它重新生成（删除后已存的密码/私钥将无法解密）", path, masterKeyLen)
		}
		return data, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("读取 %s 失败：%w", path, err)
	}

	key := make([]byte, masterKeyLen)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("生成主密钥失败：%w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("创建数据目录失败：%w", err)
	}
	// O_EXCL：并发/重复启动时不要互相覆盖密钥（覆盖等于把已存的密码全部变成解不开的密文）。
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return loadOrCreateMasterKey(path)
		}
		return nil, fmt.Errorf("创建 %s 失败：%w", path, err)
	}
	if _, err := f.Write(key); err != nil {
		f.Close()
		os.Remove(path)
		return nil, fmt.Errorf("写入主密钥失败：%w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(path)
		return nil, fmt.Errorf("写入主密钥失败：%w", err)
	}
	// 飞牛存储卷上 O_CREAT 的模式位不可靠（会落成 0000），必须显式 chmod 一次。
	if err := os.Chmod(path, 0o600); err != nil {
		return nil, fmt.Errorf("设置主密钥权限失败：%w", err)
	}
	return key, nil
}

// aad 是本项目的附加认证数据：版本 + 字段名 + 会话 id。
func (c *Cryptor) aad(field, sid string) []byte {
	return []byte(fmt.Sprintf("webssh:v%d:%s:%s", secretVersion, field, sid))
}

// Encrypt 加密一个凭据字段。明文为空串时返回 nil（表示「没有这个凭据」）。
func (c *Cryptor) Encrypt(field, sid, plain string) (*Secret, error) {
	if plain == "" {
		return nil, nil
	}
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("生成随机数失败：%w", err)
	}
	ct := c.aead.Seal(nil, nonce, []byte(plain), c.aad(field, sid))
	return &Secret{V: secretVersion, CT: base64.StdEncoding.EncodeToString(append(nonce, ct...))}, nil
}

// Decrypt 解密一个凭据字段。nil / 空密文 = 该字段没有值（返回 ""）。
//
// 解密失败**必须**把错误带出去：静默当成空密码的表现是「密码莫名其妙失效了」，
// 而真正的原因（换了 master.key / 密文被搬过字段）永远查不出来。
func (c *Cryptor) Decrypt(field, sid string, s *Secret) (string, error) {
	if s == nil || strings.TrimSpace(s.CT) == "" {
		return "", nil
	}
	if s.V != 0 && s.V != secretVersion {
		return "", fmt.Errorf("凭据信封版本 v=%d 不受支持", s.V)
	}
	raw, err := base64.StdEncoding.DecodeString(s.CT)
	if err != nil {
		return "", fmt.Errorf("凭据密文不是合法 base64：%w", err)
	}
	ns := c.aead.NonceSize()
	if len(raw) < ns+c.aead.Overhead() {
		return "", errors.New("凭据密文长度不足，文件可能被截断")
	}
	plain, err := c.aead.Open(nil, raw[:ns], raw[ns:], c.aad(field, sid))
	if err != nil {
		return "", fmt.Errorf("凭据解密失败（主密钥变了或密文被搬到了别的字段/会话）：%w", err)
	}
	return string(plain), nil
}
