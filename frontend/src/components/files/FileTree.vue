<script setup lang="ts">
// FileTree — 左栏文件树（懒加载、只看目录）。
//
// 只列目录：左栏的用途就是「快速跳到某个目录」（会话提示语写的就是「在文件树里点选目录即可
// 浏览」），把文件也铺进来会让树变得又长又难找，而且点文件的语义（预览？下载？）在 11rem 宽
// 的栏里也说不清。文件操作一律在文件管理面板里做。
//
// 点目录 = navigate + 切到「文件管理」子标签 + emit open-files：外层（SessionView）拿到
// 事件后把面板亮出来，这样树和面板的激活状态只由 sessionUi.activeSubOf 一个来源决定。
import { computed, nextTick, reactive, ref, watch } from 'vue'
import { t } from '@/i18n'
import { describeError } from '@/serverapi'
import { useSessionUiStore, FILES_TAB } from '@/stores/sessionUi'
import { useSftpStore } from '@/stores/sftp'
import { useToastStore } from '@/stores/toast'
import { baseName } from '@/utils/format'
import PromptDialog from '@/components/PromptDialog.vue'
import UploadDialog from './UploadDialog.vue'

const { sid, mobile = false } = defineProps<{ sid: string; mobile?: boolean }>()
const emit = defineEmits<{ 'open-files': [path: string] }>()

const sftp = useSftpStore()
const sessionUi = useSessionUiStore()
const toastStore = useToastStore()

const state = computed(() => sftp.stateOf(sid))

interface TreeRow {
  path: string
  name: string
  depth: number
}

/** 展开状态下的可见行（扁平化，模板里只渲染一层 v-for）。 */
const rows = computed<TreeRow[]>(() => {
  const st = state.value
  const root = st.home || st.dir
  if (!root) return []
  const out: TreeRow[] = [{ path: root, name: baseName(root) || t('files_home'), depth: 0 }]
  const walk = (parent: string, depth: number) => {
    // 没展开就不列子项：懒加载的全部意义就在这里（大目录不要一次性铺开）
    if (!st.expanded.includes(parent)) return
    for (const entry of st.treeChildren[parent] ?? []) {
      out.push({ path: entry.path, name: entry.name, depth })
      walk(entry.path, depth + 1)
    }
  }
  walk(root, 1)
  return out
})

const isExpanded = (path: string) => state.value.expanded.includes(path)
const isLoading = (path: string) => state.value.treeLoading[path] === true

function toast(msg: string, type: 'success' | 'error' | 'info' = 'error') {
  toastStore.show(msg, type)
}

// ---- 点击 / 展开 ----

async function openDir(path: string) {
  await sftp.navigate(sid, path)
  // 点中一个还没展开的目录就顺手展开：下拉一层马上能看到子目录，也让当前目录的高亮
  // 不至于停在「屏幕上看不见的那一行」。展开失败（权限等）不算错误，导航已经成功了。
  if (!isExpanded(path)) {
    try {
      await sftp.toggleExpand(sid, path)
    } catch {
      /* 忽略：导航本身已经成功 */
    }
  }
  // 激活的文件管理子标签 + 通知外层换面板：两者缺一个都会出现「点了树却没反应」
  sessionUi.setActiveSub(sid, FILES_TAB)
  emit('open-files', path)
}

async function toggleNode(path: string, event: MouseEvent) {
  event.stopPropagation()
  try {
    await sftp.toggleExpand(sid, path)
  } catch (e) {
    toast(`${t('files_load_failed')}：${describeError(e)}`)
  }
}

/**
 * 把当前目录在树里露出来（挂载时执行一次）。
 *
 * 不做的话，从面包屑 / 文件管理器进到深层目录再回到左栏，树还停在展开的根节点上，
 * 用户看不到自己「在哪」——高亮的那一行根本不在屏幕上。
 */
let revealedFor = ''
async function revealCurrent() {
  const st = state.value
  const root = st.home
  const dir = st.dir
  if (!root || !dir || dir === root || !dir.startsWith(root)) return
  const rest = dir.slice(root.length).split('/').filter(Boolean)
  const expand = async (path: string) => {
    if (st.expanded.includes(path)) return
    await sftp.toggleExpand(sid, path)
  }
  let parent = root
  try {
    await expand(parent)
    // 只需要展开「当前目录的祖辈」：末段自己那行已经可见了
    for (let i = 0; i < rest.length - 1; i += 1) {
      const child = `${parent}/${rest[i]}`
      if (!st.treeChildren[parent]?.some((entry) => entry.path === child)) break
      await expand(child)
      parent = child
    }
  } catch {
    // 露不出来不是错误（目录可能已被删），静默停在能展开的那一层
  }
}

// 家目录要等 ensure 拿到才算数，所以盯 home 而不是在 mounted 里直接跑
watch(
  () => state.value.home,
  (home) => {
    if (!home || revealedFor === sid) return
    revealedFor = sid
    void nextTick(revealCurrent)
  }
)

// ---- 节点菜单：上传到此目录 / 新建文件夹 ----

const menu = reactive({ show: false, x: 0, y: 0, path: '' })
const menuEl = ref<HTMLElement | null>(null)
const MENU_MARGIN = 8

function openMenu(path: string, event: MouseEvent) {
  menu.show = true
  menu.x = event.clientX
  menu.y = event.clientY
  menu.path = path
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

/** 在空白处右键 = 对根节点操作（家目录还没拿到时不给菜单，否则菜单指向一个空路径）。 */
function openRootMenu(event: MouseEvent) {
  const root = rows.value[0]
  if (!root) return
  openMenu(root.path, event)
}

const uploadVisible = ref(false)
const uploadDir = ref('')

function uploadHere() {
  uploadDir.value = menu.path
  hideMenu()
  uploadVisible.value = true
}

const prompt = reactive<{ show: boolean }>({ show: false })
let pendingDir = ''

function newDirHere() {
  pendingDir = menu.path
  hideMenu()
  prompt.show = true
}

async function confirmNewDir(value: string) {
  prompt.show = false
  const name = value.trim()
  if (!name) {
    toast(t('files_name_required'))
    return
  }
  if (name === '.' || name === '..' || name.includes('/')) {
    toast(t('files_name_invalid'))
    return
  }
  const parent = pendingDir
  if (!parent) return
  try {
    await sftp.createDir(sid, parent, name)
    toast(t('files_created', { name }), 'success')
    // 新建的目录得看得见：父节点没展开的话顺手展开（createDir 已经把缓存刷新了）
    if (!state.value.expanded.includes(parent)) await sftp.toggleExpand(sid, parent)
  } catch (e) {
    toast(t('files_op_failed', { error: describeError(e) }))
  }
}
</script>

<template>
  <div class="flex h-full min-h-0 flex-col">
    <div
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
          @click="openDir(row.path)"
          @contextmenu.prevent="openMenu(row.path, $event)"
        >
          <span class="shrink-0" :style="{ width: `${row.depth * 12}px` }" />
          <button
            type="button"
            class="shrink-0 rounded p-0.5 text-ink-faint hover:text-ink dark:hover:text-white"
            :title="t('files_open')"
            :aria-label="t('files_open')"
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
        type="button"
        class="block w-full whitespace-nowrap px-3 py-1.5 text-left text-ink hover:bg-black/5 dark:text-ink-dark dark:hover:bg-white/5"
        @click="uploadHere"
      >
        {{ t('files_upload_here') }}
      </button>
      <button
        type="button"
        class="block w-full whitespace-nowrap px-3 py-1.5 text-left text-ink hover:bg-black/5 dark:text-ink-dark dark:hover:bg-white/5"
        @click="newDirHere"
      >
        {{ t('files_new_dir') }}
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
      :title="t('files_new_dir_title')"
      :label="t('files_new_name_prompt')"
      @update:visible="prompt.show = $event"
      @confirm="confirmNewDir"
      @cancel="prompt.show = false"
    />
  </div>
</template>
