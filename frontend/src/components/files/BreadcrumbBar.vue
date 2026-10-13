<script setup lang="ts">
// BreadcrumbBar — 左栏顶部的**按钮条**（不显示路径，见契约 11.3 / 13.1）。
//
// 为什么去掉路径：左栏宽度只有 11~20rem，一串「根目录 / vol1 / 1000 / docker / …」在这里
// 只能靠横向滚动看，而它想回答的「我在哪」在文件管理器的面包屑里已经回答过了（那里有整行
// 宽度）。左栏这一行的价值在于**动作**：
//   [家目录] [在此目录新建终端] [刷新]
//
// 第三轮删掉了这一行里的「完整文件管理」按钮：子标签栏恢复了固定的「文件管理」子标签，
// 它与左栏是**二选一**（切到文件管理子标签时左栏收起），所以左栏里再放一个切换入口就是
// 两套开关——而且 `sessionUi` 里那套 mainMode 已经删掉，判据只该有 activeSub 一个。
//
// 本组件不 emit 任何事件：动作全部直接落到 store（sftp / terminals）上。
import { computed, nextTick } from 'vue'
import { t } from '@/i18n'
import { usePaneControlsStore } from '@/stores/paneControls'
import { useSftpStore } from '@/stores/sftp'
import { useTerminalsStore } from '@/stores/terminals'

const { sid } = defineProps<{ sid: string }>()

const sftp = useSftpStore()
const terminals = useTerminalsStore()
const panes = usePaneControlsStore()

const state = computed(() => sftp.stateOf(sid))

/**
 * 家目录按钮。
 *
 * 做两件事，缺一个都不完整：
 *   1) 把当前目录切回家目录（这是按钮的本来意思）；
 *   2) 让树**全部折叠、只展开家目录那一条链路**（11.3）：树可能正开在 /var/log 上，
 *      光导航过去的话那一行还在屏幕外，用户看不到自己到了哪里。
 */
function goHome() {
  const home = state.value.home
  if (!home) return
  void sftp.navigate(sid, home)
  void sftp.expandPath(sid, home)
}

/** 在当前目录里新开一个终端（目录还没有（ensure 未完成）时不给点）。 */
function openTerminalHere() {
  const dir = state.value.dir || state.value.home
  if (!dir) return
  // openTerminalAt 会把新标签设成激活项（→ sessionUi.setActiveSub）：子标签栏随之切到这个
  // 终端、左栏让位给主区域，用户立刻看到自己新开的 shell。这里只需把键盘焦点也交给它。
  terminals.openTerminalAt(sid, dir)
  void nextTick(() => {
    const uid = terminals.activeOf(sid)?.uid ?? terminals.tabsOf(sid)[0]?.uid
    if (uid) panes.get(sid, uid)?.focus()
  })
}

function refresh() {
  void (state.value.dir ? sftp.refresh(sid) : sftp.ensure(sid))
}
</script>

<template>
  <div
    class="flex shrink-0 items-center gap-0.5 border-b border-line px-1 py-1 dark:border-line-dark"
  >
    <!-- 家目录：带文字，因为它是这一行里唯一的「去哪」而不是「做什么」 -->
    <button
      type="button"
      class="tree-tool !w-auto gap-1 px-1.5 text-xs"
      :disabled="!state.home"
      :title="t('files_home')"
      :aria-label="t('files_home')"
      @click="goHome"
    >
      <svg
        class="h-4 w-4 shrink-0"
        viewBox="0 0 24 24"
        fill="none"
        stroke="currentColor"
        stroke-width="1.5"
        stroke-linecap="round"
        stroke-linejoin="round"
      >
        <path d="m3 10 9-7 9 7v10a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2z" />
        <path d="M9 22V12h6v10" />
      </svg>
      <span class="truncate">{{ t('files_home') }}</span>
    </button>

    <button
      type="button"
      class="tree-tool"
      :disabled="!state.dir && !state.home"
      :title="t('session_new_terminal_here')"
      :aria-label="t('session_new_terminal_here')"
      @click="openTerminalHere"
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
        <path d="M4 17l6-5-6-5" />
        <path d="M12 19h8" />
      </svg>
    </button>

    <button
      type="button"
      class="tree-tool"
      :title="t('files_refresh')"
      :aria-label="t('files_refresh')"
      @click="refresh"
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
  </div>
</template>
