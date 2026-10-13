<script setup lang="ts">
// FileTree — 左栏文件树（以远程 `/` 为根、懒加载、只列目录）。
//
// 只列目录：左栏的用途就是「快速跳到某个目录」（会话提示语写的就是「在文件树里点选目录即可
// 浏览」），把文件也铺进来会让树变得又长又难找，而且点文件的语义（预览？下载？）在 11rem 宽
// 的栏里也说不清。文件操作一律在文件管理面板里做。
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
import { computed, nextTick, reactive, ref, watch } from 'vue'
import { t } from '@/i18n'
import { ApiError, describeError, type ArchiveFormat } from '@/serverapi'
import { usePaneControlsStore } from '@/stores/paneControls'
import { useSftpStore } from '@/stores/sftp'
import { useTerminalsStore } from '@/stores/terminals'
import { useToastStore } from '@/stores/toast'
import { baseName, joinPath, parentPath } from '@/utils/format'
import ConfirmDialog from '@/components/ConfirmDialog.vue'
import PromptDialog from '@/components/PromptDialog.vue'
import UploadDialog from './UploadDialog.vue'

const { sid, mobile = false } = defineProps<{ sid: string; mobile?: boolean }>()

// 本组件**不 emit**：点目录只更新共享的当前目录（sftp store）与展开状态；主区域显示什么由
// 子标签栏决定（`sessionUi.activeSubOf`），从树里点一下就切走会打断正在终端里干活的人。
const sftp = useSftpStore()
const terminals = useTerminalsStore()
const panes = usePaneControlsStore()
const toastStore = useToastStore()

const state = computed(() => sftp.stateOf(sid))

interface TreeRow {
  path: string
  name: string
  depth: number
}

/** 展开状态下的可见行（扁平化，模板里只渲染一层 v-for）。根永远是第一行。 */
const rows = computed<TreeRow[]>(() => {
  const st = state.value
  const out: TreeRow[] = [{ path: '/', name: t('files_root'), depth: 0 }]
  const walk = (parent: string, depth: number) => {
    // 没展开就不列子项：懒加载的全部意义就在这里（大目录不要一次性铺开）
    if (!st.expanded.includes(parent)) return
    for (const entry of st.treeChildren[parent] ?? []) {
      out.push({ path: entry.path, name: entry.name, depth })
      walk(entry.path, depth + 1)
    }
  }
  walk('/', 1)
  return out
})

const isExpanded = (path: string) => state.value.expanded.includes(path)
const isLoading = (path: string) => state.value.treeLoading[path] === true
const isHome = (path: string) => !!state.value.home && path === state.value.home

function toast(msg: string, type: 'success' | 'error' | 'info' = 'error') {
  toastStore.show(msg, type)
}

// ---- 点击 / 展开 ----

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
/** 已经做过首次定位的会话：home 之后再变（理论上不该）也不把树重新拽回家目录。 */
const revealed = new Set<string>()

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

// 家目录要等 ensure 拿到响应才知道（不同账号可能是 /root、/home/x），所以盯 home 而不是
// 在 mounted 里直接跑。
watch(
  () => state.value.home,
  (home) => {
    if (!home || revealed.has(sid)) return
    revealed.add(sid)
    // 从 `/` 一路展开到 home：树上就「站在」用户的主场，不用一层层点进去
    void sftp.expandPath(sid, home).then(() => void scrollTo(home))
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

const menu = reactive({ show: false, x: 0, y: 0, path: '' })
const menuEl = ref<HTMLElement | null>(null)
const MENU_MARGIN = 8

function openMenu(path: string, event: MouseEvent) {
  menu.show = true
  menu.x = event.clientX
  menu.y = event.clientY
  menu.path = path
  // 菜单里「导出至飞牛」是否可点取决于运行信息，点开菜单时确保它已经拿到
  void sftp.ensureRuntime()
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

/** 在空白处右键 = 对根节点操作。 */
function openRootMenu(event: MouseEvent) {
  openMenu('/', event)
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

function askDelete() {
  const path = menu.path
  if (path === '/') return
  const name = baseName(path) || path
  hideMenu()
  askConfirm(t('common_tip'), t('files_delete_confirm', { name }), () => void doRemove(path, name))
}

async function doRemove(path: string, name: string) {
  try {
    await sftp.remove(sid, path, true)
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

/** 菜单项顺序按契约 11.3；根目录下把「不存在父目录/不能改」的几项去掉。 */
const menuItems = computed<MenuItem[]>(() => {
  const path = menu.path
  if (!path) return []
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
})
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

      <template v-for="row in rows" :key="row.path">
        <div
          class="group flex cursor-pointer items-center gap-1 rounded px-1.5"
          :class="[
            mobile ? 'py-2' : 'py-1',
            state.dir === row.path
              ? 'bg-brand-soft font-medium text-ink dark:text-ink-dark'
              : 'text-ink-soft hover:bg-black/5 dark:text-ink-soft-dark dark:hover:bg-white/5'
          ]"
          :data-tree-path="row.path"
          :title="row.path"
          @click="openDir(row.path)"
          @contextmenu.prevent="openMenu(row.path, $event)"
        >
          <span class="shrink-0" :style="{ width: `${row.depth * 12}px` }" />
          <button
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
          <svg
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
          <span class="min-w-0 flex-1 truncate" :title="row.path">{{ row.name }}</span>
          <!-- 家目录标一下：树是以 / 为根的，用户得能一眼看到自己「家」在哪 -->
          <svg
            v-if="isHome(row.path)"
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
          <button
            type="button"
            class="tree-tool hover-reveal h-6 w-6 shrink-0"
            :title="t('files_more')"
            :aria-label="t('files_more')"
            @click.stop="openMenu(row.path, $event)"
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
          v-if="isLoading(row.path)"
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
