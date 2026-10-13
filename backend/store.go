package main

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
)

// ---------------------------------------------------------------------------
// 数据目录（契约第 3 节）
//
//	data/
//	├── master.key                  0600，AES-256-GCM 主密钥（见 crypto.go）
//	├── known_hosts.json            已信任的主机密钥（TOFU）
//	├── sessions/<sid>.json         远程会话配置（凭据字段是密文）
//	├── history/<sid>/<tid>.log     终端历史（会话存活期间；会话关闭时删整个目录）
//	└── quickcmds/{global,<sid>}.json  全局 / 会话专属快捷指令
//
// 目录里**没有** meta.json：契约把它标为「可选（终端标题等）」。终端是纯内存态
// （进程重启即消失），把标题另存一份没有任何读取者，属于死代码，故不实现。
// ---------------------------------------------------------------------------

const (
	// idLen 是新生成的 id（会话 / 终端 / bootID）的字符长度：16 字节随机数的 hex。
	idLen = 32
)

type store struct {
	dir string
	// mu 串行化全部落盘：写文件一律 tmp + rename（原子替换），进程内并发靠这把锁排队。
	// 不追求跨进程安全——两个实例同时用同一个数据目录会互相覆盖，AGENTS.md 里明确禁止。
	mu sync.Mutex
}

func newStore(dir string) (*store, error) {
	s := &store{dir: dir}
	for _, d := range []string{s.sessionsDir(), s.historyRoot(), s.quickcmdsDir()} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, fmt.Errorf("创建数据目录 %s 失败：%w", d, err)
		}
	}
	return s, nil
}

func (s *store) root() string           { return s.dir }
func (s *store) sessionsDir() string    { return filepath.Join(s.dir, "sessions") }
func (s *store) historyRoot() string    { return filepath.Join(s.dir, "history") }
func (s *store) quickcmdsDir() string   { return filepath.Join(s.dir, "quickcmds") }
func (s *store) knownHostsPath() string { return filepath.Join(s.dir, "known_hosts.json") }

func (s *store) sessionPath(id string) string { return filepath.Join(s.sessionsDir(), id+".json") }
func (s *store) historyDir(sid string) string { return filepath.Join(s.historyRoot(), sid) }

// newID 生成一个随机 id（会话 / 终端 / bootID / 快捷指令）。
//
// 字符集是 [0-9a-f]，正好落在前端 TID_PAYLOAD_RE（`\x1b]tid;([0-9a-zA-Z_-]+)\x07`）
// 允许的范围内——id 里出现分号或 ESC 会把控制帧提前截断。
func newID() string {
	var b [idLen / 2]byte
	if _, err := rand.Read(b[:]); err != nil {
		// 随机数不可用是极罕见的系统级故障；用时间戳兜底保证「能起来」，但一定要记日志。
		warnf("生成随机 id 失败（%v），已回退时间戳", err)
		return strings.ReplaceAll(time.Now().Format("20060102150405.000000000"), ".", "")
	}
	return hex.EncodeToString(b[:])
}

// validID 校验来自请求的 id：只允许 [0-9a-zA-Z_-]。
//
// 这条校验是**路径安全的边界**：id 会被拼进 `sessions/<sid>.json`、`history/<sid>/`，
// 放任 `../../etc/passwd` 这种输入就会变成任意文件读写。
func validID(id string) bool {
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

// readJSON 读一个 JSON 文件；文件不存在时返回 os.ErrNotExist（调用方决定是否容忍）。
func (s *store) readJSON(path string, out any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("解析 %s 失败：%w", path, err)
	}
	return nil
}

// writeJSON 原子写一个 JSON 文件：先写同目录的临时文件再 rename。
//
// 直接原地重写会在「写到一半进程被杀 / 磁盘写满」时留下半截 JSON，下次启动直接解析
// 失败——会话配置是用户手工建立的数据，丢一份就是丢一个会话。rename 在同一文件系统
// 上是原子的：要么旧内容，要么新内容。
func (s *store) writeJSON(path string, v any, mode os.FileMode) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writeJSONLocked(path, v, mode)
}

func (s *store) writeJSONLocked(path string, v any, mode os.FileMode) error {
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

// sessionConfig 是 sessions/<sid>.json 的形态（契约第 3 节 + 11.1）。
type sessionConfig struct {
	Version    int     `json:"version"`
	ID         string  `json:"id"`
	Name       string  `json:"name"`
	Host       string  `json:"host"`
	Port       int     `json:"port"`
	User       string  `json:"user"`
	AuthType   string  `json:"authType"`
	Password   *secret `json:"password"`
	PrivateKey *secret `json:"privateKey"`
	Passphrase *secret `json:"passphrase"`
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

// credentialsSaved 报告这个会话是否把凭据加密落盘（缺省 = 是）。
func (sc *sessionConfig) credentialsSaved() bool {
	return sc.CredentialSaved == nil || *sc.CredentialSaved
}

// identity 是会话身份：`user@host:port`（端口默认 22 参与比较）。
//
// 同一身份只允许一个会话（契约第 3 节）：否则用户会把同一台机器建两遍，两个卡片看起来
// 一样，凭据却可能不同，排查「为什么这台机器连不上」时完全对不上号。
func (sc *sessionConfig) identity() string {
	return fmt.Sprintf("%s@%s:%d", sc.User, sc.Host, sc.Port)
}

func (s *store) loadSession(id string) (*sessionConfig, error) {
	if !validID(id) {
		return nil, os.ErrNotExist
	}
	var sc sessionConfig
	if err := s.readJSON(s.sessionPath(id), &sc); err != nil {
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

// listSessions 读取全部会话配置。单个文件损坏时**跳过并在日志里记一条**，不整体失败：
// 一个坏文件不该让用户看不到其余所有会话（也就没法删掉那个坏的了）。
func (s *store) listSessions() ([]*sessionConfig, error) {
	entries, err := os.ReadDir(s.sessionsDir())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	out := make([]*sessionConfig, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(e.Name(), ".json")
		sc, err := s.loadSession(id)
		if err != nil {
			warnf("读取会话配置 %s 失败，已跳过：%v", e.Name(), err)
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

func (s *store) saveSession(sc *sessionConfig) error {
	sc.Version = 1
	return s.writeJSON(s.sessionPath(sc.ID), sc, 0o600)
}

func (s *store) deleteSession(id string) error {
	if !validID(id) {
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

// createHistoryFile 打开（或创建）一个终端的历史文件。
//
// **必须** O_RDWR：只写打开（O_WRONLY）会让挂载时的回放与 /api/terminals/history 的
// 读取全部 EBADF 失败——历史上踩过这个坑，症状是「终端能连上、但历史永远是空的」。
// O_APPEND 保证并发写入只追加到末尾。
func (s *store) createHistoryFile(sid, tid string) (*os.File, error) {
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

// purgeStaleHistory 清掉上次进程留下的历史目录。
//
// 历史文件的语义是「会话存活期间」：进程重启后所有内存态终端都不复存在，这些文件再也
// 不会被任何客户端挂载（前端拿到的会话状态是空的），留着只会一直占盘。放在启动时做，
// 而不是退出时做：进程可能是被 SIGKILL 掉的，退出路径不一定会跑到。
func (s *store) purgeStaleHistory() {
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
		warnf("清理了 %d 个上次运行留下的终端历史目录", removed)
	}
}

// removeHistoryDir 删除一个会话的历史目录（会话关闭时调用）。
func (s *store) removeHistoryDir(sid string) {
	if !validID(sid) {
		return
	}
	if err := os.RemoveAll(s.historyDir(sid)); err != nil {
		warnf("删除历史目录 %s 失败：%v", s.historyDir(sid), err)
	}
}

// ---------------------------------------------------------------------------
// 快捷指令
// ---------------------------------------------------------------------------

func (s *store) quickCmdsPath(sid string) string {
	if sid == "" {
		return filepath.Join(s.quickcmdsDir(), "global.json")
	}
	return filepath.Join(s.quickcmdsDir(), sid+".json")
}

func (s *store) deleteQuickCmds(sid string) {
	if sid == "" || !validID(sid) {
		return
	}
	if err := os.Remove(s.quickCmdsPath(sid)); err != nil && !errors.Is(err, os.ErrNotExist) {
		warnf("删除会话快捷指令 %s 失败：%v", sid, err)
	}
}
