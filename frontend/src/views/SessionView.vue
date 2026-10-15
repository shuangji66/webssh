<script setup lang="ts">
// SessionView — 一个已打开会话的视图：子标签栏（固定「文件管理」+ 终端标签）+ 左栏 + 面板区。
//
// 四条不能改的规则：
//  1. **激活子标签只有 sessionUi 一个来源**（契约 6.1）：终端标签激活写自己的 uid，文件管理
//     标签写 FILES_TAB。写入口都经过 store：终端标签走 `terminals.setActive`（它同时更新
//     「用户刚才在哪个终端」，文件管理里的「返回终端」靠它），ensure/add/closeTab 也同步它；
//     本组件不直接改 sessionUi 里的终端 uid。
//  2. **「文件管理」子标签与左栏是二选一**（契约 13.1）：激活文件管理 → 左栏收起（文件管理器
//     自带面包屑）、主区域是文件管理；激活某个终端标签 → 左栏展开、主区域是那个终端。
//     第六轮又叠了一个**用户偏好**：左栏可以被手动收起（sessionUi.leftHiddenOf），收起时子
//     标签栏左侧出现「展开面包屑」。三者是交集关系，主区域仍然只看 activeSub（见 showLeft）。
//     判据只有 `activeSubOf(sid) === FILES_TAB` 一处，**不要**再引入第二个「主区域模式」状态
//     （第二轮那套 `sessionUi.mainMode` 就是因此删掉的：两个开关会互相打架）。
//  3. 面板**全部保持挂载**，只切显隐，而且必须用 `visibility: hidden`（Tailwind 的
//     `invisible`）而**不能**用 v-show / display:none：display:none 下容器尺寸为 0，
//     xterm 的 FitAddon 量不到尺寸（fit 出的行列数是错的），WebGL 渲染器还会被 0 尺寸反复
//     触发重排；visibility 保留元素尺寸，xterm 根本不知道自己被隐藏过，重新可见时尺寸本来就是
//     对的、不需要重新 fit（style.css 里 xterm 滚动条那段写 `visibility: inherit` 就是这个前提）。
//     `invisible` 只挂在面板自己身上：FileManager 的弹窗（UploadDialog/PreviewDialog）都
//     Teleport 到 body，不受这里影响（已在两个组件里确认）。
//     例外是**左栏**：那里没有 xterm，且它必须真的让出横向空间，所以用 v-show（见 showLeft）。
//  4. 主区域绝不允许落在「两块面板都隐藏」的中间态（面板是 absolute 叠放的，谁也盖不住谁，
//     用户看到的就是一片空白）：一个终端标签都没有时交给文件管理；sessionUi 指向一个已经不
//     存在的终端 uid 时退到本会话最后激活的终端。
import { computed, nextTick, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { t } from '@/i18n'
import { useWideLayout } from '@/composables/useMobileLayout'
import { useAppTabsStore } from '@/stores/appTabs'
import { FILES_TAB, useSessionUiStore } from '@/stores/sessionUi'
import { usePaneControlsStore } from '@/stores/paneControls'
import { useQuickAccessStore } from '@/stores/quickAccess'
import { termStatusDotClass, useTerminalsStore, type TermTab } from '@/stores/terminals'
import { revealActive, wheelToHorizontal } from '@/utils/scroll'
import BreadcrumbBar from '@/components/files/BreadcrumbBar.vue'
import FileManager from '@/components/files/FileManager.vue'
import FileTree from '@/components/files/FileTree.vue'
import QuickAccessDialog from '@/components/files/QuickAccessDialog.vue'
import TerminalPane from '@/components/terminal/TerminalPane.vue'
import ConfirmDialog from '@/components/ConfirmDialog.vue'
import PromptDialog from '@/components/PromptDialog.vue'

const props = defineProps<{ sid: string }>()

const emit = defineEmits<{
  /** 工具条里的「快捷指令」：弹窗挂在 App.vue（要跨会话复用同一个实例）。 */
  (e: 'quick-cmds'): void
}>()

const sessionUi = useSessionUiStore()
const terminals = useTerminalsStore()
const paneControls = usePaneControlsStore()
/**
 * 快捷访问（第二十轮）：弹窗显隐存在 store 里（两个入口在两个兄弟组件上：文件管理工具栏与
 * 左栏面包屑按钮条），弹窗本体挂在**这里**——它是两者的共同父级，且属于「一个会话一个」的
 * 界面状态（与下面那两个子标签弹窗同一层）。
 */
const quickAccess = useQuickAccessStore()

/**
 * 切走这个会话标签时关掉快捷访问弹窗。
 *
 * 为什么需要这一句：弹窗 Teleport 到 body，而会话视图在切标签时只是 `v-show` 隐藏——
 * Teleport 出去的节点**不在**那棵 display:none 的子树里，会一直浮在首页/另一个会话上面。
 * 列表类弹窗（重命名、关闭确认）都是「点了立刻有结果」的短命弹窗，捞不到也就算了；快捷访问
 * 是一个用户会读、会停一会儿再点的面板，留着它挡住别的标签是明显的错。
 */
const appTabs = useAppTabsStore()
watch(
  () => appTabs.activeId,
  (active) => {
    if (active !== props.sid) quickAccess.setDialog(props.sid, false)
  }
)
/**
 * 左栏要不要占位，看的是**视口宽不宽**（≥ Tailwind 的 md 断点），而不是「是不是触屏」。
 *
 * 第十轮用户的原话：「平板不是窄屏，可以按模式显示面包屑的」。iPad 的 CSS 宽度是 768/834/1024，
 * 用触屏判定会把它的左栏整个吃掉；而左栏在平板上完全放得下（宽度还能用手柄拖，见 19.1）。
 * 触屏专属的东西（底部辅助键条、`.hover-reveal`、16px 输入框）仍然用 `useMobileLayout()`——
 * 「屏幕大不大」和「有没有键盘/鼠标」是两件事，别混成一条判据。
 */
const wide = useWideLayout()

const termTabs = computed(() => terminals.tabsOf(props.sid))
const activeSub = computed(() => sessionUi.activeSubOf(props.sid))
/** 固定的「文件管理」子标签是不是当前激活的那个（它的高亮只认这个，不认下面的兜底）。 */
const filesTabActive = computed(() => activeSub.value === FILES_TAB)

/**
 * 当前激活的终端 uid（null = 没有激活的终端：文件管理子标签在前台，或本会话一个终端都没有）。
 *
 * 它同时决定三件事，所以必须只有这一份：主区域显示哪个终端面板、面板区的显隐、工具条
 * （搜索/粘贴/清屏/重连/快捷指令）作用在哪个终端。契约 13.1 里用户的原话是「每个子终端标签页
 * 都要有快捷指令按钮，否则在弹窗里点指令都不知道输入到了哪个终端」——所以文件管理在前台时
 * **故意**没有作用对象（工具条整体禁用），而不是偷偷打到某个看不见的终端上。
 *
 * 「一个终端都没有」也算没有对象：此时终端面板无处可显示，主区域交给文件管理器（规则 4）。
 */
const activeTermUid = computed<string | null>(() => {
  const sub = activeSub.value
  if (sub === FILES_TAB) return null
  if (termTabs.value.some((tab) => tab.uid === sub)) return sub
  // sessionUi 指向一个已经不存在的终端 uid（标签被关掉、sessionUi 还没跟上）：退回本会话最后
  // 激活的终端，别让面板区空着（下面 filesActive 与这个判断必须一致，否则会出现空白）。
  return terminals.activeOf(props.sid)?.uid ?? termTabs.value.at(-1)?.uid ?? null
})

/** 主区域显示文件管理器吗 = 没有激活的终端（判据只有 activeTermUid 一处，保证两者不会矛盾）。 */
const filesActive = computed(() => activeTermUid.value === null)

// ---------- 工具条的作用对象 ----------

function controlsFor() {
  return paneControls.get(props.sid, activeTermUid.value)
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

/** 固定的「文件管理」子标签：桌面与移动端都是同一个动作（切回终端就点终端标签）。 */
function activateFiles() {
  sessionUi.setActiveSub(props.sid, FILES_TAB)
}

function activateTerm(uid: string) {
  // 走 terminals.setActive：它同时写 sessionUi（唯一真相还是那边），并让 terminals 知道
  // 「用户刚才在哪个终端」——文件管理子标签里的「返回终端」才不会回到一个陌生的标签上
  // （第五轮用户报的 bug：只写 sessionUi 时 activeUid 停在最后新建的标签上）。
  terminals.setActive(props.sid, uid)
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
  // terminals.add 会同时把新标签设为激活项（写入 sessionUi）——「+」建出来的终端立刻可见，
  // 因为主区域显示哪个面板完全由 activeSub 决定，不需要（也不该）再动第二个状态。
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

// ---------- 左栏：与「文件管理」子标签二选一 + 拖拽调宽 ----------

const LEFT_MIN = 180
const LEFT_WIDTH_KEY = 'webssh-left-width'

/** 最大宽度现算（窗口一半）：写死像素在窄窗口上会把主面板挤没，宽窗口又白白浪费。 */
function leftMax() {
  return Math.max(LEFT_MIN, Math.floor(window.innerWidth / 2))
}

const leftWidth = ref(Math.min(Number(localStorage.getItem(LEFT_WIDTH_KEY)) || 300, leftMax()))

/**
 * 左栏（面包屑按钮条 + 目录树）的可见性 = 三个条件的**交集**（第六轮起）：
 *   1. **宽视口**（窄屏没有左栏的位置，文件管理是整页的子标签；平板 ≥768px 算宽，见 wide）；
 *   2. 激活的是某个终端（切到文件管理子标签就收起——文件管理器自带面包屑，再挂一条目录树是
 *      重复的）。判据用 filesActive 而不是直接写 activeSubOf !== FILES_TAB，是为了与主区域
 *      **严格互斥**：「一个终端都没有 + sessionUi 指着幽灵 uid」这种退化状态下主区域会兜底
 *      显示文件管理器，此时左栏也必须收起，否则会出现「左栏 + 文件管理」同时在场；
 *   3. 用户没有手动收起它（`sessionUi.leftHiddenOf`，第六轮新增的按钮）。
 *
 * 这**不是**第二个「主区域模式」：主区域显示什么仍然只由 activeSub 决定，这里只是把左栏的
 * 一个「用户偏好」叠加上去（第二轮那版 mainMode 之所以被删，是因为它自己也能决定主区域）。
 */
const showLeft = computed(
  () => wide.value && !filesActive.value && !sessionUi.leftHiddenOf(props.sid)
)

/**
 * 左栏被用户手动收起（且此刻本该显示）——子标签栏最左侧就出现「展开面包屑」按钮。
 *
 * 为什么带上 `!filesActive`：在文件管理子标签里，左栏本来就不该显示（规则 2），这时给一个
 * 「展开面包屑」按钮是骗人的——点了也不会出现。用户在那个状态想找左栏，就点终端子标签
 * （左栏会回来，除非确实被手动收起过）。
 */
const leftCollapsed = computed(
  () => wide.value && !filesActive.value && sessionUi.leftHiddenOf(props.sid)
)

function showLeftAgain() {
  sessionUi.setLeftHidden(props.sid, false)
}

function startResize(event: PointerEvent) {
  // 手柄按下会顺手选中文本 / 触发一次点击，先挡掉；顺便把焦点从手柄上摘掉（
  // 手柄在 Chrome 上会获得焦点，用户接着按空格/回车会「再按一次」它）
  event.preventDefault()
  ;(event.currentTarget as HTMLElement | null)?.blur()
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
  // 确认框的按钮是刚被卸载的那个：焦点会掉回 body，用户接着敲的键全丢。
  // 关闭后激活项会挪到邻近标签（面板自己会聚焦），这一句是给它兜底。
  const next = terminals.activeOf(props.sid)?.uid
  if (next) focusTermSoon(next)
}

/**
 * 取消 / 点遮罩：什么都没关，焦点必须还给原来那个终端。
 *
 * 与 confirmCloseTerm 同一条 AGENTS 约束（第 5 节）：焦点留在按钮上时，用户接着敲的空格/回车
 * 会去「再按一次按钮」——这里是「再关一次终端」，比多点一次「+」更让人摸不着头脑。
 */
function cancelCloseTerm() {
  const uid = closeUid.value
  closeUid.value = null
  if (uid) focusTermSoon(uid)
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
    <!-- 左栏（桌面端，且只在终端标签激活时显示；移动端不渲染 —— 文件管理在窄屏是整页子标签）。
         用 v-show 而不是 v-if：子标签切换会频繁收起/展开左栏，v-if 每次都重建组件（目录树会
         重新挂载、丢掉滚动位置，还可能重跑「展开到登录用户家目录」那段逻辑）。这里 display:none
         是安全的——左栏里没有 xterm，规则 3 的「尺寸量不到」问题只针对终端面板。
         BreadcrumbBar / FileTree 都不向外抛事件（切主区域就是切子标签，契约 13.1）。 -->
    <aside
      v-show="showLeft"
      class="relative flex shrink-0 flex-col border-r border-line dark:border-line-dark"
      :style="{ width: `${leftWidth}px` }"
      :title="t('session_left_title')"
    >
      <BreadcrumbBar :sid="sid" />
      <FileTree :sid="sid" class="min-h-0 flex-1" />

      <!-- 拖动手柄（第九轮）：吸附在分隔线上、居于面包屑按钮条右侧，**代替**直接拖那条 1px 的分隔线。
           为什么需要它：平板是触屏、没有鼠标，1px 的线根本抓不住（手指的落点精度差一个数量级）。
           手柄 `-translate-x-1/2` 骑在边框上（一半在左栏、一半在主区域），触屏上是一个 20×32 的
           实体目标；`touch-none` 保证拖动时不会顺手滚动画布。桌面端也留着——它同时是个可见的
           「这里可以拖」提示。 -->
      <button
        v-if="showLeft"
        type="button"
        class="tree-tool absolute top-1.5 right-0 z-10 !h-8 !w-5 -translate-x-1/2 touch-none !cursor-col-resize border border-line bg-surface shadow-sm dark:border-line-dark dark:bg-surface-dark"
        :title="t('session_left_resize')"
        :aria-label="t('session_left_resize')"
        @pointerdown="startResize"
      >
        <svg
          class="h-3.5 w-3.5"
          viewBox="0 0 24 24"
          fill="none"
          stroke="currentColor"
          stroke-width="2"
          stroke-linecap="round"
        >
          <path d="M9 6v12M15 6v12" />
        </svg>
      </button>
    </aside>

    <section class="flex min-w-0 flex-1 flex-col">
      <!-- 子标签栏：固定段 = 「文件管理」（不参与横向滚动，否则终端一多就被挤出屏幕、再也点不回来）
           + 滚动段 = 终端标签 + 新建。激活它 = 主区域切到文件管理、左栏收起（契约 13.1）。 -->
      <div class="flex shrink-0 items-center gap-1 border-b border-line px-2 py-1 dark:border-line-dark">
        <div class="flex shrink-0 items-center gap-1">
          <!-- 展开面包屑（第六轮）：只在左栏被手动收起时出现，点一下左栏回来、它自己消失。
               放在「文件管理」子标签左侧（用户原话），与左栏里那个「隐藏面包屑」互为一对。 -->
          <button
            v-if="leftCollapsed"
            class="tree-tool"
            :title="t('session_show_left')"
            :aria-label="t('session_show_left')"
            @click="showLeftAgain"
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
              <path d="M9 4v16" />
              <path d="m14 9 2 3-2 3" />
            </svg>
          </button>

          <button
            class="subtab"
            :class="filesTabActive ? 'subtab-active' : 'subtab-idle'"
            :title="t('session_files_tab')"
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
            <span class="h-1.5 w-1.5 shrink-0 rounded-full" :class="termStatusDotClass(tab)"></span>
            <span class="truncate">{{ terminals.titleOf(sid, tab) }}</span>
            <!-- 关闭按钮：鼠标悬停才变红，触屏常驻红（.close-danger，见 style.css）。
                 只剩这一个终端时不显示：关掉它这个会话就一个 shell 都不剩（要重新连还得再点
                 「+」），而它恰恰是用户唯一在用的那个，误触代价最大。首页卡片里那一行仍可关闭，
                 所以不是「关不掉」而是「别在唯一标签上手滑」。 -->
            <button
              v-if="termTabs.length > 1"
              class="close-danger flex h-4 w-4 shrink-0 items-center justify-center"
              :class="activeSub === tab.uid ? 'on-brand' : ''"
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

        <!-- 桌面工具条（子标签栏右侧）：作用于当前激活终端。
             文件管理子标签在前台时**整条隐藏**（`hasTarget` 为假，五个按钮加折叠/展开都没有作用
             对象）：用户的原话是「文件管理标签显示时这些按钮不该出现」，而留一排灰按钮只是噪音
             ——切回终端时它们本来就回来了（不涉及任何状态，v-if 是安全的：这些按钮没有需要保留
             的 DOM 状态，与规则 3 禁 v-if 的「面板」不是一回事）。 -->
        <div v-if="!filesActive" class="hidden shrink-0 items-center gap-0.5 md:flex">
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

          <!-- 快捷指令（契约 13.1）：放在会话工具条里、作用于当前激活终端，用户才知道指令发进
               了哪个终端。文件管理子标签在前台时没有激活终端 → 随其余动作按钮一起禁用。 -->
          <button
            class="g-btn-ghost !h-8 !px-2 text-xs"
            :disabled="!hasTarget"
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
        </div>
      </div>

      <!-- 移动端工具行：图标常显（触屏没有 hover，也没有「功能名」这个概念）。
           内容与桌面行一致（搜索/粘贴/清屏/重连/快捷指令）；设置只在首页工具条上。
           与桌面一样：文件管理子标签在前台时整行隐藏（这一行里只有那几个按钮，
           藏着它们等于整行没有内容，留着一条空行只会白占屏高）。 -->
      <div
        v-if="!filesActive"
        class="flex min-h-10 shrink-0 items-center justify-end gap-0.5 border-b border-line/70 bg-black/[0.03] px-2 py-1.5 md:hidden dark:border-line-dark/70 dark:bg-white/[0.04]"
      >
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
            :disabled="!hasTarget"
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

      <!-- 面板区：全部保持挂载、absolute 叠放，非激活的加 invisible（理由见文件头第 3 条）。
           hidden 的元素不接收指针事件，所以叠着的隐藏面板不会挡住当前面板的操作。 -->
      <div class="relative min-h-0 flex-1">
        <div class="absolute inset-0" :class="filesActive ? '' : 'invisible'">
          <!-- `mobile` 判的也是「窄不窄」而不是「是不是触屏」：平板放得下权限列，
               空白处右键（含多选时的批量菜单）也该照常弹（第十轮）。 -->
          <FileManager :sid="sid" :mobile="!wide" />
        </div>
        <TerminalPane
          v-for="tab in termTabs"
          :key="tab.uid"
          :sid="sid"
          :tab="tab"
          :active="activeTermUid === tab.uid"
          class="absolute inset-0"
          :class="activeTermUid === tab.uid ? '' : 'invisible'"
        />
      </div>
    </section>

    <!-- 关闭子终端前确认（会终止远端的 shell）。`visible` 必须用 v-model 接回去：
         漏接的话弹窗关一次之后就再也弹不出来（症状与原因见 ConfirmDialog 头部注释）。 -->
    <ConfirmDialog
      v-model:visible="closeVisible"
      :title="t('session_close_confirm_title')"
      :message="t('session_close_confirm_msg', { title: closeTitle })"
      :danger="true"
      :confirm-text="t('common_close')"
      :cancel-text="t('common_cancel')"
      @confirm="confirmCloseTerm"
      @cancel="cancelCloseTerm"
    />

    <PromptDialog
      v-model:visible="renameVisible"
      :title="t('session_rename_title')"
      :label="t('session_rename_prompt')"
      :initial-value="renameInitial"
      @confirm="onRenamed"
    />

    <!-- 快捷访问弹窗（第二十轮）：文件管理工具栏与左栏面包屑按钮条上的两个书签按钮打开的是
         **同一个**弹窗，所以它挂在这一层（两处的共同父级），显隐由 store 里的 per-sid 开关
         驱动。`visible` 必须接回去（漏接的症状见 ConfirmDialog 头部注释）。 -->
    <QuickAccessDialog
      :sid="sid"
      :visible="quickAccess.isDialogOpen(sid)"
      @update:visible="quickAccess.setDialog(sid, $event)"
    />
  </div>
</template>
