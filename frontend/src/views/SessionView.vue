<script setup lang="ts">
// SessionView — 一个已打开会话的视图：左栏（面包屑 + 文件树）+ 子标签栏 + 面板区。
//
// 三条不能改的规则：
//  1. **激活子标签只有 sessionUi 一个来源**（契约 6.1）：终端标签激活写自己的 uid，
//     文件管理写 FILES_TAB。终端 store 的 ensure/add/closeTab 也会同步它，这里不再自己改。
//  2. 面板**全部保持挂载**，只切显隐，而且必须用 `visibility: hidden`（Tailwind 的
//     `invisible`）而**不能**用 v-show / display:none：display:none 下容器尺寸为 0，
//     xterm 的 FitAddon 量不到尺寸（fit 出的行列数是错的），WebGL 渲染器还会被 0 尺寸反复
//     触发重排；visibility 保留元素尺寸，xterm 根本不知道自己被隐藏过，重新可见时尺寸本来就是
//     对的、不需要重新 fit（style.css 里 xterm 滚动条那段写 `visibility: inherit` 就是这个前提）。
//     `invisible` 只挂在面板自己身上：FileManager 的弹窗（UploadDialog/PreviewDialog）都
//     Teleport 到 body，不受这里影响（已在两个组件里确认）。
//  3. 激活项指向一个**已经不存在的终端 uid** 时也要显示文件管理面板：否则两块面板都隐藏，
//     用户看到一片空白（面板区是 absolute 叠放的，谁也盖不住谁）。
import { computed, nextTick, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { t } from '@/i18n'
import { useMobileLayout } from '@/composables/useMobileLayout'
import { FILES_TAB, useSessionUiStore } from '@/stores/sessionUi'
import { usePaneControlsStore } from '@/stores/paneControls'
import { useTerminalsStore, type TermTab } from '@/stores/terminals'
import { revealActive, wheelToHorizontal } from '@/utils/scroll'
import BreadcrumbBar from '@/components/files/BreadcrumbBar.vue'
import FileManager from '@/components/files/FileManager.vue'
import FileTree from '@/components/files/FileTree.vue'
import TerminalPane from '@/components/terminal/TerminalPane.vue'
import ConfirmDialog from '@/components/ConfirmDialog.vue'
import PromptDialog from '@/components/PromptDialog.vue'

const props = defineProps<{ sid: string }>()

const emit = defineEmits<{
  (e: 'settings'): void
  (e: 'quick-cmds'): void
}>()

const sessionUi = useSessionUiStore()
const terminals = useTerminalsStore()
const paneControls = usePaneControlsStore()
const isMobile = useMobileLayout()

const termTabs = computed(() => terminals.tabsOf(props.sid))
const activeSub = computed(() => sessionUi.activeSubOf(props.sid))
/** 面板区当前该显示文件管理吗（激活项是 files，或指向一个已不存在的终端 uid）。 */
const filesActive = computed(
  () =>
    activeSub.value === FILES_TAB || !termTabs.value.some((tab) => tab.uid === activeSub.value)
)

// ---------- 工具条的作用对象 ----------

/**
 * 工具条按钮 / 快捷指令发到哪个终端：先用 sessionUi 指的那个终端；它指向文件管理
 * （或一个已经不存在的 uid）时退到「本会话最后激活的终端」，一个终端都没有才算没有目标。
 */
function controlsFor() {
  const list = termTabs.value
  const sub = activeSub.value
  const uid = list.some((tab) => tab.uid === sub)
    ? sub
    : (terminals.activeOf(props.sid)?.uid ?? list.at(-1)?.uid ?? null)
  return paneControls.get(props.sid, uid)
}

const hasTarget = computed(() => !!controlsFor())
/** 有目标但还没连上：按钮可以点，但要在按钮态上体现出来（灰一点）。 */
const targetConnected = computed(() => controlsFor()?.connected === true)

/**
 * 工具条动作（搜索 / 粘贴 / 清屏 / 重连）作用到当前终端。
 *
 * 点完必须把**键盘焦点还给终端**：按钮在 Chrome 上点击后会获得焦点，焦点留在按钮上时
 * 用户接着敲的空格/回车会去「再按一次按钮」——清屏/重连被重复触发，而普通字符则完全
 * 进不了 shell（e2e 里就是这样：点完重连后打字毫无反应）。搜索是例外：它要保留焦点在
 * 自己的输入框里，所以不还。
 */
function runAction(action: 'search' | 'paste' | 'clear' | 'reconnect', event?: MouseEvent) {
  const ctl = controlsFor()
  ctl?.[action]()
  if (action === 'search') return
  ;(event?.currentTarget as HTMLElement | null)?.blur()
  ctl?.focus()
}

// ---------- 子标签切换 ----------

function activateFiles() {
  sessionUi.setActiveSub(props.sid, FILES_TAB)
}

function activateTerm(uid: string) {
  sessionUi.setActiveSub(props.sid, uid)
  focusTermSoon(uid)
}

/**
 * 切到某个终端后把键盘焦点交给它。
 *
 * 不做这一步是个真 bug（e2e 抓到的）：点子标签 / 点「+」之后，焦点留在**那个按钮**上，
 * 用户接着敲命令时按下的空格会去「激活」这个还在焦点里的按钮 —— 点一次「+」之后按三个
 * 空格，就等于又开了三个终端（每开一个都会真的拨号建一条 SSH 会话，代价很实在）。
 * 把焦点移进终端面板，既让后续输入直接进 shell，也顺手解掉按钮的「空格/回车即点击」。
 *
 * 走 paneControls 的 focus()（契约 6.1 的命令出口）而不是直接摸 DOM：面板还没 open()
 * 时它会挂起聚焦意图并在 open() 后补发（见 TerminalPane 的 pendingFocus）。
 */
function focusTermSoon(uid: string) {
  void nextTick(() => paneControls.get(props.sid, uid)?.focus())
}

function addTerminal() {
  // terminals.add 会同时把新标签设为激活项（写入 sessionUi），这里不用再管激活
  const tab = terminals.add(props.sid)
  // 新面板刚 push 进模板，要等它挂载完（并完成 paneControls 注册）才能聚焦
  focusTermSoon(tab.uid)
}

// 切换子标签后把激活项滚进可视区（终端标签多了会滚出屏幕）
watch(activeSub, async () => {
  await nextTick()
  revealActive(strip.value, '.subtab-active')
})

watch(
  () => termTabs.value.length,
  async () => {
    await nextTick()
    revealActive(strip.value, '.subtab-active')
  }
)

// ---------- 标签溢出时自动收起功能名（照 terminal 的实现）----------

const strip = ref<HTMLElement | null>(null)
const labelsOn = ref(true)
// 手动展开的时间点：展开后 2s 内不自动收回，否则用户刚点开就被「标签溢出」立刻收掉
let lastManualExpandAt = 0

/** 桌面工具行是否在屏幕上：用 Tailwind md 断点这条线（与移动端第二行的显隐互补）。 */
const desktopRow =
  typeof window !== 'undefined' ? window.matchMedia('(min-width: 768px)') : null

function toggleLabels() {
  labelsOn.value = !labelsOn.value
  if (labelsOn.value) lastManualExpandAt = Date.now()
}

function updateLabelState() {
  const el = strip.value
  if (!el || !desktopRow?.matches) return
  if (labelsOn.value && el.scrollWidth > el.clientWidth + 8) {
    if (Date.now() - lastManualExpandAt > 2000) labelsOn.value = false
  }
}

let stripRo: ResizeObserver | null = null

// 观察标签条宽度：标签变多/窗口变窄时它溢出，此时自动收起功能名。
// 挂在 onMounted 而不是 watch(模板 ref)：标签条永远存在（没有 v-if），挂载后元素就在，
// 这里能确保观察者一定装上，不用赌模板 ref 的赋值时机。
onMounted(() => {
  const el = strip.value
  updateLabelState()
  if (el && window.ResizeObserver) {
    stripRo = new ResizeObserver(() => updateLabelState())
    stripRo.observe(el)
  }
})

watch(labelsOn, () => nextTick(updateLabelState))

// ---------- 左栏：显隐 + 拖拽调宽 ----------

const LEFT_MIN = 180
const LEFT_WIDTH_KEY = 'webssh-left-width'

/** 最大宽度现算（窗口一半）：写死像素在窄窗口上会把主面板挤没，宽窗口又白白浪费。 */
function leftMax() {
  return Math.max(LEFT_MIN, Math.floor(window.innerWidth / 2))
}

const leftWidth = ref(Math.min(Number(localStorage.getItem(LEFT_WIDTH_KEY)) || 300, leftMax()))
const leftVisible = computed(() => sessionUi.leftIsVisible(props.sid))
const showLeft = computed(() => !isMobile.value && leftVisible.value)

function startResize(event: PointerEvent) {
  // 手柄按下会顺手选中文本 / 触发一次点击，先挡掉
  event.preventDefault()
  const startX = event.clientX
  const startWidth = leftWidth.value
  const move = (e: PointerEvent) => {
    leftWidth.value = Math.min(Math.max(startWidth + (e.clientX - startX), LEFT_MIN), leftMax())
  }
  const up = () => {
    window.removeEventListener('pointermove', move)
    window.removeEventListener('pointerup', up)
    // pointercancel 必须一起摘：触屏手势被系统接管、或拖拽被取消时不会再来 pointerup，
    // 监听留在 window 上的表现是「之后鼠标一动，树就跟着缩」，直到下一次松手。
    window.removeEventListener('pointercancel', up)
    localStorage.setItem(LEFT_WIDTH_KEY, String(leftWidth.value))
  }
  window.addEventListener('pointermove', move)
  window.addEventListener('pointerup', up)
  window.addEventListener('pointercancel', up)
}

onBeforeUnmount(() => {
  stripRo?.disconnect()
  stripRo = null
})

// ---------- 终端标签：状态点 / 关闭 / 重命名 ----------

function dotClass(tab: TermTab): string {
  if (tab.status === 'open' && !tab.restoring) return 'bg-success'
  if (tab.status === 'connecting' || tab.restoring || tab.status === 'detached') return 'bg-warning'
  if (tab.status === 'exited') return 'bg-ink-faint dark:bg-ink-faint-dark'
  return 'bg-danger'
}

const closeVisible = ref(false)
const closeUid = ref<string | null>(null)

function requestCloseTerm(uid: string) {
  closeUid.value = uid
  closeVisible.value = true
}

function confirmCloseTerm() {
  const uid = closeUid.value
  closeUid.value = null
  // closeTab 内部会发 /api/terminals/close（远程 shell 就此终止）并把激活项挪到邻近标签
  if (uid) terminals.closeTab(props.sid, uid)
}

const closeTitle = computed(() => {
  const tab = termTabs.value.find((item) => item.uid === closeUid.value)
  return tab ? terminals.titleOf(props.sid, tab) : ''
})

const renameVisible = ref(false)
const renameUid = ref<string | null>(null)
const renameInitial = ref('')

function startRenameTerm(tab: TermTab) {
  renameUid.value = tab.uid
  renameInitial.value = tab.title
  renameVisible.value = true
}

function onRenamed(value: string) {
  const uid = renameUid.value
  renameUid.value = null
  // 空值 = 回到默认的「终端 N」（terminals.setTitle 里 title 为空即用默认名）
  if (uid) terminals.setTitle(props.sid, uid, value.trim())
}

</script>

<template>
  <div class="flex h-full min-h-0 bg-bg dark:bg-bg-dark">
    <!-- 左栏（桌面端；移动端不渲染，文件管理直接当子页面） -->
    <aside
      v-if="showLeft"
      class="flex shrink-0 flex-col border-r border-line dark:border-line-dark"
      :style="{ width: `${leftWidth}px` }"
      :title="t('session_left_hint')"
    >
      <BreadcrumbBar :sid="sid" @show-full="activateFiles" />
      <FileTree :sid="sid" class="min-h-0 flex-1" @open-files="activateFiles" />
    </aside>
    <div
      v-if="showLeft"
      class="w-1 shrink-0 cursor-col-resize bg-transparent transition-colors hover:bg-brand/40"
      @pointerdown="startResize"
    ></div>

    <section class="flex min-w-0 flex-1 flex-col">
      <!-- 子标签栏：固定段（文件管理，不参与横向滚动）+ 滚动段（终端标签 + 新建） -->
      <div class="flex shrink-0 items-center gap-1 border-b border-line px-2 py-1 dark:border-line-dark">
        <div class="flex shrink-0 items-center gap-1">
          <button
            class="subtab"
            :class="filesActive ? 'subtab-active' : 'subtab-idle'"
            :title="t('session_full_files')"
            @click="activateFiles"
          >
            <svg
              viewBox="0 0 24 24"
              fill="none"
              stroke="currentColor"
              stroke-width="1.5"
              stroke-linecap="round"
              stroke-linejoin="round"
              class="h-3.5 w-3.5"
            >
              <path d="M3 7a2 2 0 0 1 2-2h4l2 2h8a2 2 0 0 1 2 2v8a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2z" />
            </svg>
            <span class="truncate">{{ t('session_files_tab') }}</span>
          </button>

          <!-- 左栏开关（桌面端才有左栏） -->
          <button
            v-if="!isMobile"
            class="tree-tool"
            :class="leftVisible ? '!text-brand' : ''"
            :title="t('session_left_toggle')"
            @click="sessionUi.toggleLeft(sid)"
          >
            <svg
              viewBox="0 0 24 24"
              fill="none"
              stroke="currentColor"
              stroke-width="1.5"
              stroke-linecap="round"
              stroke-linejoin="round"
              class="h-4 w-4"
            >
              <rect x="3" y="4" width="18" height="16" rx="2" />
              <line x1="9" y1="4" x2="9" y2="20" />
            </svg>
          </button>

          <span class="mx-0.5 h-4 w-px shrink-0 bg-line dark:bg-line-dark"></span>
        </div>

        <div
          ref="strip"
          class="no-scrollbar flex min-w-0 flex-1 items-center gap-1 overflow-x-auto"
          @wheel="wheelToHorizontal"
        >
          <div
            v-for="tab in termTabs"
            :key="tab.uid"
            class="subtab group max-w-[14rem]"
            :class="activeSub === tab.uid ? 'subtab-active' : 'subtab-idle'"
            :title="terminals.titleOf(sid, tab)"
            @click="activateTerm(tab.uid)"
            @dblclick="startRenameTerm(tab)"
          >
            <span class="h-1.5 w-1.5 shrink-0 rounded-full" :class="dotClass(tab)"></span>
            <span class="truncate">{{ terminals.titleOf(sid, tab) }}</span>
            <button
              class="hover-reveal flex h-4 w-4 shrink-0 items-center justify-center rounded-full"
              :class="
                activeSub === tab.uid
                  ? 'text-white/80 hover:bg-white/20'
                  : 'text-ink-faint hover:bg-black/10 dark:hover:bg-white/10'
              "
              :title="t('session_close_terminal')"
              @click.stop="requestCloseTerm(tab.uid)"
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

          <button class="tree-tool shrink-0" :title="t('session_new_terminal')" @click="addTerminal">
            <svg
              viewBox="0 0 24 24"
              fill="none"
              stroke="currentColor"
              stroke-width="2"
              stroke-linecap="round"
              class="h-4 w-4"
            >
              <line x1="12" y1="5" x2="12" y2="19" />
              <line x1="5" y1="12" x2="19" y2="12" />
            </svg>
          </button>
        </div>

        <!-- 桌面工具条（子标签栏右侧）：作用于当前激活终端 -->
        <div class="hidden shrink-0 items-center gap-0.5 md:flex">
          <button
            class="g-btn-ghost !h-8 !px-1.5"
            :title="labelsOn ? t('term_labels_collapse') : t('term_labels_expand')"
            @click="toggleLabels"
          >
            <svg
              v-if="labelsOn"
              viewBox="0 0 24 24"
              fill="none"
              stroke="currentColor"
              stroke-width="2"
              stroke-linecap="round"
              stroke-linejoin="round"
              class="h-3.5 w-3.5"
            >
              <path d="m6 17 5-5-5-5" />
              <path d="m13 17 5-5-5-5" />
            </svg>
            <svg
              v-else
              viewBox="0 0 24 24"
              fill="none"
              stroke="currentColor"
              stroke-width="2"
              stroke-linecap="round"
              stroke-linejoin="round"
              class="h-3.5 w-3.5"
            >
              <path d="m11 17-5-5 5-5" />
              <path d="m18 17-5-5 5-5" />
            </svg>
          </button>

          <button
            class="g-btn-ghost !h-8 !px-2 text-xs"
            :class="targetConnected ? '' : 'opacity-50'"
            :disabled="!hasTarget"
            :title="t('term_act_search')"
            @click="runAction('search', $event)"
          >
            <svg
              viewBox="0 0 24 24"
              fill="none"
              stroke="currentColor"
              stroke-width="1.5"
              stroke-linecap="round"
              stroke-linejoin="round"
              class="h-4 w-4"
            >
              <circle cx="11" cy="11" r="8" />
              <line x1="21" y1="21" x2="16.65" y2="16.65" />
            </svg>
            <span :class="labelsOn ? '' : 'hidden'">{{ t('term_act_search') }}</span>
          </button>

          <button
            class="g-btn-ghost !h-8 !px-2 text-xs"
            :class="targetConnected ? '' : 'opacity-50'"
            :disabled="!hasTarget"
            :title="t('term_act_paste')"
            @click="runAction('paste', $event)"
          >
            <svg
              viewBox="0 0 24 24"
              fill="none"
              stroke="currentColor"
              stroke-width="1.5"
              stroke-linecap="round"
              stroke-linejoin="round"
              class="h-4 w-4"
            >
              <path d="M16 4h2a2 2 0 0 1 2 2v14a2 2 0 0 1-2 2H6a2 2 0 0 1-2-2V6a2 2 0 0 1 2-2h2" />
              <rect x="8" y="2" width="8" height="4" rx="1" />
            </svg>
            <span :class="labelsOn ? '' : 'hidden'">{{ t('term_act_paste') }}</span>
          </button>

          <button
            class="g-btn-ghost !h-8 !px-2 text-xs"
            :class="targetConnected ? '' : 'opacity-50'"
            :disabled="!hasTarget"
            :title="t('term_act_clear')"
            @click="runAction('clear', $event)"
          >
            <svg
              viewBox="0 0 24 24"
              fill="none"
              stroke="currentColor"
              stroke-width="1.5"
              stroke-linecap="round"
              stroke-linejoin="round"
              class="h-4 w-4"
            >
              <rect x="3" y="3" width="18" height="18" rx="2" />
              <path d="m9 9 6 6M15 9l-6 6" />
            </svg>
            <span :class="labelsOn ? '' : 'hidden'">{{ t('term_act_clear') }}</span>
          </button>

          <button
            class="g-btn-ghost !h-8 !px-2 text-xs"
            :class="targetConnected ? '' : 'opacity-50'"
            :disabled="!hasTarget"
            :title="t('term_act_reconnect')"
            @click="runAction('reconnect', $event)"
          >
            <svg
              viewBox="0 0 24 24"
              fill="none"
              stroke="currentColor"
              stroke-width="1.5"
              stroke-linecap="round"
              stroke-linejoin="round"
              class="h-4 w-4"
            >
              <path d="M3 12a9 9 0 0 1 9-9 9.75 9.75 0 0 1 6.74 2.74L21 8" />
              <path d="M21 3v5h-5" />
              <path d="M21 12a9 9 0 0 1-9 9 9.75 9.75 0 0 1-6.74-2.74L3 16" />
              <path d="M3 21v-5h5" />
            </svg>
            <span :class="labelsOn ? '' : 'hidden'">{{ t('term_act_reconnect') }}</span>
          </button>

          <button
            class="g-btn-ghost !h-8 !px-2 text-xs"
            :title="t('term_act_quick_cmds')"
            @click="emit('quick-cmds')"
          >
            <svg
              viewBox="0 0 24 24"
              fill="none"
              stroke="currentColor"
              stroke-width="1.5"
              stroke-linecap="round"
              stroke-linejoin="round"
              class="h-4 w-4"
            >
              <polyline points="4 17 10 11 4 5" />
              <line x1="12" y1="19" x2="20" y2="19" />
            </svg>
            <span :class="labelsOn ? '' : 'hidden'">{{ t('term_act_quick_cmds') }}</span>
          </button>

          <button class="g-btn-ghost !h-8 !px-2 text-xs" :title="t('term_act_settings')" @click="emit('settings')">
            <svg
              viewBox="0 0 24 24"
              fill="none"
              stroke="currentColor"
              stroke-width="1.5"
              stroke-linecap="round"
              stroke-linejoin="round"
              class="h-4 w-4"
            >
              <path
                d="M12.22 2h-.44a2 2 0 0 0-2 2v.18a2 2 0 0 1-1 1.73l-.43.25a2 2 0 0 1-2 0l-.15-.08a2 2 0 0 0-2.73.73l-.22.38a2 2 0 0 0 .73 2.73l.15.1a2 2 0 0 1 1 1.72v.51a2 2 0 0 1-1 1.74l-.15.09a2 2 0 0 0-.73 2.73l.22.38a2 2 0 0 0 2.73.73l.15-.08a2 2 0 0 1 2 0l.43.25a2 2 0 0 1 1 1.73V20a2 2 0 0 0 2 2h.44a2 2 0 0 0 2-2v-.18a2 2 0 0 1 1-1.73l.43-.25a2 2 0 0 1 2 0l.15.08a2 2 0 0 0 2.73-.73l.22-.39a2 2 0 0 0-.73-2.73l-.15-.08a2 2 0 0 1-1-1.74v-.5a2 2 0 0 1 1-1.74l.15-.09a2 2 0 0 0 .73-2.73l-.22-.38a2 2 0 0 0-2.73-.73l-.15.08a2 2 0 0 1-2 0l-.43-.25a2 2 0 0 1-1-1.73V4a2 2 0 0 0-2-2z"
              />
              <circle cx="12" cy="12" r="3" />
            </svg>
            <span :class="labelsOn ? '' : 'hidden'">{{ t('term_act_settings') }}</span>
          </button>
        </div>
      </div>

      <!-- 移动端第二行工具条：图标常显（触屏没有 hover，也没有「功能名」这个概念） -->
      <div
        class="flex min-h-10 shrink-0 items-center justify-between gap-0.5 border-b border-line/70 bg-black/[0.03] px-2 py-1.5 md:hidden dark:border-line-dark/70 dark:bg-white/[0.04]"
      >
        <button class="g-btn-ghost !h-7 !px-2" :title="t('term_act_settings')" @click="emit('settings')">
          <svg
            viewBox="0 0 24 24"
            fill="none"
            stroke="currentColor"
            stroke-width="1.5"
            stroke-linecap="round"
            stroke-linejoin="round"
            class="h-4 w-4"
          >
            <path
              d="M12.22 2h-.44a2 2 0 0 0-2 2v.18a2 2 0 0 1-1 1.73l-.43.25a2 2 0 0 1-2 0l-.15-.08a2 2 0 0 0-2.73.73l-.22.38a2 2 0 0 0 .73 2.73l.15.1a2 2 0 0 1 1 1.72v.51a2 2 0 0 1-1 1.74l-.15.09a2 2 0 0 0-.73 2.73l.22.38a2 2 0 0 0 2.73.73l.15-.08a2 2 0 0 1 2 0l.43.25a2 2 0 0 1 1 1.73V20a2 2 0 0 0 2 2h.44a2 2 0 0 0 2-2v-.18a2 2 0 0 1 1-1.73l.43-.25a2 2 0 0 1 2 0l.15.08a2 2 0 0 0 2.73-.73l.22-.39a2 2 0 0 0-.73-2.73l-.15-.08a2 2 0 0 1-1-1.74v-.5a2 2 0 0 1 1-1.74l.15-.09a2 2 0 0 0 .73-2.73l-.22-.38a2 2 0 0 0-2.73-.73l-.15.08a2 2 0 0 1-2 0l-.43-.25a2 2 0 0 1-1-1.73V4a2 2 0 0 0-2-2z"
            />
            <circle cx="12" cy="12" r="3" />
          </svg>
        </button>
        <div class="flex items-center gap-0.5">
          <button
            class="g-btn-ghost !h-7 !px-2"
            :disabled="!hasTarget"
            :title="t('term_act_search')"
            @click="runAction('search', $event)"
          >
            <svg
              viewBox="0 0 24 24"
              fill="none"
              stroke="currentColor"
              stroke-width="1.5"
              stroke-linecap="round"
              stroke-linejoin="round"
              class="h-4 w-4"
            >
              <circle cx="11" cy="11" r="8" />
              <line x1="21" y1="21" x2="16.65" y2="16.65" />
            </svg>
          </button>
          <button
            class="g-btn-ghost !h-7 !px-2"
            :disabled="!hasTarget"
            :title="t('term_act_paste')"
            @click="runAction('paste', $event)"
          >
            <svg
              viewBox="0 0 24 24"
              fill="none"
              stroke="currentColor"
              stroke-width="1.5"
              stroke-linecap="round"
              stroke-linejoin="round"
              class="h-4 w-4"
            >
              <path d="M16 4h2a2 2 0 0 1 2 2v14a2 2 0 0 1-2 2H6a2 2 0 0 1-2-2V6a2 2 0 0 1 2-2h2" />
              <rect x="8" y="2" width="8" height="4" rx="1" />
            </svg>
          </button>
          <button
            class="g-btn-ghost !h-7 !px-2"
            :disabled="!hasTarget"
            :title="t('term_act_clear')"
            @click="runAction('clear', $event)"
          >
            <svg
              viewBox="0 0 24 24"
              fill="none"
              stroke="currentColor"
              stroke-width="1.5"
              stroke-linecap="round"
              stroke-linejoin="round"
              class="h-4 w-4"
            >
              <rect x="3" y="3" width="18" height="18" rx="2" />
              <path d="m9 9 6 6M15 9l-6 6" />
            </svg>
          </button>
          <button
            class="g-btn-ghost !h-7 !px-2"
            :disabled="!hasTarget"
            :title="t('term_act_reconnect')"
            @click="runAction('reconnect', $event)"
          >
            <svg
              viewBox="0 0 24 24"
              fill="none"
              stroke="currentColor"
              stroke-width="1.5"
              stroke-linecap="round"
              stroke-linejoin="round"
              class="h-4 w-4"
            >
              <path d="M3 12a9 9 0 0 1 9-9 9.75 9.75 0 0 1 6.74 2.74L21 8" />
              <path d="M21 3v5h-5" />
              <path d="M21 12a9 9 0 0 1-9 9 9.75 9.75 0 0 1-6.74-2.74L3 16" />
              <path d="M3 21v-5h5" />
            </svg>
          </button>
          <button
            class="g-btn-ghost !h-7 !px-2"
            :title="t('term_act_quick_cmds')"
            @click="emit('quick-cmds')"
          >
            <svg
              viewBox="0 0 24 24"
              fill="none"
              stroke="currentColor"
              stroke-width="1.5"
              stroke-linecap="round"
              stroke-linejoin="round"
              class="h-4 w-4"
            >
              <polyline points="4 17 10 11 4 5" />
              <line x1="12" y1="19" x2="20" y2="19" />
            </svg>
          </button>
        </div>
      </div>

      <!-- 面板区：全部保持挂载、absolute 叠放，非激活的加 invisible（理由见文件头第 2 条）。
           hidden 的元素不接收指针事件，所以叠着的隐藏面板不会挡住当前面板的操作。 -->
      <div class="relative min-h-0 flex-1">
        <div class="absolute inset-0" :class="filesActive ? '' : 'invisible'">
          <FileManager :sid="sid" :mobile="isMobile" />
        </div>
        <TerminalPane
          v-for="tab in termTabs"
          :key="tab.uid"
          :sid="sid"
          :tab="tab"
          :active="activeSub === tab.uid"
          class="absolute inset-0"
          :class="activeSub === tab.uid ? '' : 'invisible'"
        />
      </div>
    </section>

    <ConfirmDialog
      :visible="closeVisible"
      :title="t('session_close_confirm_title')"
      :message="t('session_close_confirm_msg', { title: closeTitle })"
      :danger="true"
      :confirm-text="t('common_close')"
      :cancel-text="t('common_cancel')"
      @confirm="confirmCloseTerm"
      @cancel="closeUid = null"
      @update:visible="(v) => { if (!v) closeUid = null }"
    />

    <PromptDialog
      v-model:visible="renameVisible"
      :title="t('session_rename_title')"
      :label="t('session_rename_prompt')"
      :initial-value="renameInitial"
      @confirm="onRenamed"
    />
  </div>
</template>
