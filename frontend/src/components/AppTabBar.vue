<script setup lang="ts">
// AppTabBar — 应用级标签条（壳层顶栏）：首页标签固定不可关 + 每个已打开的会话一个标签。
//
// 关闭会话标签是**应用级动作**（契约 6.3）：先在后端终止该会话（终端 + SFTP 全断），
// 成功后再关标签并清四个 store 的前端状态。顺序不能反：先关标签再关后端，一旦后端失败，
// 用户就再也点不到那个会话去重试了（标签已经没了，只能刷新页面）。
//
// 右侧不再有任何按钮（契约 13.2）：快捷指令在会话工具条里（要「作用于哪个终端」才说得清，
// 13.1），设置在首页工具条里。所以这里不抛任何事件，只渲染标签。
import { nextTick, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { t } from '@/i18n'
import { describeError } from '@/serverapi'
import { useAppTabsStore, type AppTab } from '@/stores/appTabs'
import { useHomeStore } from '@/stores/home'
import { useTerminalsStore } from '@/stores/terminals'
import { useToastStore } from '@/stores/toast'
import { sessionLabel } from '@/utils/format'
import { revealActive, wheelToHorizontal } from '@/utils/scroll'
import ConfirmDialog from './ConfirmDialog.vue'
import PromptDialog from './PromptDialog.vue'

const tabs = useAppTabsStore()
const home = useHomeStore()
const terminals = useTerminalsStore()
const toast = useToastStore()

function labelOf(sid: string): string {
  const session = home.sessions.find((item) => item.id === sid)
  return session ? sessionLabel(session) : sid
}

function titleOf(tab: AppTab): string {
  // 首页标题走 i18n：store 里那份是创建时的快照，切语言后不会变
  return tab.kind === 'home' ? t('app_tab_home') : tab.title
}

/** 会话状态点：后端不认它开着 = 灰；有终端在连接/恢复 = 琥珀；有终端出错/被顶号 = 红。 */
function dotClass(sid: string): string {
  if (!home.isOpen(sid)) return 'bg-ink-faint dark:bg-ink-faint-dark'
  const list = terminals.tabsOf(sid)
  if (list.some((tab) => tab.status === 'connecting' || tab.restoring)) return 'bg-warning'
  if (list.some((tab) => tab.status === 'error' || tab.status === 'detached')) return 'bg-danger'
  return 'bg-success'
}

// ---------- 标签条：滚轮/拖动横向滚动 + 选中项滚进可视区 ----------

const strip = ref<HTMLElement | null>(null)
// 拖动横滚只对精确指针（鼠标）开放：触屏上横向拖动本该是页面/标签自己的手势
const isFinePointer =
  typeof window !== 'undefined' && window.matchMedia('(pointer: fine)').matches

let dragging = false
let dragged = false
let dragStartX = 0
let dragStartScroll = 0

function onStripDown(e: MouseEvent) {
  if (!isFinePointer || e.button !== 0) return
  dragging = true
  dragged = false
  dragStartX = e.clientX
  dragStartScroll = strip.value?.scrollLeft ?? 0
}

function onStripMove(e: MouseEvent) {
  if (!dragging || !strip.value) return
  const dx = e.clientX - dragStartX
  // 拖动超过 5px 才算拖动：否则一点点的抖动会把「点标签」吃掉，用户觉得标签点不动
  if (Math.abs(dx) > 5) dragged = true
  strip.value.scrollLeft = dragStartScroll - dx
}

function stopDrag() {
  dragging = false
}

function onTabClick(id: string) {
  if (dragged) {
    dragged = false
    return
  }
  tabs.activate(id)
}

// 新建会话标签（会话可能在滚动区外）后把它滚进可视区
watch(
  () => tabs.activeId,
  async () => {
    await nextTick()
    revealActive(strip.value, '.app-tab-active')
  }
)

watch(
  () => tabs.tabs.length,
  async () => {
    await nextTick()
    revealActive(strip.value, '.app-tab-active')
  }
)

// ---------- 双击重命名 ----------

const renameVisible = ref(false)
const renameTarget = ref<AppTab | null>(null)

function startRename(tab: AppTab) {
  if (tab.kind !== 'session') return
  renameTarget.value = tab
  renameVisible.value = true
}

async function onRenamed(value: string) {
  const target = renameTarget.value
  renameTarget.value = null
  const next = value.trim()
  // 清空 = 放弃改名（保持原样；store 里也会拒绝空值）
  if (!target || !next) return
  // 先改展示层：改名必须立刻可见，等一个网络往返再变的话用户会以为没生效
  const before = target.title
  tabs.setTitle(target.id, next)
  try {
    // 从这一轮起标签名就是会话名（契约 11.1.4）：改名要跟着落进会话配置，否则刷新页面
    // （或换个设备打开）名字又变回去。home.renameSession 成功后会把配置与标签一起同步。
    await home.renameSession(target.id, next)
  } catch (e) {
    // 配置没存住就把展示层回滚，否则会出现「顶栏显示新名、刷新后变回旧名」的前后不一致
    tabs.setTitle(target.id, before)
    toast.show(t('home_rename_failed', { error: describeError(e) }), 'error')
  }
}

function cancelRename() {
  renameTarget.value = null
}

// ---------- 关闭（含「关闭其他标签」）----------

const confirmVisible = ref(false)
const pendingSids = ref<string[]>([])
const confirmMessage = ref('')

function askClose(sids: string[]) {
  if (!sids.length) return
  pendingSids.value = sids
  confirmMessage.value = t('home_close_confirm_msg', {
    name: sids.map(labelOf).join(' / ')
  })
  confirmVisible.value = true
}

/** 关闭单个会话标签：没有任何终端且后端不认为它开着时不必确认（关掉只是收拾残局）。 */
function requestClose(sid: string) {
  if (!home.isOpen(sid) && terminals.countOf(sid) === 0) {
    void doClose(sid)
    return
  }
  askClose([sid])
}

function requestCloseOthers(id: string) {
  const others = tabs.sessionTabs.filter((tab) => tab.id !== id).map((tab) => tab.id)
  askClose(others)
}

async function onConfirmClose() {
  const sids = pendingSids.value
  pendingSids.value = []
  // 串行关闭：每个都会打后端，并发没有收益，失败了也好定位是哪一个
  for (const sid of sids) await doClose(sid)
}

function onCancelClose() {
  pendingSids.value = []
}

async function doClose(sid: string) {
  try {
    // 「后端已经没这个会话了」由 home.closeSession 内部消化（本地状态对齐）；
    // 走到这里还抛的就都是真失败（网络 / internal），必须报出来并保留标签，
    // 否则用户以为会话已经关了，实际后端还开着。
    await home.closeSession(sid)
  } catch (e) {
    toast.show(t('home_close_failed', { error: describeError(e) }), 'error')
    return
  }
  home.forgetSession(sid)
}

// ---------- 右键菜单（关闭其他标签）----------

const menu = ref<{ x: number; y: number; id: string } | null>(null)

function onContext(tab: AppTab, ev: MouseEvent) {
  if (tab.kind !== 'session') return
  menu.value = { x: ev.clientX, y: ev.clientY, id: tab.id }
  // 用 { once: true } 的 window 点击关菜单：比给 document 挂永久监听再摘干净可靠
  window.setTimeout(() => window.addEventListener('click', hideMenu, { once: true }), 0)
}

function hideMenu() {
  menu.value = null
}

function onCloseOthers() {
  const id = menu.value?.id
  hideMenu()
  if (id) requestCloseOthers(id)
}

onMounted(() => {
  void nextTick(() => revealActive(strip.value, '.app-tab-active'))
})

onBeforeUnmount(() => {
  window.removeEventListener('click', hideMenu)
})
</script>

<template>
  <div class="app-header-row flex items-center gap-1.5 px-2 sm:px-3">
    <!-- 标签条：滚轮纵向增量转横向滚动（utils/scroll），鼠标可拖动 -->
    <div
      ref="strip"
      class="flex min-w-0 flex-1 select-none items-center gap-1 overflow-x-auto py-1 no-scrollbar md:cursor-grab md:active:cursor-grabbing"
      @wheel="wheelToHorizontal"
      @mousedown="onStripDown"
      @mousemove="onStripMove"
      @mouseup="stopDrag"
      @mouseleave="stopDrag"
    >
      <div
        v-for="tab in tabs.tabs"
        :key="tab.id"
        class="app-tab group"
        :class="tab.id === tabs.activeId ? 'app-tab-active' : 'app-tab-idle'"
        role="button"
        tabindex="0"
        :title="tab.kind === 'session' ? t('app_tab_rename') : titleOf(tab)"
        @click="onTabClick(tab.id)"
        @dblclick="startRename(tab)"
        @click.middle="tab.kind === 'session' && requestClose(tab.id)"
        @contextmenu.prevent="onContext(tab, $event)"
        @keydown.enter.prevent="onTabClick(tab.id)"
      >
        <!-- 状态点：会话标签才有（首页没有「连接状态」可言） -->
        <span
          v-if="tab.kind === 'session'"
          class="h-1.5 w-1.5 shrink-0 rounded-full"
          :class="dotClass(tab.id)"
          :title="home.isOpen(tab.id) ? t('home_opened') : t('home_closed')"
        ></span>

        <span class="truncate">{{ titleOf(tab) }}</span>

        <!-- 终端子标签数量：一眼看出这个会话下面开着几个终端 -->
        <span
          v-if="tab.kind === 'session' && terminals.countOf(tab.id) > 0"
          class="shrink-0 rounded-full px-1.5 text-[10px] leading-4"
          :class="tab.id === tabs.activeId ? 'bg-white/20' : 'bg-black/10 dark:bg-white/10'"
        >
          {{ terminals.countOf(tab.id) }}
        </span>

        <!-- 关闭按钮：鼠标悬停才变红，触屏常驻红（.close-danger，见 style.css） -->
        <button
          v-if="tab.closable"
          class="close-danger flex h-4 w-4 shrink-0 items-center justify-center"
          :class="tab.id === tabs.activeId ? 'on-brand' : ''"
          :title="t('common_close')"
          @click.stop="requestClose(tab.id)"
        >
          <svg
            viewBox="0 0 24 24"
            fill="none"
            stroke="currentColor"
            stroke-width="2.5"
            stroke-linecap="round"
            class="h-3 w-3"
          >
            <line x1="18" y1="6" x2="6" y2="18" />
            <line x1="6" y1="6" x2="18" y2="18" />
          </svg>
        </button>
      </div>
    </div>

    <!-- 顶栏右侧不再有任何按钮（契约 13.2）：只剩应用标签本身 -->

    <!-- 右键菜单：只放「关闭其他标签」（其余动作在标签上和首页卡片上） -->
    <div
      v-if="menu"
      class="fixed z-50 rounded-md border border-line bg-surface py-1 text-sm shadow-pop dark:border-line-dark dark:bg-surface-dark"
      :style="{ left: `${menu.x}px`, top: `${menu.y}px` }"
      @click="onCloseOthers"
    >
      <button
        class="block w-full whitespace-nowrap px-3 py-1.5 text-left text-ink hover:bg-black/5 dark:text-ink-dark dark:hover:bg-white/5"
        type="button"
      >
        {{ t('app_tab_close_others') }}
      </button>
    </div>

    <PromptDialog
      v-model:visible="renameVisible"
      :title="t('app_tab_rename')"
      :label="t('common_name')"
      :placeholder="t('app_tab_title_placeholder')"
      :initial-value="renameTarget?.title ?? ''"
      @confirm="onRenamed"
      @cancel="cancelRename"
    />

    <ConfirmDialog
      :visible="confirmVisible"
      :title="t('home_close_confirm_title')"
      :message="confirmMessage"
      :danger="true"
      :confirm-text="t('home_close')"
      :cancel-text="t('common_cancel')"
      @confirm="onConfirmClose"
      @cancel="onCancelClose"
      @update:visible="(v) => { if (!v) confirmVisible = false }"
    />
  </div>
</template>
