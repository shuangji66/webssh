// Package store 是数据目录的持久化层（契约第 3 节）。
//
//	data/
//	├── master.key                  0600，AES-256-GCM 主密钥（见 internal/secret）
//	├── known_hosts.json            已信任的主机密钥（TOFU）
//	├── sessions/<sid>.json         远程会话配置（凭据字段是密文）
//	├── history/<sid>/<tid>.log     终端历史（会话存活期间；会话关闭时删整个目录）
//	├── quickcmds/{global,<sid>}.json  全局 / 会话专属快捷指令
//	└── quickaccess/<sid>.json      会话的快捷访问目录路径（第二十轮，见 main 包的 quickaccess.go）
//
// 目录里**没有** meta.json：契约把它标为「可选（终端标题等）」。终端是纯内存态
// （进程重启即消失），把标题另存一份没有任何读取者，属于死代码，故不实现。
//
// 本包只负责「配置怎么落盘」；会话的业务语义（打开/关闭/凭据解析）在 main 包。
package store

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"webssh/backend/internal/logx"
	"webssh/backend/internal/secret"
)

const (
	// idLen 是新生成的 id（会话 / 终端 / bootID）的字符长度：16 字节随机数的 hex。
	idLen = 32
)

// Store 是数据目录的句柄。
type Store struct {
	dir string
	// mu 串行化全部落盘：写文件一律 tmp + rename（原子替换），进程内并发靠这把锁排队。
	// 不追求跨进程安全——两个实例同时用同一个数据目录会互相覆盖，AGENTS.md 里明确禁止。
	mu sync.Mutex
}

// New 准备数据目录并返回句柄（缺目录会建出来）。
func New(dir string) (*Store, error) {
	s := &Store{dir: dir}
	for _, d := range []string{s.sessionsDir(), s.historyRoot(), s.quickcmdsDir(), s.quickAccessDir()} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, fmt.Errorf("创建数据目录 %s 失败：%w", d, err)
		}
	}
	return s, nil
}

func (s *Store) sessionsDir() string    { return filepath.Join(s.dir, "sessions") }
func (s *Store) historyRoot() string    { return filepath.Join(s.dir, "history") }
func (s *Store) quickcmdsDir() string   { return filepath.Join(s.dir, "quickcmds") }
func (s *Store) quickAccessDir() string { return filepath.Join(s.dir, "quickaccess") }
func (s *Store) knownHostsPath() string { return filepath.Join(s.dir, "known_hosts.json") }

// SessionsDir 暴露会话配置目录（测试与诊断用）。
func (s *Store) SessionsDir() string { return s.sessionsDir() }

// HistoryRoot 暴露历史根目录。
func (s *Store) HistoryRoot() string { return s.historyRoot() }

// KnownHostsPath 是已知主机密钥文件的路径（hostkeys.go 读写它）。
func (s *Store) KnownHostsPath() string { return s.knownHostsPath() }

// SessionPath 是某个会话配置的文件路径。
func (s *Store) SessionPath(id string) string { return s.sessionPath(id) }

// HistoryDir 是某个会话的历史目录。
func (s *Store) HistoryDir(sid string) string { return s.historyDir(sid) }

func (s *Store) sessionPath(id string) string { return filepath.Join(s.sessionsDir(), id+".json") }
func (s *Store) historyDir(sid string) string { return filepath.Join(s.historyRoot(), sid) }

// NewID 生成一个随机 id（会话 / 终端 / bootID / 快捷指令）。
//
// 字符集是 [0-9a-f]，正好落在前端 TID_PAYLOAD_RE（`\x1b]tid;([0-9a-zA-Z_-]+)\x07`）
// 允许的范围内——id 里出现分号或 ESC 会把控制帧提前截断。
func NewID() string {
	var b [idLen / 2]byte
	if _, err := rand.Read(b[:]); err != nil {
		// 随机数不可用是极罕见的系统级故障；用时间戳兜底保证「能起来」，但一定要记日志。
		logx.Warnf("生成随机 id 失败（%v），已回退时间戳", err)
		return strings.ReplaceAll(time.Now().Format("20060102150405.000000000"), ".", "")
	}
	return hex.EncodeToString(b[:])
}

// ValidID 校验来自请求的 id：只允许 [0-9a-zA-Z_-]。
//
// 这条校验是**路径安全的边界**：id 会被拼进 `sessions/<sid>.json`、`history/<sid>/`，
// 放任 `../../etc/passwd` 这种输入就会变成任意文件读写。
func ValidID(id string) bool {
	if id == "" || len(id) > 64 {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c == '_', c == '-':
		default:
			return false
		}
	}
	return true
}

// ReadJSON 读一个 JSON 文件；文件不存在时返回 os.ErrNotExist（调用方决定是否容忍）。
func (s *Store) ReadJSON(path string, out any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("解析 %s 失败：%w", path, err)
	}
	return nil
}

// WriteJSON 原子写一个 JSON 文件：先写同目录的临时文件再 rename。
//
// 直接原地重写会在「写到一半进程被杀 / 磁盘写满」时留下半截 JSON，下次启动直接解析
// 失败——会话配置是用户手工建立的数据，丢一份就是丢一个会话。rename 在同一文件系统
// 上是原子的：要么旧内容，要么新内容。
func (s *Store) WriteJSON(path string, v any, mode os.FileMode) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writeJSONLocked(path, v, mode)
}

func (s *Store) writeJSONLocked(path string, v any, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	// 显式 chmod：飞牛存储卷上 O_CREAT 的模式位可能被忽略，会落成 0000（连自己都读不回来）。
	if err := os.Chmod(tmpName, mode); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}

// ---------------------------------------------------------------------------
// 会话配置
// ---------------------------------------------------------------------------

// Session 是 sessions/<sid>.json 的形态（契约第 3 节 + 11.1）。
type Session struct {
	Version    int            `json:"version"`
	ID         string         `json:"id"`
	Name       string         `json:"name"`
	Host       string         `json:"host"`
	Port       int            `json:"port"`
	User       string         `json:"user"`
	AuthType   string         `json:"authType"`
	Password   *secret.Secret `json:"password"`
	PrivateKey *secret.Secret `json:"privateKey"`
	Passphrase *secret.Secret `json:"passphrase"`
	// CredentialSaved 为 false 时上面三个字段**一律为 null**：凭据不落盘，只在该会话
	// 打开期间留在内存里（见 credentials.go）。
	//
	// 用指针而不是 bool：第一轮存下的配置里根本没有这个字段，它们都是「保存凭据」的。
	// 把缺省当成 false 会让所有老会话在升级后突然要求重新输入密码——那是最糟的一种
	// 「升级即故障」。nil 一律按 true 解释；omitempty 让 true 时不写这个字段，保持
	// 磁盘上的文件形态与老版本一致。
	CredentialSaved *bool  `json:"credentialSaved,omitempty"`
	KeyFingerprint  string `json:"keyFingerprint"`
	Note            string `json:"note"`
	CreatedAt       string `json:"createdAt"`
	UpdatedAt       string `json:"updatedAt"`
	LastUsedAt      string `json:"lastUsedAt,omitempty"`
}

// SessionInput 是新增 / 编辑会话的输入。
//
// 凭据字段用指针：`null`/缺省 = 保留已存值，空字符串 = 清除该字段。
// 前端保证「不修改时字段根本不发」（见契约第 4.3 节），这里把两种语义分开才能照做。
type SessionInput struct {
	ID         string  `json:"id"`
	Name       *string `json:"name"`
	Host       string  `json:"host"`
	Port       int     `json:"port"`
	User       string  `json:"user"`
	AuthType   string  `json:"authType"`
	Password   *string `json:"password"`
	PrivateKey *string `json:"privateKey"`
	Passphrase *string `json:"passphrase"`
	Note       *string `json:"note"`
	// SaveCredential 对应表单里的「保存凭据」开关（契约 11.1）：false = 三个凭据字段
	// 一概不落盘，凭据只在该会话打开期间留在内存里。
	//
	// 缺省语义按「新建 / 更新」分开：新建缺省 = true（与第一轮一致）；更新缺省 = 保持原值
	// ——前端改会话名（home.renameSession）时只发 name/host/user/authType，若把缺省当 true，
	// 一次改名就把「不保存凭据」的会话改成「保存凭据」（而它根本没有凭据可存），下次 open
	// 会从 credential_required 退化成 no_credential。
	SaveCredential *bool `json:"saveCredential"`
}

// SessionView 是「远程会话配置」的对外形态，**不含任何明文凭据**。
type SessionView struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Host          string `json:"host"`
	Port          int    `json:"port"`
	User          string `json:"user"`
	AuthType      string `json:"authType"`
	HasPassword   bool   `json:"hasPassword"`
	HasPrivateKey bool   `json:"hasPrivateKey"`
	// CredentialSaved 为 false 表示这份配置里没有任何凭据（前端据此标注「不保存凭据」，
	// 并在打开时先弹凭据输入框）。契约 11.1：字段必须下发。
	CredentialSaved bool   `json:"credentialSaved"`
	KeyFingerprint  string `json:"keyFingerprint,omitempty"`
	Note            string `json:"note"`
	CreatedAt       string `json:"createdAt"`
	UpdatedAt       string `json:"updatedAt"`
	LastUsedAt      string `json:"lastUsedAt,omitempty"`
}

// CredentialsSaved 报告这个会话是否把凭据加密落盘（缺省 = 是）。
func (sc *Session) CredentialsSaved() bool {
	return sc.CredentialSaved == nil || *sc.CredentialSaved
}

// Identity 是会话身份：`user@host:port`（端口默认 22 参与比较）。
//
// 同一身份只允许一个会话（契约第 3 节）：否则用户会把同一台机器建两遍，两个卡片看起来
// 一样，凭据却可能不同，排查「为什么这台机器连不上」时完全对不上号。
func (sc *Session) Identity() string {
	return fmt.Sprintf("%s@%s:%d", sc.User, sc.Host, sc.Port)
}

// View 转成对外的 SessionView（不含任何凭据）。
func (sc *Session) View() SessionView {
	return SessionView{
		ID:              sc.ID,
		Name:            sc.Name,
		Host:            sc.Host,
		Port:            sc.Port,
		User:            sc.User,
		AuthType:        sc.AuthType,
		HasPassword:     sc.Password != nil,
		HasPrivateKey:   sc.PrivateKey != nil,
		CredentialSaved: sc.CredentialsSaved(),
		KeyFingerprint:  sc.KeyFingerprint,
		Note:            sc.Note,
		CreatedAt:       sc.CreatedAt,
		UpdatedAt:       sc.UpdatedAt,
		LastUsedAt:      sc.LastUsedAt,
	}
}

// DisplayName 是配置对外的**展示名**：没起名字（或已被用户清空）就退回 `user@host`，
// 与前端 sessionLabel 的口径一致。只用于文案——字段本身允许为空。
func (sc *Session) DisplayName() string {
	if strings.TrimSpace(sc.Name) != "" {
		return sc.Name
	}
	return sc.User + "@" + sc.Host
}

// LoadSession 读取一份会话配置。
func (s *Store) LoadSession(id string) (*Session, error) {
	if !ValidID(id) {
		return nil, os.ErrNotExist
	}
	var sc Session
	if err := s.ReadJSON(s.sessionPath(id), &sc); err != nil {
		return nil, err
	}
	if sc.ID == "" {
		sc.ID = id
	}
	if sc.Port == 0 {
		sc.Port = 22
	}
	return &sc, nil
}

// ListSessions 读取全部会话配置。单个文件损坏时**跳过并在日志里记一条**，不整体失败：
// 一个坏文件不该让用户看不到其余所有会话（也就没法删掉那个坏的了）。
func (s *Store) ListSessions() ([]*Session, error) {
	entries, err := os.ReadDir(s.sessionsDir())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	out := make([]*Session, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(e.Name(), ".json")
		sc, err := s.LoadSession(id)
		if err != nil {
			logx.Warnf("读取会话配置 %s 失败，已跳过：%v", e.Name(), err)
			continue
		}
		out = append(out, sc)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt != out[j].CreatedAt {
			return out[i].CreatedAt < out[j].CreatedAt
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

// SaveSession 原子写入一份会话配置（0600）。
func (s *Store) SaveSession(sc *Session) error {
	sc.Version = 1
	return s.WriteJSON(s.sessionPath(sc.ID), sc, 0o600)
}

// DeleteSession 删除一份会话配置（文件不存在不算失败）。
func (s *Store) DeleteSession(id string) error {
	if !ValidID(id) {
		return os.ErrNotExist
	}
	err := os.Remove(s.sessionPath(id))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// ---------------------------------------------------------------------------
// 终端历史
// ---------------------------------------------------------------------------

// CreateHistoryFile 打开（或创建）一个终端的历史文件。
//
// **必须** O_RDWR：只写打开（O_WRONLY）会让挂载时的回放与 /api/terminals/history 的
// 读取全部 EBADF 失败——历史上踩过这个坑，症状是「终端能连上、但历史永远是空的」。
// O_APPEND 保证并发写入只追加到末尾。
func (s *Store) CreateHistoryFile(sid, tid string) (*os.File, error) {
	dir := s.historyDir(sid)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, tid+".log")
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	return f, nil
}

// PurgeStaleHistory 清掉上次进程留下的历史目录。
//
// 历史文件的语义是「会话存活期间」：进程重启后所有内存态终端都不复存在，这些文件再也
// 不会被任何客户端挂载（前端拿到的会话状态是空的），留着只会一直占盘。放在启动时做，
// 而不是退出时做：进程可能是被 SIGKILL 掉的，退出路径不一定会跑到。
func (s *Store) PurgeStaleHistory() {
	entries, err := os.ReadDir(s.historyRoot())
	if err != nil {
		return
	}
	var removed int
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if err := os.RemoveAll(filepath.Join(s.historyRoot(), e.Name())); err == nil {
			removed++
		}
	}
	if removed > 0 {
		logx.Warnf("清理了 %d 个上次运行留下的终端历史目录", removed)
	}
}

// RemoveHistoryDir 删除一个会话的历史目录（会话关闭时调用）。
func (s *Store) RemoveHistoryDir(sid string) {
	if !ValidID(sid) {
		return
	}
	if err := os.RemoveAll(s.historyDir(sid)); err != nil {
		logx.Warnf("删除历史目录 %s 失败：%v", s.historyDir(sid), err)
	}
}

// ---------------------------------------------------------------------------
// 快捷指令
// ---------------------------------------------------------------------------

// QuickCmdsPath 是某个会话（sid 为空 = 全局）的快捷指令文件路径。
func (s *Store) QuickCmdsPath(sid string) string {
	if sid == "" {
		return filepath.Join(s.quickcmdsDir(), "global.json")
	}
	return filepath.Join(s.quickcmdsDir(), sid+".json")
}

// DeleteQuickCmds 删除某个会话的专属快捷指令（会话被删时调用）。
func (s *Store) DeleteQuickCmds(sid string) {
	if sid == "" || !ValidID(sid) {
		return
	}
	if err := os.Remove(s.QuickCmdsPath(sid)); err != nil && !errors.Is(err, os.ErrNotExist) {
		logx.Warnf("删除会话快捷指令 %s 失败：%v", sid, err)
	}
}

// QuickCmdsScopes 列出磁盘上**已存在**的快捷指令表（"" = 全局，其余是会话 id）。
//
// 只给「启动时把第一轮留下的明文命令内容加密」那次一次性迁移用（见 main 包的
// migrateAllQuickCmds）：前端只会加载全局表与**当前会话**的表，别的会话的表可能几个月都不被
// 读一次，只靠读取时迁移的话，明文就会一直躺在磁盘上。目录不存在（还没有任何指令）返回空。
//
// **文件名必须恰好是 `global.json` 或 `<合法会话 id>.json`**：作用域是密文 AAD 的一部分，
// 名字不合法就不是一张能解释的表（见下面的过滤与告警）。
func (s *Store) QuickCmdsScopes() ([]string, error) {
	entries, err := os.ReadDir(s.quickcmdsDir())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		// WriteJSON 的临时文件是 `<名字>.json.tmp-<随机>`（扩展名不是 .json），这里天然被
		// 过滤掉——它是「上次写到一半被杀」的残渣，不是一张表。
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		// 不能用「削掉 .json 后缀就算作用域」那种写法：名字叫 `.json` 时会削出空串（= 全局），
		// 这个文件于是被当成全局表去迁移（而真正的 global.json 反过来被漏掉）；`a b.json` /
		// `a:b.json` 这类名字又会带着空格/冒号被当成会话 id 参与迁移，让 AAD 产生歧义（作用域
		// 与指令 id 之间只有一个冒号当分隔符）。只有手工编辑数据目录才造得出这些名字，跳过并
		// 记一条日志——它不是一张我们能解释的表。
		name := strings.TrimSuffix(e.Name(), ".json")
		var sid string
		switch {
		case name == "global":
			sid = ""
		case ValidID(name):
			sid = name
		default:
			logx.Warnf("快捷指令目录里的 %s 文件名不是合法作用域（只认 global.json 或 <会话 id>.json），已跳过", e.Name())
			continue
		}
		out = append(out, sid)
	}
	sort.Strings(out)
	return out, nil
}

// ---------------------------------------------------------------------------
// 快捷访问（第二十轮）
// ---------------------------------------------------------------------------

// QuickAccessPath 是某个会话的快捷访问路径文件。
//
// **没有全局表**（与快捷指令不同）：路径是远程主机上的绝对路径，跨会话共享会把 A 主机的
// `/vol1/1000` 挂到 B 会话的弹窗里，点开只能报错。会话身份（user@host:port）去重保证
// 「一个主机一个会话」，因此「按会话」就等于「按主机」。
func (s *Store) QuickAccessPath(sid string) string {
	return filepath.Join(s.quickAccessDir(), sid+".json")
}

// DeleteQuickAccess 删除某个会话的快捷访问表（会话被删时调用）。
func (s *Store) DeleteQuickAccess(sid string) {
	if sid == "" || !ValidID(sid) {
		return
	}
	if err := os.Remove(s.QuickAccessPath(sid)); err != nil && !errors.Is(err, os.ErrNotExist) {
		logx.Warnf("删除会话快捷访问 %s 失败：%v", sid, err)
	}
}
