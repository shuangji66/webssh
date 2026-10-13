<script setup lang="ts">
// BreadcrumbBar — 左栏顶部的**按钮条**（第二轮起不再显示路径，见契约 11.2 / 11.3）。
//
// 为什么去掉路径：左栏宽度只有 11~20rem，一串「根目录 / vol1 / 1000 / docker / …」在这里
// 只能靠横向滚动看，而它想回答的「我在哪」在文件管理器的面包屑里已经回答过了（那里有整行
// 宽度）。左栏这一行的价值在于**动作**：
//   [家目录] [完整文件管理（切换主区域）] [在此目录新建终端] [刷新]
//
// 与文件管理器的分工：这里不 emit 任何事件，主区域的切换直接写 sessionUi.mainMode——
// 唯一的真相在 store 里，组件之间不需要再传一层事件（事件接错一处就会出现「点了没反应」）。
import { computed, nextTick } from 'vue'
import { t } from '@/i18n'
import { usePaneControlsStore } from '@/stores/paneControls'
import { useSessionUiStore } from '@/stores/sessionUi'
import { useSftpStore } from '@/stores/sftp'
import { useTerminalsStore } from '@/stores/terminals'

const { sid } = defineProps<{ sid: string }>()

const sftp = useSftpStore()
const sessionUi = useSessionUiStore()
const terminals = useTerminalsStore()
const panes = usePaneControlsStore()

const state = computed(() => sftp.stateOf(sid))
const filesActive = computed(() => sessionUi.mainModeOf(sid) === 'files')

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

/** 切换主区域：终端 ↔ 完整文件管理。切回终端时还要把焦点还给 xterm。 */
function toggleMain() {
  const mode = sessionUi.toggleMainMode(sid)
  if (mode !== 'terminal') return
  void nextTick(() => {
    const uid = terminals.activeOf(sid)?.uid ?? terminals.tabsOf(sid)[0]?.uid
    if (uid) panes.get(sid, uid)?.focus()
  })
}

/** 在当前目录里新开一个终端（目录还没有（ensure 未完成）时不给点）。 */
function openTerminalHere() {
  const dir = state.value.dir || state.value.home
  if (!dir) return
  terminals.openTerminalAt(sid, dir)
  sessionUi.setMainMode(sid, 'terminal')
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

    <!-- 完整文件管理：主区域双模式的开关，当前已经在文件管理时高亮 -->
    <button
      type="button"
      class="tree-tool"
      :class="filesActive ? '!text-brand' : ''"
      :title="filesActive ? t('session_hide_files') : t('session_full_files')"
      :aria-label="filesActive ? t('session_hide_files') : t('session_full_files')"
      :aria-pressed="filesActive ? 'true' : 'false'"
      @click="toggleMain"
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
        <path d="M3 7a2 2 0 0 1 2-2h4l2 2h8a2 2 0 0 1 2 2v8a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2z" />
        <path d="m9 13 2 2 4-4" />
      </svg>
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
