package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"webssh/backend/internal/apierr"
	"webssh/backend/internal/logx"
	"webssh/backend/internal/secret"
	"webssh/backend/internal/store"
)

// ---------------------------------------------------------------------------
// 快捷指令（全局 + 会话专属）
//
// 存储：quickcmds/global.json 与 quickcmds/<sid>.json，都是「整表提交 / 整表返回」。
// 前端每次改动（增、删、排序）都把完整列表发回来，我们只做校验与落盘——这样排序、
// 去重这类规则只在前端实现一遍，后端不会多出一份「部分更新」的语义要对齐。
//
// **命令内容加密落盘**（文件版本 2，见 saveQuickCmds）：`content` 以
// `{"v":1,"ct":"<base64(nonce||ciphertext)>"}` 落盘；`id` / `name` / `auto` 仍是明文。
// 用户往「常用指令」里放的常常是 `mysql -uroot -p<密码>`、带 token 的 curl 这类东西，
// 而拿到磁盘快照/备份的人不该顺手读到它们（威胁模型与凭据那层完全一致，见
// internal/secret 的包注释）；名称是给人看的标签，加密它反而让文件没法排查。
//
// 加密边界只在**落盘这一层**：API 收发的始终是明文 content，前端完全不需要知道这件事。
//
// 第一轮留下的**明文**文件会被主动迁移（读到就加密重写、启动时扫一遍数据目录，
// 见 migrateQuickCmds / migrateAllQuickCmds）：不能等「用户下次保存」——没被打开过的会话的
// 表永远不会被保存，明文就一直躺在磁盘上。
// ---------------------------------------------------------------------------

// quickCmdMigrationPlaceholder 是「明文内容加密失败」时替换进去的文案（它自己也是密文落盘）。
//
// 为什么要一个可见的占位而不是清成空串：空内容在界面上看不出来被清过，用户会以为是自己记错了；
// 而这条内容已经拿不回来了（加密都失败的机器上，原明文也不该再留在磁盘上）。这是**落盘的数据**，
// 不是界面文案，所以不进 i18n 字典（而且字典是构建期冻结的）。
const quickCmdMigrationPlaceholder = "命令加密失败已清空，请重新添加"

// maxQuickCmds 是一张表的上限。前端已经把整张表渲染成一列按钮，几千条既没法用，
// 也会把响应体撑大；给一个明确的拒绝比默默接受更好。
const maxQuickCmds = 500

// quickCmd 是 QuickCmd（契约第 4.3 节）。
type quickCmd struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Content string `json:"content"`
	Auto    bool   `json:"auto"`
}

// quickCmdStored 是一条指令在磁盘上的形态（请求/响应里那一份是 quickCmd，content 是明文）。
type quickCmdStored struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Auto bool   `json:"auto"`
	// Content 是加密后的命令内容。nil 表示这条指令没有内容（sanitize 拒绝了空内容，正常
	// 数据里不会出现；手工编辑过的文件可能出现，那时照「没有内容」处理）。
	Content *secret.Secret `json:"content"`
	// plain / legacy 只在读到**版本 1**（明文）文件时有意义，见 UnmarshalJSON。
	// 两个字段都未导出 → 不参与 json.Marshal，也就不会把明文写回磁盘。
	plain  string
	legacy bool
}

// UnmarshalJSON 兼容第一轮留下的明文文件（`"content": "df -h"`）。
//
// 按 JSON 类型分流：字符串 = 版本 1 的明文，对象 = 版本 2 的密文信封——两种形态不会歧义。
// 读到明文会打上 `legacy` 标记，由 migrateQuickCmds 加密后重写（**不做**「读到就一直用明文、
// 等下次保存再说」那种懒迁移：没被打开过的会话的表永远不会被保存）。
func (c *quickCmdStored) UnmarshalJSON(data []byte) error {
	var raw struct {
		ID      string          `json:"id"`
		Name    string          `json:"name"`
		Auto    bool            `json:"auto"`
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	c.ID, c.Name, c.Auto, c.Content, c.plain, c.legacy = raw.ID, raw.Name, raw.Auto, nil, "", false
	v := bytes.TrimSpace(raw.Content)
	switch {
	case len(v) == 0 || bytes.Equal(v, []byte("null")):
		return nil
	case v[0] == '"':
		c.legacy = true
		return json.Unmarshal(v, &c.plain)
	default:
		// 交给 json 解信封（v / ct 两个字段）；内容不是字符串也不是对象（数字、数组……）
		// 时在这里报错，而不是把命令静默变成空串。
		return json.Unmarshal(v, &c.Content)
	}
}

type quickCmdsFile struct {
	Version int `json:"version"`
	// Commands 用指针切片没意义（null 与 [] 在前端都当空处理），统一在读取时归一成 []。
	Commands []quickCmdStored `json:"commands"`
}

// quickCmdAADField 是命令内容密文的 AAD 字段名：把**指令 id** 绑进去。
//
// secret 的 AAD 形态是 `webssh:v1:<field>:<sid>`：field 这一格放指令 id，sid 这一格放表的
// 作用域（全局 = 空串、会话专属 = 会话 id）。于是「同一张表里两条指令的密文互换位置」与
// 「把某个会话的表改名成 global.json」都会在解密时直接失败，而不是悄悄把 A 的命令接到 B 的
// 按钮上（按钮上写的是 B 的名字，点下去跑的是 A 的内容）。
func quickCmdAADField(id string) string { return "quickcmd:" + id }

// loadQuickCmdsFile 读一张表的磁盘形态，并**顺手做迁移**：文件里只要有第一轮的明文内容，
// 就地加密重写（WriteJSON = 同目录临时文件 + rename，原子替换）。
//
// 迁移失败（加密不了、或重写不进去）时**报错**，而不是「先把明文内容返回给用户、下次再说」：
// 报告出来的失败是可查的，静默保留明文则违背这条改动的全部意义。文件读不到（还没有指令）
// 按空表处理——那是正常状态，不是错误。
//
// 它（连同 saveQuickCmds / migrateAllQuickCmds）是 main 包的自由函数，不是 store.Store 的方法：
// quickCmd 与 apierr.Error 都属于 API 层，store 包只提供通用的 ReadJSON/WriteJSON 与路径；
// 加解密要主密钥，所以由调用方把 crypt 传进来。
func loadQuickCmdsFile(st *store.Store, crypt *secret.Cryptor, sid string) (quickCmdsFile, error) {
	var f quickCmdsFile
	if err := st.ReadJSON(st.QuickCmdsPath(sid), &f); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return quickCmdsFile{}, nil
		}
		return f, err
	}
	migrated, err := migrateQuickCmds(crypt, sid, &f)
	if err != nil {
		return f, err
	}
	if !migrated {
		return f, nil
	}
	if err := st.WriteJSON(st.QuickCmdsPath(sid), f, 0o600); err != nil {
		return f, fmt.Errorf("加密重写 %s 失败：%w", st.QuickCmdsPath(sid), err)
	}
	return f, nil
}

// migrateQuickCmds 把一张表里**第一轮的明文内容**加密成版本 2 的信封，返回是否发生了迁移。
//
// 规则：
//   - 已经是信封的条目**原样保留**（不重新加密：没必要换密文，也就少一次「解不开」的机会）；
//   - 明文内容加密成功 → 换成信封，内容与格式逐字节不变；
//   - 明文内容为空（空内容存不下来，Encrypt 返回 nil）或加密失败 → 换成**加密后的占位文案**
//     （quickCmdMigrationPlaceholder），并把 `auto` 关掉——一句中文提示不该被「点一下就自动
//     回车」当成命令跑到远端 shell 上；
//   - 连占位文案都加密不了（系统随机数不可用这种级别）→ 返回错误、**一个字节都不写**：
//     写不出密文，也不能为了「看起来迁移成功」而留下明文。
func migrateQuickCmds(crypt *secret.Cryptor, sid string, f *quickCmdsFile) (bool, error) {
	changed := false
	for i := range f.Commands {
		e := &f.Commands[i]
		if !e.legacy {
			continue
		}
		changed = true
		sec, err := crypt.Encrypt(quickCmdAADField(e.ID), sid, e.plain)
		if err != nil || sec == nil {
			// 这里是**唯一**会让用户丢内容的路径，所以原因必须可查（用户只看到占位文案）。
			if err == nil {
				// Encrypt("") 返回 nil：空内容存不下来（正常也不会有——第一轮的 sanitize 就
				// 拒绝了空内容，这里只可能是手工编辑过的文件）。
				logx.Warnf("快捷指令「%s」（%s，%s）的明文内容为空，已换成占位文案",
					e.Name, e.ID, scopeOf(sid))
			} else {
				logx.Warnf("快捷指令「%s」（%s，%s）的明文内容加密失败，已清空成占位文案：%v",
					e.Name, e.ID, scopeOf(sid), err)
			}
			placeholder, perr := crypt.Encrypt(quickCmdAADField(e.ID), sid, quickCmdMigrationPlaceholder)
			if perr != nil {
				return false, fmt.Errorf("加密占位内容失败：%w", perr)
			}
			if placeholder == nil {
				return false, errors.New("加密占位内容返回了空信封")
			}
			sec = placeholder
			e.Auto = false
		}
		e.Content, e.plain, e.legacy = sec, "", false
	}
	if changed {
		// 发生过迁移就一定按版本 2 落盘（哪怕这张表原本没写 version）。
		f.Version = 2
	}
	return changed, nil
}

// migrateAllQuickCmds 在启动时把数据目录里**所有**快捷指令表的明文内容加密一次。
//
// 为什么不能只在读取时迁移：前端只加载全局表与**当前会话**的表，别的会话的表可能几个月都不被
// 读一次——明文就一直躺在磁盘上。扫一遍目录才能兜住「数据目录里不再有明文命令内容」。
//
// 某个文件失败不拦住启动（返回第一个错误，调用方只记日志）：应用起不来比一条指令还是明文更糟，
// 而用户真的打开那张表时 loadQuickCmds 会再迁移一次并报出明确原因。
func migrateAllQuickCmds(st *store.Store, crypt *secret.Cryptor) error {
	scopes, err := st.QuickCmdsScopes()
	if err != nil {
		return fmt.Errorf("列出快捷指令表失败：%w", err)
	}
	var firstErr error
	for _, sid := range scopes {
		// 和读取走同一条路（读 → 迁移 → 需要时原子重写），迁移规则只有一处实现。
		if _, err := loadQuickCmdsFile(st, crypt, sid); err != nil {
			logx.Warnf("迁移快捷指令（%s）失败，打开那张表时会重试：%v", scopeOf(sid), err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

// loadQuickCmds 读一张表并把命令内容解成明文（`/api/quickcmds/list` 用）。
func loadQuickCmds(st *store.Store, crypt *secret.Cryptor, sid string) ([]quickCmd, *apierr.Error) {
	f, err := loadQuickCmdsFile(st, crypt, sid)
	if err != nil {
		return nil, apierr.Internal("读取快捷指令失败：%v", err)
	}
	out := make([]quickCmd, 0, len(f.Commands))
	for _, e := range f.Commands {
		content, err := decryptQuickCmdContent(crypt, sid, e)
		if err != nil {
			// 解密失败必须报错（与凭据那条纪律一致）：静默给一个空内容，用户看到的是
			// 「我的指令内容没了」，而真正的原因（换了 master.key / 密文被搬到了别的指令或
			// 表上）永远查不出来，而且他一点保存就会把服务端那份覆盖成空的。
			return nil, apierr.Internal("解密快捷指令「%s」（%s）的命令内容失败：%v", e.Name, e.ID, err)
		}
		out = append(out, quickCmd{ID: e.ID, Name: e.Name, Content: content, Auto: e.Auto})
	}
	return out, nil
}

// decryptQuickCmdContent 解出一条指令的命令内容。
//
// 走到这里的内容**一定是密文**：明文条目在 migrateQuickCmds 里已经全部被加密重写（失败的话
// loadQuickCmdsFile 直接报错）。`content` 是 null / 缺字段时按「没有内容」处理——那是手工编辑
// 过的文件，不是明文。
func decryptQuickCmdContent(crypt *secret.Cryptor, sid string, e quickCmdStored) (string, error) {
	if e.Content == nil {
		return "", nil
	}
	return crypt.Decrypt(quickCmdAADField(e.ID), sid, e.Content)
}

// saveQuickCmds 加密整张表并落盘（版本 2）。
//
// 每次保存都重新加密（GCM 每次的 nonce 都是新的）：整表重写本来就会换掉整个文件，没有
// 「保留原密文」的意义，而解密再加密的往返反而多一次失败机会。
func saveQuickCmds(st *store.Store, crypt *secret.Cryptor, sid string, cmds []quickCmd) *apierr.Error {
	stored := make([]quickCmdStored, 0, len(cmds))
	for _, c := range cmds {
		sec, err := crypt.Encrypt(quickCmdAADField(c.ID), sid, c.Content)
		if err != nil {
			return apierr.Internal("加密快捷指令「%s」的命令内容失败：%v", c.Name, err)
		}
		if sec == nil {
			// Encrypt 对空明文返回 nil，而空内容已经被 sanitize 拒掉；走到这里说明调用
			// 顺序被改坏了。别顺手写一条没有内容的指令进去。
			return apierr.Internal("快捷指令「%s」的命令内容为空，拒绝落盘", c.Name)
		}
		stored = append(stored, quickCmdStored{ID: c.ID, Name: c.Name, Auto: c.Auto, Content: sec})
	}
	if err := st.WriteJSON(st.QuickCmdsPath(sid), quickCmdsFile{Version: 2, Commands: stored}, 0o600); err != nil {
		return apierr.Internal("保存快捷指令失败：%v", err)
	}
	return nil
}

// sanitizeQuickCmds 校验并规整整张表：补齐缺失的 id、去重、trim 名称。
//
// 名称去空白，**命令内容原样保留**（只判「全是空白 = 空」）：内容里的前导/行尾空格与换行
// 都是命令的一部分（heredoc、多行脚本、`printf '  x'`），顺手 trim 就是把用户写下的东西
// 改掉——前端的 QuickCmdEditDialog 早就只 trim 名称、并明确注释了这件事，后端这一份 trim
// 属于两套口径，症状是「保存时看到的和重新打开后看到的不一样」。
//
// 自动补 id 而不是拒绝：新增一条指令时前端可能只发 {name, content, auto}，
// 为「没带 id」回一个 400 只会让用户看到一句没头没脑的报错。
func sanitizeQuickCmds(in []quickCmd) ([]quickCmd, *apierr.Error) {
	if len(in) > maxQuickCmds {
		return nil, apierr.TooLarge("快捷指令最多 %d 条（收到 %d 条）", maxQuickCmds, len(in))
	}
	out := make([]quickCmd, 0, len(in))
	seen := make(map[string]bool, len(in))
	for _, c := range in {
		c.Name = strings.TrimSpace(c.Name)
		if c.Name == "" || strings.TrimSpace(c.Content) == "" {
			return nil, apierr.Invalid("快捷指令需要名称与命令内容")
		}
		if c.ID == "" || !store.ValidID(c.ID) {
			c.ID = store.NewID()
		}
		if seen[c.ID] {
			// 同一 id 出现两次：保留先出现的那条（前端按 id 做列表 key，重复 id 会让渲染错乱）。
			continue
		}
		seen[c.ID] = true
		out = append(out, c)
	}
	return out, nil
}

func (m *AdminMux) quickCmdScope(sid string) (*store.Session, *apierr.Error) {
	if sid == "" {
		return nil, nil // 全局
	}
	if !store.ValidID(sid) {
		return nil, errSessionGone("会话不存在")
	}
	sc, ok := m.sessions.getConfig(sid)
	if !ok {
		return nil, errSessionGone("会话不存在（可能已被删除）：%s", sid)
	}
	return sc, nil
}

// handleQuickCmdsList 读取一张表（sid 为空 = 全局）。
func (m *AdminMux) handleQuickCmdsList(w http.ResponseWriter, r *http.Request) *apierr.Error {
	var req struct {
		SID string `json:"sid"`
	}
	if apiErr := m.decodeJSON(w, r, &req); apiErr != nil {
		return apiErr
	}
	if _, apiErr := m.quickCmdScope(req.SID); apiErr != nil {
		return apiErr
	}
	cmds, apiErr := loadQuickCmds(m.st, m.crypt, req.SID)
	if apiErr != nil {
		return apiErr
	}
	return apierr.Respond(w, map[string]any{"ok": true, "scope": scopeOf(req.SID), "commands": cmds})
}

// handleQuickCmdsSave 整表保存。
func (m *AdminMux) handleQuickCmdsSave(w http.ResponseWriter, r *http.Request) *apierr.Error {
	var req struct {
		SID      string     `json:"sid"`
		Commands []quickCmd `json:"commands"`
	}
	if apiErr := m.decodeJSON(w, r, &req); apiErr != nil {
		return apiErr
	}
	if _, apiErr := m.quickCmdScope(req.SID); apiErr != nil {
		return apiErr
	}
	cmds, apiErr := sanitizeQuickCmds(req.Commands)
	if apiErr != nil {
		return apiErr
	}
	if apiErr := saveQuickCmds(m.st, m.crypt, req.SID, cmds); apiErr != nil {
		return apiErr
	}
	return apierr.Respond(w, map[string]any{"ok": true, "scope": scopeOf(req.SID), "commands": cmds})
}

// scopeOf 是响应里的 scope 字段：`global` / `session`（前端只用它区分两张表）。
func scopeOf(sid string) string {
	if sid == "" {
		return "global"
	}
	return "session"
}
