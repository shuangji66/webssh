package main

import (
	"bytes"
	"io"
	"net/http"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"

	"webssh/backend/internal/apierr"
	"webssh/backend/internal/logx"
)

// ---------------------------------------------------------------------------
// 预览：文本判定（/api/fs/read）与图片白名单（/api/fs/image，契约第 25 节）
//
// 两个接口刻意分开，而不是给 /api/fs/read 加一个「二进制也回 base64」的开关：
//   - 图片走**原始字节 + inline Content-Type**：浏览器直接按图渲染，不必把 4 MiB 的图片
//     膨胀成 5.4 MiB 的 base64 塞进 JSON，再在前端拼成 data: URL 让浏览器解一遍；
//   - 文本走 JSON：内容要进 <pre>，还要带 size / tooLarge / encoding 这些元信息。
//
// 图片接口是**唯一**会把远端文件内容以 inline 形式交给浏览器的接口，所以它只认白名单里
// 的类型（按魔术字节判，不看扩展名）、并且带 nosniff——见 handleFsImage 的注释。
// ---------------------------------------------------------------------------

// 编码名（/api/fs/read 的 encoding 字段）：只报我们真的认出来的编码，认不出来时是空串
// （此时 text 也是 false，前端按「不是文本」处理）。
const (
	encodingUTF8    = "utf-8"
	encodingUTF16LE = "utf-16le"
	encodingUTF16BE = "utf-16be"
)

var (
	utf8BOM    = []byte{0xef, 0xbb, 0xbf}
	utf16LEBOM = []byte{0xff, 0xfe}
	utf16BEBOM = []byte{0xfe, 0xff}
)

// handleFsRead 读取远程文本文件（文件管理器预览用）。
//
// 只回**能当文本展示**的内容：不是文本（二进制 / 认不出的编码）与超过上限时都只回元信息，
// 让界面说清原因并引导去下载，绝不把二进制塞进 JSON 让前端显示成乱码——乱码会让人以为
// 文件已经损坏。
func (m *AdminMux) handleFsRead(w http.ResponseWriter, r *http.Request) *apierr.Error {
	var req fsReq
	if apiErr := m.decodeJSON(w, r, &req); apiErr != nil {
		return apiErr
	}
	rt, apiErr := m.sessions.runtimeFor(req.SID)
	if apiErr != nil {
		return apiErr
	}
	var (
		size     int64
		content  []byte
		tooLarge bool
	)
	opErr := rt.withSFTP(r.Context(), func(sess *sftpSession) error {
		target, err := requiredRemotePath(sess, req.Path)
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
		// 只接受普通文件（见 errNotRegularFile）：读打开一个 FIFO 会阻塞到有写者出现，
		// 而这条请求整段持着 sftpMu，一次点击就能把整个文件管理卡住。
		if !fi.Mode().IsRegular() {
			return errNotRegularFile
		}
		size = fi.Size()
		if size > maxReadBytes {
			tooLarge = true
			return nil
		}
		f, err := sess.client.Open(target)
		if err != nil {
			return err
		}
		defer f.Close()
		content, err = io.ReadAll(io.LimitReader(f, maxReadBytes+1))
		if int64(len(content)) > maxReadBytes {
			// 读了之后才变大（写日志的文件正是这样）：按超限处理，别把内容塞进 JSON。
			tooLarge = true
			content = nil
		}
		return err
	})
	if opErr != nil {
		return fsError("读取文件失败", opErr)
	}
	decoded, encoding, isText := "", "", false
	if !tooLarge {
		decoded, encoding, isText = decodeText(content)
	}
	if !isText {
		decoded = ""
	}
	return apierr.Respond(w, map[string]any{
		"ok":       true,
		"size":     size,
		"text":     isText,
		"tooLarge": tooLarge,
		"content":  decoded,
		"encoding": encoding,
	})
}

// decodeText 判定 content 能不能当文本展示；能则返回 UTF-8 文本与编码名。
//
// 认三种情况（NAS 上最常见，全部是标准库能解的）：
//  1. UTF-8：BOM 剥掉——留着的话界面上第一行会多一个看不见的字符；
//  2. 带 BOM 的 UTF-16 LE/BE：Windows 记事本「Unicode」另存、PowerShell 5 重定向的产物，
//     这些正是用户想看的「配置文件」，按二进制拒掉就白瞎了这个功能；
//  3. 不带 BOM 的 UTF-16：按 NUL 的分布猜字节序（见 utf16Order），猜完还要过 mostlyPrintable。
//
// **GBK / GB18030 不认**：解码要 golang.org/x/text，而本仓库的 go.mod 只允许 x/crypto 与
// pkg/sftp（见 AGENTS 第 3 节）。按字节硬猜的后果是把乱码当内容展示给用户——那比明说
// 「不是文本」更糟（用户会以为文件坏了）。前端文案里点了「其它编码」，指向的就是这一种。
func decodeText(content []byte) (text string, encoding string, ok bool) {
	if len(content) == 0 {
		// 空文件是文本：打开一个空配置文件不该看到「不是文本文件」。
		return "", encodingUTF8, true
	}
	if bytes.HasPrefix(content, utf8BOM) {
		rest := content[len(utf8BOM):]
		if utf8.Valid(rest) && !bytes.ContainsRune(rest, 0) {
			return string(rest), encodingUTF8, true
		}
		return "", "", false
	}
	if bytes.HasPrefix(content, utf16LEBOM) {
		return decodeUTF16(content[len(utf16LEBOM):], false)
	}
	if bytes.HasPrefix(content, utf16BEBOM) {
		return decodeUTF16(content[len(utf16BEBOM):], true)
	}
	if bytes.IndexByte(content, 0) < 0 {
		// 没有 NUL：只认合法 UTF-8（这是原来就有的口径，保持不动）。
		if utf8.Valid(content) {
			return string(content), encodingUTF8, true
		}
		return "", "", false
	}
	// 有 NUL 又没有 BOM：可能是无 BOM 的 UTF-16（Windows 上并不少见）。猜错了会把乱码
	// 当文本，所以这一路要过三道关：NUL 分布像 UTF-16、解出来没有替换字符、且能给人看。
	// （带 BOM 的文件不看后两道——BOM 已经是明确声明，文件本身残缺也照样展示。）
	if bigEndian, ok := utf16Order(content); ok {
		decoded, enc, decodedOK := decodeUTF16(content, bigEndian)
		if decodedOK && !strings.ContainsRune(decoded, utf8.RuneError) && mostlyPrintable(decoded) {
			return decoded, enc, true
		}
	}
	return "", "", false
}

// decodeUTF16 把 UTF-16 字节串解成 UTF-8 文本（utf16.Decode 会把落单的代理项换成 U+FFFD）。
//
// 解出来的文本里只要还有 NUL，就说明这根本不是 UTF-16 文本（多半是二进制里恰好凑出了
// 「NUL 只落在同一奇偶性」的分布），一律判为非文本。
func decodeUTF16(b []byte, bigEndian bool) (text string, encoding string, ok bool) {
	encoding = encodingUTF16LE
	if bigEndian {
		encoding = encodingUTF16BE
	}
	if len(b)%2 != 0 {
		// 落单的字节：文件被截断在半个字符上，丢掉末尾那个字节就好，不必整篇判死。
		b = b[:len(b)-1]
	}
	units := make([]uint16, len(b)/2)
	for i := range units {
		if bigEndian {
			units[i] = uint16(b[2*i])<<8 | uint16(b[2*i+1])
		} else {
			units[i] = uint16(b[2*i+1])<<8 | uint16(b[2*i])
		}
	}
	decoded := string(utf16.Decode(units))
	// 解出来还有 NUL，说明这根本不是 UTF-16 文本（多半是二进制里恰好凑出了「NUL 只落在
	// 同一奇偶性」的分布）。
	if strings.IndexByte(decoded, 0) >= 0 {
		return "", "", false
	}
	return decoded, encoding, true
}

// utf16Order 用开头一段的 NUL 分布猜无 BOM UTF-16 的字节序。
//
// UTF-16 里 ASCII 字符的高位字节是 0，所以 NUL 只会落在**同一个奇偶性**的位置上
// （小端落奇数位、大端落偶数位）。两种位置都出现 NUL 就不是 UTF-16——宁可判成二进制，
// 也不要按错的字节序解出一屏乱码。
//
// 长度必须是偶数：奇数长度的字节串不可能是完整的 UTF-16（测试里那个 3 字节的
// "a\x00b" 正是靠这一条被挡住——它按 NUL 位置看恰好「像」小端 UTF-16）。
func utf16Order(b []byte) (bigEndian bool, ok bool) {
	if len(b) < 4 || len(b)%2 != 0 {
		return false, false
	}
	sample := b
	if len(sample) > 4096 {
		sample = sample[:4096]
	}
	var even, odd int
	for i, c := range sample {
		if c != 0 {
			continue
		}
		if i%2 == 0 {
			even++
		} else {
			odd++
		}
	}
	switch {
	case even == 0 && odd > 0:
		return false, true
	case odd == 0 && even > 0:
		return true, true
	default:
		return false, false
	}
}

// mostlyPrintable 判断一段文本是不是「能给人看」：可打印字符（含中文）占比 ≥ 95%。
//
// 只用于**猜出来的**编码（无 BOM 的 UTF-16）：猜错时解出来的是控制字符与 U+FFFD 的垃圾，
// 这一关把它挡在 <pre> 外面。带 BOM 的文件不看这一条——BOM 已经是明确声明。
func mostlyPrintable(s string) bool {
	if s == "" {
		return true
	}
	printable, total := 0, 0
	for _, r := range s {
		total++
		if unicode.IsPrint(r) || r == '\n' || r == '\r' || r == '\t' {
			printable++
		}
	}
	return printable*100 >= total*95
}

// imageTypes 是图片预览的白名单：**按魔术字节嗅探出来的 MIME** 才认，扩展名一概不看
// （把文本文件改名成 .png 不会因此被当成图片）。
//
// 故意不含 SVG（image/svg+xml）：SVG 是能带 <script> 的 XML，而本接口是把远端文件以
// inline 形式交给浏览器的**唯一**入口。用 <img> 渲染时脚本不执行，但 blob:/同源地址一旦
// 被当页面打开就是一个同源脚本执行入口（本应用没有鉴权，同源脚本能读会话、开终端）。
// SVG 归文本预览看源码——它本来就是 XML 文本，在 <pre> 里读也合适。
var imageTypes = map[string]bool{
	"image/png":    true,
	"image/jpeg":   true,
	"image/gif":    true,
	"image/webp":   true,
	"image/bmp":    true,
	"image/x-icon": true,
	"image/avif":   true,
}

// sniffImageType 返回白名单内的图片 MIME；不是白名单图片就回 false。
//
// 先交给 http.DetectContentType（标准库认 png/jpeg/gif/webp/bmp/ico），再补一个 AVIF：
// 它是 ISO-BMFF 容器（ftyp 盒），标准库的嗅探表里没有它（实测回 application/octet-stream）。
func sniffImageType(data []byte) (string, bool) {
	ct := http.DetectContentType(data)
	if imageTypes[ct] {
		return ct, true
	}
	if isAVIF(data) {
		return "image/avif", true
	}
	return "", false
}

// isAVIF 判断 data 是不是 ISO-BMFF 的 AVIF/AVIF 序列（ftyp 盒里带 avif / avis 品牌）。
//
// 不能只看 major brand（ftyp 盒的第 8–12 字节）：AVIF 规范只要求 `avif` 出现在
// **compatible_brands** 列表里，把 avif 放在 compatible_brands、major brand 用别的
// （`mif1` / `heic` 这类）的文件是合法且常见的。只看 major brand 会把它们判成「不是图片」，
// 用户看到的是「内容不是图片，只是名字像」——而浏览器其实渲染得出来。
//
// ftyp 盒的布局（ISO/IEC 14496-12 §4.3）：size(4) | 'ftyp'(4) | major_brand(4) |
// minor_version(4) | compatible_brands(4 × n，直到盒尾)。盒长自述在头 4 字节里，
// 但可能不可信（0 = 直到文件尾，1 = 64 位长度），所以再按**实际可用的字节数**封顶。
func isAVIF(data []byte) bool {
	if len(data) < 12 || string(data[4:8]) != "ftyp" {
		return false
	}
	if brand := data[8:12]; string(brand) == "avif" || string(brand) == "avis" {
		return true
	}
	end := len(data)
	if size := uint32(data[0])<<24 | uint32(data[1])<<16 | uint32(data[2])<<8 | uint32(data[3]); size >= 16 && int(size) < end {
		end = int(size)
	}
	for off := 16; off+4 <= end; off += 4 {
		switch string(data[off : off+4]) {
		case "avif", "avis":
			return true
		}
	}
	return false
}

// handleFsImage 读取远程图片文件（文件管理器预览用），成功时回**原始字节 + inline**。
//
// 上限 maxImageBytes（4 MiB，用户要求）：超过就回 too_large，界面**不显示**预览、只说明
// 原因并给「下载」。先 Stat 再读，所以超限的图片一个字节都不会真的传过来。
func (m *AdminMux) handleFsImage(w http.ResponseWriter, r *http.Request) *apierr.Error {
	var req fsReq
	if apiErr := m.decodeJSON(w, r, &req); apiErr != nil {
		return apiErr
	}
	rt, apiErr := m.sessions.runtimeFor(req.SID)
	if apiErr != nil {
		return apiErr
	}
	var data []byte
	opErr := rt.withSFTP(r.Context(), func(sess *sftpSession) error {
		target, err := requiredRemotePath(sess, req.Path)
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
		// 与 /api/fs/read 同一条判据（见 errNotRegularFile）：不能拿 FIFO / 设备节点去 Open。
		if !fi.Mode().IsRegular() {
			return errNotRegularFile
		}
		if fi.Size() > maxImageBytes {
			return apierr.TooLarge("图片 %s 超过预览上限 %s", humanSize(fi.Size()), humanSize(maxImageBytes))
		}
		f, err := sess.client.Open(target)
		if err != nil {
			return err
		}
		defer f.Close()
		data, err = io.ReadAll(io.LimitReader(f, maxImageBytes+1))
		if int64(len(data)) > maxImageBytes {
			// 读了之后才变大（正在被写入的文件）：同样按超限处理。
			return apierr.TooLarge("图片 %s 超过预览上限 %s", humanSize(int64(len(data))), humanSize(maxImageBytes))
		}
		return err
	})
	if opErr != nil {
		return fsError("读取图片失败", opErr)
	}
	ctype, ok := sniffImageType(data)
	if !ok {
		// invalid（400）而不是 internal：这是「你点的这个文件不是图片」，不是服务端故障。
		return apierr.Invalid("不是可预览的图片格式（按文件内容判定，不看扩展名）")
	}
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.Header().Set("Cache-Control", "no-store")
	// 内容原样回，声明什么类型就是什么类型：不让浏览器再去猜（猜出 HTML 就等于给这个地址
	// 开了一个同源脚本执行入口）。
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(data); err != nil {
		// 正文已经发出去了，再回 JSON 也没用（只会多一条 superfluous WriteHeader）。
		logx.Warnf("图片预览 %s 中断：%v", req.Path, err)
	}
	return nil
}
