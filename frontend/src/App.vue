<script setup lang="ts">
// App.vue — 应用外壳：顶栏（应用标签）+ 首页 / 已进入过的会话视图（保持挂载）+ Toast。
//
// 启动顺序（契约 6.3，不能调换）：
//   1. useTheme() + useViewportHeight()：先把主题与可视视口变量准备好（壳层高度依赖后者，
//      晚一步会看到高度跳一下）；
//   2. home.loadInfo()：拿后端进程实例 bootId（判定「后端重启过」的基准）；
//   3. home.restore()：拉会话配置 + 已打开会话的实时状态，把后端仍在运行的会话恢复成
//      **应用标签 + 终端子标签**（返回 sid 列表）。
//
// **启动后停在首页，恢复的会话标签不激活。** 「打开前端」与「进入某个会话」是两件事：
// 自动激活最后一个恢复的会话，在手机上是一打开就钻进终端，在另一台设备上则是顺手接管
// （单挂载点会顶掉别人）一个用户根本没打算用的终端。首页卡片上的子终端列表已经由
// restore 填好，点卡片/标签就是明确的「进入」。
//
// 会话视图**延迟挂载**：只有被用户激活过的会话才渲染 SessionView（`enteredSessions`）。
// 一旦进入过就保持挂载（v-show 切显隐），后台标签的终端与文件浏览状态不丢。恢复时只建标签
// 不挂载，还能避免「打开首页就把所有恢复会话的终端全部 attach 一遍」——那会顶掉其他设备。
//
// 应用回到前台 / 切回首页时再对齐一次后端状态（`syncOpenSessions`）：别的设备新开的会话
// 会补成标签、卡片上的子终端列表也会出现；关掉会话的由后端状态反映，不需要刷新页面。
//
// 弹窗归属（契约 13.1/13.2）：顶栏不再有快捷指令与设置入口——快捷指令按钮进会话工具条
// （它的作用对象是「当前激活终端」，属于会话内部的语境），设置按钮进首页工具条。两个弹窗实例
// 仍然由壳层持有：设置本来就是应用级的（主题/语言/字体），快捷指令只需要一个实例 + 一个 sid
// 上下文，没必要在每个会话视图里各挂一份。
import { computed, onBeforeUnmount, onMounted, reactive, ref, watch } from 'vue'
import { t } from '@/i18n'
import { useTheme } from '@/composables/useTheme'
import { useViewportHeight } from '@/composables/useViewportHeight'
import { HOME_TAB_ID, useAppTabsStore } from '@/stores/appTabs'
import { useHomeStore } from '@/stores/home'
import { useToastStore } from '@/stores/toast'
import AppTabBar from '@/components/AppTabBar.vue'
import SettingsDialog from '@/components/SettingsDialog.vue'
import QuickCmdsDialog from '@/components/QuickCmdsDialog.vue'
import Toast from '@/components/Toast.vue'
import HomeView from '@/views/HomeView.vue'
import SessionView from '@/views/SessionView.vue'

useTheme()
useViewportHeight()

const tabs = useAppTabsStore()
const home = useHomeStore()
const toast = useToastStore()

const settingsVisible = ref(false)
const quickCmdsVisible = ref(false)
/** 快捷指令弹窗的会话上下文：由「打开它的那个会话视图」给（工具条按钮就在那儿）。 */
const quickCmdsSid = ref<string | null>(null)
/** 启动恢复期间显示提示条（终端的恢复在面板里自己等 `\x1b]ready\x07`）。 */
const restoring = ref(true)
/**
 * 用户真正进入过的会话（点击应用标签 / 卡片上的终端行 / 卡片「进入」按钮）。
 *
 * 恢复回来的标签只进 `tabs`、不进这里：SessionView 不挂载，终端就不会发起 attach（不带
 * `\x1b]tid` 的挂载会新建终端，带 tid 的挂载会顶掉别的设备）。进入过一次以后常驻，
 * 切回首页也不会卸载它（面板要保持挂载）。
 */
const enteredSessions = reactive(new Set<string>())

/**
 * 实际渲染的会话视图：进入过的，加上此刻就是激活标签的那个。
 *
 * 后半句是给「同一帧里刚 openSession 激活」用的——默认 flush 的 watcher 会在渲染前把
 * sid 记进 enteredSessions，这里再加一道兜底，保证点卡片/标签的第一帧就能挂载。
 */
const liveSessionTabs = computed(() =>
  tabs.sessionTabs.filter((tab) => tab.id === tabs.activeId || enteredSessions.has(tab.id))
)

watch(
  () => tabs.activeId,
  (id) => {
    if (tabs.sessionTabs.some((tab) => tab.id === id)) {
      enteredSessions.add(id)
      return
    }
    // 回到首页时对齐一次后端状态：别的设备开了会话，标签与卡片列表要跟上来
    if (id === HOME_TAB_ID) void syncOpenSessions()
  }
)

// 关掉标签后把 enteredSessions 里的孤儿清掉，避免集合只增不减
watch(
  () => tabs.sessionTabs.map((tab) => tab.id),
  (ids) => {
    const alive = new Set(ids)
    for (const sid of [...enteredSessions]) {
      if (!alive.has(sid)) enteredSessions.delete(sid)
    }
  }
)

function openQuickCmds(sid: string) {
  quickCmdsSid.value = sid
  quickCmdsVisible.value = true
}

/**
 * 重新对齐「后端开着的会话」与本地标签（不激活任何会话）。
 *
 * 触发点：应用回到前台（`visibilitychange` / `focus`）与切回首页。移动端把网页当 PWA
 * 常驻时尤其需要它——页面没有重新加载，不主动拉一次就永远停在「打开前端时」的旧快照上。
 * 单飞：`syncing` 挡住同一时刻的重复请求；隐藏时不同步（省一次请求）。
 */
let syncing = false
async function syncOpenSessions() {
  if (syncing || document.visibilityState === 'hidden') return
  syncing = true
  try {
    await home.restore(true)
  } catch {
    // 同步失败保持现有标签与状态：下一次回到前台/首页会再试，不打断用户当前操作
  } finally {
    syncing = false
  }
}

function onResume() {
  if (document.visibilityState === 'visible') void syncOpenSessions()
}

onMounted(async () => {
  document.title = t('app_name')
  try {
    await home.loadInfo()
  } catch {
    // /api/info 失败不阻塞启动：没有 bootId 只是少了「后端重启」的探测能力
  }
  let sids: string[] = []
  try {
    // restore 内部会先 load()；这里**不**激活任何会话标签，启动后停在首页
    sids = await home.restore()
  } catch {
    // restore 内部已把失败记进 home.error，首页会显示原因与重试
  }
  restoring.value = false
  if (sids.length) toast.show(t('app_restore_done', { n: sids.length }), 'success')
  // 监听放在恢复之后：启动阶段的 /api/sessions 已经拉过一次，不需要再被 focus 触发一遍
  document.addEventListener('visibilitychange', onResume)
  window.addEventListener('focus', onResume)
})

onBeforeUnmount(() => {
  document.removeEventListener('visibilitychange', onResume)
  window.removeEventListener('focus', onResume)
})
</script>

<template>
  <div class="app-shell flex flex-col overflow-hidden bg-bg text-ink dark:bg-bg-dark dark:text-ink-dark">
    <!-- 顶栏：class="app-header" 是 useViewportHeight 测量 --header-h 的锚点，
         弹窗定位带（.region-center）靠它避开顶栏，别改名。 -->
    <header class="app-header shrink-0 border-b border-line bg-surface dark:border-line-dark dark:bg-surface-dark">
      <AppTabBar />
    </header>

    <main class="relative min-h-0 flex-1">
      <!-- 首页常驻挂载；会话视图只在进入过后挂载（`liveSessionTabs`），
           之后一直保持挂载：切标签不重建终端与文件浏览状态。
           入口按钮在各视图自己的工具条上（13.1/13.2），弹窗实例由壳层持有。 -->
      <HomeView
        v-show="tabs.activeId === HOME_TAB_ID"
        class="absolute inset-0"
        @settings="settingsVisible = true"
      />
      <SessionView
        v-for="tab in liveSessionTabs"
        v-show="tabs.activeId === tab.id"
        :key="tab.id"
        :sid="tab.id"
        class="absolute inset-0"
        @quick-cmds="openQuickCmds(tab.id)"
      />

      <div
        v-if="restoring"
        class="pointer-events-none absolute inset-0 z-20 flex items-center justify-center bg-bg/70 text-sm text-ink-soft backdrop-blur-sm dark:bg-bg-dark/70 dark:text-ink-soft-dark"
      >
        {{ t('app_restoring') }}
      </div>
    </main>

    <Toast />
    <SettingsDialog v-model:visible="settingsVisible" />
    <QuickCmdsDialog v-model:visible="quickCmdsVisible" :sid="quickCmdsSid" />
  </div>
</template>
