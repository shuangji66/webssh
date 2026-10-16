package main

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path"
	"sort"
	"strings"
	"time"

	"webssh/backend/internal/apierr"
	"webssh/backend/internal/logx"
)

// ---------------------------------------------------------------------------
// 搜索：POST /api/fs/search（第二十二轮，契约第 32 节）
//
// 语义：在**请求里那个目录的整棵子树**里，按**名字**做不区分大小写的子串匹配（输入 log 命中
// app.log、logs/；**不**命中 logs/app.log——那是「路径里含 log」）。逐层 BFS 遍历，结果
// **边搜边回**。
//
// ⚠️ 匹配的是**名字**而不是整条相对路径（第二十二轮用户返工要求）：按路径匹配时，输入 `logs`
// 会把那个目录下的**每一个**文件都命中，一个常见目录名就能刷出几千条——那不是用户要找的东西。
// 相对路径仍然会由前端拼出来显示（让用户知道命中的那一项在哪一层），只是不参与匹配。
//
// 为什么是流式（NDJSON）而不是一次返回一份 JSON：
//   - 递归搜索可能要跑几秒到十几秒，而用户要的是「搜到一点显示一点」——一次性返回时，用户在
//     整个遍历结束前看到的是空白（大目录里的表现就是「点了没反应」）；
//   - 反过来，前端拿到一半结果就能用（点进去看），后面到达的追加即可。
//   一条记录一行 JSON（NDJSON），行与行之间用 \n 分隔：前端按行解析，遇到不完整的尾巴就留到
//   下一块数据（流式 TextDecoder + 缓冲），不需要任何额外的分帧协议。
//
// **必须显式关掉中间层的缓冲**（`X-Accel-Buffering: no`）：飞牛统一网关是 nginx，它默认把
// 上流的响应整条缓冲下来再给浏览器（proxy_buffering on）——那样「边搜边回」会被还原成
// 「搜完一次性到达」，白白多等十几秒。
//
// 上限（都要有，否则一次搜索就能把这条会话的 SFTP 通道与内存占住很久）：
//   - `maxSearchResults` 条结果、`maxSearchScanned` 个条目、`maxSearchDepth` 层、
//     `searchDeadline` 时间；任一条触顶即**停止遍历并标记 truncated**（界面说「已截断」，
//     而不是假装搜完了）。
//   - 子目录读不出来（权限 / 已被删 / 目录在遍历途中被删）**跳过并继续**：一个没权限的子树
//     不该让整次搜索失败。只有**起点目录**读不出来才算失败。
//
// 软链接**不递归进去**（ReadDir 给的是 lstat 属性，所以 `IsDir()` 对链接恒为 false）：跟随
// 软链接会走出起点子树，还可能绕成死循环（`/proc/self/root`、互相指来指去的链接）。链接
// 自己照样会作为结果出现（带 symlink / broken 标记，与列表口径一致）。
//
// 隐藏文件**不过滤**：这是文件管理器，不是「找图片」的搜索框，`~/.bashrc` 该能搜到。
//
// 取消：客户端点「取消」→ fetch abort → `r.Context()` 结束。遍历每层都看一眼 ctx 主动退出
// （ReadDir 不认识 context，靠它自己发现断开要等下一次往返）。已经开流时错误只能写成一行，
// 而客户端已经走了——那行写不出去就算了。
// ---------------------------------------------------------------------------

const (
	// maxSearchResults 是返回给前端的结果条数上限。2000 条足够覆盖「找一个文件」的全部现实
	// 场景，而每条都要占一份 JSON 与一份界面状态；再多说明关键词本身太宽（界面会提示截断）。
	maxSearchResults = 2000
	// maxSearchScanned 是遍历的条目总数上限。深目录（node_modules、/usr）里几万个条目扫过去
	// 是几分钟的 SFTP 往返，必须有个头。
	maxSearchScanned = 200000
	// maxSearchDepth 是相对搜索起点的层数上限（起点 = 0 层）。
	maxSearchDepth = 10
	// searchDeadline 是一次搜索的总时长上限：前两条都在「远端很快」的前提下成立，这条兜住
	// 高延迟 / 慢盘的远端（跨洋链路下每个 ReadDir 都可能要几百毫秒）。
	searchDeadline = 15 * time.Second
	// searchBatchSize / searchBatchInterval 决定「多久往回流一次」：攒够 50 条立刻回，
	// 或者距上次回流超过 200ms 也回（命中稀疏时，这几百毫秒就是「边搜边显示」的手感）。
	searchBatchSize     = 50
	searchBatchInterval = 200 * time.Millisecond
	// searchProgressInterval 是进度记录（已扫描多少项）的节流间隔。
	searchProgressInterval = 400 * time.Millisecond
)

// searchContentType 是流式响应的类型（NDJSON：一条记录一行）。
const searchContentType = "application/x-ndjson; charset=utf-8"

// fsSearchReq 是搜索请求体（契约 4.2）。
type fsSearchReq struct {
	SID   string `json:"sid"`
	Path  string `json:"path"`
	Query string `json:"query"`
}

// searchRecord 是流式响应里的一行。
//
// 所有记录共用这一个结构（而不是每种记录一个类型）：字段少、靠 `type` 分流，前端一个接口
// 就能覆盖；`omitempty` 保证不会把一堆零值发出去。
type searchRecord struct {
	Type string `json:"type"`

	// start：本轮结果的搜索起点（后端解析后的**绝对**路径）。前端拿它把每条结果的 path
	// 折成相对路径，也拿它记住「这批结果属于哪个目录」。
	Path string `json:"path,omitempty"`

	// batch：这一批结果
	Entries []fsEntry `json:"entries,omitempty"`

	// progress（扫描进度）/ done（收尾统计）
	Scanned   int   `json:"scanned,omitempty"`
	Dirs      int   `json:"dirs,omitempty"`
	Skipped   int   `json:"skipped,omitempty"`
	Total     int   `json:"total,omitempty"`
	Truncated bool  `json:"truncated,omitempty"`
	ElapsedMS int64 `json:"elapsedMs,omitempty"`

	// error：开流之后才发生的失败（HTTP 状态码早已发出去，只能把错误写成一行）
	Code  string `json:"code,omitempty"`
	Error string `json:"error,omitempty"`
}

// searchStream 往响应里写 NDJSON 记录：第一行写出时补响应头，每行写完立刻 flush。
//
// 为什么不提前写响应头：搜索**开始之前**的失败（会话没了、起点目录打不开、关键词为空）要走
// 普通的 JSON 错误响应 + 正确的状态码，前端那条路是 ApiError（带 code 与状态码）。一旦写了
// 响应头，状态码就定死成 200，再报错只能在正文里说。所以这里懒写头：`wrote` 就是「这次错误
// 还能不能用普通 JSON 回」的判据。
type searchStream struct {
	w     http.ResponseWriter
	wrote bool
}

// line 写一行记录并 flush；返回错误说明客户端已经走了（写失败）。
func (s *searchStream) line(rec searchRecord) error {
	if !s.wrote {
		h := s.w.Header()
		h.Set("Content-Type", searchContentType)
		h.Set("Cache-Control", "no-store")
		// 网关（nginx）默认会缓冲整条响应：不显式关掉的话，「边搜边回」到浏览器就变成了
		// 「搜完一次性到达」。这条响应必须逐行透传。
		h.Set("X-Accel-Buffering", "no")
		s.wrote = true
	}
	raw, err := json.Marshal(rec)
	if err != nil {
		// 我们自己的结构体，理论上不会失败；真失败了也要让调用方知道（否则会静默少一批结果）
		return err
	}
	raw = append(raw, '\n')
	if _, err := s.w.Write(raw); err != nil {
		return err
	}
	if f, ok := s.w.(http.Flusher); ok {
		f.Flush()
	}
	return nil
}

// searchBuf 攒批：命中稀疏时靠时间兜底，命中密集时靠条数兜底。
type searchBuf struct {
	stream    *searchStream
	pending   []fsEntry
	lastFlush time.Time
}

func (b *searchBuf) add(entry fsEntry) error {
	b.pending = append(b.pending, entry)
	if len(b.pending) < searchBatchSize && time.Since(b.lastFlush) < searchBatchInterval {
		return nil
	}
	return b.flush()
}

func (b *searchBuf) flush() error {
	if len(b.pending) == 0 {
		b.lastFlush = time.Now()
		return nil
	}
	entries := b.pending
	// 先清空再写：失败时这些结果已经没意义了，留着只会让下一次 flush 重复发送。
	b.pending = nil
	b.lastFlush = time.Now()
	return b.stream.line(searchRecord{Type: "batch", Entries: entries})
}

// searchDirNode 是待遍历的一个目录（depth 相对搜索起点，起点为 0）。
type searchDirNode struct {
	path  string
	depth int
}

// searchWalk 是一次遍历的全部状态。
type searchWalk struct {
	ctx    context.Context
	sess   *sftpSession
	out    *searchStream
	buf    *searchBuf
	root   string
	needle string // 已经转成小写的关键词

	started      time.Time
	lastProgress time.Time
	scanned      int
	dirs         int
	skipped      int
	hits         int
	truncated    bool
}

// handleFsSearch 搜索（流式 NDJSON，见文件头）。
func (m *AdminMux) handleFsSearch(w http.ResponseWriter, r *http.Request) *apierr.Error {
	var req fsSearchReq
	if apiErr := m.decodeJSON(w, r, &req); apiErr != nil {
		return apiErr
	}
	// 关键词去首尾空白：搜索框不是路径，从别处粘进来的尾巴空格只会让结果莫名其妙地变少
	// （而每条结果都带相对路径，多命中的一条一眼就能看出是什么）。全空白 = 没输入关键词。
	// **不做**通配符 / 正则：用户要的是「名字里有这几个字」，不是让它再记一门语法。
	query := strings.TrimSpace(req.Query)
	if query == "" {
		return apierr.Invalid("缺少搜索关键词")
	}
	rt, apiErr := m.sessions.runtimeFor(req.SID)
	if apiErr != nil {
		return apiErr
	}

	out := &searchStream{w: w}
	walk := &searchWalk{
		ctx:     r.Context(),
		out:     out,
		needle:  strings.ToLower(query),
		started: time.Now(),
	}
	walk.buf = &searchBuf{stream: out, lastFlush: time.Now()}

	opErr := rt.withSFTP(r.Context(), func(sess *sftpSession) error {
		root, err := resolveRemotePath(sess, req.Path)
		if err != nil {
			return err
		}
		walk.sess, walk.root = sess, root
		// **先把起点目录读出来，再开流**：读不出来时（没有权限 / 不是目录 / 已经不在）一个
		// 字节都还没出去，可以走普通的 JSON 错误响应（带 403/404/502 与裸码），前端据此显示
		// 「读取目录失败：权限不足」这种明确提示。开流之后再报错，状态码只能钉死 200，界面
		// 只能显示正文里的一句话——差的不是一点。
		rootInfos, err := sess.client.ReadDir(root)
		if err != nil {
			return err
		}
		// start 是流里的第一行：前端据此把每条结果的 path 折成相对路径，也据此记住这批结果
		// 属于哪个目录（跨目录时的提示、以及「再点搜索秒回」的判据都要它）。
		if err := out.line(searchRecord{Type: "start", Path: root}); err != nil {
			return err
		}
		if err := walk.run(rootInfos); err != nil {
			return err
		}
		// 收尾前把攒着的那批回掉：不能等到 done 之后（顺序反了前端会以为少了一批结果）
		return walk.buf.flush()
	})

	if opErr != nil {
		if !out.wrote {
			// 一个字节都没出去：按普通 JSON 错误回（带状态码），前端走 ApiError 那条路。
			return fsError("搜索失败", opErr)
		}
		// 已经开流：状态码早就发出去了，错误只能作为一行写回去。裸码必须照 fsError 的口径
		// 翻译（permission → 界面「权限不足」），不能一律 internal。客户端自己断开时这一行
		// 也写不出去（写失败），那就没什么可做的了——那次请求已经结束。
		apiErr := fsError("搜索失败", opErr)
		_ = out.line(searchRecord{Type: "error", Code: apiErr.Code, Error: apiErr.Message})
		return nil
	}
	// 收尾统计。写不出去只说明客户端在这之前就走了，不是服务端故障。
	_ = out.line(searchRecord{
		Type:      "done",
		Total:     walk.hits,
		Scanned:   walk.scanned,
		Dirs:      walk.dirs,
		Skipped:   walk.skipped,
		Truncated: walk.truncated,
		ElapsedMS: time.Since(walk.started).Milliseconds(),
	})
	return nil
}

// run 逐层（BFS）遍历起点目录的子树：先看近的，命中也先出现在近处。
//
// `rootInfos` 是调用方**已经读好**的起点目录内容（起点能读出来才开流，见 handleFsSearch）。
func (w *searchWalk) run(rootInfos []os.FileInfo) error {
	children, err := w.collect(w.root, 0, rootInfos)
	if err != nil {
		return err
	}
	if w.truncated {
		return nil
	}
	queue := children
	for len(queue) > 0 {
		// 客户端点「取消」/ 关页面 → ctx 结束。协作式退出：这里主动停下，而不是等某个 SFTP
		// 调用失败（ReadDir 不认识 context，一个卡住的远方目录会一直等下去）。
		if err := w.ctx.Err(); err != nil {
			return err
		}
		if time.Since(w.started) > searchDeadline {
			w.truncated = true
			return nil
		}
		node := queue[0]
		queue = queue[1:]
		children, err := w.scanDir(node.path, node.depth)
		if err != nil {
			// 子树里某个目录读不出来（权限 / 遍历途中被删）**跳过并继续**：一个没权限的
			// 目录不该让整次搜索失败。跳过了几个目录，界面在收尾统计里会说。
			w.skipped++
			logx.Warnf("搜索跳过目录 %s（起点 %s）：%v", node.path, w.root, err)
			continue
		}
		if w.truncated {
			// 触顶：停止遍历（已经攒下的结果由调用方回完），truncated 标记让界面说明情况
			return nil
		}
		queue = append(queue, children...)
		if err := w.maybeProgress(); err != nil {
			return err
		}
	}
	return nil
}

// scanDir 读一个子目录（起点那一层由 run 直接用已经读好的内容，走 collect）。
func (w *searchWalk) scanDir(dir string, depth int) ([]searchDirNode, error) {
	infos, err := w.sess.client.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	return w.collect(dir, depth, infos)
}

// collect 处理一个目录的内容：命中的写进缓冲，返回要接着遍历的子目录。
func (w *searchWalk) collect(dir string, depth int, infos []os.FileInfo) ([]searchDirNode, error) {
	w.dirs++
	// 目录内按名字排序：同样的目录搜两次顺序一致（也才好写断言）。
	sort.SliceStable(infos, func(i, j int) bool {
		ni, nj := strings.ToLower(infos[i].Name()), strings.ToLower(infos[j].Name())
		if ni != nj {
			return ni < nj
		}
		return infos[i].Name() < infos[j].Name()
	})
	var children []searchDirNode
	for _, fi := range infos {
		w.scanned++
		if w.scanned > maxSearchScanned {
			w.truncated = true
			return children, nil
		}
		full := path.Join(dir, fi.Name())
		if matchSearch(fi.Name(), w.needle) {
			// 与列目录用同一个构造函数：软链接的指向、失效标记、mode/mtime 的口径只有一份
			// （软链接会多两次往返，但软链接本来就少，一致性比这点开销值）。
			w.hits++
			if err := w.buf.add(buildFsEntry(w.sess.client, dir, fi)); err != nil {
				return children, err
			}
			if w.hits >= maxSearchResults {
				w.truncated = true
				return children, nil
			}
		}
		// 只进**真正的目录**：ReadDir 给的是 lstat 属性，指向目录的软链接 IsDir() 为 false，
		// 因此不会被递归进去（理由见文件头）。
		if fi.IsDir() && depth+1 <= maxSearchDepth {
			children = append(children, searchDirNode{path: full, depth: depth + 1})
		}
	}
	return children, nil
}

// matchSearch 判断一个条目**自己的名字**是否命中关键词。
//
// 只匹配名字（不看路径、不看内容）：用户要的是「找那个叫 xxx 的文件/目录」。按路径匹配会把
// 命中目录下的所有东西一起倒出来（输入 `logs` 就是那个目录的完整列表），量与噪音都不可接受。
// 大小写不敏感：文件名的写法是用户随手写的，让他为了搜到 `README.md` 去试 `readme` 之外的
// 几种拼法是没道理的。
func matchSearch(name, needle string) bool {
	return strings.Contains(strings.ToLower(name), needle)
}

// maybeProgress 节流回一条扫描进度（界面显示「已扫描 N 项」），让长搜索看得出还在动。
func (w *searchWalk) maybeProgress() error {
	if time.Since(w.lastProgress) < searchProgressInterval {
		return nil
	}
	w.lastProgress = time.Now()
	return w.out.line(searchRecord{Type: "progress", Scanned: w.scanned, Dirs: w.dirs})
}
