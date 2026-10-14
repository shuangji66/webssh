// 按**文件名**判断「这是个什么文件」的共用判据（预览 / 编辑 / 解压都用它）。
//
// 为什么单独一个模块：同一套判据要在三处保持一致——预览弹窗决定用哪种预览、文件管理与目录树
// 决定文件行显不显示「编辑」「解压」。各写一份必然走样，而走样的表现是「同一行菜单在两个入口
// 里不一样」。
//
// 这里的判据一律**只看扩展名**（后端再按内容确认，见契约 25.2）：按名字猜是为了决定「用哪种
// 界面」，不是「这个文件到底是什么」——把文本文件改名成 .png 不会因此被当成图片，
// 后端会回 invalid。

/**
 * 能预览的图片扩展名。**与后端 files_preview.go 的 imageTypes 白名单一致**，两边都改才算
 * 支持一种新格式：这里决定「用哪种预览」，后端决定「内容真的是不是图片」。
 *
 * 不含 svg：它是能带脚本的 XML，后端刻意不当图片回（见 imageTypes 的注释），前端也就按
 * 文本预览它——XML 源码本来也适合在 <pre> 里读。
 */
export const IMAGE_EXTS = new Set(['png', 'jpg', 'jpeg', 'jpe', 'gif', 'webp', 'bmp', 'ico', 'avif'])

/** 可解压的归档后缀（与后端 archive.go 的 tarExts 逐字对应）。 */
const ARCHIVE_RE = /\.(tar|tar\.gz|tgz|tar\.bz2|tbz|tbz2|tar\.xz|txz|tar\.zst|tzst|zip)$/i

/** 小写扩展名（不含点）；没有扩展名时是空串。 */
export function extOf(path: string): string {
  const base = path.slice(path.lastIndexOf('/') + 1)
  const at = base.lastIndexOf('.')
  return at <= 0 ? '' : base.slice(at + 1).toLowerCase()
}

/** 预览形态：图片（走 /api/fs/image）还是文本（走 /api/fs/read）。 */
export function previewKindOf(path: string): 'image' | 'text' {
  return IMAGE_EXTS.has(extOf(path)) ? 'image' : 'text'
}

/**
 * 能不能编辑（= 文本预览那一类）：图片没有编辑按钮，目录更不能编辑。
 *
 * 判据只到「按扩展名算文本」这一步——真正的结论由后端给（不是文本 / 没有写权限都回错误码，
 * 界面据此拒绝进入编辑页，见契约 26.2）。这里的作用是**别给不可能编辑的东西显示「编辑」**。
 */
export function canEditPath(path: string): boolean {
  return previewKindOf(path) === 'text'
}

/** 条目版判据（目录一律不可编辑）。 */
export function canEditEntry(entry: { dir: boolean; path: string }): boolean {
  return !entry.dir && canEditPath(entry.path)
}

/** 能不能解压（tar 家族 + zip）。 */
export function isArchivePath(name: string): boolean {
  return ARCHIVE_RE.test(name)
}
