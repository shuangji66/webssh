<script setup lang="ts">
// FileManager — 会话内的文件管理面板（工具栏 + 面包屑 + 列表 / 网格 + 行内菜单 + 上传）。
//
// 目录内容全部来自 sftp store（见 stores/sftp.ts 的说明）：这个面板与左栏文件树共用同一份
// 数据，组件只管「怎么显示」与「把用户动作翻成 store 调用」，不自己缓存目录列表。
//
// 第二轮在这里落的四件事：
//   1) 列表表头可点排序（升序 → 降序 → 取消），状态与持久化都在 store，这里只画箭头；
//   2) 权限列在符号（-rw-r--r--）与八进制（0644）之间切换，八进制由 modeToOctal 换算；
//   3) 「更多」菜单补齐 复制 / 剪切 / 粘贴 / 压缩为 tar.gz / 解压 / 导出至飞牛 /
//      在此目录新建终端（去掉了「打开」——点行本身就是打开）；
//   4) 桌面端工具栏加「返回终端」：主区域双模式下，文件管理器占满主区域，用户需要一个
//      就地回到终端的入口（另一个入口是左栏顶部那个「完整文件管理」图标按钮）。
//
// 弹窗（重命名 / 新建 用 PromptDialog，删除与覆盖确认用 ConfirmDialog）由 shell 模块提供，见契约 6.2。
import { computed, nextTick, onBeforeUnmount, reactive, ref, watch } from 'vue'
import { t } from '@/i18n'
import {
  ApiError,
  describeError,
  type FsEntry,
  type UploadEntry,
  type UploadResult
} from '@/serverapi'
import { usePaneControlsStore } from '@/stores/paneControls'
import { useSessionUiStore } from '@/stores/sessionUi'
import { useSftpStore } from '@/stores/sftp'
import { useTerminalsStore } from '@/stores/terminals'
import { useToastStore } from '@/stores/toast'
import {
  baseName,
  formatTime,
  humanSize,
  joinPath,
  modeToOctal,
  parentPath,
  permText,
  type SortKey
} from '@/utils/format'
import { revealActive, wheelToHorizontal } from '@/utils/scroll'
import ConfirmDialog from '@/components/ConfirmDialog.vue'
import PromptDialog from '@/components/PromptDialog.vue'
import PreviewDialog from './PreviewDialog.vue'
import UploadDialog from './UploadDialog.vue'

// Vue 3.5 的响应式 props 解构：sid 在组件生命周期内不变，但解构写法让模板里少一层 props.。
const { sid, mobile = false } = defineProps<{ sid: string; mobile?: boolean }>()

const sftp = useSftpStore()
const sessionUi = useSessionUiStore()
const terminals = useTerminalsStore()
const panes = usePaneControlsStore()
const toastStore = useToastStore()

/** 统一的提示出口：文案一律走 i18n，失败必须说清原因（不能「点了没反应」）。 */
function toast(msg: string, type: 'success' | 'error' | 'info' = 'error') {
  toastStore.show(msg, type)
}

const state = computed(() => sftp.stateOf(sid))
/** 展示用条目：排序（若启用）在 store 里算，这里拿到的已经是最终顺序。 */
const entries = computed(() => sftp.sorted(sid))
const selected = computed(() => state.value.selected)
const selecting = computed(() => selected.value.length > 0)

// ---- 生命周期：进面板就定位家目录 ----

// ensure 幂等（左栏文件树也会调它），失败时内部会允许重试，因此这里不需要额外判重。
watch(() => sid, (id) => void sftp.ensure(id), { immediate: true })

/**
 * 运行信息（导出目标目录 / 压缩包输出目录）也要尽早拿：它对「导出至飞牛」这个菜单项是
 * 禁用还是可点说了算，等到用户点开菜单才去请求，会先闪一下「不可用」。
 */
watch(() => sid, () => void sftp.ensureRuntime(), { immediate: true })

// 换目录就清掉选择：选择项是按「当前目录下的完整路径」记的，跟着跑到别的目录去，
// 「删除所选」会在用户看不见的地方删东西。
watch(() => state.value.dir, () => sftp.clearSelection(sid))

// ---- 面包屑 ----

const crumbEl = ref<HTMLElement | null>(null)

const crumbs = computed(() => {
  const dir = state.value.dir || state.value.home || ''
  const out = [{ label: t('files_root'), path: '/' }]
  let acc = ''
  for (const part of dir.split('/').filter(Boolean)) {
    acc += `/${part}`
    out.push({ label: part, path: acc })
  }
  return out
})

// 进到深层目录后末段会跑到可视区外，切片一换就把它滚回来
watch(
  () => state.value.dir,
  () => void nextTick(() => revealActive(crumbEl.value, '.crumb-current'))
)

function go(path: string) {
  void sftp.navigate(sid, path)
}

// ---- 工具栏动作 ----

function goUp() {
  if (state.value.dir === '/' || !state.value.dir) return
  void sftp.navigate(sid, parentPath(state.value.dir))
}

function retry() {
  // 家目录还没定位到（ensure 失败）时重试 ensure；否则重拉当前目录
  void (state.value.dir ? sftp.refresh(sid) : sftp.ensure(sid))
}

function toggleView() {
  sftp.setView(sid, state.value.view === 'list' ? 'grid' : 'list')
}

function selectAll() {
  sftp.select(
    sid,
    entries.value.map((entry) => entry.path)
  )
}

/**
 * 回到终端。
 *
 * 主区域双模式（桌面）下，文件管理器占满主区域，这个按钮就是回程；切换之后还要把焦点
 * 还给 xterm——只切模式不聚焦的话，用户接着敲的字符没有落点（要点一下终端才生效），
 * 这正是「点了按钮就没反应」的经典来源。
 */
function backToTerminal() {
  sessionUi.setMainMode(sid, 'terminal')
  focusTerminal()
}

/** 聚焦当前激活的终端；activeOf 为 null（激活项在文件管理上）时退回第一个终端。 */
function focusTerminal() {
  void nextTick(() => {
    const uid = terminals.activeOf(sid)?.uid ?? terminals.tabsOf(sid)[0]?.uid
    if (uid) panes.get(sid, uid)?.focus()
  })
}

/** 在当前（或菜单指向的）目录里新开一个终端。 */
function openTerminalHere(dir: string) {
  const target = dir || state.value.dir
  if (!target) return
  hideMenu()
  terminals.openTerminalAt(sid, target)
  // 「在此目录新建终端」的结果是终端，不把主区域切过去就等于没有反馈（桌面）
  sessionUi.setMainMode(sid, 'terminal')
  focusTerminal()
}

// ---- 排序 / 权限显示 ----

/** 表头：key 用来排序，宽度类必须与列表行一致，否则表头与内容会错位。 */
const headers = computed<{ key: SortKey; label: string; cls: string }[]>(() => [
  { key: 'name', label: t('files_name'), cls: 'min-w-0 flex-1 justify-start' },
  { key: 'size', label: t('files_size'), cls: 'w-16 shrink-0 justify-end' },
  { key: 'mtime', label: t('files_modified'), cls: 'w-24 shrink-0 justify-end' }
])

/** 该列当前的排序方向；null = 这一列没在排序。 */
function sortDirOf(key: SortKey): 'asc' | 'desc' | null {
  return state.value.sort.key === key ? state.value.sort.dir : null
}

/**
 * 表头的提示与无障碍名：说明**点下去会发生什么**（循环 升序 → 降序 → 取消），
 * 而不是当前状态——用户看到箭头就知道现在是什么状态了。
 */
function sortTitle(key: SortKey, label: string): string {
  const dir = sortDirOf(key)
  const next =
    dir === null ? t('files_sort_asc') : dir === 'asc' ? t('files_sort_desc') : t('files_sort_none')
  return `${label} ${next}`
}

/** 权限列的文本：符号串原样显示，八进制由 modeToOctal 从符号串换算（认不出来就留空）。 */
function modeText(entry: FsEntry): string {
  return state.value.mode === 'octal' ? modeToOctal(entry.mode) : permText(entry.mode)
}

/** 权限显示模式按钮的提示 = **目标**形态（点下去会切成什么）。 */
const modeTitle = computed(() =>
  state.value.mode === 'symbolic' ? t('files_mode_octal') : t('files_mode_symbolic')
)

/**
 * 压缩包输出目录的说明条。
 *
 * 压缩完成后 store 会把当前目录切到 archiveDir（产物在那儿），用户会突然站在 /tmp 下——
 * 这一行就是回答「我为什么在这儿」，否则看起来像是自己跳错了目录。
 */
const inArchiveDir = computed(() => !!sftp.archiveDir() && state.value.dir === sftp.archiveDir())

// ---- 行内菜单（右键 / 「···」都走它） ----

const menu = reactive({ show: false, x: 0, y: 0, path: '' })
const menuEl = ref<HTMLElement | null>(null)
/** 菜单离窗口边缘至少留这么多，免得贴着边被切掉 */
const MENU_MARGIN = 8

function openMenu(entry: FsEntry, event: MouseEvent) {
  menu.show = true
  menu.x = event.clientX
  menu.y = event.clientY
  menu.path = entry.path
  // 菜单里「导出至飞牛」是否可点取决于运行信息：万一 home store 那次请求失败了，这里再试一次
  void sftp.ensureRuntime()
  // 先按指针位置摆好，渲染出来量到真实尺寸后再夹回窗口内（同一帧内完成，看不出来）
  void nextTick(clampMenu)
  window.setTimeout(() => window.addEventListener('click', hideMenu, { once: true }), 0)
}

function clampMenu() {
  const el = menuEl.value
  if (!el) return
  const { width, height } = el.getBoundingClientRect()
  menu.x = Math.max(MENU_MARGIN, Math.min(menu.x, window.innerWidth - width - MENU_MARGIN))
  menu.y = Math.max(MENU_MARGIN, Math.min(menu.y, window.innerHeight - height - MENU_MARGIN))
}

function hideMenu() {
  menu.show = false
}

// ---- 打开 / 下载 / 复制路径 ----

const previewVisible = ref(false)
const previewPath = ref('')

function onEntryClick(entry: FsEntry) {
  if (selecting.value) {
    toggleSelected(entry)
    return
  }
  openEntry(entry)
}

function toggleSelected(entry: FsEntry) {
  const next = selected.value.includes(entry.path)
    ? selected.value.filter((p) => p !== entry.path)
    : [...selected.value, entry.path]
  sftp.select(sid, next)
}

function openEntry(entry: FsEntry) {
  hideMenu()
  if (entry.dir) {
    go(entry.path)
    return
  }
  previewPath.value = entry.path
  previewVisible.value = true
}

async function downloadEntry(entry: FsEntry) {
  hideMenu()
  try {
    await sftp.download(sid, entry.path)
    toast(t('files_download_started', { name: entry.name }), 'success')
  } catch (e) {
    toast(t('files_download_failed', { error: describeError(e) }))
  }
}

/**
 * 复制完整路径。
 *
 * 优先用 navigator.clipboard；HTTP（非安全上下文）下它根本不存在，此时退回
 * textarea + execCommand —— 本应用经常被反代到 http 上用，直接失败会让「复制路径」
 * 变成一句「点了没反应」。
 */
async function copyPath(path: string) {
  hideMenu()
  try {
    if (navigator.clipboard?.writeText) {
      await navigator.clipboard.writeText(path)
    } else {
      const area = document.createElement('textarea')
      area.value = path
      area.setAttribute('readonly', '')
      area.style.position = 'fixed'
      area.style.opacity = '0'
      document.body.appendChild(area)
      area.select()
      const ok = document.execCommand('copy')
      document.body.removeChild(area)
      if (!ok) throw new Error('copy failed')
    }
    toast(t('files_path_copied'), 'success')
  } catch (e) {
    toast(t('files_op_failed', { error: describeError(e) }))
  }
}

// ---- 新建 / 重命名 / 删除（PromptDialog / ConfirmDialog） ----

interface PromptJob {
  title: string
  label: string
  initial: string
  run: (value: string) => void
}

const prompt = reactive<PromptJob & { show: boolean }>({
  show: false,
  title: '',
  label: '',
  initial: '',
  run: () => {}
})

function askName(job: PromptJob) {
  prompt.title = job.title
  prompt.label = job.label
  prompt.initial = job.initial
  prompt.run = job.run
  prompt.show = true
}

function onPromptConfirm(value: string) {
  const run = prompt.run
  prompt.show = false
  prompt.run = () => {}
  run(value)
}

const confirm = reactive<{
  show: boolean
  title: string
  message: string
  danger: boolean
  run: () => void
}>({ show: false, title: '', message: '', danger: false, run: () => {} })

function askConfirm(title: string, message: string, run: () => void) {
  confirm.title = title
  confirm.message = message
  confirm.danger = true
  confirm.run = run
  confirm.show = true
}

function onConfirmOk() {
  const run = confirm.run
  confirm.show = false
  run()
}

/** 名字合法判据：非空、不含分隔符、不是 . / ..（否则会被后端当成路径而不是名字）。 */
function badName(name: string): 'empty' | 'invalid' | '' {
  if (!name.trim()) return 'empty'
  const n = name.trim()
  if (n === '.' || n === '..' || n.includes('/')) return 'invalid'
  return ''
}

function newEntry(dir: boolean) {
  askName({
    title: dir ? t('files_new_dir_title') : t('files_new_file_title'),
    label: t('files_new_name_prompt'),
    initial: '',
    run: (raw) => {
      const bad = badName(raw)
      if (bad === 'empty') {
        toast(t('files_name_required'))
        return
      }
      if (bad) {
        toast(t('files_name_invalid'))
        return
      }
      void createEntry(dir, raw.trim())
    }
  })
}

async function createEntry(dir: boolean, name: string) {
  const parent = state.value.dir
  if (!parent) return
  try {
    if (dir) await sftp.createDir(sid, parent, name)
    else await sftp.createFile(sid, parent, name)
    toast(t('files_created', { name }), 'success')
  } catch (e) {
    toast(t('files_op_failed', { error: describeError(e) }))
  }
}

function renameEntry() {
  const path = menu.path
  const name = baseName(path)
  hideMenu()
  askName({
    title: t('files_rename_title'),
    label: t('files_rename_prompt'),
    initial: name,
    run: (raw) => {
      const bad = badName(raw)
      if (bad) {
        // 重命名同时挡住空名字与分隔符：名字里带 / 是「搬到子目录」，不是用户按重命名时的意思
        toast(t('files_rename_invalid'))
        return
      }
      const next = raw.trim()
      if (next === name) return
      void doRename(path, next)
    }
  })
}

async function doRename(path: string, name: string) {
  try {
    await sftp.rename(sid, path, joinPath(parentPath(path), name))
    toast(t('files_renamed', { name }), 'success')
  } catch (e) {
    toast(t('files_op_failed', { error: describeError(e) }))
  }
}

/**
 * 菜单动作作用在哪些路径上。
 *
 * 「右键点在选中项上」就作用于整个选择：用户已经勾了三项，再对着其中一项点删除，意图显然是
 * 删这三项（资源管理器都是这个行为）。右键点在没勾的项上则只作用于它自己，避免「顺手点开
 * 另一项、结果删掉了刚才勾的一批」。压缩用得上多项，复制 / 剪切 / 删除同理。
 */
function targetPaths(entry: FsEntry): string[] {
  if (selected.value.length > 1 && selected.value.includes(entry.path)) return [...selected.value]
  return [entry.path]
}

function askDelete(entry: FsEntry) {
  hideMenu()
  const targets = targetPaths(entry)
  if (targets.length > 1) {
    askConfirm(
      t('common_tip'),
      t('files_delete_selected_confirm', { n: targets.length }),
      () => void removePaths(targets)
    )
    return
  }
  askConfirm(t('common_tip'), t('files_delete_confirm', { name: entry.name }), () =>
    void removePaths(targets)
  )
}

/** 按路径逐个删（后端一次只收一个路径）。dir 取当下列表里的真实类型。 */
async function removePaths(paths: string[]) {
  // 单个删除给一条「已删除 xxx」；批量删只报失败——一次删十项弹十条提示反而盖住了报错
  const single = paths.length === 1 ? state.value.entries.find((e) => e.path === paths[0]) : null
  let failed = false
  for (const path of paths) {
    const entry = state.value.entries.find((e) => e.path === path)
    if (!entry) continue
    try {
      await sftp.remove(sid, entry.path, entry.dir)
    } catch (e) {
      failed = true
      toast(t('files_op_failed', { error: describeError(e) }))
    }
  }
  sftp.clearSelection(sid)
  if (single && !failed) toast(t('files_deleted', { name: single.name }), 'success')
}

function askDeleteSelected() {
  const paths = selected.value.filter((path) => state.value.entries.some((e) => e.path === path))
  if (!paths.length) {
    sftp.clearSelection(sid)
    return
  }
  askConfirm(t('common_tip'), t('files_delete_selected_confirm', { n: paths.length }), () =>
    void removePaths(paths)
  )
}

// ---- 剪贴板：复制 / 剪切 / 粘贴 ----

function copyEntries(paths: string[]) {
  hideMenu()
  sftp.copyToClipboard(sid, paths)
  toast(t('files_copied_items', { n: paths.length }), 'success')
}

function cutEntries(paths: string[]) {
  hideMenu()
  sftp.cutToClipboard(sid, paths)
  toast(t('files_cut_items', { n: paths.length }), 'success')
}

/**
 * 粘贴到某个目录。
 *
 * 失败**不清空**剪贴板（由 store 保证），用户可以重试或换个目录粘；成功后清空，避免
 * 「同一批再粘一遍」这种误操作真的多出一份文件。失败原因原样报出来（同名 / 权限 / 目标没了）。
 */
async function pasteInto(toDir: string) {
  hideMenu()
  if (!toDir) return
  // 剪贴板里第一项的名字：后端把「同名」报成 duplicate 错误码时（11.5 的表格两种都提了，
  // 实现按 skipped 回，这里把两种都兜住）用它说明是哪一个撞了名字。
  const first = sftp.clipboardFor(sid)?.paths[0]
  try {
    const res = await sftp.paste(sid, toDir)
    if (res.count) toast(t('files_paste_done', { n: res.count }), 'success')
    // 后端只回「跳过了几项」，名字是 store 按「源里的名字没出现在结果里」对出来的
    const conflict = res.conflicts[0]
    if (res.skipped && conflict) toast(t('files_paste_exists', { name: conflict }))
  } catch (e) {
    if (e instanceof ApiError && e.code === 'duplicate') {
      toast(first ? t('files_paste_exists', { name: baseName(first) }) : t('files_op_failed_short'))
      return
    }
    toast(t('files_op_failed', { error: describeError(e) }))
  }
}

// ---- 压缩 / 解压 / 导出至飞牛 ----

/** 可解压判据：tar 家族 + zip。后缀表与后端 archive.go 的 tarExts 逐字对应。 */
const ARCHIVE_RE = /\.(tar|tar\.gz|tgz|tar\.bz2|tbz|tbz2|tar\.xz|txz|tar\.zst|tzst|zip)$/i

function isArchive(name: string): boolean {
  return ARCHIVE_RE.test(name)
}

/**
 * 压缩为 tar.gz。
 *
 * 归档名（按被压缩项推导）与「跳到产物目录」都在 store 里做：产物落在服务端固定的
 * archiveDir 下，用户点压缩的目的就是拿到那个包，停在原目录只看到一条 toast 等于没完成。
 */
async function compressEntries(paths: string[]) {
  hideMenu()
  if (!paths.length) return
  try {
    const archive = await sftp.compress(sid, paths)
    toast(t('files_compress_done', { path: archive.path }), 'success')
  } catch (e) {
    toast(t('files_op_failed', { error: describeError(e) }))
  }
}

async function extractEntry(entry: FsEntry) {
  hideMenu()
  try {
    const dir = await sftp.extract(sid, entry.path)
    toast(t('files_extract_done', { dir }), 'success')
  } catch (e) {
    toast(t('files_op_failed', { error: describeError(e) }))
  }
}

/**
 * 导出至飞牛（NAS 共享目录）。
 *
 * 目标已有同名项时后端回 `duplicate`，这里**不静默覆盖**：问一次再带着 overwrite 重试。
 * 别的失败原样报出（未配置共享目录后端也会直接回 no_share_path）。
 */
async function exportEntry(entry: FsEntry, overwrite: boolean) {
  hideMenu()
  try {
    const out = await sftp.exportToNas(sid, entry.path, overwrite)
    toast(t('files_export_nas_done', { path: out.path }), 'success')
  } catch (e) {
    if (e instanceof ApiError && e.code === 'duplicate') {
      // 拿被导出的那个路径当确认信息：用户要能看出「覆盖的是哪一个」
      askConfirm(t('files_export_overwrite'), entry.path, () => void exportEntry(entry, true))
      return
    }
    toast(t('files_op_failed', { error: describeError(e) }))
  }
}

// ---- 上传：弹窗（三来源） + 拖拽（带进度与取消） ----

const uploadVisible = ref(false)

/** 拖拽上传的进度：工具栏「上传」走 UploadDialog 自带的进度与取消，这里管的是拖进来的那批。 */
const dropUploading = ref(false)
const dropPercent = ref(0)
const dragging = ref(false)
let dropController: AbortController | null = null

function onDragOver(event: DragEvent) {
  // 只认「拖文件进来」：在列表上拖选文本也会触发 dragover，不该把投放区点亮
  if (!event.dataTransfer?.types.includes('Files')) return
  dragging.value = true
}

function onDragLeave(event: DragEvent) {
  const to = event.relatedTarget as Node | null
  if (!to || !(event.currentTarget as HTMLElement).contains(to)) dragging.value = false
}

async function onDrop(event: DragEvent) {
  dragging.value = false
  const files = Array.from(event.dataTransfer?.files ?? [])
  if (!files.length) {
    // 拖目录进来时 Chrome 给不出 File（要递归 webkitGetAsEntry），只能引导去上传弹窗
    toast(t('upload_none'), 'info')
    return
  }
  if (dropUploading.value) return
  const ctl = new AbortController()
  dropController = ctl
  dropUploading.value = true
  dropPercent.value = 0
  const entries: UploadEntry[] = files.map((file) => ({ file, path: file.name }))
  try {
    // 拖进来的文件不覆盖同名（和后端默认语义一致）：拖拽太容易误操作，静默覆盖代价太大
    const res = await sftp.uploadLocal(
      sid,
      state.value.dir,
      entries,
      false,
      (p) => {
        dropPercent.value = p
      },
      ctl.signal
    )
    toast(t('upload_done', { n: res.files.length }), 'success')
    if (res.skipped > 0) toast(t('upload_skipped', { n: res.skipped }), 'info')
  } catch (e) {
    if (ctl.signal.aborted) toast(t('upload_canceled'), 'info')
    else toast(t('upload_failed', { error: describeError(e) }))
  } finally {
    if (dropController === ctl) dropController = null
    dropUploading.value = false
  }
}

function cancelDropUpload() {
  dropController?.abort()
}

/** 上传弹窗报完成时目录已经由 store 刷新过，这里只把过时的选择态清掉。 */
function onUploaded(_result: UploadResult) {
  sftp.clearSelection(sid)
}

// ---- 菜单项 ----

/**
 * `dim` = 看起来不可点，但点下去会给一句原因。
 *
 * 为什么不用真的 disabled：真 disabled 的按钮连事件都没有，「导出至飞牛」在没配共享目录时
 * 就会变成用户不知道为什么不响的按钮；给一句「未配置飞牛共享目录」才知道该怎么办。
 */
type MenuItem = { label: string; run: () => void; danger?: boolean; dim?: boolean }

/** 粘贴项：没有剪贴板就整项不出现；剪贴板来自别的会话时禁用并说明原因（11.4）。 */
function pasteItem(toDir: string): MenuItem | null {
  if (sftp.clipboardIsForeign(sid)) {
    return {
      label: t('files_paste'),
      dim: true,
      run: () => toast(t('files_paste_cross_session'), 'info')
    }
  }
  if (!sftp.clipboardFor(sid)) return null
  return { label: t('files_paste'), run: () => void pasteInto(toDir) }
}

/** 导出项：exportRoot 为空 = 服务端没配共享目录，禁用并说明。 */
function exportItem(entry: FsEntry): MenuItem {
  if (!sftp.exportRoot()) {
    return {
      label: t('files_export_nas'),
      dim: true,
      run: () => toast(t('files_export_nas_unavailable'), 'info')
    }
  }
  return { label: t('files_export_nas'), run: () => void exportEntry(entry, false) }
}

const menuItems = computed<MenuItem[]>(() => {
  const entry = entries.value.find((item) => item.path === menu.path)
  if (!entry) return []
  const targets = targetPaths(entry)
  const items: MenuItem[] = []
  // 顺序按契约 11.4：粘贴（有剪贴板时）/ 重命名 / 复制 / 剪切 / 删除 / 下载 / 导出至飞牛 /
  // 压缩为 tar.gz / 解压（可解压时）/ 复制完整路径 / 在此目录新建终端
  const paste = pasteItem(entry.dir ? entry.path : state.value.dir)
  if (paste) items.push(paste)
  items.push({ label: t('files_rename'), run: renameEntry })
  items.push({ label: t('files_copy'), run: () => copyEntries(targets) })
  items.push({ label: t('files_cut'), run: () => cutEntries(targets) })
  items.push({ label: t('common_delete'), danger: true, run: () => askDelete(entry) })
  items.push({ label: t('files_download'), run: () => void downloadEntry(entry) })
  items.push(exportItem(entry))
  items.push({
    label:
      targets.length > 1
        ? t('files_compress_selected', { n: targets.length })
        : t('files_compress'),
    run: () => void compressEntries(targets)
  })
  if (!entry.dir && isArchive(entry.name)) {
    items.push({ label: t('files_extract'), run: () => void extractEntry(entry) })
  }
  items.push({ label: t('files_copy_path'), run: () => void copyPath(entry.path) })
  items.push({
    label: t('files_open_terminal_here'),
    run: () => openTerminalHere(entry.dir ? entry.path : state.value.dir)
  })
  return items
})

onBeforeUnmount(hideMenu)
</script>

<template>
  <div class="flex h-full min-h-0 flex-col">
    <!-- 工具栏：窄面板放不下时自己折行，按钮只有图标（说明挂原生 title，触屏上点完就消失） -->
    <div
      class="flex shrink-0 flex-wrap items-center gap-0 border-b border-line px-1 py-1 dark:border-line-dark"
    >
      <button
        type="button"
        class="tree-tool"
        :disabled="!state.back.length"
        :title="t('files_back')"
        :aria-label="t('files_back')"
        @click="sftp.back(sid)"
      >
        <svg
          class="h-4 w-4"
          viewBox="0 0 24 24"
          fill="none"
          stroke="currentColor"
          stroke-width="1.5"
          stroke-linecap="round"
          stroke-linejoin="round"
        >
          <path d="M19 12H5" />
          <path d="m12 19-7-7 7-7" />
        </svg>
      </button>
      <button
        type="button"
        class="tree-tool"
        :disabled="!state.forward.length"
        :title="t('files_forward')"
        :aria-label="t('files_forward')"
        @click="sftp.forward(sid)"
      >
        <svg
          class="h-4 w-4"
          viewBox="0 0 24 24"
          fill="none"
          stroke="currentColor"
          stroke-width="1.5"
          stroke-linecap="round"
          stroke-linejoin="round"
        >
          <path d="M5 12h14" />
          <path d="m12 5 7 7-7 7" />
        </svg>
      </button>
      <button
        type="button"
        class="tree-tool"
        :title="t('files_refresh')"
        :aria-label="t('files_refresh')"
        @click="retry"
      >
        <svg
          class="h-4 w-4"
          viewBox="0 0 24 24"
          fill="none"
          stroke="currentColor"
          stroke-width="1.5"
          stroke-linecap="round"
          stroke-linejoin="round"
        >
          <path d="M21 12a9 9 0 1 1-3-6.7" />
          <path d="M21 4v5h-5" />
        </svg>
      </button>
      <button
        type="button"
        class="tree-tool"
        :disabled="!state.dir || state.dir === '/'"
        :title="t('files_up')"
        :aria-label="t('files_up')"
        @click="goUp"
      >
        <svg
          class="h-4 w-4"
          viewBox="0 0 24 24"
          fill="none"
          stroke="currentColor"
          stroke-width="1.5"
          stroke-linecap="round"
          stroke-linejoin="round"
        >
          <path d="M12 19V5" />
          <path d="m5 12 7-7 7 7" />
        </svg>
      </button>

      <span class="mx-1 h-4 w-px shrink-0 bg-line dark:bg-line-dark" />

      <button
        type="button"
        class="tree-tool"
        :title="t('files_new_file')"
        :aria-label="t('files_new_file')"
        @click="newEntry(false)"
      >
        <svg
          class="h-4 w-4"
          viewBox="0 0 24 24"
          fill="none"
          stroke="currentColor"
          stroke-width="1.5"
          stroke-linecap="round"
          stroke-linejoin="round"
        >
          <path d="M14 3H7a2 2 0 0 0-2 2v14a2 2 0 0 0 2 2h10a2 2 0 0 0 2-2V8z" />
          <path d="M14 3v5h5" />
          <path d="M12 11v6M9 14h6" />
        </svg>
      </button>
      <button
        type="button"
        class="tree-tool"
        :title="t('files_new_dir')"
        :aria-label="t('files_new_dir')"
        @click="newEntry(true)"
      >
        <svg
          class="h-4 w-4"
          viewBox="0 0 24 24"
          fill="none"
          stroke="currentColor"
          stroke-width="1.5"
          stroke-linecap="round"
          stroke-linejoin="round"
        >
          <path d="M4 20a2 2 0 0 1-2-2V6a2 2 0 0 1 2-2h4l2 2h6a2 2 0 0 1 2 2v12a2 2 0 0 1-2 2z" />
          <path d="M12 11v6M9 14h6" />
        </svg>
      </button>
      <button
        type="button"
        class="tree-tool"
        :title="t('files_upload')"
        :aria-label="t('files_upload')"
        @click="uploadVisible = true"
      >
        <svg
          class="h-4 w-4"
          viewBox="0 0 24 24"
          fill="none"
          stroke="currentColor"
          stroke-width="1.5"
          stroke-linecap="round"
          stroke-linejoin="round"
        >
          <path d="M12 16V4" />
          <path d="m5 11 7-7 7 7" />
          <path d="M4 20h16" />
        </svg>
      </button>
      <button
        type="button"
        class="tree-tool"
        :title="t('files_select_all')"
        :aria-label="t('files_select_all')"
        @click="selectAll"
      >
        <svg
          class="h-4 w-4"
          viewBox="0 0 24 24"
          fill="none"
          stroke="currentColor"
          stroke-width="1.5"
          stroke-linecap="round"
          stroke-linejoin="round"
        >
          <path d="M9 11l3 3 8-8" />
          <path d="M20 12v6a2 2 0 0 1-2 2H6a2 2 0 0 1-2-2V6a2 2 0 0 1 2-2h9" />
        </svg>
      </button>

      <span class="mx-1 h-4 w-px shrink-0 bg-line dark:bg-line-dark" />

      <button
        type="button"
        class="tree-tool"
        :title="state.view === 'list' ? t('files_view_grid') : t('files_view_list')"
        :aria-label="state.view === 'list' ? t('files_view_grid') : t('files_view_list')"
        @click="toggleView"
      >
        <svg
          v-if="state.view === 'list'"
          class="h-4 w-4"
          viewBox="0 0 24 24"
          fill="none"
          stroke="currentColor"
          stroke-width="1.5"
          stroke-linecap="round"
          stroke-linejoin="round"
        >
          <path d="M4 4h7v7H4zM13 4h7v7h-7zM4 13h7v7H4zM13 13h7v7h-7z" />
        </svg>
        <svg
          v-else
          class="h-4 w-4"
          viewBox="0 0 24 24"
          fill="none"
          stroke="currentColor"
          stroke-width="1.5"
          stroke-linecap="round"
          stroke-linejoin="round"
        >
          <path d="M8 6h13M8 12h13M8 18h13M3 6h.01M3 12h.01M3 18h.01" />
        </svg>
      </button>
      <!--
        权限显示模式开关：只在桌面端出现——权限列本身在移动端不渲染（列宽不够），
        留着这个按钮就会是「点了什么也没变」。
        图标区显示的是**当前**形态的文字（-rw / 0644）：两种状态用图形区分不出来，
        直接显示它把权限画成什么样最直观，也和权限列的等宽字体对得上。
      -->
      <button
        v-if="!mobile"
        type="button"
        class="tree-tool"
        :title="modeTitle"
        :aria-label="modeTitle"
        @click="sftp.toggleMode(sid)"
      >
        <span class="font-mono text-[11px]">{{ state.mode === 'octal' ? '0644' : '-rw-' }}</span>
      </button>
      <!-- 桌面端的「返回终端」：文件管理器占满主区域时，这是就地切回终端的入口 -->
      <button
        v-if="!mobile"
        type="button"
        class="tree-tool"
        :title="t('session_hide_files')"
        :aria-label="t('session_hide_files')"
        @click="backToTerminal"
      >
        <svg
          class="h-4 w-4"
          viewBox="0 0 24 24"
          fill="none"
          stroke="currentColor"
          stroke-width="1.5"
          stroke-linecap="round"
          stroke-linejoin="round"
        >
          <path d="M9 4H6a2 2 0 0 0-2 2v12a2 2 0 0 0 2 2h3" />
          <path d="m16 8-4 4 4 4" />
          <path d="M20 12h-8" />
        </svg>
      </button>
    </div>

    <!-- 有选中项时面包屑让位给「已选 / 删除所选」；否则是路径分段（点哪段去哪层） -->
    <div
      v-if="selecting"
      class="flex shrink-0 items-center gap-2 border-b border-line px-2 py-1 dark:border-line-dark"
    >
      <span class="min-w-0 flex-1 truncate text-xs text-ink-soft dark:text-ink-soft-dark">
        {{ t('files_selected', { n: selected.length }) }}
      </span>
      <button type="button" class="g-btn-ghost h-7" @click="sftp.clearSelection(sid)">
        {{ t('files_clear_selection') }}
      </button>
      <button type="button" class="g-btn-ghost h-7 text-danger" @click="askDeleteSelected">
        {{ t('files_delete_selected', { n: selected.length }) }}
      </button>
    </div>
    <div
      v-else
      ref="crumbEl"
      class="thin-scroll flex shrink-0 items-center overflow-x-auto border-b border-line px-2 py-1 text-sm dark:border-line-dark"
      @wheel="wheelToHorizontal"
    >
      <template v-for="(seg, i) in crumbs" :key="seg.path">
        <span v-if="i" class="shrink-0 px-0.5 text-ink-faint">/</span>
        <button
          type="button"
          class="max-w-[12rem] shrink-0 truncate rounded px-1 py-0.5 hover:bg-black/5 dark:hover:bg-white/5"
          :class="
            i === crumbs.length - 1
              ? 'crumb-current font-medium text-ink dark:text-ink-dark'
              : 'text-ink-soft dark:text-ink-soft-dark'
          "
          :title="seg.path"
          @click="go(seg.path)"
        >
          {{ seg.label }}
        </button>
      </template>
    </div>

    <!-- 站在压缩包输出目录时说明一句：用户是被「压缩完成后跳转」带过来的 -->
    <div
      v-if="inArchiveDir"
      class="shrink-0 truncate border-b border-line px-2 py-1 text-[11px] text-ink-faint dark:border-line-dark"
    >
      {{ t('files_archive_dir', { dir: sftp.archiveDir() }) }}
    </div>

    <!-- 拖拽上传的进度与取消（工具栏「上传」走弹窗自带的进度） -->
    <div
      v-if="dropUploading"
      class="flex shrink-0 items-center gap-2 border-b border-line px-2 py-1 dark:border-line-dark"
    >
      <div class="h-1.5 min-w-0 flex-1 overflow-hidden rounded-full bg-black/10 dark:bg-white/10">
        <div
          class="h-full rounded-full bg-brand transition-[width] duration-150"
          :style="{ width: `${dropPercent}%` }"
        />
      </div>
      <span class="shrink-0 text-[11px] text-ink-soft dark:text-ink-soft-dark">
        {{ t('upload_uploading', { percent: dropPercent }) }}
      </span>
      <button type="button" class="g-btn-ghost h-7 shrink-0 text-danger" @click="cancelDropUpload">
        {{ t('upload_cancel') }}
      </button>
    </div>

    <!--
      列表的表头：名称 / 大小 / 修改时间三列可点排序（循环 升序 → 降序 → 取消）。
      目录恒在文件前面由 compareEntries 保证，与点哪一列无关。
      权限列在移动端省掉（列宽不够），与工具栏那个显示模式开关的显隐一致。
    -->
    <div
      v-if="state.view === 'list'"
      class="flex shrink-0 items-center gap-2 border-b border-line px-2 py-1 text-xs font-medium text-ink-soft dark:border-line-dark dark:text-ink-soft-dark"
    >
      <button
        v-for="head in headers"
        :key="head.key"
        type="button"
        class="flex items-center gap-0.5 rounded px-0.5 hover:text-ink dark:hover:text-white"
        :class="head.cls"
        :title="sortTitle(head.key, head.label)"
        :aria-label="sortTitle(head.key, head.label)"
        @click="sftp.cycleSort(sid, head.key)"
      >
        <span class="truncate">{{ head.label }}</span>
        <svg
          v-if="sortDirOf(head.key)"
          class="h-3 w-3 shrink-0"
          viewBox="0 0 24 24"
          fill="none"
          stroke="currentColor"
          stroke-width="2"
          stroke-linecap="round"
          stroke-linejoin="round"
        >
          <path :d="sortDirOf(head.key) === 'asc' ? 'm6 15 6-6 6 6' : 'm6 9 6 6 6-6'" />
        </svg>
      </button>
      <span v-if="!mobile" class="w-20 shrink-0 text-right">{{ t('files_mode') }}</span>
      <span class="w-6 shrink-0" />
    </div>

    <div
      class="thin-scroll min-h-0 flex-1 overflow-auto"
      :class="dragging ? 'ring-2 ring-inset ring-brand/40' : ''"
      @dragover.prevent="onDragOver"
      @dragleave="onDragLeave"
      @drop.prevent="onDrop"
    >
      <div v-if="state.loading" class="px-3 py-6 text-center text-sm text-ink-faint">
        {{ t('files_loading') }}
      </div>

      <div v-else-if="state.error" class="px-3 py-4 text-sm">
        <p class="leading-relaxed break-all text-danger">
          {{ t('files_load_failed') }}：{{ state.error }}
        </p>
        <button type="button" class="g-btn-ghost mt-1 h-8 text-danger" @click="retry">
          {{ t('common_retry') }}
        </button>
      </div>

      <div v-else-if="!entries.length" class="px-3 py-6 text-center text-sm text-ink-faint">
        {{ t('files_empty') }}
      </div>

      <template v-else-if="state.view === 'list'">
        <div
          v-for="entry in entries"
          :key="entry.path"
          class="group flex cursor-pointer items-center gap-2 border-b border-line/60 px-2 py-1.5 text-sm hover:bg-black/5 dark:border-line-dark/60 dark:hover:bg-white/5"
          :class="selected.includes(entry.path) ? 'bg-brand-soft' : ''"
          @click="onEntryClick(entry)"
          @contextmenu.prevent="openMenu(entry, $event)"
        >
          <svg
            v-if="entry.dir"
            class="h-4 w-4 shrink-0 text-warning"
            viewBox="0 0 24 24"
            fill="none"
            stroke="currentColor"
            stroke-width="1.5"
            stroke-linecap="round"
            stroke-linejoin="round"
            :title="t('files_dir')"
          >
            <path d="M4 20a2 2 0 0 1-2-2V6a2 2 0 0 1 2-2h4l2 2h6a2 2 0 0 1 2 2v12a2 2 0 0 1-2 2z" />
          </svg>
          <svg
            v-else
            class="h-4 w-4 shrink-0 text-ink-faint"
            viewBox="0 0 24 24"
            fill="none"
            stroke="currentColor"
            stroke-width="1.5"
            stroke-linecap="round"
            stroke-linejoin="round"
            :title="t('files_file')"
          >
            <path d="M14 3H7a2 2 0 0 0-2 2v14a2 2 0 0 0 2 2h10a2 2 0 0 0 2-2V8z" />
            <path d="M14 3v5h5" />
          </svg>
          <span class="min-w-0 flex-1 truncate" :title="entry.name">{{ entry.name }}</span>
          <span v-if="entry.symlink" class="g-chip shrink-0">
            {{ entry.broken ? t('files_broken_symlink') : t('files_symlink') }}
          </span>
          <span class="w-16 shrink-0 text-right text-xs text-ink-faint">
            {{ entry.dir ? '' : humanSize(entry.size) }}
          </span>
          <span class="w-24 shrink-0 text-right text-xs text-ink-faint">
            {{ formatTime(entry.mtime) }}
          </span>
          <span
            v-if="!mobile"
            class="w-20 shrink-0 truncate text-right font-mono text-[11px] text-ink-faint"
          >
            {{ modeText(entry) }}
          </span>
          <button
            type="button"
            class="tree-tool hover-reveal h-6 w-6 shrink-0"
            :title="t('files_more')"
            :aria-label="t('files_more')"
            @click.stop="openMenu(entry, $event)"
          >
            <svg
              class="h-4 w-4"
              viewBox="0 0 24 24"
              fill="none"
              stroke="currentColor"
              stroke-width="1.5"
              stroke-linecap="round"
              stroke-linejoin="round"
            >
              <path d="M5 12h.01M12 12h.01M19 12h.01" />
            </svg>
          </button>
        </div>
      </template>

      <!-- 网格：列宽按容器自适应，用 inline style 而不是 Tailwind 任意值——后者要写一长串
           带括号与逗号的类名，扫不到就静默退化成单列 -->
      <div
        v-else
        class="grid gap-1 p-2"
        style="grid-template-columns: repeat(auto-fill, minmax(6.5rem, 1fr))"
      >
        <div
          v-for="entry in entries"
          :key="entry.path"
          class="group relative flex cursor-pointer flex-col items-center gap-1 rounded-md border border-transparent px-2 py-2 text-center hover:border-line hover:bg-black/5 dark:hover:border-line-dark dark:hover:bg-white/5"
          :class="selected.includes(entry.path) ? 'bg-brand-soft' : ''"
          @click="onEntryClick(entry)"
          @contextmenu.prevent="openMenu(entry, $event)"
        >
          <svg
            v-if="entry.dir"
            class="h-6 w-6 text-warning"
            viewBox="0 0 24 24"
            fill="none"
            stroke="currentColor"
            stroke-width="1.5"
            stroke-linecap="round"
            stroke-linejoin="round"
          >
            <path d="M4 20a2 2 0 0 1-2-2V6a2 2 0 0 1 2-2h4l2 2h6a2 2 0 0 1 2 2v12a2 2 0 0 1-2 2z" />
          </svg>
          <svg
            v-else
            class="h-6 w-6 text-ink-faint"
            viewBox="0 0 24 24"
            fill="none"
            stroke="currentColor"
            stroke-width="1.5"
            stroke-linecap="round"
            stroke-linejoin="round"
          >
            <path d="M14 3H7a2 2 0 0 0-2 2v14a2 2 0 0 0 2 2h10a2 2 0 0 0 2-2V8z" />
            <path d="M14 3v5h5" />
          </svg>
          <span class="line-clamp-2 w-full break-all text-xs leading-tight" :title="entry.name">
            {{ entry.name }}
          </span>
          <span class="text-[11px] text-ink-faint">
            {{ entry.dir ? t('files_dir') : humanSize(entry.size) }}
          </span>
          <button
            type="button"
            class="tree-tool hover-reveal absolute top-0 right-0 h-6 w-6"
            :title="t('files_more')"
            :aria-label="t('files_more')"
            @click.stop="openMenu(entry, $event)"
          >
            <svg
              class="h-4 w-4"
              viewBox="0 0 24 24"
              fill="none"
              stroke="currentColor"
              stroke-width="1.5"
              stroke-linecap="round"
              stroke-linejoin="round"
            >
              <path d="M5 12h.01M12 12h.01M19 12h.01" />
            </svg>
          </button>
        </div>
      </div>
    </div>

    <!-- 行内「···」/ 右键菜单：固定定位 + 夹回窗口内 -->
    <div
      v-if="menu.show"
      ref="menuEl"
      class="fixed z-50 min-w-[10rem] rounded-md border border-line bg-surface py-1 text-sm shadow-pop dark:border-line-dark dark:bg-surface-dark"
      :style="{ left: `${menu.x}px`, top: `${menu.y}px` }"
    >
      <button
        v-for="item in menuItems"
        :key="item.label"
        type="button"
        class="block w-full whitespace-nowrap px-3 py-1.5 text-left hover:bg-black/5 dark:hover:bg-white/5"
        :class="[
          item.danger ? 'text-danger' : 'text-ink dark:text-ink-dark',
          item.dim ? 'opacity-50' : ''
        ]"
        :aria-disabled="item.dim ? 'true' : 'false'"
        @click="item.run()"
      >
        {{ item.label }}
      </button>
    </div>

    <UploadDialog
      :visible="uploadVisible"
      :sid="sid"
      :dir="state.dir"
      @update:visible="uploadVisible = $event"
      @done="onUploaded"
    />
    <PreviewDialog
      :visible="previewVisible"
      :sid="sid"
      :path="previewPath"
      @update:visible="previewVisible = $event"
    />
    <PromptDialog
      :visible="prompt.show"
      :title="prompt.title"
      :label="prompt.label"
      :initial-value="prompt.initial"
      @update:visible="prompt.show = $event"
      @confirm="onPromptConfirm"
      @cancel="prompt.show = false"
    />
    <ConfirmDialog
      :visible="confirm.show"
      :title="confirm.title"
      :message="confirm.message"
      :danger="confirm.danger"
      @update:visible="confirm.show = $event"
      @confirm="onConfirmOk"
      @cancel="confirm.show = false"
    />
  </div>
</template>
