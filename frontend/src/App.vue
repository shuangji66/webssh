<script setup lang="ts">
// App.vue — 应用外壳：顶栏（应用标签）+ 首页 / 各会话视图（全部保持挂载）+ 全局弹窗 + Toast。
//
// 启动顺序（契约 6.3，不能调换）：
//   1. useTheme() + useViewportHeight()：先把主题与可视视口变量准备好（壳层高度依赖后者，
//      晚一步会看到高度跳一下）；
//   2. home.loadInfo()：拿后端进程实例 bootId（判定「后端重启过」的基准）；
//   3. home.load()：会话配置 + 已打开会话的实时状态；
//   4. home.restore()：把后端仍在运行的会话恢复成应用标签（返回 sid 列表）；
//   5. 逐个会话接线终端标签：terminals.ensure(state) 恢复已有的终端，桌面端一个都没有时
//      自动建一个——「打开了会话就能直接打字」。
// 恢复数量 > 0 时给一条 toast：用户刷新后需要知道「刚才那些会话回来了」。
import { onMounted, ref } from 'vue'
import { t } from '@/i18n'
import { useMobileLayout } from '@/composables/useMobileLayout'
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
const isMobile = useMobileLayout()

const settingsVisible = ref(false)
const quickCmdsVisible = ref(false)
/** 快捷指令弹窗的会话上下文：首页打开时为空（弹窗只显示全局分组）。 */
const quickCmdsSid = ref<string | null>(null)
/** 启动恢复期间显示提示条（终端的恢复在面板里自己等 `\x1b]ready\x07`）。 */
const restoring = ref(true)

function openQuickCmds(sid: string | null) {
  quickCmdsSid.value = sid
  quickCmdsVisible.value = true
}

function onTabBarQuickCmds() {
  openQuickCmds(tabs.activeId === HOME_TAB_ID ? null : tabs.activeId)
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
    // 桌面端没有终端标签就自动建一个（移动端由用户点 + 新建，避免一进会话就占满屏幕）
    if (!isMobile.value && terminals.countOf(sid) === 0) terminals.add(sid)
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
      <AppTabBar @settings="settingsVisible = true" @quick-cmds="onTabBarQuickCmds" />
    </header>

    <main class="relative min-h-0 flex-1">
      <!-- 首页与每个会话视图都保持挂载：切标签不重建终端与文件浏览状态 -->
      <HomeView v-show="tabs.activeId === HOME_TAB_ID" class="absolute inset-0" />
      <SessionView
        v-for="tab in tabs.sessionTabs"
        v-show="tabs.activeId === tab.id"
        :key="tab.id"
        :sid="tab.id"
        class="absolute inset-0"
        @settings="settingsVisible = true"
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
