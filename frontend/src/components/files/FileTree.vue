<script setup lang="ts">
// FileTree — 左栏文件树（以远程 `/` 为根、懒加载；第十六轮起目录与文件都列）。
//
// 第十六轮起**也列文件**（用户要求：能在树里直接找到配置文件去编辑）：
//   - 每个目录只列**前 TREE_FILE_LIMIT（20）个文件**（按文件名），多出来的在目录末尾给一行
//     「···更多文件」，点它去文件管理里打开那个目录——整棵树的文件总数不设上限，
//     上限按「单个目录」算（契约 26.4）；
//   - 点**目录**仍然只更新共享的当前目录并展开（不切主区域，见下）；点**文件**打开预览
//     （与文件管理器里点一行一致）；
//   - 文件行的菜单补齐完整的文件菜单：预览 / 编辑 / 重命名 / 复制 / 剪切 / 删除 / 下载 /
//     导出至飞牛 / 压缩为 tar.gz / 压缩为 zip / 解压（可解压时）/ 信息 / 复制完整路径。
//
// 在此之前它刻意只列目录：左栏的用途是「快速跳到某个目录」，而点文件的语义在 11rem 宽的栏里
// 说不清。用户确认要列文件之后，语义就按「与文件管理一致」定下来（预览/编辑走同一套接口与
// 同一份判据，见 fileKind.ts）。
//
// 第二轮的行为（契约 11.3）：
//   - 根是**远程 `/`**（不是家目录）：树上要能看到 /home /var /tmp 这些同级的目录，
//     否则用户没法从家目录走到系统目录；
//   - 首次进入会话自动展开到登录用户的家目录、高亮并滚进视野；
//   - **点目录不再切换主区域**（也不 emit 任何事件）：只把「当前目录」改成它（与文件管理器
//     共用同一个当前目录）并展开/高亮。从树里点一下就跳走会打断正在终端里干活的人；
//   - 右键 / 「…」菜单补齐目录可用的全部动作（新建 / 上传 / 新终端 / 粘贴 / 重命名 / 删除 /
//     压缩为 tar.gz / 压缩为 zip / 导出至飞牛 / 复制路径 / 刷新）。
//
// 菜单里对 `/` 隐藏了重命名 / 删除 / 压缩 / 导出：根目录没有父目录可压缩、也不能被改名或
// 删除（后端会拒），藏起来比给一个点了报错的项好。
import { computed, nextTick, onBeforeUnmount, reactive, ref, watch } from 'vue'
import { t } from '@/i18n'
import { ApiError, describeError, type ArchiveFormat } from '@/serverapi'
import { useFileEditorStore } from '@/stores/fileEditor'
import { usePaneControlsStore } from '@/stores/paneControls'
import { FILES_TAB, useSessionUiStore } from '@/stores/sessionUi'
import { TREE_FILE_LIMIT, useSftpStore } from '@/stores/sftp'
import { useTerminalsStore } from '@/stores/terminals'
import { useToastStore } from '@/stores/toast'
import { baseName, joinPath, parentPath } from '@/utils/format'
import ConfirmDialog from '@/components/ConfirmDialog.vue'
import PromptDialog from '@/components/PromptDialog.vue'
import InfoDialog from './InfoDialog.vue'
import PreviewDialog from './PreviewDialog.vue'
import UploadDialog from './UploadDialog.vue'
import { canEditPath, isArchivePath } from './fileKind'

const { sid } = defineProps<{ sid: string }>()

// 本组件**不 emit**：点目录只更新共享的当前目录（sftp store）与展开状态；主区域显示什么由
// 子标签栏决定（`sessionUi.activeSubOf`），从树里点一下就切走会打断正在终端里干活的人。
const sftp = useSftpStore()
const ui = useSessionUiStore()
const terminals = useTerminalsStore()
const panes = usePaneControlsStore()
const editor = useFileEditorStore()
const toastStore = useToastStore()

const state = computed(() => sftp.stateOf(sid))

/**
 * 树里的一行：目录、文件，或者「···更多文件」（它代表**它所属的那个目录**里没显示出来的文件，
 * 点一下去文件管理打开该目录）。
 *
 * `more` 行的 `path` 就是它所属目录的路径——所以模板里的 key 必须带上 kind，
 * 否则它和那个目录自己的行撞 key。
 */
interface TreeRow {
  kind: 'dir' | 'file' | 'more'
  path: string
  name: string
  depth: number
  /** 文件行的大小（预览弹窗要用它把「图片超过 4 MB」之类的数字说清楚） */
  size?: number
}

/** 展开状态下的可见行（扁平化，模板里只渲染一层 v-for）。根永远是第一行。 */
const rows = computed<TreeRow[]>(() => {
  const st = state.value
  const out: TreeRow[] = [{ kind: 'dir', path: '/', name: t('files_root'), depth: 0 }]
  const walk = (parent: string, depth: number) => {
    // 没展开就不列子项：懒加载的全部意义就在这里（大目录不要一次性铺开）
    if (!st.expanded.includes(parent)) return
    for (const entry of st.treeChildren[parent] ?? []) {
      out.push({
        kind: entry.dir ? 'dir' : 'file',
        path: entry.path,
        name: entry.name,
        depth,
        size: entry.size
      })
      // 只有目录才往下走：文件没有子项
      if (entry.dir) walk(entry.path, depth + 1)
    }
    // 文件被截断的目录：末尾补一行「···更多文件」（去文件管理看全部）
    if (fileTotalOf(parent) > TREE_FILE_LIMIT) {
      out.push({ kind: 'more', path: parent, name: t('files_tree_more_files'), depth })
    }
  }
  walk('/', 1)
  return out
})

/** 某个目录里一共有多少个文件（store 记的是总数，树里只显示前 TREE_FILE_LIMIT 个）。 */
function fileTotalOf(path: string): number {
  return state.value.treeFileTotal[path] ?? 0
}

/** 「···更多文件」那一行的提示：还有多少个没显示（数字来自 store，不是猜的）。 */
function moreFilesHint(path: string): string {
  return t('files_tree_more_files_hint', { n: Math.max(0, fileTotalOf(path) - TREE_FILE_LIMIT) })
}

const isExpanded = (path: string) => state.value.expanded.includes(path)
const isLoading = (path: string) => state.value.treeLoading[path] === true
const isHome = (path: string) => !!state.value.home && path === state.value.home

function toast(msg: string, type: 'success' | 'error' | 'info' = 'error') {
  toastStore.show(msg, type)
}

// ---- 点击 / 展开 ----

/**
 * 行点击：目录 = 导航 + 展开（原语义，不切主区域）；文件 = 预览；「···更多文件」= 在文件管理里
 * 打开那个目录（用户要求）。
 */
function onRowClick(row: TreeRow) {
  if (row.kind === 'file') {
    previewPath.value = row.path
    previewSize.value = row.size ?? 0
    previewVisible.value = true
    return
  }
  if (row.kind === 'more') {
    openDirInFiles(row.path)
    return
  }
  void openDir(row.path)
}

/**
 * 「···更多文件」：跳到那个目录并切到文件管理子标签。
 *
 * 用 `ui.setActiveSub(FILES_TAB)` 而不是别的开关：主区域显示什么只有这一个判据（契约 13.1），
 * 切过去之后左栏自己会收起（`showLeft` = 宽屏 && 不在文件管理 && 用户没手动收起）。
 */
function openDirInFiles(path: string) {
  void sftp.navigate(sid, path)
  ui.setActiveSub(sid, FILES_TAB)
}

async function openDir(path: string) {
  // 顺序有讲究：先 navigate（当前目录立刻切过去，高亮跟着动），再展开。
  // 展开失败（权限、目录刚被删）不算错误——导航本身已经成功了，提示反而会误导。
  await sftp.navigate(sid, path)
  if (!isExpanded(path)) {
    try {
      await sftp.toggleExpand(sid, path)
    } catch {
      /* 忽略：导航已经成功 */
    }
  }
}

async function toggleNode(path: string, event: MouseEvent) {
  event.stopPropagation()
  try {
    await sftp.toggleExpand(sid, path)
  } catch (e) {
    toast(`${t('files_load_failed')}：${describeError(e)}`)
  }
}

// ---- 首次定位 + 高亮滚入视野 ----

const scrollEl = ref<HTMLElement | null>(null)

async function scrollTo(path: string) {
  await nextTick()
  const el = scrollEl.value
  if (!el) return
  // 用 data 属性找行而不是 ref 数组：行是 v-for 出来的，逐行收集 ref 会在展开/收起时
  // 反复增删，反而更容易错位。这里只在需要滚动时扫一遍（可见行通常几十行）。
  const row = [...el.querySelectorAll<HTMLElement>('[data-tree-path]')].find(
    (item) => item.dataset.treePath === path
  )
  row?.scrollIntoView({ block: 'nearest', inline: 'nearest' })
}

/**
 * 把树拉回登录用户的家目录：只展开到 home 那一条链路（`expandPath` 会顺手收起链外的节点），
 * 并把那一行滚进视野。
 *
 * 两个时机各做一次（第八轮）——这就是原来那个「家目录」按钮的活，按钮删掉了，行为留着：
 *  1. **新打开会话**（`home` 第一次到手）：树上要「站在」用户的主场，不用一层层点进去；
 *  2. **重新展开面包屑**（左栏从收起变回显示）：收起期间用户可能已经在别处逛过，链路也
 *     被收掉了；重新展开时把视线拉回主场。
 *
 * 左栏是 `v-show` 隐藏的：隐藏时 `display:none`，`scrollIntoView` 一点效果都没有，
 * 所以两个入口都先 `await nextTick()` 等 DOM 真的可见了再滚。
 */
async function revealHome() {
  const home = state.value.home
  if (!home) return
  await sftp.expandPath(sid, home)
  await nextTick()
  await scrollTo(home)
}

// 家目录要等 ensure 拿到响应才知道（不同账号可能是 /root、/home/x），所以盯 home 而不是
// 在 mounted 里直接跑。
watch(
  () => state.value.home,
  (home) => {
    if (home) void revealHome()
  }
)

// 重新展开面包屑（leftHidden: true → false）时再拉一次家目录。只在「收起 → 显示」那一刻做：
// hidden 变 true 时左栏正要消失，什么都不用做（此刻滚也滚不动）。
watch(
  () => ui.leftHiddenOf(sid),
  (hidden, wasHidden) => {
    if (hidden || !wasHidden) return
    void nextTick().then(() => revealHome())
  }
)

// 当前目录变了（树里点、文件管理器里点面包屑、点了「家目录」）就把那一行滚进视野。
// 行不存在（当前目录不在已展开的链上）时什么都不做，不会把树滚花。
watch(
  () => state.value.dir,
  (dir) => {
    if (dir) void scrollTo(dir)
  }
)

// ---- 菜单（右键 / 「…」都走它） ----

const menu = reactive({ show: false, x: 0, y: 0, path: '', file: false })
const menuEl = ref<HTMLElement | null>(null)
const MENU_MARGIN = 8

/**
 * 打开菜单。`file` 决定这一份是**文件菜单**还是**目录菜单**（第十六轮起树里有文件了）——
 * 菜单状态里必须记住这一点：渲染时再回 `treeChildren` 里翻是查不到的（比如菜单开在某个
 * 文件上、用户接着把父目录收起来了，菜单还开着却查不到目标）。
 */
function openMenu(path: string, event: MouseEvent, file = false) {
  menu.show = true
  menu.x = event.clientX
  menu.y = event.clientY
  menu.path = path
  menu.file = file
  // 菜单里「导出至飞牛」是否可点取决于运行信息，点开菜单时确保它已经拿到
  void sftp.ensureRuntime()
  void nextTick(clampMenu)
  armDismiss()
}

/**
 * window 上的「点别处就关」。
 *
 * 用 **pointerdown + 捕获阶段**，不用 click（AGENTS 第 5 节，FileManager 里也踩过同一个坑）：
 * 触屏上 tap 出来的 click 未必会到达 window——滚动判定的手指微动、别处处理器的
 * stopPropagation 都会吃掉它，用户看到的是「点别处菜单不消失」。落在菜单自己身上的那一击
 * 不算「别处」（那一击留给菜单项，动作里会 hideMenu）。
 */
function onDocPointerDown(event: PointerEvent) {
  const el = menuEl.value
  if (el && event.target instanceof Node && el.contains(event.target)) return
  hideMenu()
}

/**
 * 武装「点别处关闭」。
 *
 * setTimeout 0 是为了**放过打开菜单的那一击**；挂之前先摘一次，避免每开一次菜单就往
 * window 上多塞一个监听器。
 */
function armDismiss() {
  window.setTimeout(() => {
    window.removeEventListener('pointerdown', onDocPointerDown, true)
    window.addEventListener('pointerdown', onDocPointerDown, true)
  }, 0)
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
  // 顺手摘掉 window 上的监听（点别处 / 动作做完 / 组件卸载都走这里）
  window.removeEventListener('pointerdown', onDocPointerDown, true)
}

/** 在空白处右键 = 对根节点操作。 */
function openRootMenu(event: MouseEvent) {
  openMenu('/', event)
}

/** 行右键：目录菜单 / 文件菜单。 */
function onRowContext(row: TreeRow, event: MouseEvent) {
  // 「···更多文件」不是一个文件系统对象，没有可操作的菜单——它自己的点击就是唯一动作
  if (row.kind === 'more') return
  openMenu(row.path, event, row.kind === 'file')
}

// ---- 菜单动作 ----

const uploadVisible = ref(false)
const uploadDir = ref('')

function uploadHere() {
  uploadDir.value = menu.path
  hideMenu()
  uploadVisible.value = true
}

interface PromptJob {
  title: string
  initial: string
  run: (value: string) => void
}

const prompt = reactive<PromptJob & { show: boolean }>({
  show: false,
  title: '',
  initial: '',
  run: () => {}
})

function askName(job: PromptJob) {
  prompt.title = job.title
  prompt.initial = job.initial
  prompt.run = job.run
  prompt.show = true
}

const confirm = reactive<{ show: boolean; title: string; message: string; run: () => void }>({
  show: false,
  title: '',
  message: '',
  run: () => {}
})

function askConfirm(title: string, message: string, run: () => void) {
  confirm.title = title
  confirm.message = message
  confirm.run = run
  confirm.show = true
}

function onConfirmOk() {
  const run = confirm.run
  confirm.show = false
  run()
}

function onPromptConfirm(value: string) {
  const run = prompt.run
  prompt.show = false
  prompt.run = () => {}
  run(value)
}

/** 名字合法判据与 FileManager 一致：非空、不含 /、不是 . / ..（否则后端会当成路径）。 */
function badName(name: string): 'empty' | 'invalid' | '' {
  const n = name.trim()
  if (!n) return 'empty'
  if (n === '.' || n === '..' || n.includes('/')) return 'invalid'
  return ''
}

function newHere(dir: boolean) {
  const parent = menu.path
  hideMenu()
  askName({
    title: dir ? t('files_new_dir_title') : t('files_new_file_title'),
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
      void createHere(parent, raw.trim(), dir)
    }
  })
}

async function createHere(parent: string, name: string, dir: boolean) {
  try {
    if (dir) await sftp.createDir(sid, parent, name)
    else await sftp.createFile(sid, parent, name)
    toast(t('files_created', { name }), 'success')
    // 新建的东西得看得见：父节点没展开就顺手展开（createDir 已经把缓存刷新了）
    if (!isExpanded(parent)) await sftp.toggleExpand(sid, parent)
  } catch (e) {
    toast(t('files_op_failed', { error: describeError(e) }))
  }
}

function renameHere() {
  const path = menu.path
  if (path === '/') return
  const name = baseName(path)
  hideMenu()
  askName({
    title: t('files_rename_title'),
    initial: name,
    run: (raw) => {
      if (badName(raw)) {
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
 * 树里某一项的**真实类型**（删除接口的 `dir` 参数必须与它一致）。
 *
 * 为什么不能只信 `menu.file`：菜单开着的时候父目录可能已经被收起或删掉（缓存里就查不到了），
 * 那种情况退回菜单记下的 kind。为什么必须传对：后端对 `dir` 与真实类型不一致是**拒绝**
 * （invalid），不是猜——文件行硬写 `true` 会让「在树里删文件」永远失败，而用户看到的只是一句
 * 「参数不合法」（第十七轮的真 bug）。
 */
function entryIsDir(path: string): boolean {
  for (const list of Object.values(state.value.treeChildren)) {
    const hit = list.find((entry) => entry.path === path)
    if (hit) return hit.dir
  }
  return !menu.file
}

function askDelete() {
  const path = menu.path
  if (path === '/') return
  const name = baseName(path) || path
  const dir = entryIsDir(path)
  hideMenu()
  askConfirm(t('common_tip'), t('files_delete_confirm', { name }), () => void doRemove(path, name, dir))
}

async function doRemove(path: string, name: string, dir: boolean) {
  try {
    await sftp.remove(sid, path, dir)
    toast(t('files_deleted', { name }), 'success')
    // 删掉的正好是当前目录：往上退一层，否则列表停在一个已经不存在的路径上
    if (state.value.dir === path) await sftp.navigate(sid, parentPath(path))
  } catch (e) {
    toast(t('files_op_failed', { error: describeError(e) }))
  }
}

async function compressHere(format: ArchiveFormat) {
  const path = menu.path
  if (path === '/') return
  hideMenu()
  try {
    const archive = await sftp.compress(sid, [path], format)
    toast(t('files_compress_done', { path: archive.path }), 'success')
  } catch (e) {
    // 远端缺 tar/zip（no_archive_tool）、权限不足、名字非法……一律把后端原文报出来
    toast(t('files_op_failed', { error: describeError(e) }))
  }
}

/**
 * 导出至飞牛：同名冲突时问一次再带 overwrite 重试（与 FileManager 同一条流程）。
 *
 * `path` 由调用方传进来而不是这里读 menu.path：确认框弹出期间用户可能又点了别的菜单，
 * 那时 menu.path 已经换了目标，重试就会导出另一个东西。
 */
async function exportHere(path: string, overwrite: boolean) {
  if (path === '/') return
  hideMenu()
  try {
    const out = await sftp.exportToNas(sid, path, overwrite)
    toast(t('files_export_nas_done', { path: out.path }), 'success')
  } catch (e) {
    if (e instanceof ApiError && e.code === 'duplicate') {
      askConfirm(t('files_export_overwrite'), path, () => void exportHere(path, true))
      return
    }
    toast(t('files_op_failed', { error: describeError(e) }))
  }
}

/** 复制完整路径（与 FileManager 的同名实现一致：http 下 navigator.clipboard 不存在，要兜底）。 */
async function copyPath() {
  const path = menu.path
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
 * 在此目录新建终端。
 *
 * 只做两件事：建标签（openTerminalAt 内部会把它设成激活项 → 子标签栏切过去、主区域显示它）
 * 与把键盘焦点交给它。第三轮删掉「主区域模式」之后这里不再需要切模式——激活子标签本身
 * 就是切换。
 */
function openTerminalHere() {
  const dir = menu.path
  hideMenu()
  terminals.openTerminalAt(sid, dir)
  void nextTick(() => {
    const uid = terminals.activeOf(sid)?.uid ?? terminals.tabsOf(sid)[0]?.uid
    if (uid) panes.get(sid, uid)?.focus()
  })
}

/** 粘贴到什么目录：就是菜单指向的那个（树里每一项都是目录）。 */
async function pasteHere() {
  const dir = menu.path
  hideMenu()
  // 与 FileManager 的 pasteInto 同一套处理：duplicate 错误码也兜住，提示里带上是哪个名字
  const first = sftp.clipboardFor(sid)?.paths[0]
  try {
    const res = await sftp.paste(sid, dir)
    if (res.count) toast(t('files_paste_done', { n: res.count }), 'success')
    // 名字是 store 按「源里的名字没出现在结果里」对出来的；真拿不到名字时宁可不提示，
    // 也不显示一句带空名字的「目标已存在同名项「」」。
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

async function refreshHere() {
  const path = menu.path
  hideMenu()
  try {
    await sftp.refreshTree(sid, path)
  } catch (e) {
    toast(t('files_op_failed', { error: describeError(e) }))
  }
}

// ---- 预览 / 编辑 / 下载 / 信息（文件行，第十六轮） ----

const previewVisible = ref(false)
const previewPath = ref('')
/** 列表里已知的体积：图片超限时后端不回内容，预览弹窗要靠它把「多大」说清楚。 */
const previewSize = ref(0)

const infoVisible = ref(false)
const infoPath = ref('')

function openInfo(path: string) {
  hideMenu()
  if (!path) return
  infoPath.value = path
  infoVisible.value = true
}

/**
 * 进入编辑页。加载成功之后 store 会把激活子标签切到文件管理（编辑页在那儿覆盖文件管理器）；
 * 失败（最常见的是 `permission`：这个文件登录用户改不了）**只报错**，不切标签页、不进编辑页。
 *
 * 提示文案用 `files_editor_load_failed`：`describeError` 会把 `permission` 翻成
 * 「权限不足…」，用户看到的是「为什么改不了」而不是一句「操作失败」。
 */
async function enterEdit(path: string) {
  hideMenu()
  try {
    await editor.open(sid, path)
  } catch (e) {
    toast(t('files_editor_load_failed', { error: describeError(e) }))
  }
}

/**
 * 预览弹窗里点了「编辑」：**进了编辑页才**关掉预览（弹窗层级比编辑页高，不关会盖住它；
 * 但失败时留着——用户还在读这个文件）。
 */
async function onPreviewEdit(path: string) {
  await enterEdit(path)
  if (editor.isOpen(sid)) previewVisible.value = false
}

async function downloadFile(path: string) {
  hideMenu()
  try {
    await sftp.download(sid, path)
    toast(t('files_download_started', { name: baseName(path) }), 'success')
  } catch (e) {
    toast(t('files_download_failed', { error: describeError(e) }))
  }
}

/** 复制 / 剪切：走与文件管理器**同一个**剪贴板（store 里全局一份），所以树里复制、列表里粘贴。 */
function copyFile(path: string) {
  hideMenu()
  sftp.copyToClipboard(sid, [path])
  toast(t('files_copied_items', { n: 1 }), 'success')
}

function cutFile(path: string) {
  hideMenu()
  sftp.cutToClipboard(sid, [path])
  toast(t('files_cut_items', { n: 1 }), 'success')
}

async function extractFile(path: string) {
  hideMenu()
  try {
    const dir = await sftp.extract(sid, path)
    toast(t('files_extract_done', { dir }), 'success')
  } catch (e) {
    toast(t('files_op_failed', { error: describeError(e) }))
  }
}

type MenuItem = { label: string; run: () => void; danger?: boolean; dim?: boolean }

/**
 * 「粘贴」项：没有剪贴板就不出现；剪贴板来自别的会话时禁用并说明原因（不跨会话是硬约束，
 * 见 stores/sftp.ts 的说明）。`dim` 的项点了给原因，而不是真的 disabled——真 disabled 的
 * 按钮连点都点不动，用户只会以为界面坏了。
 */
function pasteItem(): MenuItem | null {
  if (sftp.clipboardIsForeign(sid)) {
    return {
      label: t('files_paste'),
      dim: true,
      run: () => toast(t('files_paste_cross_session'), 'info')
    }
  }
  if (!sftp.clipboardFor(sid)) return null
  return { label: t('files_paste'), run: () => void pasteHere() }
}

/** 导出项：exportRoot 为空 = 服务端没配 TRIM_DATA_SHARE_PATHS，禁用并说明。 */
function exportItem(path: string): MenuItem {
  if (!sftp.exportRoot()) {
    return {
      label: t('files_export_nas'),
      dim: true,
      run: () => toast(t('files_export_nas_unavailable'), 'info')
    }
  }
  return { label: t('files_export_nas'), run: () => void exportHere(path, false) }
}

/** 目录菜单：顺序按契约 11.3；根目录下把「不存在父目录/不能改」的几项去掉。 */
function dirMenuItems(path: string): MenuItem[] {
  const root = path === '/'
  const items: MenuItem[] = [{ label: t('files_new_dir'), run: () => newHere(true) }]
  items.push({ label: t('files_new_file'), run: () => newHere(false) })
  items.push({ label: t('files_upload_here'), run: uploadHere })
  items.push({ label: t('files_open_terminal_here'), run: openTerminalHere })
  const paste = pasteItem()
  if (paste) items.push(paste)
  if (!root) {
    items.push({ label: t('files_rename'), run: renameHere })
    items.push({ label: t('common_delete'), danger: true, run: askDelete })
    items.push({ label: t('files_compress'), run: () => void compressHere('tar.gz') })
    items.push({ label: t('files_compress_zip'), run: () => void compressHere('zip') })
    items.push(exportItem(path))
  }
  items.push({ label: t('files_copy_path'), run: () => void copyPath() })
  items.push({ label: t('files_refresh'), run: () => void refreshHere() })
  return items
}

/**
 * 文件菜单（第十六轮）：与文件管理器行菜单同一套动作与顺序（预览 / 编辑 / 重命名 / 复制 /
 * 剪切 / 删除 / 下载 / 导出至飞牛 / 压缩为 tar.gz / 压缩为 zip / 解压（可解压时）/ 信息 /
 * 复制完整路径）——「树里看到的文件」与「列表里看到的文件」是同一个对象，动作不该有差别。
 *
 * 「编辑」只对按扩展名算文本的文件显示（图片没有编辑器；真正的可写性由后端判定，
 * 见契约 26.2）；「解压」只对归档后缀显示。
 */
function fileMenuItems(path: string): MenuItem[] {
  const items: MenuItem[] = [
    {
      label: t('files_preview'),
      run: () => {
        hideMenu()
        previewPath.value = path
        previewSize.value = fileSizeOf(path)
        previewVisible.value = true
      }
    }
  ]
  if (canEditPath(path)) items.push({ label: t('common_edit'), run: () => void enterEdit(path) })
  items.push({ label: t('files_rename'), run: renameHere })
  items.push({ label: t('files_copy'), run: () => copyFile(path) })
  items.push({ label: t('files_cut'), run: () => cutFile(path) })
  items.push({ label: t('common_delete'), danger: true, run: askDelete })
  items.push({ label: t('files_download'), run: () => void downloadFile(path) })
  items.push(exportItem(path))
  items.push({ label: t('files_compress'), run: () => void compressHere('tar.gz') })
  items.push({ label: t('files_compress_zip'), run: () => void compressHere('zip') })
  if (isArchivePath(baseName(path))) {
    items.push({ label: t('files_extract'), run: () => void extractFile(path) })
  }
  items.push({ label: t('files_info'), run: () => openInfo(path) })
  items.push({ label: t('files_copy_path'), run: () => void copyPath() })
  return items
}

/** 文件行的大小：从树缓存里查（菜单里点「预览」时用）。 */
function fileSizeOf(path: string): number {
  for (const list of Object.values(state.value.treeChildren)) {
    const hit = list.find((entry) => entry.path === path)
    if (hit) return hit.size
  }
  return 0
}

const menuItems = computed<MenuItem[]>(() => {
  const path = menu.path
  if (!path) return []
  return menu.file ? fileMenuItems(path) : dirMenuItems(path)
})

// 组件卸载时摘掉 window 上的「点别处关闭」监听（hideMenu 里会摘，这里兜住「菜单开着就卸载」
// 的情况——例如会话被关掉时整个左栏一起消失）。
onBeforeUnmount(hideMenu)
</script>

<template>
  <div class="flex h-full min-h-0 flex-col">
    <div
      ref="scrollEl"
      class="thin-scroll min-h-0 flex-1 overflow-auto py-1 text-sm"
      @contextmenu.self.prevent="openRootMenu($event)"
    >
      <div v-if="!rows.length" class="px-2 py-3 text-xs text-ink-faint">
        {{ t('common_loading') }}
      </div>

      <!-- key 必须带上 kind：「···更多文件」那一行的 path 就是它所属目录的 path，
           只用 path 会和目录自己的行撞 key（Vue 会复用错节点，表现为高亮跳到别处） -->
      <template v-for="row in rows" :key="`${row.kind}:${row.path}`">
        <div
          class="group flex cursor-pointer items-center gap-1 rounded px-1.5"
          :class="[
            'py-1',
            row.kind !== 'more' && state.dir === row.path
              ? 'bg-brand-soft font-medium text-ink dark:text-ink-dark'
              : 'text-ink-soft hover:bg-black/5 dark:text-ink-soft-dark dark:hover:bg-white/5',
            row.kind === 'more' ? 'italic' : ''
          ]"
          :data-tree-path="row.kind === 'more' ? undefined : row.path"
          :title="row.kind === 'more' ? moreFilesHint(row.path) : row.path"
          @click="onRowClick(row)"
          @contextmenu.prevent="onRowContext(row, $event)"
        >
          <span class="shrink-0" :style="{ width: `${row.depth * 12}px` }" />
          <!-- 展开箭头只属于目录；文件与「更多文件」用等宽占位，图标才会上下对齐 -->
          <button
            v-if="row.kind === 'dir'"
            type="button"
            class="shrink-0 rounded p-0.5 text-ink-faint hover:text-ink dark:hover:text-white"
            :title="row.path"
            :aria-label="row.name"
            :aria-expanded="isExpanded(row.path)"
            @click="toggleNode(row.path, $event)"
          >
            <svg
              class="h-3.5 w-3.5 transition-transform"
              :class="isExpanded(row.path) ? 'rotate-90' : ''"
              viewBox="0 0 24 24"
              fill="none"
              stroke="currentColor"
              stroke-width="1.5"
              stroke-linecap="round"
              stroke-linejoin="round"
            >
              <path d="m9 6 6 6-6 6" />
            </svg>
          </button>
          <span v-else class="h-4.5 w-4.5 shrink-0" />

          <svg
            v-if="row.kind === 'dir'"
            class="h-4 w-4 shrink-0 text-warning"
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
            v-else-if="row.kind === 'file'"
            class="h-4 w-4 shrink-0 text-ink-faint"
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

          <span class="min-w-0 flex-1 truncate" :title="row.kind === 'more' ? '' : row.path">
            {{ row.name }}
          </span>

          <!-- 家目录标一下：树是以 / 为根的，用户得能一眼看到自己「家」在哪 -->
          <svg
            v-if="row.kind === 'dir' && isHome(row.path)"
            class="h-3.5 w-3.5 shrink-0 text-brand"
            viewBox="0 0 24 24"
            fill="none"
            stroke="currentColor"
            stroke-width="1.5"
            stroke-linecap="round"
            stroke-linejoin="round"
            :title="t('files_home')"
          >
            <path d="m3 10 9-7 9 7v10a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2z" />
            <path d="M9 22V12h6v10" />
          </svg>

          <!-- 「···更多文件」没有菜单（它不是一个文件系统对象）：它的点击就是唯一动作 -->
          <button
            v-if="row.kind !== 'more'"
            type="button"
            class="tree-tool hover-reveal h-6 w-6 shrink-0"
            :title="t('files_more')"
            :aria-label="t('files_more')"
            @click.stop="openMenu(row.path, $event, row.kind === 'file')"
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

        <div
          v-if="row.kind === 'dir' && isLoading(row.path)"
          class="py-1 text-[11px] text-ink-faint"
          :style="{ paddingLeft: `${(row.depth + 1) * 12 + 34}px` }"
        >
          {{ t('common_loading') }}
        </div>
      </template>
    </div>

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

    <!-- 预览（文件行点击 / 文件菜单）：与文件管理器同一个弹窗、同一套接口；
         弹窗里那个「编辑」按钮回到这里的 enterEdit，走的也是同一条路 -->
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
    <UploadDialog
      :visible="uploadVisible"
      :sid="sid"
      :dir="uploadDir"
      @update:visible="uploadVisible = $event"
    />
    <PromptDialog
      :visible="prompt.show"
      :title="prompt.title"
      :label="t('files_new_name_prompt')"
      :initial-value="prompt.initial"
      @update:visible="prompt.show = $event"
      @confirm="onPromptConfirm"
      @cancel="prompt.show = false"
    />
    <ConfirmDialog
      :visible="confirm.show"
      :title="confirm.title"
      :message="confirm.message"
      :danger="true"
      @update:visible="confirm.show = $event"
      @confirm="onConfirmOk"
      @cancel="confirm.show = false"
    />
  </div>
</template>
