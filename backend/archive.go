package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path"
	"strings"
	"time"
	"webssh/backend/internal/apierr"
	"webssh/backend/internal/logx"
)

// ---------------------------------------------------------------------------
// 压缩 / 解压（契约 11.4 / 11.5 / 13.5）：调**远程主机**的 tar / zip / unzip
//
// 为什么是远程命令而不是 SFTP 自己打包：tar.gz 的压缩比与流式处理由 tar 做得好得多，
// 而且压缩包必须落在远程主机上（用户接下来要在远程文件管理器里看到它、下载它、或者解压
// 它）——把它拉到本机再传回去纯属浪费带宽。
//
// 所有插值路径都过 shellQuote（见 remote_exec.go）：文件名里带空格、引号、`$(...)` 都是
// 合法且常见的，未经引用的拼接等于把一条注入通道交给远端文件名。
// ---------------------------------------------------------------------------

// 归档格式的识别结果（按扩展名，契约 11.4：「按扩展名识别」）。
const (
	archiveTar = "tar"
	archiveZip = "zip"
)

// 压缩请求的 format 取值（契约 13.5）。
const (
	archiveFormatTarGz = "tar.gz"
	archiveFormatZip   = "zip"
)

// resolveArchiveFormat 校验请求里的 format：缺省（含空串）= tar.gz，也接受 zip，
// 其余一律 invalid——**不猜**（例如把 "tgz" 当 tar.gz 收下）会让「前端发错了字段值」这类
// 问题永远查不出来，而用户会以为「我选了 zip，出来的却是 tar.gz」。
func resolveArchiveFormat(raw string) (string, *apierr.Error) {
	switch strings.TrimSpace(raw) {
	case "", archiveFormatTarGz:
		return archiveFormatTarGz, nil
	case archiveFormatZip:
		return archiveFormatZip, nil
	default:
		return "", apierr.Invalid("不支持的压缩格式：%q（只支持 %s 与 %s）", raw, archiveFormatTarGz, archiveFormatZip)
	}
}

// archiveExtOf 返回某种格式的文件名后缀。
func archiveExtOf(format string) string {
	if format == archiveFormatZip {
		return ".zip"
	}
	return ".tar.gz"
}

// tarExts 是 tar 家族的后缀。GNU tar 会自己按魔数选解压器，所以 `tar -xf` 对全部这些后缀
// 都有效，不需要我们给 tar 加 -z/-j/-J。
var tarExts = []string{
	".tar", ".tar.gz", ".tgz", ".tar.bz2", ".tbz", ".tbz2", ".tar.xz", ".txz", ".tar.zst", ".tzst",
}

// maxCompressItems 是一次压缩最多接受多少个条目。
//
// 为什么需要这条上限：请求体本身已经有 1 MiB 的上限（约几千条路径），但那条限制是「整体
// 大小」而不是「条目数」，而且压缩的产物大小在跑完之前不可知（要提前算出总字节数，得先把
// 整棵树 stat 一遍）。给一个宽松的条目数上限，既挡住畸形请求，也不会挡掉正常用法——真要
// 压缩上万个文件，压它们所在的目录才是对的做法（也是唯一能把「同一批数据」表达清楚的
// 选择）。
const maxCompressItems = 2000

// archiveKindOf 按扩展名判断归档格式；认不出来返回空串（调用方回 archive_unsupported）。
//
// 只看后缀不看内容：SFTP 侧读文件头要额外开一次文件，而且「后缀不认识」本身就是用户能
// 立刻理解、也能立刻改的输入问题（重命名一下或换 tar）。
func archiveKindOf(name string) string {
	lower := strings.ToLower(path.Base(name))
	// .zip 先判：它不会被上面的 tar 后缀误吞，但顺序明确一点更容易读。
	if strings.HasSuffix(lower, ".zip") {
		return archiveZip
	}
	for _, ext := range tarExts {
		if strings.HasSuffix(lower, ext) {
			return archiveTar
		}
	}
	return ""
}

// archiveProbeUnwritable 是探针脚本里「两个目录都不可写」的专用退出码。
// 用 73 而不是 1：要能把它和「脚本本身没跑起来」（127 等）区分开，见 resolveArchiveDir。
const archiveProbeUnwritable = 73

// resolveArchiveDir 决定这次压缩把产物写到哪里，顺带处理「配置的目录不可写」。
//
// 为什么不能直接 `mkdir -p <root>` 就开打（第六轮用户报的「压缩提示权限不足」）：
// archiveDir 默认是**远端主机**的 /tmp/webssh-archive。远端 /tmp 一般世界可写，但这个目录
// 本身可能是别的账号（典型是历史上以 root 跑过一次）建的：当前登录用户既写不进去、也删不掉，
// 于是用户只看到一句「权限不足」——既不知道说的是远端的哪个目录，也不知道能怎么办。
//
// 规则（第九轮起是三档）：
//  1. 配置的目录可写 → 用它（现有行为完全不变）；
//  2. 不可写 → 退到同级的 `<root>-<登录用户名>`：这个目录是自己建的，不会与别的账号撞车，
//     也不会去动别人已有的东西；
//  3. 还不可写 → 再退到**登录用户家目录**下的 `$HOME/.webssh-archive`（用户要求：/tmp 整个
//     不可写的机器也得能压缩；家目录是登录用户唯一保证能写的地方）；
//  4. 三档都不可写 → 明确报错：把三个候选目录、原始输出、以及 WEBSSH_ARCHIVE_DIR 都写出来。
//
// 探测放在远端 shell 里做（一次命令搞定，不用发两次）：
// `-d` 与 `-w` 都要查：`-w` 判的是当前登录用户的实际权限（比在服务端猜 uid/权限位可靠），
// `-d` 则保证配置成一个**文件**时也走回退分支，不会把文件当目录去打包。
func (r *sessionRuntime) resolveArchiveDir(ctx context.Context, root string) (string, *apierr.Error) {
	user := ""
	if cfg, ok := r.config(); ok {
		user = cfg.User
	}
	fallback := root + "-" + dirNameSafe(user)
	// 第三档：家目录下的隐藏目录。用 `$HOME` 而不是 `~`：`~` 只在交互式 shell 里展开，
	// `sh -c` 里不一定（POSIX 只要求 `$HOME`）。`$HOME` 为空时跳过这一档。
	homeDir := "$HOME/.webssh-archive"
	// 只用 shell 内建 + `command -v` 判断 mkdir 在不在：远端 PATH 坏掉时（比如那台主机
	// 上什么都没有）**不能**在这里就报「目录不可写」——真正的原因（缺 tar）要让后面的
	// 打包命令去说，否则会把一个 no_archive_tool 误报成权限问题。这种情况下原样返回
	// 配置的目录，交给后面那条命令去失败并报告。
	// `try` 判的是「建得出来 + 真的是目录 + 当前登录用户可写」；三档按顺序试，都不行就以
	// 73 退出——让 Go 那边给出说得清的报错，而不是一句裸的 mkdir 失败。
	probe := "d=" + shellQuote(root) + "; " +
		"if command -v mkdir >/dev/null 2>&1; then " +
		"try() { mkdir -p \"$1\" 2>/dev/null || true; [ -d \"$1\" ] && [ -w \"$1\" ]; }; " +
		"if try \"$d\"; then :; " +
		"elif try " + shellQuote(fallback) + "; then d=" + shellQuote(fallback) + "; " +
		"elif [ -n \"$HOME\" ] && try \"$HOME/.webssh-archive\"; then d=\"$HOME/.webssh-archive\"; " +
		"else printf 'NOTWRITABLE %s\\n' \"$d\"; exit 73; fi; " +
		"fi; " +
		"printf '%s\\n' \"$d\""
	res, err := r.runRemote(ctx, probe)
	if err != nil {
		return "", err
	}
	// 只有探针**明确说**「两个目录都不可写」（它自己 exit 73）时才把它当成权限问题：
	// 探针本身跑不起来（远端 PATH 坏掉、连 sh 都没有）不是「目录不可写」，那种情况
	// 要原样返回配置目录，让后面的打包命令去失败 —— 否则一个 no_archive_tool 会被误报成权限问题。
	if res.Code != archiveProbeUnwritable {
		if dir := firstLine(res.Stdout); res.ok() && dir != "" {
			return strings.TrimSpace(dir), nil
		}
		logx.Warnf("会话 %s 的归档目录探针没有结论（退出码 %d：%s），按配置目录继续", r.id, res.Code, res.diagnostic())
		return root, nil
	}
	{
		return "", apierr.Permission(
			"远端压缩包输出目录不可写：%s、%s、%s 都试过了（第三档是登录用户家目录下的隐藏目录）。"+
				"压缩包是在**远程主机**上生成的，和运行本应用的机器无关；"+
				"请给远端登录用户写权限，或用环境变量 WEBSSH_ARCHIVE_DIR 换一个可写目录。原始输出：%s",
			root, fallback, homeDir, res.diagnostic())
	}
}

// dirNameSafe 把登录名收拾成能安全拼进目录名的一小串字符。
//
// 远端登录名可能带 @、空格、Unicode：拼进路径要避免分隔符/空白，也别因为名字特别长撑爆路径。
// 允许的字符之外一律换成 `_`；全被换掉时退回 "user"。
func dirNameSafe(name string) string {
	var sb strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			sb.WriteRune(r)
		default:
			sb.WriteRune('_')
		}
	}
	out := strings.Trim(sb.String(), "._-")
	if out == "" {
		return "user"
	}
	if len(out) > 32 {
		out = out[:32]
	}
	return out
}

// firstLine 取输出的第一行（远端探目录时用来读回它选的那个目录）。
func firstLine(out string) string {
	line, _, _ := strings.Cut(out, "\n")
	return strings.TrimSpace(strings.TrimRight(line, "\r"))
}

// handleFsCompress 把远程的若干条目打成 <archiveDir>/<name>.<ext>（契约 11.5 / 13.5）。
//
// format 决定用哪个工具与后缀：tar.gz（缺省，`tar -czf`）或 zip（`zip -qry`）。
func (m *AdminMux) handleFsCompress(w http.ResponseWriter, r *http.Request) *apierr.Error {
	var req struct {
		SID    string   `json:"sid"`
		Paths  []string `json:"paths"`
		Name   string   `json:"name"`
		Format string   `json:"format"`
	}
	if apiErr := m.decodeJSON(w, r, &req); apiErr != nil {
		return apiErr
	}
	root := strings.TrimSpace(m.renv.ArchiveDir)
	if root == "" {
		// 正常配置下不会发生（有默认值），但配置被显式设成空串时要如实报错，
		// 而不是拼出一个相对路径、把压缩包写进莫名其妙的目录。
		return apierr.Internal("未配置压缩包输出目录（WEBSSH_ARCHIVE_DIR）")
	}
	if len(req.Paths) == 0 {
		return apierr.Invalid("缺少 paths")
	}
	if len(req.Paths) > maxCompressItems {
		return apierr.Invalid("一次最多压缩 %d 个条目（本次 %d 个）：请改为压缩它们所在的目录", maxCompressItems, len(req.Paths))
	}
	format, apiErr := resolveArchiveFormat(req.Format)
	if apiErr != nil {
		return apiErr
	}
	name, apiErr := archiveName(req.Name, format)
	if apiErr != nil {
		return apiErr
	}
	rt, apiErr := m.sessions.runtimeFor(req.SID)
	if apiErr != nil {
		return apiErr
	}
	// 真正的输出目录由远端探一遍再定：配置的目录不可写时退到同级的 <root>-<登录用户>
	// （见 resolveArchiveDir 的说明——用户报的「压缩提示权限不足」就出在这里）。
	archiveDir, apiErr := rt.resolveArchiveDir(r.Context(), root)
	if apiErr != nil {
		return apiErr
	}
	out := path.Join(archiveDir, name)

	// 1) 解析路径（相对路径相对家目录）+ 校验存在 + 去重，并先把同名产物挪开。
	//    这一步用 SFTP 做：路径语义（家目录、相对路径）与其它文件接口保持一致，
	//    交给 SFTP 比让远程 shell 去 cd 更可控。
	var items []string
	opErr := rt.withSFTP(r.Context(), func(sess *sftpSession) error {
		var err error
		items, err = resolveSources(sess, req.Paths)
		if err != nil {
			return err
		}
		return prepareArchiveTarget(sess, out)
	})
	if opErr != nil {
		return fsError("压缩失败", opErr)
	}

	// 2) 一次远程命令完成「建目录 + 打包」。用 `&&` 串起来：建目录失败就不该继续打包，
	//    否则 tar 会以「输出目录不存在」报错，把真正的原因（没权限建目录）盖掉。
	//    用 mkdir -p 是因为 archiveDir 固定且可能在 /tmp 下、开机后不存在。
	archiveCmd := tarCommand(out, items)
	if format == archiveFormatZip {
		archiveCmd = zipCommand(out, items)
	}
	command := "mkdir -p " + shellQuote(archiveDir) + " && " + archiveCmd
	res, runErr := rt.runRemote(r.Context(), command)
	if runErr != nil {
		return runErr
	}
	if !res.ok() {
		// 失败时清掉半成品压缩包：写到一半失败会留下一个看着正常、其实不完整（也解不开）
		// 的归档，用户会把它当成一个可用的包下载走。
		removeArchiveQuietly(rt, out)
		return archiveCommandError("压缩失败", res)
	}

	// 3) 回填产物大小：前端要显示它，也要拿 path/dir 跳转到那个目录。
	var size int64
	statErr := rt.withSFTP(r.Context(), func(sess *sftpSession) error {
		fi, err := sess.client.Stat(out)
		if err != nil {
			return err
		}
		size = fi.Size()
		return nil
	})
	if statErr != nil {
		return fsError("读取压缩包信息失败", statErr)
	}
	// 产物大小的上限（契约 11.5 最后一条：压缩也要有上限）。压缩包的体积事先算不出来，
	// 所以这里只能事后判：超限就把刚生成的包删掉并如实报错——远端 /tmp 通常很小，一个
	// 意外巨大的包会把整个主机的临时空间吃掉，那比「这次压缩失败」严重得多。
	if size > maxTransferBytes {
		removeArchiveQuietly(rt, out)
		return apierr.TooLarge("压缩包超过单次上限 %s（实际 %s），已删除产物：请分批压缩", humanSize(maxTransferBytes), humanSize(size))
	}
	return apierr.Respond(w, map[string]any{
		"ok":      true,
		"archive": map[string]any{"path": out, "size": size, "dir": archiveDir},
	})
}

// tarCommand 拼出 tar 命令（契约 11.5 的规则）。
//
// 单父目录：`-C <父目录> <项的名字>` —— 压缩包里的成员就是用户选中的那些名字（没有多余的
// 父目录前缀），解压出来与原来一致。
// 多父目录：只能 `-C /` + 绝对路径（tar 只接受一个 -C 会作用于其后的所有成员）。此时
// 成员名带上了各自的目录层级，这是唯一能同时表达「来自不同目录的几个条目」的方式。
func tarCommand(out string, items []string) string {
	parents := make([]string, 0, 1)
	seen := map[string]bool{}
	for _, item := range items {
		parent := path.Dir(item)
		if !seen[parent] {
			seen[parent] = true
			parents = append(parents, parent)
		}
	}
	var sb strings.Builder
	sb.WriteString("tar -czf ")
	sb.WriteString(shellQuote(out))
	if len(parents) == 1 {
		sb.WriteString(" -C ")
		sb.WriteString(shellQuote(parents[0]))
		for _, item := range items {
			sb.WriteString(" ")
			sb.WriteString(shellQuote(path.Base(item)))
		}
		return sb.String()
	}
	// -C / 之后用**去掉前导斜杠**的路径：GNU tar 会自己剥掉 `/` 并打一条 "Removing leading
	// `/'" 警告（无害但会污染 stderr，让我们后面的错误判定多一堆噪声）。
	sb.WriteString(" -C /")
	for _, item := range items {
		sb.WriteString(" ")
		sb.WriteString(shellQuote(strings.TrimPrefix(item, "/")))
	}
	return sb.String()
}

// zipCommand 拼出 zip 命令（契约 13.5）。
//
// zip 的原生语义是「往一个包里追加/更新条目」，所以条目分属不同父目录时依次 `cd` 进各自的
// 目录再追加进同一个包——这比 tar 的「一个 -C 作用于其后所有成员」更贴近用户「把选中的
// 东西放进一个包」的预期，包里的名字也保持用户在界面上看到的样子（不带多余的目录层级）。
//
// 为什么是 `-qry` 而不是 `-qr`（多处一个 -y）：不加 -y 时 zip 会**跟随软链接**，
//   - 目录软链接会把链接指向的整棵树搬进包（可能是在别处的几百 MB，用户只想压一个链接）；
//   - 失效软链接会被静默跳过（zip 只打一条 warning、退出码 0）→ 用户以为压进去了；
//     若选中的**只有**失效链接，zip 直接 `Nothing to do!` 退出 12，整个压缩失败。
//
// 加 -y 后软链接按「链接本身」存（unzip 解压时会重建链接），与 tar 侧不跟随链接的行为
// （没加 -h）保持一致。
func zipCommand(out string, items []string) string {
	// 按父目录分组，组的顺序 = 条目首次出现的顺序（决定 `cd` 的顺序，不影响结果）。
	type zipGroup struct {
		dir   string
		names []string
	}
	var groups []zipGroup
	index := map[string]int{}
	for _, item := range items {
		dir := path.Dir(item)
		i, ok := index[dir]
		if !ok {
			index[dir] = len(groups)
			groups = append(groups, zipGroup{dir: dir})
			i = len(groups) - 1
		}
		groups[i].names = append(groups[i].names, path.Base(item))
	}
	var sb strings.Builder
	for i, g := range groups {
		if i > 0 {
			sb.WriteString(" && ")
		}
		sb.WriteString("cd ")
		sb.WriteString(shellQuote(g.dir))
		sb.WriteString(" && zip -qry ")
		sb.WriteString(shellQuote(out))
		for _, name := range g.names {
			sb.WriteString(" ")
			sb.WriteString(shellQuote(name))
		}
	}
	return sb.String()
}

// prepareArchiveTarget 在打包前处理同名产物（由 SFTP 侧执行，且必须在写命令之前）。
//
//   - 同名的**目录**直接拒绝：zip/tar 都写不进去，而失败后的清理绝不能去删一个本来就在
//     那儿的目录——那是用户的数据，不是我们的半成品。
//   - 同名的文件/软链接先删掉：**zip 是追加/更新语义**，同名包会把上一次留下的条目继续
//     带在包里（这次没选的文件也在），删掉才能保证「包里的东西 = 这次选中的东西」；
//     tar 本来就是覆盖写，两边一致更省心。
func prepareArchiveTarget(sess *sftpSession, out string) error {
	fi, err := sess.client.Lstat(out)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if fi.IsDir() && fi.Mode()&os.ModeSymlink == 0 {
		return apierr.Invalid("压缩包名字与一个已存在的目录重名：%s（请换个名字）", out)
	}
	if err := sess.client.Remove(out); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// archiveName 规整压缩包文件名（契约 11.5 / 13.5：缺省用 webssh-<时间戳>，后缀按 format）。
//
// 名字是**用户输入**且会被拼进远程路径，所以必须严格：不允许路径分隔符（否则能写到
// archiveDir 外面）、不允许 `.`/`..`、不允许控制字符。
//
// 后缀处理：用户往往是拿「选中的那个条目的名字」当包名（前端的菜单就是这么传的），
// 而它可能自带别的归档后缀（`docs.tar.gz`、`backup.zip`）。这时**换掉**而不是在后面再挂
// 一个——`docs.tar.gz.zip` 这种名字看着就像出了问题。只有认识的后缀才换，
// `notes.txt` 这种普通后缀保留（`notes.txt.zip` 是合理结果）。
func archiveName(raw, format string) (string, *apierr.Error) {
	name := strings.TrimSpace(raw)
	if name == "" {
		name = "webssh-" + time.Now().Format("20060102-150405")
	}
	if len(name) > 128 {
		return "", apierr.Invalid("压缩包名字过长（最多 128 字符）")
	}
	if strings.ContainsAny(name, `/\`) || name == "." || name == ".." {
		return "", apierr.Invalid("压缩包名字不能包含路径分隔符：%q", raw)
	}
	if strings.ContainsFunc(name, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return "", apierr.Invalid("压缩包名字不能包含控制字符：%q", raw)
	}
	ext := archiveExtOf(format)
	if !strings.HasSuffix(name, ext) {
		name = strings.TrimSuffix(name, knownArchiveExt(name)) + ext
	}
	// 只剩后缀的名字（".zip" / ".tar.gz"）会生成一个隐藏文件：用户在归档目录里列不出来，
	// 只会看到「压缩成功了但找不到产物」。与其让他对着这一步发呆，不如当场拒绝。
	if strings.TrimSuffix(name, ext) == "" {
		return "", apierr.Invalid("压缩包名字不能只有后缀：%q", raw)
	}
	return name, nil
}

// knownArchiveExt 返回名字末尾那个**我们认识的**归档后缀（没有则空串）。
//
// 取最长的匹配：`a.tar.gz` 必须返回 `.tar.gz`，返 `.gz`（不在表里）或 `.tar`（不匹配）
// 都会导致换后缀时把名字切错。
func knownArchiveExt(name string) string {
	lower := strings.ToLower(name)
	best := ""
	for _, ext := range append(append([]string{}, tarExts...), ".zip") {
		if strings.HasSuffix(lower, ext) && len(ext) > len(best) {
			best = ext
		}
	}
	if best == "" {
		return ""
	}
	// 用原名里的那一段（保留用户的大小写习惯）。
	return name[len(name)-len(best):]
}

// handleFsExtract 就地解压（契约 11.5）：dir 返回压缩包所在目录。
func (m *AdminMux) handleFsExtract(w http.ResponseWriter, r *http.Request) *apierr.Error {
	var req fsReq
	if apiErr := m.decodeJSON(w, r, &req); apiErr != nil {
		return apiErr
	}
	rt, apiErr := m.sessions.runtimeFor(req.SID)
	if apiErr != nil {
		return apiErr
	}

	var (
		target string
		dir    string
	)
	opErr := rt.withSFTP(r.Context(), func(sess *sftpSession) error {
		var err error
		target, err = requiredRemotePath(sess, req.Path)
		if err != nil {
			return err
		}
		fi, err := sess.client.Stat(target)
		if err != nil {
			return err
		}
		if fi.IsDir() {
			return errTargetIsDir
		}
		dir = path.Dir(target)
		return nil
	})
	if opErr != nil {
		return fsError("解压失败", opErr)
	}

	// 显式 -C / -d 指定解压目标：exec 通道里的工作目录是登录用户的家目录，不指定的话
	// 「就地解压」会变成「解到家目录里」——用户会以为解压丢了。
	var command string
	switch archiveKindOf(target) {
	case archiveTar:
		command = "tar -xf " + shellQuote(target) + " -C " + shellQuote(dir)
	case archiveZip:
		// -o：不加的话 unzip 会为每个同名文件交互式地问一句（远端没有 stdin，直接卡住超时）。
		command = "unzip -o " + shellQuote(target) + " -d " + shellQuote(dir)
	default:
		return apierr.ArchiveUnsupported("不支持的压缩包格式：%s（只支持 tar 家族与 zip）", path.Base(target))
	}

	res, runErr := rt.runRemote(r.Context(), command)
	if runErr != nil {
		return runErr
	}
	if !res.ok() {
		return archiveCommandError("解压失败", res)
	}
	return apierr.Respond(w, map[string]any{"ok": true, "dir": dir})
}

// archiveCommandError 把「tar/unzip 跑了但非 0 退出」翻成错误码。
//
// 缺工具单独识别：远端没有 tar / unzip 是环境问题，用户该看到 err_no_archive_tool
// （有专门的文案），而不是一句笼统的内部错误。其余按输出里的权限字样区分 permission /
// internal，并且**一定带上命令输出的关键行**——真正的原因就写在远端那条命令的输出里，
// 隐掉它等于让用户去猜。
func archiveCommandError(action string, res remoteResult) *apierr.Error {
	detail := res.diagnostic()
	if res.Truncated {
		detail += "（输出过长已截断）"
	}
	if detail == "" {
		detail = fmt.Sprintf("命令退出码 %d 且没有任何输出", res.Code)
	}
	switch {
	case res.Code == 127 || looksLikeMissingTool(detail):
		return apierr.NoArchiveTool("%s：远端主机上没有这个工具（%s）", action, detail)
	case looksLikePermission(detail):
		return apierr.Permission("%s：%s", action, detail)
	default:
		return apierr.Internal("%s：%s", action, detail)
	}
}

// cleanupTimeout 是善后操作（删半成品、回读产物信息）的时限。
const cleanupTimeout = 30 * time.Second

// cleanupContext 派生一个「不跟着请求取消、但有超时」的上下文，用于失败后的善后操作。
//
// 为什么不用 r.Context()：最常见的失败恰恰是「客户端断开了」（关页面、点取消），那时
// r.Context() 已经结束，用它做的善后（删掉写了一半的压缩包）会立刻被取消——半成品就
// 永远留在远端磁盘上。善后必须用独立的机会去跑，但不能无限等，所以给个短超时。
func cleanupContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
}

// removeArchiveQuietly 删掉一个「我们刚写坏的」压缩产物（善后专用：失败只记日志，
// 不再上抛——此时更重要的错误（原始失败原因）已经在返回路径上了）。
//
// 与 removeRemoteTree 的区别在于多了一道「不是目录」的检查：清理的只能是我们自己写坏的
// 那个文件。同名路径上如果是个目录，那是用户的数据（见 prepareArchiveTarget），
// 无论什么情况都不能删——「清理半成品把用户的目录删了」是所有失败路径里最不能接受的一种。
func removeArchiveQuietly(rt *sessionRuntime, out string) {
	ctx, cancel := cleanupContext(context.Background())
	defer cancel()
	err := rt.withSFTP(ctx, func(sess *sftpSession) error {
		fi, err := sess.client.Lstat(out)
		if err != nil {
			return err
		}
		if fi.IsDir() && fi.Mode()&os.ModeSymlink == 0 {
			logx.Warnf("清理压缩产物 %s 时发现它是目录，已跳过（不动用户数据）", out)
			return nil
		}
		return sess.client.Remove(out)
	})
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		logx.Warnf("清理压缩产物 %s 失败：%v", out, err)
	}
}
