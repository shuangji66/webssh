<script setup lang="ts">
// SessionCard — 首页的会话卡片（大号卡片，点「打开」即连接）。
//
// 只做展示与抛事件：打开/关闭/编辑/删除的完整流程（确认框、toast、终端接线）在 HomeView，
// 因为那里知道「其他卡片的状态」与「表单弹窗的显隐」，卡片自己不该管这些。
import { computed } from 'vue'
import { t } from '@/i18n'
import type { SessionState, SessionView } from '@/serverapi'
import { relativeTime, sessionLabel, sessionTarget } from '@/utils/format'

const props = defineProps<{
  session: SessionView
  /** 后端实时状态（未打开时为空）。 */
  state?: SessionState
  /** 该会话下的终端子标签数量。 */
  terminalCount: number
  /** 首页共享的「当前时间」，用于让「打开于 / 最近使用」这两个相对时间自己走字。 */
  now: Date
  /** 正在打开/关闭：按钮先禁用，避免连点重复请求。 */
  busy?: boolean
}>()

const emit = defineEmits<{
  (e: 'open'): void
  (e: 'close'): void
  (e: 'edit'): void
  (e: 'remove'): void
}>()

const label = computed(() => sessionLabel(props.session))
const target = computed(() => `${props.session.user}@${sessionTarget(props.session)}`)
const openedAt = computed(() => props.state?.openedAt ?? '')
const isOpen = computed(() => props.state?.open === true)
</script>

<template>
  <article class="g-panel group flex flex-col p-5 transition-shadow hover:shadow-card">
    <div class="flex items-start gap-3">
      <div class="min-w-0 flex-1">
        <h3 class="truncate text-lg font-semibold text-ink dark:text-ink-dark" :title="label">
          {{ label }}
        </h3>
        <p class="mt-1 truncate font-mono text-sm text-ink-soft dark:text-ink-soft-dark" :title="target">
          {{ target }}
        </p>
      </div>
      <!-- 认证方式徽标：密码 / 密钥 -->
      <span class="g-chip shrink-0">
        <svg
          viewBox="0 0 24 24"
          fill="none"
          stroke="currentColor"
          stroke-width="1.5"
          stroke-linecap="round"
          stroke-linejoin="round"
          class="h-3.5 w-3.5"
        >
          <template v-if="session.authType === 'password'">
            <rect x="3" y="11" width="18" height="10" rx="2" />
            <path d="M7 11V7a5 5 0 0 1 10 0v4" />
          </template>
          <template v-else>
            <circle cx="7.5" cy="15.5" r="4.5" />
            <path d="m10.5 12.5 8-8" />
            <path d="m15 5 3 3" />
          </template>
        </svg>
        {{ session.authType === 'password' ? t('home_auth_password') : t('home_auth_key') }}
      </span>
    </div>

    <p v-if="session.note" class="mt-3 line-clamp-2 text-sm text-ink-soft dark:text-ink-soft-dark">
      {{ session.note }}
    </p>

    <!-- 打开状态：未打开 / 已打开 + 终端子标签数 + 打开于多久之前 -->
    <div class="mt-4 flex flex-wrap items-center gap-x-3 gap-y-1.5 text-xs">
      <span class="inline-flex items-center gap-1.5 font-medium text-ink dark:text-ink-dark">
        <span
          class="h-2 w-2 shrink-0 rounded-full"
          :class="isOpen ? 'bg-success' : 'bg-ink-faint dark:bg-ink-faint-dark'"
        ></span>
        {{ isOpen ? t('home_opened') : t('home_closed') }}
      </span>
      <span v-if="isOpen" class="g-chip">{{ t('home_terminal_count', { n: terminalCount }) }}</span>
      <span v-if="isOpen && openedAt" class="text-ink-faint dark:text-ink-faint-dark">
        {{ t('home_opened_at', { time: relativeTime(openedAt, now) }) }}
      </span>
    </div>

    <p class="mt-2 text-xs text-ink-faint dark:text-ink-faint-dark">
      {{
        session.lastUsedAt
          ? t('home_last_used', { time: relativeTime(session.lastUsedAt, now) })
          : t('home_never_used')
      }}
    </p>

    <!-- 操作：打开（或关闭，显眼）/ 编辑 / 删除 -->
    <div class="mt-4 flex items-center gap-2">
      <button
        v-if="!isOpen"
        class="g-btn-primary flex-1"
        :disabled="busy"
        @click="emit('open')"
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
          <path d="M15 3h6v6" />
          <path d="M10 14 21 3" />
          <path d="M18 13v6a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2V8a2 2 0 0 1 2-2h6" />
        </svg>
        {{ t('home_open') }}
      </button>
      <button
        v-else
        class="g-btn-danger flex-1"
        :disabled="busy"
        @click="emit('close')"
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
          <path d="M18.36 6.64A9 9 0 1 1 5.64 6.64" />
          <line x1="12" y1="2" x2="12" y2="12" />
        </svg>
        {{ t('home_close') }}
      </button>
      <button class="g-btn-secondary" :disabled="busy" :title="t('common_edit')" @click="emit('edit')">
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
        class="g-btn-ghost !h-10 !px-3 !text-danger hover:!bg-danger/10"
        :disabled="busy"
        :title="t('common_delete')"
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
  </article>
</template>
