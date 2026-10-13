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
)

// ---------------------------------------------------------------------------
// 压缩 / 解压（契约 11.4 / 11.5）：调**远程主机**的 tar / unzip
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

// handleFsCompress 把远程的若干条目打成 <archiveDir>/<name>.tar.gz（契约 11.5）。
func (m *AdminMux) handleFsCompress(w http.ResponseWriter, r *http.Request) *apiError {
	var req struct {
		SID   string   `json:"sid"`
		Paths []string `json:"paths"`
		Name  string   `json:"name"`
	}
	if apiErr := m.decodeJSON(w, r, &req); apiErr != nil {
		return apiErr
	}
	root := strings.TrimSpace(m.renv.ArchiveDir)
	if root == "" {
		// 正常配置下不会发生（有默认值），但配置被显式设成空串时要如实报错，
		// 而不是拼出一个相对路径、把压缩包写进莫名其妙的目录。
		return errInternal("未配置压缩包输出目录（WEBSSH_ARCHIVE_DIR）")
	}
	if len(req.Paths) == 0 {
		return errInvalid("缺少 paths")
	}
	if len(req.Paths) > maxCompressItems {
		return errInvalid("一次最多压缩 %d 个条目（本次 %d 个）：请改为压缩它们所在的目录", maxCompressItems, len(req.Paths))
	}
	name, apiErr := archiveName(req.Name)
	if apiErr != nil {
		return apiErr
	}
	rt, apiErr := m.sessions.runtimeFor(req.SID)
	if apiErr != nil {
		return apiErr
	}

	// 1) 解析路径（相对路径相对家目录）+ 校验存在 + 去重。这一步用 SFTP 做：路径语义
	//    （家目录、相对路径）与其它文件接口保持一致，交给 SFTP 比让远程 shell 去 cd 更可控。
	var items []string
	opErr := rt.withSFTP(r.Context(), func(sess *sftpSession) error {
		var err error
		items, err = resolveSources(sess, req.Paths)
		return err
	})
	if opErr != nil {
		return fsError("压缩失败", opErr)
	}

	out := path.Join(root, name)
	// 2) 一次远程命令完成「建目录 + 打包」。用 `&&` 串起来：建目录失败就不该继续打包，
	//    否则 tar 会以「输出目录不存在」报错，把真正的原因（没权限建目录）盖掉。
	//    用 mkdir -p 是因为 archiveDir 固定且可能在 /tmp 下、开机后不存在。
	command := "mkdir -p " + shellQuote(root) + " && " + compressCommand(out, items)
	res, runErr := rt.runRemote(r.Context(), command)
	if runErr != nil {
		return runErr
	}
	if !res.ok() {
		// 失败时清掉半成品压缩包：tar 写到一半失败会留下一个看着正常、其实不完整（也解不开）
		// 的 .tar.gz，用户会把它当成一个可用的归档下载走。
		removeRemoteQuietly(rt, out)
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
		removeRemoteQuietly(rt, out)
		return errTooLarge("压缩包超过单次上限 %s（实际 %s），已删除产物：请分批压缩", humanSize(maxTransferBytes), humanSize(size))
	}
	return respond(w, map[string]any{
		"ok":      true,
		"archive": map[string]any{"path": out, "size": size, "dir": root},
	})
}

// compressCommand 拼出 tar 命令（契约 11.5 的规则）。
//
// 单父目录：`-C <父目录> <项的名字>` —— 压缩包里的成员就是用户选中的那些名字（没有多余的
// 父目录前缀），解压出来与原来一致。
// 多父目录：只能 `-C /` + 绝对路径（tar 只接受一个 -C 会作用于其后的所有成员）。此时
// 成员名带上了各自的目录层级，这是唯一能同时表达「来自不同目录的几个条目」的方式。
func compressCommand(out string, items []string) string {
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

// archiveName 规整压缩包文件名（契约 11.5：缺省用 webssh-<时间戳>）。
//
// 名字是**用户输入**且会被拼进远程路径，所以必须严格：不允许路径分隔符（否则能写到
// archiveDir 外面）、不允许 `.`/`..`、不允许控制字符。没带 `.tar.gz` 后缀时自动补——
// 前端的「压缩为 tar.gz」菜单项传的就是选中的名字。
func archiveName(raw string) (string, *apiError) {
	name := strings.TrimSpace(raw)
	if name == "" {
		name = "webssh-" + time.Now().Format("20060102-150405")
	}
	if len(name) > 128 {
		return "", errInvalid("压缩包名字过长（最多 128 字符）")
	}
	if strings.ContainsAny(name, `/\`) || name == "." || name == ".." {
		return "", errInvalid("压缩包名字不能包含路径分隔符：%q", raw)
	}
	if strings.ContainsFunc(name, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return "", errInvalid("压缩包名字不能包含控制字符：%q", raw)
	}
	if !strings.HasSuffix(name, ".tar.gz") {
		name += ".tar.gz"
	}
	return name, nil
}

// handleFsExtract 就地解压（契约 11.5）：dir 返回压缩包所在目录。
func (m *AdminMux) handleFsExtract(w http.ResponseWriter, r *http.Request) *apiError {
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
		return errArchiveUnsupported("不支持的压缩包格式：%s（只支持 tar 家族与 zip）", path.Base(target))
	}

	res, runErr := rt.runRemote(r.Context(), command)
	if runErr != nil {
		return runErr
	}
	if !res.ok() {
		return archiveCommandError("解压失败", res)
	}
	return respond(w, map[string]any{"ok": true, "dir": dir})
}

// archiveCommandError 把「tar/unzip 跑了但非 0 退出」翻成错误码。
//
// 缺工具单独识别：远端没有 tar / unzip 是环境问题，用户该看到 err_no_archive_tool
// （有专门的文案），而不是一句笼统的内部错误。其余按输出里的权限字样区分 permission /
// internal，并且**一定带上命令输出的关键行**——真正的原因就写在远端那条命令的输出里，
// 隐掉它等于让用户去猜。
func archiveCommandError(action string, res remoteResult) *apiError {
	detail := res.diagnostic()
	if res.Truncated {
		detail += "（输出过长已截断）"
	}
	if detail == "" {
		detail = fmt.Sprintf("命令退出码 %d 且没有任何输出", res.Code)
	}
	switch {
	case res.Code == 127 || looksLikeMissingTool(detail):
		return errNoArchiveTool("%s：远端主机上没有这个工具（%s）", action, detail)
	case looksLikePermission(detail):
		return errPermission("%s：%s", action, detail)
	default:
		return errInternal("%s：%s", action, detail)
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

// removeRemoteQuietly 尽力删掉一个远端路径（善后专用：失败只记日志，不再上抛——
// 因为此时更重要的错误（原始失败原因）已经在返回路径上了）。
func removeRemoteQuietly(rt *sessionRuntime, target string) {
	ctx, cancel := cleanupContext(context.Background())
	defer cancel()
	err := rt.withSFTP(ctx, func(sess *sftpSession) error {
		return removeRemoteTree(sess.client, target)
	})
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		warnf("清理远端半成品 %s 失败：%v", target, err)
	}
}
