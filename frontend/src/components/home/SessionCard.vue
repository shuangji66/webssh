<script setup lang="ts">
// SessionCard — 首页的会话卡片（点右上角的电源图标即连接）。
//
// 只做展示与抛事件：打开/进入/关闭/编辑/删除的完整流程（确认框、toast、终端接线）在
// HomeView，因为那里知道「其他卡片的状态」与「表单弹窗的显隐」，卡片自己不该管这些。
//
// 第五轮：卡片上的状态灯、「未打开/已打开」、打开时间、最近使用时间全部去掉——现在唯一的
// 实时指示是终端列表本身。
// 第六轮（当前这版）：
//   - 操作按钮移到**右上角**，仅图标、无边框，与会话名同一行且不换行；名字放不下就
//     横向滚动（`overflow-x-auto whitespace-nowrap`），而不是把按钮挤下去或换行；
//     已打开时多一个「进入」（后端开着但本机还没标签时不必先关再开）；
//   - 不再标注认证方式（密码/密钥那两个徽标删掉）；
//   - 打开的子终端**直接列在卡片下**（不再藏进弹窗），点一行就是「快捷打开」；
//     每行右侧保留单个关闭按钮（它会终止远端的 shell，所以 HomeView 那边还有一次确认）。
import { computed } from 'vue'
import { t } from '@/i18n'
import type { SessionState, SessionView } from '@/serverapi'
import {
  cpuLevel,
  diskLevel,
  loadLevel,
  memLevel,
  statLevelClass,
  useSessionStatsStore
} from '@/stores/sessionStats'
import { termStatusDotClass, useTerminalsStore, type TermTab } from '@/stores/terminals'
import { formatTime, humanSize, sessionLabel, sessionTarget } from '@/utils/format'

const props = defineProps<{
  session: SessionView
  /** 后端实时状态（未打开时为空）。 */
  state?: SessionState
  /** 该会话已打开的终端子标签（直接列在卡片下）。 */
  tabs: TermTab[]
  /** 正在打开/关闭：按钮先禁用，避免连点重复请求。 */
  busy?: boolean
}>()

const emit = defineEmits<{
  (e: 'open'): void
  /** 进入一个**已经打开**的会话（后端会话仍在，本机只是还没进入）。 */
  (e: 'enter'): void
  (e: 'close'): void
  (e: 'edit'): void
  (e: 'remove'): void
  /** 快捷打开某个子终端：切到该会话并激活那个终端（HomeView 里接线）。 */
  (e: 'open-terminal', uid: string): void
  /** 关闭某个子终端（HomeView 会先确认：这会在远端杀掉那个 shell）。 */
  (e: 'close-terminal', uid: string): void
}>()

const terminals = useTerminalsStore()
const sessionStats = useSessionStatsStore()

const label = computed(() => sessionLabel(props.session))
const target = computed(() => `${props.session.user}@${sessionTarget(props.session)}`)
const isOpen = computed(() => props.state?.open === true)
/**
 * 已连接时的系统信息（undefined = 这一块不显示：没打开、或远端读不出来）。
 *
 * 判据叠加 `isOpen`，不只看 store 里有没有条目：条目可能比「打开状态」多活一拍
 * （一轮轮询恰在关会话那一刻在飞、响应迟一点回来），那时卡片已经写着「未打开」，
 * 却还挂着一份**不再更新**的旧数字。store 侧也加了写回守卫，这里再兜一层。
 */
const stats = computed(() => (isOpen.value ? sessionStats.statsOf(props.session.id) : undefined))
</script>

<template>
  <article class="g-panel group flex flex-col p-4 transition-shadow hover:shadow-card">
    <!-- 第一行：会话名（放不下就横向滚动）+ 右上角三个仅图标按钮。
         名字外面那层滚动容器必须有 min-w-0，否则 flex 子项不会收缩、滚动条永远不出现。 -->
    <div class="flex items-center gap-1.5">
      <div class="thin-scroll min-w-0 flex-1 overflow-x-auto">
        <h3 class="whitespace-nowrap text-base font-semibold text-ink dark:text-ink-dark" :title="label">
          {{ label }}
        </h3>
      </div>

      <!-- 「进入」：后端已经开着这个会话、但本机还没有标签/子终端时可点。
           没有它时卡片上只有「关闭」一个动作，用户被逼着「先关掉再打开」才能进去
           （第二十五轮用户上报：换台设备只看见已连接的卡片，没有标签和子终端列表）。 -->
      <button
        v-if="isOpen"
        class="g-btn-ghost !h-8 !w-8 shrink-0 !px-0"
        :disabled="busy"
        :title="t('home_enter')"
        :aria-label="t('home_enter')"
        @click="emit('enter')"
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
          <path d="M14 3h5a2 2 0 0 1 2 2v14a2 2 0 0 1-2 2h-5" />
          <polyline points="10 17 15 12 10 7" />
          <line x1="15" y1="12" x2="3" y2="12" />
        </svg>
      </button>

      <!-- 关闭用红色（第七轮要求）：它是「中断连接」这类破坏性动作，和「打开」共用一格时
           靠颜色区分当前是哪一个；删除按钮本来就是红的，两者并排也不冲突。 -->
      <button
        class="g-btn-ghost !h-8 !w-8 shrink-0 !px-0"
        :class="isOpen ? '!text-danger hover:!bg-danger/10' : ''"
        :disabled="busy"
        :title="isOpen ? t('home_close') : t('home_open')"
        :aria-label="isOpen ? t('home_close') : t('home_open')"
        @click="isOpen ? emit('close') : emit('open')"
      >
        <svg
          v-if="isOpen"
          viewBox="0 0 24 24"
          fill="none"
          stroke="currentColor"
          stroke-width="1.5"
          stroke-linecap="round"
          stroke-linejoin="round"
          class="h-4 w-4"
        >
          <path d="M18.36 6.64A9 9 0 1 1 5.64 6.64" />
          <line x1="12" y1="2" x2="12" y2="12" />
        </svg>
        <svg
          v-else
          viewBox="0 0 24 24"
          fill="none"
          stroke="currentColor"
          stroke-width="1.5"
          stroke-linecap="round"
          stroke-linejoin="round"
          class="h-4 w-4"
        >
          <path d="M15 3h6v6" />
          <path d="M10 14 21 3" />
          <path d="M18 13v6a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2V8a2 2 0 0 1 2-2h6" />
        </svg>
      </button>

      <button
        class="g-btn-ghost !h-8 !w-8 shrink-0 !px-0"
        :disabled="busy"
        :title="t('common_edit')"
        :aria-label="t('common_edit')"
        @click="emit('edit')"
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
          <path d="M17 3a2.828 2.828 0 1 1 4 4L7.5 20.5 2 22l1.5-5.5L17 3z" />
        </svg>
      </button>

      <button
        class="g-btn-ghost !h-8 !w-8 shrink-0 !px-0 !text-danger hover:!bg-danger/10"
        :disabled="busy"
        :title="t('common_delete')"
        :aria-label="t('common_delete')"
        @click="emit('remove')"
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
          <path d="M3 6h18" />
          <path d="M19 6l-1 14a2 2 0 0 1-2 2H8a2 2 0 0 1-2-2L5 6" />
          <path d="M10 11v6M14 11v6" />
          <path d="M9 6V4a1 1 0 0 1 1-1h4a1 1 0 0 1 1 1v2" />
        </svg>
      </button>
    </div>

    <p class="mt-0.5 truncate font-mono text-xs text-ink-soft dark:text-ink-soft-dark" :title="target">
      {{ target }}
    </p>

    <p v-if="session.note" class="mt-2 line-clamp-2 text-sm text-ink-soft dark:text-ink-soft-dark">
      {{ session.note }}
    </p>

    <!-- 子终端列表**之上**的系统信息（第十三轮，契约 23）：已连接时才有，两行纯数字
         （CPU + 负载 / 内存 + 磁盘），5 秒刷新一次（见 stores/sessionStats.ts）。
         **数值**按阈值染成绿/黄/红（阈值与理由在 store 的 STATS_THRESHOLDS，标签保持中性色）；
         整块带一条 title 说明口径与分档，免得颜色变成看不懂的暗号。
         读不到就整块不显示——远端不是 Linux、连接断了这些都不该在卡片上堆错误文案。 -->
    <div
      v-if="stats"
      class="mt-2 flex flex-col gap-0.5 font-mono text-xs text-ink-soft dark:text-ink-soft-dark"
      :title="t('home_stat_hint')"
    >
      <div class="flex flex-wrap gap-x-4">
        <span v-if="stats.cpuPercent !== undefined">
          {{ t('home_stat_cpu') }}
          <span :class="statLevelClass(cpuLevel(stats.cpuPercent))">
            {{ stats.cpuPercent.toFixed(1) }}%
          </span>
        </span>
        <span v-if="stats.load?.length">
          {{ t('home_stat_load') }}
          <span :class="statLevelClass(loadLevel(stats.load[0] ?? 0, stats.cpuCount))">
            {{ stats.load.map((v) => v.toFixed(2)).join(' ') }}
          </span>
        </span>
      </div>
      <div class="flex flex-wrap gap-x-4">
        <span v-if="stats.memTotal">
          {{ t('home_stat_mem') }}
          <span :class="statLevelClass(memLevel(stats.memUsed ?? 0, stats.memTotal))">
            {{ humanSize(stats.memUsed ?? 0) }} / {{ humanSize(stats.memTotal) }}
          </span>
        </span>
        <span v-if="stats.diskPercent !== undefined">
          {{ t('home_stat_disk') }}
          <span :class="statLevelClass(diskLevel(stats.diskPercent))">
            {{ stats.diskPercent.toFixed(1) }}%
          </span>
        </span>
      </div>
    </div>

    <!-- 凭据未保存的提醒（不是敏感信息，是「下次连要重新输入」这个事实） -->
    <span
      v-if="!session.credentialSaved"
      class="g-chip mt-2 self-start"
      :title="t('home_cred_unsaved_hint')"
    >
      <svg
        viewBox="0 0 24 24"
        fill="none"
        stroke="currentColor"
        stroke-width="1.5"
        stroke-linecap="round"
        stroke-linejoin="round"
        class="h-3.5 w-3.5 text-warning"
      >
        <path d="M10.29 3.86 1.82 18a2 2 0 0 0 1.71 3h16.94a2 2 0 0 0 1.71-3L13.71 3.86a2 2 0 0 0-3.42 0z" />
        <path d="M12 9v4" />
        <path d="M12 17h.01" />
      </svg>
      {{ t('home_cred_unsaved') }}
    </span>

    <!-- 打开的子终端：直接列在卡片下（第六轮：不再藏进弹窗）。
         整行是「快捷打开」（切到该会话 + 激活这个终端 + 把焦点交给它），右侧的 × 单独关掉它。 -->
    <ul v-if="tabs.length" class="mt-3 space-y-1">
      <li
        v-for="tab in tabs"
        :key="tab.uid"
        class="flex items-center gap-1 rounded-md border border-line/70 px-1.5 py-1 dark:border-line-dark/70"
      >
        <button
          class="flex min-w-0 flex-1 items-center gap-2 rounded px-0.5 py-0.5 text-left hover:bg-black/5 dark:hover:bg-white/5"
          :title="t('home_term_open')"
          @click="emit('open-terminal', tab.uid)"
        >
          <span class="h-1.5 w-1.5 shrink-0 rounded-full" :class="termStatusDotClass(tab)"></span>
          <span class="min-w-0 flex-1 truncate text-sm text-ink dark:text-ink-dark">
            {{ terminals.titleOf(session.id, tab) }}
          </span>
          <span class="shrink-0 text-[11px] text-ink-faint dark:text-ink-faint-dark">
            {{ formatTime(tab.createdAt) }}
          </span>
        </button>
        <!-- 关闭这个终端：鼠标悬停才变红，触屏常驻红（.close-danger，见 style.css） -->
        <button
          class="close-danger flex h-6 w-6 shrink-0 items-center justify-center"
          :title="t('home_term_close')"
          :aria-label="t('home_term_close')"
          @click.stop="emit('close-terminal', tab.uid)"
        >
          <svg
            viewBox="0 0 24 24"
            fill="none"
            stroke="currentColor"
            stroke-width="2"
            stroke-linecap="round"
            class="h-3.5 w-3.5"
          >
            <line x1="18" y1="6" x2="6" y2="18" />
            <line x1="6" y1="6" x2="18" y2="18" />
          </svg>
        </button>
      </li>
    </ul>
  </article>
</template>
