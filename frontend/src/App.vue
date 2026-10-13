<script setup lang="ts">
// App.vue — 应用外壳：顶栏（应用标签）+ 首页 / 各会话视图（全部保持挂载）+ Toast。
//
// 启动顺序（契约 6.3，不能调换）：
//   1. useTheme() + useViewportHeight()：先把主题与可视视口变量准备好（壳层高度依赖后者，
//      晚一步会看到高度跳一下）；
//   2. home.loadInfo()：拿后端进程实例 bootId（判定「后端重启过」的基准）；
//   3. home.load()：会话配置 + 已打开会话的实时状态；
//   4. home.restore()：把后端仍在运行的会话恢复成应用标签（返回 sid 列表）；
//   5. 逐个会话接线终端标签：terminals.ensure(state) 恢复已有的终端，一个都没有时自动建
//      一个（桌面与窄屏一致）——「打开了会话就能直接打字」。
// 恢复数量 > 0 时给一条 toast：用户刷新后需要知道「刚才那些会话回来了」。
//
// 弹窗归属（契约 13.1/13.2）：顶栏不再有快捷指令与设置入口——快捷指令按钮进会话工具条
// （它的作用对象是「当前激活终端」，属于会话内部的语境），设置按钮进首页工具条。两个弹窗实例
// 仍然由壳层持有：设置本来就是应用级的（主题/语言/字体），快捷指令只需要一个实例 + 一个 sid
// 上下文，没必要在每个会话视图里各挂一份。
import { onMounted, ref } from 'vue'
import { t } from '@/i18n'
import { useTheme } from '@/composables/useTheme'
import { useViewportHeight } from '@/composables/useViewportHeight'
import { HOME_TAB_ID, useAppTabsStore } from '@/stores/appTabs'
import { useHomeStore } from '@/stores/home'
import { useTerminalsStore } from '@/stores/terminals'
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
const terminals = useTerminalsStore()
const toast = useToastStore()

const settingsVisible = ref(false)
const quickCmdsVisible = ref(false)
/** 快捷指令弹窗的会话上下文：由「打开它的那个会话视图」给（工具条按钮就在那儿）。 */
const quickCmdsSid = ref<string | null>(null)
/** 启动恢复期间显示提示条（终端的恢复在面板里自己等 `\x1b]ready\x07`）。 */
const restoring = ref(true)

function openQuickCmds(sid: string) {
  quickCmdsSid.value = sid
  quickCmdsVisible.value = true
}

onMounted(async () => {
  document.title = t('app_name')
  try {
    await home.loadInfo()
  } catch {
    // /api/info 失败不阻塞启动：没有 bootId 只是少了「后端重启」的探测能力
  }
  await home.load()
  let sids: string[] = []
  try {
    sids = await home.restore()
  } catch {
    // restore 内部已把失败记进 home.error，首页会显示原因与重试
  }
  for (const sid of sids) {
    const state = home.stateOf(sid)
    if (!state) continue
    terminals.ensure(sid, state)
    // 没有终端标签就自动建一个：恢复回来的会话必须能直接打字（第五轮起窄屏也一样——
    // 用户原话：移动页面新打开/恢复的会话也要自动进入「终端 1」，不是文件管理子标签）
    if (terminals.countOf(sid) === 0) terminals.add(sid)
  }
  restoring.value = false
  if (sids.length) toast.show(t('app_restore_done', { n: sids.length }), 'success')
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
      <!-- 首页与每个会话视图都保持挂载：切标签不重建终端与文件浏览状态。
           入口按钮在各视图自己的工具条上（13.1/13.2），弹窗实例由壳层持有。 -->
      <HomeView
        v-show="tabs.activeId === HOME_TAB_ID"
        class="absolute inset-0"
        @settings="settingsVisible = true"
      />
      <SessionView
        v-for="tab in tabs.sessionTabs"
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
