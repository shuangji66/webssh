<script setup lang="ts">
// FileManager — 会话内的文件管理面板（工具栏 + 面包屑 + 列表 / 网格 + 行内菜单 + 上传）。
//
// 目录内容全部来自 sftp store（见 stores/sftp.ts 的说明）：这个面板与左栏文件树共用同一份
// 数据，组件只管「怎么显示」与「把用户动作翻成 store 调用」，不自己缓存目录列表。
//
// 这个面板负责的事：
//   1) 列表表头三列可点排序（升序 → 降序 → 取消），状态与持久化都在 store，这里只画箭头；
//   2) 表头的「权限」列可点，在符号（-rw-r--r--）与八进制（0644）之间切换显示形态
//      （八进制由 modeToOctal 换算）。它**故意**与排序共用表头但互不干扰：排序是三列的三态
//      循环，权限列只切换显示形态，不参与排序——两件事混在一起后用户点一下会同时改变两处；
//   3) 行内「更多」菜单 + **空白处右键菜单**（桌面）：新建文件 / 新建文件夹 / 上传 / 粘贴 /
//      打开终端 / 复制路径；
//   4) 复制 / 剪切 / 粘贴 / 压缩为 tar.gz / 压缩为 zip / 解压 / 导出至飞牛 / 在此目录新建终端。
//
// 第五轮新增两件事：
//   1) 「信息」（行菜单/空白处菜单都有）：弹窗显示条目或当前目录的属性，数据走 /api/fs/info
//      （契约 15.5）——列目录拿不到「当前目录自己」的属性，而目录的「内含多少目录和文件」
//      也只有服务端顺手数一遍最省（只数一层，见 InfoDialog）；
//   2) 地址栏下面那行**常驻剪贴板**（复制之后一直在，直到点「取消」）：它里面的
//      「粘贴到此处」**不清空**剪贴板（可以连着粘到好几个目录），而右键菜单里的「粘贴」
//      保持原有语义（粘完就清）——两种入口的区别在 sftp.paste 的 keepClipboard 参数上。
//      第十一轮起**剪切是例外**：粘完就清、那一行随之消失（不管从哪个入口粘的），
//      因为剪切是「搬走」，搬完这次就该结束（见 sftp.paste 的说明）。
//
// 第十六轮新增两件事：
//   1) 行菜单里的**「编辑」**（只对按扩展名算文本的文件显示）：进入编辑页，编辑页由本组件以
//      `absolute inset-0` **覆盖**在文件管理页面上（文件管理器保持挂载，退出后目录、视图、
//      滚动位置都还在）。三个入口（预览弹窗的编辑按钮 / 行菜单 / 左栏目录树）共用
//      stores/fileEditor.ts 的状态与「先验证权限、再进编辑页」的顺序，见契约 26.3；
//   2) 预览弹窗里点了「编辑」也走同一条路（进了编辑页才关弹窗：它 Teleport 到 body、层级在
//      编辑页之上，不关会盖住编辑页；失败时留着，用户还在读那个文件）。
//
// 第二十轮新增一件事（契约 30）：
//   快捷访问的两个入口——工具栏右侧的书签按钮（与左栏面包屑按钮条上那一个打开**同一个**
//   弹窗，见 QuickAccessDialog）与目录行菜单/空白处菜单里的「添加快捷访问」（直接加当前目录，
//   不弹窗；文件没有这一项）。点弹窗里的路径走 `sftp.jumpTo`（先探测、成功了才切过去），
//   没权限时只报错、不进入目录、也不切子标签。
//
// 切回终端由工具栏最左侧的「返回终端」按钮负责（第四轮新增）：它只把 sessionUi 的激活子标签
// 改回**上一次激活的终端**，左栏（面包屑 + 目录树）随之回来——与左栏那条按钮条里的「切换到
// 文件管理」互为一对，两处共用同一个状态（契约 13.1 的「二选一」判据没变，也没有第二套开关）。
//
// 弹窗（重命名 / 新建 用 PromptDialog，删除与覆盖确认用 ConfirmDialog）由 shell 模块提供，见契约 6.2。
import { computed, nextTick, onBeforeUnmount, reactive, ref, watch } from 'vue'
import { t } from '@/i18n'
import {
  ApiError,
  describeError,
  type ArchiveFormat,
  type FsEntry,
  type UploadEntry,
  type UploadResult
} from '@/serverapi'
import { useFileEditorStore } from '@/stores/fileEditor'
import { usePaneControlsStore } from '@/stores/paneControls'
import { useQuickAccessStore } from '@/stores/quickAccess'
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
import FileEditor from './FileEditor.vue'
import InfoDialog from './InfoDialog.vue'
import PreviewDialog from './PreviewDialog.vue'
import UploadDialog from './UploadDialog.vue'
import { canEditEntry, isArchivePath } from './fileKind'

// Vue 3.5 的响应式 props 解构：sid 在组件生命周期内不变，但解构写法让模板里少一层 props.。
const { sid, mobile = false } = defineProps<{ sid: string; mobile?: boolean }>()

const sftp = useSftpStore()
const terminals = useTerminalsStore()
const panes = usePaneControlsStore()
const sessionUi = useSessionUiStore()
const editor = useFileEditorStore()
const quickAccess = useQuickAccessStore()
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
/** 多选模式（第九轮）：进入后点行是「选中/取消选中」，不再是打开。 */
const multi = computed(() => state.value.multi)

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
  leaveMulti()
  void sftp.navigate(sid, path)
}

// ---- 工具栏动作 ----

/**
 * 点了工具栏上任何一个「改状态」的按钮（前进/后退/刷新/上一层/新建/上传/换视图/面包屑导航），
 * 都**退出多选**（第十轮用户要求）。
 *
 * 理由：多选是一个短暂的工作流（选一批 → 做一个动作），用户接着去点视图或新建，语义上已经
 * 离开那次选择了；留着高亮只会让下一次点行「莫名其妙多选了一项」。关掉它同时也清掉 `selected`。
 */
function leaveMulti() {
  if (multi.value) sftp.exitMulti(sid)
}

function goUp() {
  leaveMulti()
  if (state.value.dir === '/' || !state.value.dir) return
  void sftp.navigate(sid, parentPath(state.value.dir))
}

function retry() {
  leaveMulti()
  // 家目录还没定位到（ensure 失败）时重试 ensure；否则重拉当前目录
  void (state.value.dir ? sftp.refresh(sid) : sftp.ensure(sid))
}

function toggleView() {
  leaveMulti()
  sftp.setView(sid, state.value.view === 'list' ? 'grid' : 'list')
}

function toggleMulti() {
  sftp.setMulti(sid, !multi.value)
}

/** 聚焦当前激活的终端；activeOf 为 null（激活项在文件管理上）时退回第一个终端。 */
function focusTerminal() {
  void nextTick(() => {
    const uid = terminals.activeOf(sid)?.uid ?? terminals.tabsOf(sid)[0]?.uid
    if (uid) panes.get(sid, uid)?.focus()
  })
}

/**
 * 「返回终端」：切回**上一次激活**的终端（与子标签栏里点那个终端标签是同一个动作），
 * 桌面端左栏（面包屑 + 目录树）随之回来。
 *
 * 为什么用 lastActiveOf 而不是 activeOf：后者在文件管理子标签在前台时按设计返回 null
 * （它是「命令发进哪个终端」的判据，见 terminals store 文件头），这里要的是导航目标。
 * 没有终端时按钮是禁用的（主区域此时只能交给文件管理，见 SessionView 规则 4），
 * 兜底再取第一个标签只是为了防御「activeUid 与标签列表暂时不同步」。
 */
function backToTerminal() {
  const tab = terminals.lastActiveOf(sid) ?? terminals.tabsOf(sid)[0]
  if (!tab) return
  // 只写 sessionUi 一处状态：主区域与左栏都由 activeSub 推导（契约 13.1 / 14.3）
  sessionUi.setActiveSub(sid, tab.uid)
  focusTerminal()
}

/** 在当前（或菜单指向的）目录里新开一个终端。 */
function openTerminalHere(dir: string) {
  const target = dir || state.value.dir
  if (!target) return
  hideMenu()
  // openTerminalAt 会把新标签设成激活项（terminals.setActive → sessionUi.setActiveSub），
  // 子标签栏随之切到那个终端、左栏也回来——不需要（也没有）额外的「主区域模式」开关。
  terminals.openTerminalAt(sid, target)
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

/**
 * 「权限」表头的提示：与三列排序不同，这里说的是**当前**形态（文案本身就是
 * 「权限显示：符号（-rw-r--r--）」这种描述），点一下切到另一种。
 */
const modeTitle = computed(() =>
  state.value.mode === 'symbolic' ? t('files_mode_symbolic') : t('files_mode_octal')
)

/**
 * 压缩包输出目录的说明条。
 *
 * 压缩完成后 store 会把当前目录切到 archiveDir（产物在那儿），用户会突然站在 /tmp 下——
 * 这一行就是回答「我为什么在这儿」，否则看起来像是自己跳错了目录。
 */
const inArchiveDir = computed(() => !!sftp.archiveDir() && state.value.dir === sftp.archiveDir())

// ---- 行内菜单（右键 / 「···」都走它；空白处右键走同一份菜单状态） ----

/**
 * `blank = true` 表示这一份菜单是「在空白处右键」弹的：作用于**当前目录**（粘贴到当前目录、
 * 在当前目录开终端、复制当前目录的路径），而不是某一行。
 *
 * 为什么共用一份状态而不是两个菜单：菜单同时只该有一个，分成两份就要各自处理「点别处关掉」、
 * 夹回窗口边界、以及互相关闭，代码会立刻翻倍。
 */
const menu = reactive({ show: false, x: 0, y: 0, path: '', blank: false })
const menuEl = ref<HTMLElement | null>(null)
/** 菜单离窗口边缘至少留这么多，免得贴着边被切掉 */
const MENU_MARGIN = 8

function openMenu(entry: FsEntry, event: MouseEvent) {
  menu.show = true
  menu.x = event.clientX
  menu.y = event.clientY
  menu.path = entry.path
  menu.blank = false
  // 菜单里「导出至飞牛」是否可点取决于运行信息：万一 home store 那次请求失败了，这里再试一次
  void sftp.ensureRuntime()
  // 先按指针位置摆好，渲染出来量到真实尺寸后再夹回窗口内（同一帧内完成，看不出来）
  void nextTick(clampMenu)
  armDismiss()
}

/**
 * 列表/网格区域的右键：行（含网格格子）自己处理，其余的落点都算「空白处」。
 *
 * 为什么不用 `.self` 判断：`event.target` 只有在真的点到容器本身时才是容器，而空白处的
 * 形态有很多种——最后一行下面的留白、网格的行尾、还有「这个目录是空的」那块占位文字。
 * 用「落点不在任何一行里」来判定，这些情况一次全覆盖（空目录里最想做的事恰恰是粘贴）。
 */
function onBlankContext(event: MouseEvent) {
  const target = event.target as HTMLElement | null
  if (target?.closest('[data-fs-entry]')) return
  openBlankMenu(event)
}

/**
 * 空白处右键（桌面）。
 *
 * 触屏不弹：长按在移动端也会触发 contextmenu，但那一套「按住不动看看有什么」的交互与桌面
 * 右键不是一回事，而这个菜单的三个动作在移动端都有各自更直接的入口（子标签、行菜单）。
 * 当前目录还没定位到（首次加载中 / 加载失败）时也不弹：三个动作全都以当前目录为对象，
 * 没有目录就没有可做的事。
 */
function openBlankMenu(event: MouseEvent) {
  if (mobile || !state.value.dir) return
  menu.show = true
  menu.x = event.clientX
  menu.y = event.clientY
  menu.path = ''
  menu.blank = true
  void sftp.ensureRuntime()
  void nextTick(clampMenu)
  armDismiss()
}

function clampMenu() {
  const el = menuEl.value
  if (!el) return
  const { width, height } = el.getBoundingClientRect()
  menu.x = Math.max(MENU_MARGIN, Math.min(menu.x, window.innerWidth - width - MENU_MARGIN))
  menu.y = Math.max(MENU_MARGIN, Math.min(menu.y, window.innerHeight - height - MENU_MARGIN))
}

/**
 * window 上的「点别处就关」。
 *
 * 用 **pointerdown + 捕获阶段**，不用 click：
 *  1. 触屏上 tap 出来的 click 未必会到达 window（滚动判定的手指微动、别处的处理器
 *     stopPropagation 都会吃掉它）——用户在手机上「点别处菜单不消失」就是这么来的
 *     （第五轮报的 bug）；
 *  2. 捕获阶段先于任何冒泡处理器执行，语义稳定：点在菜单外 → 立刻关；
 *  3. 鼠标与触摸同一套逻辑，不必分叉。
 *
 * 落在菜单自己身上的 pointerdown 不算「别处」：那一击要留给菜单项自己处理
 * （每项的动作里会 hideMenu）。
 */
function onDocPointerDown(event: PointerEvent) {
  const el = menuEl.value
  if (el && event.target instanceof Node && el.contains(event.target)) return
  hideMenu()
}

/**
 * 武装「点别处关闭」。
 *
 * setTimeout 0 是为了**放过打开菜单的那一击**（本次 pointerdown / click 已经发生过）；
 * 挂之前先摘一次：不摘的话每次打开都往里塞一个，菜单关掉后 window 上会留一串监听器。
 */
function armDismiss() {
  window.setTimeout(() => {
    window.removeEventListener('pointerdown', onDocPointerDown, true)
    window.addEventListener('pointerdown', onDocPointerDown, true)
  }, 0)
}

/** 关掉菜单；顺手摘掉 window 上的监听（点别处 / 动作做完 / 组件卸载都走它）。 */
function hideMenu() {
  menu.show = false
  window.removeEventListener('pointerdown', onDocPointerDown, true)
}

// ---- 信息弹窗 ----

const infoVisible = ref(false)
const infoPath = ref('')

/**
 * 打开「信息」。行菜单传条目的路径，空白处菜单传**当前目录**（14.x 起的约定：空白处右键
 * 是对「这个目录」做事）。先收菜单，否则弹窗后面会挂着一个点不掉的菜单。
 */
function openInfo(path: string) {
  hideMenu()
  if (!path) return
  infoPath.value = path
  infoVisible.value = true
}

// ---- 快捷访问（第二十轮，契约 30） ----

/**
 * 把一个**目录**加入快捷访问（行菜单 / 空白处菜单 / 左栏目录树菜单共用这一条路）。
 *
 * 只「加」，不弹快捷访问弹窗：用户点的是「把这个目录存下来」，再弹一个窗让他确认等于多一步；
 * 看列表、删条目、跳转都在弹窗里（工具栏右侧与面包屑按钮条的书签按钮打开它）。
 *
 * 已经在列表里时给一句「已经在快捷访问里了」——不是错误，也不重复添加（后端的整表保存同样
 * 会去重，两个设备同时加一条时也不会变成两行）。文件没有这一项：快捷访问只存目录。
 */
async function addQuickAccess(path: string) {
  hideMenu()
  if (!path) return
  try {
    const result = await quickAccess.add(sid, path)
    if (result === 'exists') toast(t('files_quick_exists'), 'info')
    else toast(t('files_quick_added', { path }), 'success')
  } catch (e) {
    toast(t('files_quick_add_failed', { error: describeError(e) }))
  }
}

/** 工具栏右侧的书签按钮 / 面包屑按钮条那个：打开同一个弹窗。 */
function openQuickAccess() {
  leaveMulti()
  quickAccess.setDialog(sid, true)
}

// ---- 剪贴板栏 ----

/** 本会话剪贴板里的内容（空 = 不显示那一行）；来自别的会话的不算，不能跨会话粘贴。 */
const clip = computed(() => sftp.clipboardFor(sid))

/**
 * 剪贴板栏的「粘贴到此处」：粘进当前目录。
 *
 * `keepClipboard=true` 只对**复制**有意义（常驻栏可以连着粘到好几个目录）；剪切在 store 里
 * 粘完就清，这一行会跟着消失（见 sftp.paste 的说明）。
 */
function pasteHere() {
  const dir = state.value.dir
  if (!dir) return
  void pasteInto(dir, true)
}

// ---- 打开 / 下载 / 复制路径 ----

const previewVisible = ref(false)
const previewPath = ref('')
/** 列表里已知的体积：图片超限时后端不回内容，预览弹窗要靠它把「多大」说清楚。 */
const previewSize = ref(0)

/**
 * 进入编辑页（契约 26.3）。`fileEditor.open` 先让后端验证**本会话的登录用户**能不能写这个
 * 文件，成功之后才把激活子标签切到文件管理并打开编辑页；失败（最常见的是 `permission`）
 * 只报错——不切标签页、不进编辑页，用户看到的是「为什么改不了」。
 */
async function enterEdit(path: string) {
  hideMenu()
  if (!path) return
  try {
    await editor.open(sid, path)
  } catch (e) {
    toast(t('files_editor_load_failed', { error: describeError(e) }))
  }
}

/**
 * 预览弹窗里点了「编辑」：**进了编辑页才**关掉预览。
 *
 * 顺序不能反：预览弹窗 Teleport 到 body、层级（z-50）比编辑页（面板内的 z-30）高，不关它
 * 会盖在编辑页上面。但失败时（最常见的是 `permission`）必须留着——用户还在读这个文件，
 * 把弹窗一起关掉等于顺手扔掉他的阅读位置，而这次点击什么都没做成。
 */
async function onPreviewEdit(path: string) {
  await enterEdit(path)
  if (editor.isOpen(sid)) previewVisible.value = false
}

function onEntryClick(entry: FsEntry) {
  // 多选模式下点行 = 选中/取消选中（不打开：用户点第二下显然不是想打开两个文件）
  if (multi.value) {
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
  previewSize.value = entry.size
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

/**
 * 批量下载：逐个走既有的单文件下载（浏览器会连着接 N 个下载）。
 *
 * 下载没有「一个请求多个文件」的形式（后端 `/api/fs/download` 一次一个路径），所以这里就是串行
 * 调 N 次；成功不说 N 句（一声「开始下载 N 项」就够），失败**必须**报出来并带上失败数。
 */
async function downloadPaths(paths: string[]) {
  hideMenu()
  let failed = 0
  let lastError = ''
  for (const path of paths) {
    try {
      await sftp.download(sid, path)
    } catch (e) {
      failed++
      lastError = describeError(e)
    }
  }
  if (failed) toast(t('files_download_failed_n', { n: failed, error: lastError }))
  if (paths.length > failed) toast(t('files_download_started_n', { n: paths.length - failed }), 'success')
  sftp.exitMulti(sid)
}

/**
 * 批量导出至飞牛。
 *
 * 同名冲突**一次问、整批覆盖**：逐个弹窗问会把用户烦死，而这一批是同一个选择、同一个意图。
 * 其余失败逐个报（带文件名，方便定位是哪一个）。
 */
async function exportPaths(paths: string[], overwrite: boolean) {
  hideMenu()
  const skipped: string[] = []
  let done = 0
  for (const path of paths) {
    try {
      await sftp.exportToNas(sid, path, overwrite)
      done++
    } catch (e) {
      if (e instanceof ApiError && e.code === 'duplicate' && !overwrite) {
        skipped.push(path)
        continue
      }
      toast(t('files_op_failed', { error: describeError(e) }))
    }
  }
  if (done) toast(t('files_export_nas_done_n', { n: done }), 'success')
  if (skipped.length) {
    askConfirm(t('files_export_overwrite'), t('files_export_overwrite_n', { n: skipped.length }), () =>
      void exportPaths(skipped, true)
    )
    return // 确认框留着，别把多选态也收掉（用户还没做完这件事）
  }
  sftp.exitMulti(sid)
}

/** 按路径批量删除（多选菜单用；单个删除仍走 askDelete(entry)）。 */
function askDeletePaths(paths: string[]) {
  hideMenu()
  if (!paths.length) return
  askConfirm(t('common_tip'), t('files_delete_selected_confirm', { n: paths.length }), () =>
    void removePaths(paths)
  )
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
  // 先把菜单收掉：菜单项的 pointerdown 落在菜单自己身上（见 onDocPointerDown），不会被「点别处」
  // 那条规则关掉，所以要在这里显式关——否则弹窗后面会一直挂着一个菜单。
  hideMenu()
  leaveMulti()
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
  // 批量删除同理：不管成没成，都从多选态收回来（失败的那几项已经在 toast 里说清了）
  sftp.exitMulti(sid)
  if (single && !failed) toast(t('files_deleted', { name: single.name }), 'success')
}

function askDeleteSelected() {
  const paths = selected.value.filter((path) => state.value.entries.some((e) => e.path === path))
  if (!paths.length) {
    // 一个都没选：按钮在界面上是禁用的，这里再兜一次（防连点/快捷键）
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
  // 批量动作做完就退出多选（用户要求：选好之后做完操作不该还停在选择态）
  sftp.exitMulti(sid)
}

function cutEntries(paths: string[]) {
  hideMenu()
  sftp.cutToClipboard(sid, paths)
  toast(t('files_cut_items', { n: paths.length }), 'success')
  sftp.exitMulti(sid)
}

/**
 * 粘贴到某个目录。
 *
 * 清不清剪贴板由 store 按「剪切 / 复制 + 入口」定（见 sftp.paste）：失败、一项都没贴上时
 * **不清空**，用户可以重试或换个目录粘；剪切贴上至少一项后清空，避免「同一批再粘一遍」这种
 * 误操作真的多出一份文件。失败原因原样报出来（同名 / 权限 / 目标没了）。
 */
async function pasteInto(toDir: string, keepClipboard = false) {
  hideMenu()
  if (!toDir) return
  // 剪贴板里第一项的名字：后端把「同名」报成 duplicate 错误码时（11.5 的表格两种都提了，
  // 实现按 skipped 回，这里把两种都兜住）用它说明是哪一个撞了名字。
  const first = sftp.clipboardFor(sid)?.paths[0]
  try {
    const res = await sftp.paste(sid, toDir, keepClipboard)
    if (res.count) toast(t('files_paste_done', { n: res.count }), 'success')
    // 后端只回「跳过了几项」，名字是 store 按「源里的名字没出现在结果里」对出来的
    const conflict = res.conflicts[0]
    if (res.skipped) {
      // 一项都没贴上（全被同名挡下）时也要说话：静默等于「点了没反应」
      toast(conflict ? t('files_paste_exists', { name: conflict }) : t('files_op_failed_short'))
    } else if (!res.count) {
      // 后端既没贴也没跳过（理论上不会发生）：兜底报一句，别让失败看起来像成功
      toast(t('files_op_failed_short'))
    }
  } catch (e) {
    if (e instanceof ApiError && e.code === 'duplicate') {
      toast(first ? t('files_paste_exists', { name: baseName(first) }) : t('files_op_failed_short'))
      return
    }
    toast(t('files_op_failed', { error: describeError(e) }))
  }
}

// ---- 压缩 / 解压 / 导出至飞牛 ----

/**
 * 压缩（tar.gz / zip）。
 *
 * 归档名（按被压缩项推导）与「跳到产物目录」都在 store 里做：产物落在服务端固定的
 * archiveDir 下，用户点压缩的目的就是拿到那个包，停在原目录只看到一条 toast 等于没完成。
 *
 * 远端缺 tar/zip 时后端回 `no_archive_tool`，`describeError` 会翻成「远程主机缺少所需的
 * 压缩工具」——所以这里不去预判工具在不在（前端无从知道），失败就把后端原文报出来。
 */
async function compressEntries(paths: string[], format: ArchiveFormat) {
  hideMenu()
  if (!paths.length) return
  try {
    const archive = await sftp.compress(sid, paths, format)
    toast(t('files_compress_done', { path: archive.path }), 'success')
    sftp.exitMulti(sid)
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
    sftp.exitMulti(sid)
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

/** 空白处右键菜单里的「上传」：与工具栏那个按钮同一个弹窗，目标目录就是当前目录。 */
function uploadHere() {
  hideMenu()
  leaveMulti()
  uploadVisible.value = true
}

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

/**
 * 剪贴板现在能不能粘：返回 null = 可以，返回文案 = 不能粘的原因。
 *
 * 空白处菜单要**总是**显示「粘贴」（放灰 + 点一下说明原因），行菜单则在没有剪贴板时整项
 * 不出现——两种菜单对「没剪贴板」的处理不同是契约里定死的（11.4 与 13.4），所以把判据
 * 抽成一个函数，免得两处各写一套、日后走样。
 */
function pasteBlockedReason(): string | null {
  if (sftp.clipboardIsForeign(sid)) return t('files_paste_cross_session')
  if (!sftp.clipboardFor(sid)) return t('files_clipboard_empty')
  return null
}

/**
 * 粘贴项（两种菜单共用一份判据）。
 *
 * **剪贴板没有内容 → 整项不出现**（第九轮用户要求；13.4 那版是「空白处总是显示、置灰+说明」，
 * 现在统一成不显示：一个永远灰着的菜单项只是噪音）。当前目录/条目能不能粘，看 `pasteBlockedReason`：
 *  - 剪贴板是空的 → 不显示；
 *  - 剪贴板来自**别的会话** → 显示但置灰，点一下说明原因——用户需要知道「东西还在，只是不能在这儿粘」。
 */
function pasteItem(toDir: string): MenuItem | null {
  const blocked = pasteBlockedReason()
  if (!blocked) return { label: t('files_paste'), run: () => void pasteInto(toDir) }
  return sftp.clipboardIsForeign(sid)
    ? { label: t('files_paste'), dim: true, run: () => toast(blocked, 'info') }
    : null
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

/**
 * 多选模式下的**批量菜单**（第十轮）：右键的时候，不管点在行上还是空白处，都给这七项，
 * 而且都作用于**整个选择**——用户此刻的上下文就是那批选中的东西。
 *
 * 顺序按用户给的原话：复制 / 剪切 / 删除 / 导出 / 下载 / 两个压缩。**一个都没选时不弹**
 * （没有作用对象；调用方见 `menuItems`）。
 */
function multiMenuItems(): MenuItem[] {
  const paths = selected.value.filter((path) => state.value.entries.some((e) => e.path === path))
  if (!paths.length) return []
  const items: MenuItem[] = []
  items.push({ label: t('files_copy'), run: () => copyEntries(paths) })
  items.push({ label: t('files_cut'), run: () => cutEntries(paths) })
  items.push({ label: t('common_delete'), danger: true, run: () => askDeletePaths(paths) })
  items.push(
    sftp.exportRoot()
      ? { label: t('files_export_nas'), run: () => void exportPaths(paths, false) }
      : {
          label: t('files_export_nas'),
          dim: true,
          run: () => toast(t('files_export_nas_unavailable'), 'info')
        }
  )
  items.push({ label: t('files_download'), run: () => void downloadPaths(paths) })
  items.push({
    label: t('files_compress_selected', { n: paths.length }),
    run: () => void compressEntries(paths, 'tar.gz')
  })
  items.push({ label: t('files_compress_zip'), run: () => void compressEntries(paths, 'zip') })
  return items
}

/**
 * 空白处菜单（13.4 + 第四轮 + 第六轮 + 第二十轮）：新建文件 / 新建文件夹 / 上传 /
 * 粘贴到当前目录 / 信息 / 在当前目录开终端 / 复制当前目录路径 / 添加快捷访问 / 刷新。
 *
 * 为什么把「新建 / 上传」也放这里：空白处右键是「对**这个目录**做事」最自然的入口，
 * 而这三件事（尤其是上传）在工具栏上都要先回到工具栏去点。粘贴仍然放在中间靠前的位置
 * （它是使用频率最高的一项），粘不了时置灰并说明原因（13.4）。
 */
function blankMenuItems(): MenuItem[] {
  const dir = state.value.dir
  const items: MenuItem[] = []
  items.push({ label: t('files_new_file'), run: () => newEntry(false) })
  items.push({ label: t('files_new_dir'), run: () => newEntry(true) })
  items.push({ label: t('files_upload'), run: uploadHere })
  const paste = pasteItem(dir)
  if (paste) items.push(paste)
  items.push({ label: t('files_info'), run: () => openInfo(dir) })
  items.push({ label: t('files_open_terminal'), run: () => openTerminalHere(dir) })
  items.push({ label: t('files_copy_path'), run: () => void copyPath(dir) })
  // 添加快捷访问（第二十轮）：空白处右键是「对**这个目录**做事」的入口，把当前目录存成
  // 快捷访问正属于这一类。**只对目录**：空白处菜单作用的对象就是当前目录，天然是目录。
  items.push({ label: t('files_quick_add'), run: () => void addQuickAccess(dir) })
  // 刷新（第七轮）：空白处右键是「对这个目录做事」的入口，刷新放这里最顺手
  items.push({ label: t('files_refresh'), run: () => retry() })
  return items
}

const menuItems = computed<MenuItem[]>(() => {
  // 多选模式：不管右键落在行上还是空白处，菜单都是那一批（第十轮）
  if (multi.value && selected.value.length) return multiMenuItems()
  if (menu.blank) return blankMenuItems()
  const entry = entries.value.find((item) => item.path === menu.path)
  if (!entry) return []
  const targets = targetPaths(entry)
  const items: MenuItem[] = []
  // 顺序按契约 11.4（13.4 加了「压缩为 zip」紧跟 tar.gz；第十六轮在「粘贴」与「重命名」
  // 之间插入「编辑」）：粘贴（有剪贴板时）/ 编辑（文本文件）/ 重命名 /
  // 复制 / 剪切 / 删除 / 下载 / 导出至飞牛 / 压缩为 tar.gz / 压缩为 zip / 解压 /
  // 复制完整路径 / 在此目录新建终端
  const paste = pasteItem(entry.dir ? entry.path : state.value.dir)
  if (paste) items.push(paste)
  // 编辑（第十六轮）：只对**按扩展名算文本**的文件出现——图片没有编辑器，目录更不能编辑。
  // 真正的可写性由后端在 edit-load 里判定（没权限回 permission，界面拒绝进编辑页）。
  if (canEditEntry(entry)) {
    items.push({ label: t('common_edit'), run: () => void enterEdit(entry.path) })
  }
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
    run: () => void compressEntries(targets, 'tar.gz')
  })
  items.push({
    label: t('files_compress_zip'),
    run: () => void compressEntries(targets, 'zip')
  })
  if (!entry.dir && isArchivePath(entry.name)) {
    items.push({ label: t('files_extract'), run: () => void extractEntry(entry) })
  }
  items.push({ label: t('files_info'), run: () => openInfo(entry.path) })
  items.push({ label: t('files_copy_path'), run: () => void copyPath(entry.path) })
  // 添加快捷访问（第二十轮）：**只对目录**出现（用户要求「文件不需要快捷访问」——文件既不是
  // 跳转目标，也没有「进入」这个动作）。
  if (entry.dir) {
    items.push({ label: t('files_quick_add'), run: () => void addQuickAccess(entry.path) })
  }
  items.push({
    label: t('files_open_terminal_here'),
    run: () => openTerminalHere(entry.dir ? entry.path : state.value.dir)
  })
  return items
})

onBeforeUnmount(hideMenu)
</script>

<template>
  <!-- relative：编辑页是它的 absolute 覆盖层（见文件末尾的 FileEditor） -->
  <div class="relative flex h-full min-h-0 flex-col">
    <!-- 工具栏：窄面板放不下时自己折行，按钮只有图标（说明挂原生 title，触屏上点完就消失） -->
    <div
      class="flex shrink-0 flex-wrap items-center gap-0 border-b border-line px-1 py-1 dark:border-line-dark"
    >
      <!-- 返回终端（第四轮新增）：主区域交回上一次激活的终端、左栏（面包屑 + 目录树）回来。
           放在最左是因为它是「离开这个页面」的动作，与左栏那条按钮条里的「切换到文件管理」
           互为一对（同一个双向箭头图标）。一个终端都没有时禁用：此时文件管理是唯一能显示的
           主区域（SessionView 规则 4），没有可返回的目标。 -->
      <button
        type="button"
        class="tree-tool"
        :disabled="!terminals.countOf(sid)"
        :title="t('files_switch_to_terminal')"
        :aria-label="t('files_switch_to_terminal')"
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
          <path d="M8 3 4 7l4 4" />
          <path d="M4 7h16" />
          <path d="m16 21 4-4-4-4" />
          <path d="M20 17H4" />
        </svg>
      </button>

      <span class="mx-1 h-4 w-px shrink-0 bg-line dark:bg-line-dark" />

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
      <!-- 多选（第九轮）：点一下**进入多选模式**（不预选任何东西），之后点行就是逐个点选；
           做完复制/剪切/删除/压缩/导出会自动退出（见 toggleMulti 与那些 exitMulti 调用点）。
           原来那个「全选」是一次性把当前目录全选上，想选三个文件还得先全选再逐个取消。 -->
      <button
        type="button"
        class="tree-tool"
        :class="multi ? '!bg-brand/10 !text-brand' : ''"
        :title="multi ? t('files_multi_exit') : t('files_multi_select')"
        :aria-label="multi ? t('files_multi_exit') : t('files_multi_select')"
        @click="toggleMulti"
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

      <!-- 快捷访问（第二十轮）：紧挨着视图切换，打开快捷访问弹窗（契约 30.3）。左栏面包屑
           按钮条上那一个书签按钮打开的是同一个弹窗（显隐在 stores/quickAccess.ts 里）。 -->
      <button
        type="button"
        class="tree-tool"
        :title="t('files_quick_access')"
        :aria-label="t('files_quick_access')"
        @click="openQuickAccess"
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
          <path d="M6 3h12a1 1 0 0 1 1 1v17l-7-4-7 4V4a1 1 0 0 1 1-1z" />
        </svg>
      </button>
      <!-- 权限显示模式不再是独立按钮：点列表表头里的「权限」就能切（见下面的表头区），
           工具栏只留「做什么」的动作按钮。 -->
    </div>

    <!-- 多选模式时面包屑让位给这条「选择条」；其余时候是路径分段（点哪段去哪层）。
         一个都没选时给一句提示（告诉用户「点行就是选」），而不是「已选 0 项」这种废话。 -->
    <div
      v-if="multi"
      class="flex shrink-0 items-center gap-2 border-b border-line bg-brand-soft px-2 py-1 dark:border-line-dark"
    >
      <span class="min-w-0 flex-1 truncate text-xs text-ink-soft dark:text-ink-soft-dark">
        {{ selecting ? t('files_selected', { n: selected.length }) : t('files_multi_hint') }}
      </span>
      <button type="button" class="g-btn-ghost h-7" @click="toggleMulti">
        {{ t('files_multi_exit') }}
      </button>
      <button
        type="button"
        class="g-btn-ghost h-7 text-danger"
        :disabled="!selecting"
        @click="askDeleteSelected"
      >
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

    <!-- 常驻剪贴板栏（第五轮）：复制之后一直在，直到点「取消」；剪切的在粘完那一刻就消失
         （第十一轮：剪切是「搬走」，搬完就结束，见 sftp.paste）。
         放在地址栏下面：先回答「我在哪」（面包屑），再回答「手上拿着什么」（这一行）。
         「粘贴到此处」**不清空**剪贴板（可以连着粘到好几个目录），这与右键菜单里的「粘贴」
         刻意不同——那个是一次性动作，粘完就清（用户明确要求两个入口各自保持现状）。
         来自别的会话的剪贴板这里**不显示**：跨会话粘贴是硬约束（见 sftp store），
         显示一行只能置灰的按钮不如不显示，菜单里那句「不能跨会话粘贴」已经把话说清了。 -->
    <div
      v-if="clip"
      class="flex shrink-0 items-center gap-2 border-b border-line bg-brand-soft px-2 py-1 text-xs dark:border-line-dark"
    >
      <svg
        viewBox="0 0 24 24"
        fill="none"
        stroke="currentColor"
        stroke-width="1.5"
        stroke-linecap="round"
        stroke-linejoin="round"
        class="h-3.5 w-3.5 shrink-0 text-brand"
      >
        <path d="M16 4h2a2 2 0 0 1 2 2v14a2 2 0 0 1-2 2H6a2 2 0 0 1-2-2V6a2 2 0 0 1 2-2h2" />
        <rect x="8" y="2" width="8" height="4" rx="1" />
      </svg>
      <span class="shrink-0 text-ink dark:text-ink-dark">
        {{
          clip.op === 'cut'
            ? t('files_cut_items', { n: clip.paths.length })
            : t('files_copied_items', { n: clip.paths.length })
        }}
      </span>
      <span
        class="min-w-0 flex-1 truncate font-mono text-ink-faint dark:text-ink-faint-dark"
        :title="clip.paths.join('\n')"
      >
        {{ clip.paths.map((p) => baseName(p)).join('、') }}
      </span>
      <button
        type="button"
        class="g-btn-ghost h-7 shrink-0 !px-2 !text-brand"
        :disabled="!state.dir"
        :title="t('files_paste_here')"
        @click="pasteHere"
      >
        {{ t('files_paste_here') }}
      </button>
      <button
        type="button"
        class="g-btn-ghost h-7 shrink-0 !px-2"
        :title="t('files_clipboard_cancel')"
        @click="sftp.clearClipboard()"
      >
        {{ t('files_clipboard_cancel') }}
      </button>
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
      列表的表头：名称 / 大小 / 修改时间三列可点排序（循环 升序 → 降序 → 取消）；
      「权限」列点一下切换显示形态（符号 ↔ 八进制），它不参与排序。
      目录恒在文件前面由 compareEntries 保证，与点哪一列无关。
      权限列在移动端省掉（列宽不够）：没有这一列，也就没有切换形态的入口。
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
      <button
        v-if="!mobile"
        type="button"
        class="flex w-20 shrink-0 items-center justify-end gap-0.5 rounded px-0.5 hover:text-ink dark:hover:text-white"
        :title="modeTitle"
        :aria-label="modeTitle"
        @click="sftp.toggleMode(sid)"
      >
        <span class="truncate">{{ t('files_mode') }}</span>
        <!-- 双向箭头：这一列点一下是「换一种写法」，不是排序（排序箭头只在另外三列上） -->
        <svg
          class="h-3 w-3 shrink-0 opacity-60"
          viewBox="0 0 24 24"
          fill="none"
          stroke="currentColor"
          stroke-width="1.5"
          stroke-linecap="round"
          stroke-linejoin="round"
        >
          <path d="M4 8h13M4 8l3-3M4 8l3 3" />
          <path d="M20 16H7M20 16l-3-3M20 16l-3 3" />
        </svg>
      </button>
      <span class="w-6 shrink-0" />
    </div>

    <div
      class="thin-scroll min-h-0 flex-1 overflow-auto"
      :class="dragging ? 'ring-2 ring-inset ring-brand/40' : ''"
      @dragover.prevent="onDragOver"
      @dragleave="onDragLeave"
      @drop.prevent="onDrop"
      @contextmenu.prevent="onBlankContext($event)"
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
          data-fs-entry
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
      <!-- 网格：格子之间的空白同样是「空白处」，由外层容器的右键处理器兜住 -->
      <!-- content-start 是**修 bug 的关键**，别删：容器有 min-h-full（占满滚动区），而网格默认
           align-content 会拉伸自动行去填满容器——目录里只有一行时，那一行被拉到整屏高，鼠标停在
           任意一个格子上，高亮（hover:bg-black/5 + 边框）就变成贯穿上下的整列（用户报的现象）。
           content-start（align-content: flex-start）让行只占内容高度，多余空间留在容器底部。 -->
      <div
        v-else
        class="grid min-h-full content-start gap-1 p-2"
        style="grid-template-columns: repeat(auto-fill, minmax(6.5rem, 1fr))"
      >
        <div
          v-for="entry in entries"
          :key="entry.path"
          class="group relative flex cursor-pointer flex-col items-center gap-1 rounded-md border border-transparent px-2 py-2 text-center hover:border-line hover:bg-black/5 dark:hover:border-line-dark dark:hover:bg-white/5"
          :class="selected.includes(entry.path) ? 'bg-brand-soft' : ''"
          data-fs-entry
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
      :size="previewSize"
      @update:visible="previewVisible = $event"
      @edit="onPreviewEdit"
    />
    <InfoDialog
      :visible="infoVisible"
      :sid="sid"
      :path="infoPath"
      @update:visible="infoVisible = $event"
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

    <!-- 编辑页（第十六轮）：覆盖在**文件管理页面**上（用户要求「编辑页覆盖文件管理页面」）。
         用 v-if 而不是常驻挂载是安全的：它里面没有 xterm / WebSocket 那类「重挂载要重新握手」
         的东西，内容全在 store 里（契约第 5 节禁 v-if 只针对面板）。 -->
    <FileEditor v-if="editor.isOpen(sid)" :sid="sid" />
  </div>
</template>
