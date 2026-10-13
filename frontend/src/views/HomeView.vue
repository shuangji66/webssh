<script setup lang="ts">
// HomeView — 首页：会话卡片网格（只平铺，不做列表/表格视图）。
//
// 打开会话的完整接线（契约 6.3）都在这里：home.openSession → appTabs.openSession →
// terminals.ensure → 桌面端没有终端标签就自动建一个。顺序不能乱：标签要先建好，
// SessionView 才会挂载，终端面板才有地方登记 paneControls。
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import { t } from '@/i18n'
import { describeError, type SessionView } from '@/serverapi'
import { useMobileLayout } from '@/composables/useMobileLayout'
import { useAppTabsStore } from '@/stores/appTabs'
import { useHomeStore } from '@/stores/home'
import { useTerminalsStore } from '@/stores/terminals'
import { useToastStore } from '@/stores/toast'
import { sessionLabel } from '@/utils/format'
import SessionCard from '@/components/home/SessionCard.vue'
import SessionFormDialog from '@/components/home/SessionFormDialog.vue'
import ConfirmDialog from '@/components/ConfirmDialog.vue'

const home = useHomeStore()
const tabs = useAppTabsStore()
const terminals = useTerminalsStore()
const toast = useToastStore()
const isMobile = useMobileLayout()

const keyword = ref('')
const formVisible = ref(false)
const editing = ref<SessionView | null>(null)
/** 正在打开/关闭的会话 id（按钮禁用，防连点）。 */
const busyId = ref('')

// ---------- 卡片列表 ----------

const list = computed(() => {
  const kw = keyword.value.trim().toLowerCase()
  const items = [...home.sessions]
  // 最近用过的排前面：会话通常比一屏多，常用的应该在第一屏（lastUsedAt 为空时退到 updatedAt）
  items.sort((a, b) => {
    const at = a.lastUsedAt || a.updatedAt || a.createdAt
    const bt = b.lastUsedAt || b.updatedAt || b.createdAt
    return bt.localeCompare(at)
  })
  if (!kw) return items
  return items.filter((s) => {
    return (
      sessionLabel(s).toLowerCase().includes(kw) ||
      s.host.toLowerCase().includes(kw) ||
      s.user.toLowerCase().includes(kw) ||
      String(s.port).includes(kw) ||
      (s.note ?? '').toLowerCase().includes(kw)
    )
  })
})

const openCount = computed(() => home.sessions.filter((s) => home.isOpen(s.id)).length)

// 相对时间（「打开于 X 分钟前」）要自己走字：没有定时器的话，只有别的状态变化才会重算，
// 放在那儿的会话会一直显示「刚刚」。30s 粒度足够（最小单位是分钟）。
const now = ref(new Date())
let tick = 0

onMounted(() => {
  if (!home.loaded) void home.load()
  tick = window.setInterval(() => {
    now.value = new Date()
  }, 30_000)
})

onBeforeUnmount(() => {
  if (tick) window.clearInterval(tick)
})

// ---------- 打开 / 关闭 ----------

async function onOpen(session: SessionView) {
  if (busyId.value) return
  busyId.value = session.id
  try {
    const state = await home.openSession(session.id)
    tabs.openSession(session.id, sessionLabel(session))
    // ensure 恢复后端已有的终端（并把激活项落到第一个），add 新建即激活 —— 两者都会写
    // sessionUi，这里不需要（也不应该）再去改激活项，否则会和 store 的选择打架。
    terminals.ensure(session.id, state)
    if (!isMobile.value && terminals.countOf(session.id) === 0) terminals.add(session.id)
    toast.show(t('home_open_success', { name: sessionLabel(session) }), 'success')
  } catch (e) {
    toast.show(t('home_open_failed', { error: describeError(e) }), 'error')
  } finally {
    busyId.value = ''
  }
}

/** 关闭会话：终止后端会话（终端 + SFTP）并清掉前端状态，但保留配置。 */
async function onClose(session: SessionView) {
  if (busyId.value) return
  busyId.value = session.id
  try {
    await home.closeSession(session.id)
  } catch (e) {
    toast.show(t('home_close_failed', { error: describeError(e) }), 'error')
    busyId.value = ''
    return
  }
  home.forgetSession(session.id)
  toast.show(t('home_close_success', { name: sessionLabel(session) }), 'success')
  busyId.value = ''
}

// ---------- 编辑 / 删除 ----------

function onAdd() {
  editing.value = null
  formVisible.value = true
}

function onEdit(session: SessionView) {
  editing.value = session
  formVisible.value = true
}

function onSaved(session: SessionView) {
  // 表单弹窗自己已 toast；这里只把标签标题跟一下（用户手动改过名的标签保持不动）。
  // 用 syncTitle 而不是 openSession：后者会把激活项切到那个会话，用户会被从首页拽走。
  tabs.syncTitle(session.id, sessionLabel(session))
}

const pending = ref<{ kind: 'close' | 'delete'; session: SessionView } | null>(null)

const confirmTitle = computed(() =>
  pending.value?.kind === 'delete' ? t('home_delete_confirm_title') : t('home_close_confirm_title')
)
const confirmMessage = computed(() => {
  const p = pending.value
  if (!p) return ''
  const name = sessionLabel(p.session)
  return p.kind === 'delete'
    ? t('home_delete_confirm_msg', { name })
    : t('home_close_confirm_msg', { name })
})

function onConfirmPending() {
  const p = pending.value
  pending.value = null
  if (!p) return
  if (p.kind === 'close') void onClose(p.session)
  else void onDelete(p.session)
}

async function onDelete(session: SessionView) {
  if (busyId.value) return
  busyId.value = session.id
  const name = sessionLabel(session)
  try {
    // 会话还开着就先关掉：后端要连配置带历史一起删，留着运行中的会话再删文件容易半途出错
    if (home.isOpen(session.id)) {
      try {
        await home.closeSession(session.id)
      } catch {
        // 关不掉不阻塞删除：删除本身如果失败会在下面报出来
      }
      home.forgetSession(session.id)
    }
    await home.remove(session.id)
    toast.show(t('home_delete_success', { name }), 'success')
  } catch (e) {
    toast.show(t('home_delete_failed', { error: describeError(e) }), 'error')
  } finally {
    busyId.value = ''
  }
}
</script>

<template>
  <div class="flex h-full min-h-0 flex-col bg-bg dark:bg-bg-dark">
    <!-- 工具条固定在顶部：会话多的时候它跟着滚出去就没法用了 -->
    <div class="shrink-0 border-b border-line px-3 py-3 dark:border-line-dark sm:px-4">
      <div class="flex flex-wrap items-center gap-2 sm:gap-3">
        <h1 class="font-display text-lg font-semibold text-ink dark:text-ink-dark">
          {{ t('home_title') }}
        </h1>
        <span v-if="home.sessions.length" class="g-chip">{{ t('home_total', { n: home.sessions.length }) }}</span>
        <span v-if="openCount" class="g-chip g-chip-active">{{ t('home_open_count', { n: openCount }) }}</span>

        <span class="flex-1"></span>

        <div class="relative">
          <svg
            viewBox="0 0 24 24"
            fill="none"
            stroke="currentColor"
            stroke-width="1.5"
            stroke-linecap="round"
            stroke-linejoin="round"
            class="pointer-events-none absolute left-3 top-1/2 h-4 w-4 -translate-y-1/2 text-ink-faint dark:text-ink-faint-dark"
          >
            <circle cx="11" cy="11" r="8" />
            <line x1="21" y1="21" x2="16.65" y2="16.65" />
          </svg>
          <input
            v-model="keyword"
            class="g-input !w-40 !pl-9 sm:!w-56"
            :placeholder="t('home_search')"
            type="search"
          />
        </div>

        <button
          class="g-btn-secondary"
          :disabled="home.loading"
          :title="t('home_refresh')"
          @click="home.load(true)"
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
          <span class="hidden sm:inline">{{ t('common_refresh') }}</span>
        </button>

        <button class="g-btn-primary" @click="onAdd">
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
          {{ t('home_new') }}
        </button>
      </div>
      <p class="mt-2 hidden text-xs text-ink-faint dark:text-ink-faint-dark sm:block">
        {{ t('home_subtitle') }}
      </p>
    </div>

    <div class="thin-scroll min-h-0 flex-1 overflow-y-auto p-3 sm:p-4">
      <!-- 加载失败要显示原因 + 重试：失败也走空态的话，用户会以为自己的会话被删光了 -->
      <div v-if="home.error" class="g-panel p-8 text-center">
        <p class="break-all text-sm text-danger">{{ t('home_load_failed', { error: home.error }) }}</p>
        <button class="g-btn-secondary mt-4" @click="home.load(true)">{{ t('common_retry') }}</button>
      </div>

      <div
        v-else-if="home.loading && !home.sessions.length"
        class="g-panel p-8 text-center text-sm text-ink-soft dark:text-ink-soft-dark"
      >
        {{ t('common_loading') }}
      </div>

      <div v-else-if="!home.sessions.length" class="g-panel px-6 py-16 text-center">
        <h2 class="font-display text-base font-semibold text-ink dark:text-ink-dark">
          {{ t('home_empty_title') }}
        </h2>
        <p class="mx-auto mt-2 max-w-md text-sm text-ink-soft dark:text-ink-soft-dark">
          {{ t('home_empty_desc') }}
        </p>
        <button class="g-btn-primary mt-5" @click="onAdd">{{ t('home_new') }}</button>
      </div>

      <div
        v-else-if="!list.length"
        class="g-panel p-8 text-center text-sm text-ink-soft dark:text-ink-soft-dark"
      >
        {{ t('home_search_empty') }}
      </div>

      <!-- 只平铺：卡片按容器宽度自动分列，卡片做大（minmax(min(22rem,100%),1fr)）——
           用 min(22rem,100%) 而不是裸的 22rem，否则窄屏（<~380px）上单列比容器还宽，会横向溢出。 -->
      <div class="grid grid-cols-[repeat(auto-fill,minmax(min(22rem,100%),1fr))] gap-4">
        <SessionCard
          v-for="session in list"
          :key="session.id"
          :session="session"
          :state="home.stateOf(session.id)"
          :terminal-count="terminals.countOf(session.id)"
          :now="now"
          :busy="busyId === session.id"
          @open="onOpen(session)"
          @close="pending = { kind: 'close', session }"
          @edit="onEdit(session)"
          @remove="pending = { kind: 'delete', session }"
        />
      </div>
    </div>

    <SessionFormDialog v-model:visible="formVisible" :session="editing" @saved="onSaved" />

    <ConfirmDialog
      :visible="pending !== null"
      :title="confirmTitle"
      :message="confirmMessage"
      :danger="pending?.kind === 'delete'"
      :confirm-text="pending?.kind === 'delete' ? t('common_delete') : t('home_close')"
      :cancel-text="t('common_cancel')"
      @confirm="onConfirmPending"
      @cancel="pending = null"
      @update:visible="(v) => { if (!v) pending = null }"
    />
  </div>
</template>
