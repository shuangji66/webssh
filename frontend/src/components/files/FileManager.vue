<script setup lang="ts">
// FileManager — 会话内的文件管理面板（工具栏 + 面包屑 + 列表 / 网格 + 行内菜单 + 上传）。
//
// 目录内容全部来自 sftp store（见 stores/sftp.ts 的说明）：这个面板与左栏文件树共用同一份
// 数据，组件只管「怎么显示」与「把用户动作翻成 store 调用」，不自己缓存目录列表。
//
// 弹窗（重命名 / 新建 用 PromptDialog，删除用 ConfirmDialog）由 shell 模块提供，见契约 6.2。
import { computed, nextTick, onBeforeUnmount, reactive, ref, watch } from 'vue'
import { t } from '@/i18n'
import { describeError, type FsEntry, type UploadEntry, type UploadResult } from '@/serverapi'
import { useSftpStore } from '@/stores/sftp'
import { useSessionUiStore } from '@/stores/sessionUi'
import { useToastStore } from '@/stores/toast'
import { baseName, formatTime, humanSize, joinPath, parentPath, permText } from '@/utils/format'
import { revealActive, wheelToHorizontal } from '@/utils/scroll'
import ConfirmDialog from '@/components/ConfirmDialog.vue'
import PromptDialog from '@/components/PromptDialog.vue'
import PreviewDialog from './PreviewDialog.vue'
import UploadDialog from './UploadDialog.vue'

// Vue 3.5 的响应式 props 解构：sid 在组件生命周期内不变，但解构写法让模板里少一层 props.。
const { sid, mobile = false } = defineProps<{ sid: string; mobile?: boolean }>()

const sftp = useSftpStore()
const sessionUi = useSessionUiStore()
const toastStore = useToastStore()

/** 统一的提示出口：文案一律走 i18n，失败必须说清原因（不能「点了没反应」）。 */
function toast(msg: string, type: 'success' | 'error' | 'info' = 'error') {
  toastStore.show(msg, type)
}

const state = computed(() => sftp.stateOf(sid))
const selected = computed(() => state.value.selected)
const selecting = computed(() => selected.value.length > 0)

// ---- 生命周期：进面板就定位家目录 ----

// ensure 幂等（左栏文件树也会调它），失败时内部会允许重试，因此这里不需要额外判重。
watch(() => sid, (id) => void sftp.ensure(id), { immediate: true })

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

function toTree() {
  // 左栏被关掉过就一直关着（见 sessionUi 的约定），这里负责把它叫回来
  sessionUi.setLeftVisible(sid, true)
}

function toggleView() {
  sftp.setView(sid, state.value.view === 'list' ? 'grid' : 'list')
}

function selectAll() {
  sftp.select(sid, state.value.entries.map((entry) => entry.path))
}

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

function askDelete(entry: FsEntry) {
  hideMenu()
  askConfirm(
    t('common_tip'),
    t('files_delete_confirm', { name: entry.name }),
    () => void removePaths([entry])
  )
}

async function removePaths(entries: FsEntry[]) {
  // 单个删除给一条「已删除 xxx」；批量删只报失败——一次删十项弹十条提示反而盖住了报错
  const single = entries.length === 1 ? entries[0] : null
  let failed = false
  for (const entry of entries) {
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
  const entries = state.value.entries.filter((entry) => selected.value.includes(entry.path))
  if (!entries.length) {
    sftp.clearSelection(sid)
    return
  }
  askConfirm(
    t('common_tip'),
    t('files_delete_selected_confirm', { n: entries.length }),
    () => void removePaths(entries)
  )
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

type MenuItem = { label: string; run: () => void; danger?: boolean }

const menuItems = computed<MenuItem[]>(() => {
  const entry = state.value.entries.find((item) => item.path === menu.path)
  if (!entry) return []
  return [
    { label: t('files_open'), run: () => openEntry(entry) },
    { label: t('files_download'), run: () => void downloadEntry(entry) },
    { label: t('files_rename'), run: () => renameEntry() },
    { label: t('files_copy_path'), run: () => void copyPath(entry.path) },
    { label: t('common_delete'), danger: true, run: () => askDelete(entry) }
  ]
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
      <!-- 移动端没有左栏，也就没有东西可切 -->
      <button
        v-if="!mobile"
        type="button"
        class="tree-tool"
        :title="t('files_switch_to_tree')"
        :aria-label="t('files_switch_to_tree')"
        @click="toTree"
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
          <path d="M4 4h16v16H4z" />
          <path d="M9 4v16" />
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

    <!-- 列表的表头：目录不显示大小，所以那一格留空；权限列在移动端省掉（列宽不够） -->
    <div
      v-if="state.view === 'list'"
      class="flex shrink-0 items-center gap-2 border-b border-line px-2 py-1 text-xs font-medium text-ink-soft dark:border-line-dark dark:text-ink-soft-dark"
    >
      <span class="min-w-0 flex-1">{{ t('files_name') }}</span>
      <span class="w-16 shrink-0 text-right">{{ t('files_size') }}</span>
      <span class="w-24 shrink-0 text-right">{{ t('files_modified') }}</span>
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

      <div v-else-if="!state.entries.length" class="px-3 py-6 text-center text-sm text-ink-faint">
        {{ t('files_empty') }}
      </div>

      <template v-else-if="state.view === 'list'">
        <div
          v-for="entry in state.entries"
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
          <span v-if="!mobile" class="w-20 shrink-0 truncate text-right font-mono text-[11px] text-ink-faint">
            {{ permText(entry.mode) }}
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
          v-for="entry in state.entries"
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
        :class="item.danger ? 'text-danger' : 'text-ink dark:text-ink-dark'"
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
