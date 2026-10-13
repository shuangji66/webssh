<script setup lang="ts">
// HomeView — 首页：会话卡片网格（只平铺，不做列表/表格视图）。
//
// 打开会话的完整接线（契约 6.3）都在这里：home.openSession → appTabs.openSession →
// terminals.ensure → 桌面端没有终端标签就自动建一个。顺序不能乱：标签要先建好，
// SessionView 才会挂载，终端面板才有地方登记 paneControls。
//
// 「不保存凭据」的会话会在这里收到 `credential_required`（契约 11.1.3）：那不是失败，
// 而是「请先输入凭据」，所以转成 CredentialPromptDialog，填完带凭据重试一次。
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import { t } from '@/i18n'
import { ApiError, describeError, type SessionView } from '@/serverapi'
import { useMobileLayout } from '@/composables/useMobileLayout'
import { useAppTabsStore } from '@/stores/appTabs'
import { useHomeStore, type SessionCredential } from '@/stores/home'
import { useTerminalsStore } from '@/stores/terminals'
import { useToastStore } from '@/stores/toast'
import { sessionLabel } from '@/utils/format'
import SessionCard from '@/components/home/SessionCard.vue'
import SessionFormDialog from '@/components/home/SessionFormDialog.vue'
import SessionTerminalsDialog from '@/components/home/SessionTerminalsDialog.vue'
import CredentialPromptDialog from '@/components/CredentialPromptDialog.vue'
import ConfirmDialog from '@/components/ConfirmDialog.vue'

const home = useHomeStore()
const tabs = useAppTabsStore()
const terminals = useTerminalsStore()
const toast = useToastStore()
const isMobile = useMobileLayout()

/** 设置弹窗挂在壳层（App.vue），这里只抛事件——它是应用级设置（主题/语言/字体），不是首页的。 */
const emit = defineEmits<{
  (e: 'settings'): void
}>()

const formVisible = ref(false)
const editing = ref<SessionView | null>(null)
/** 正在打开/关闭的会话 id（按钮禁用，防连点）。 */
const busyId = ref('')

/** 终端列表弹窗（点卡片上的「N 个终端」）。空串 = 没打开。 */
const terminalsSid = ref('')
const terminalsVisible = ref(false)

// ---------- 卡片列表 ----------

const list = computed(() => {
  const items = [...home.sessions]
  // 最近用过的排前面：会话通常比一屏多，常用的应该在第一屏（lastUsedAt 为空时退到 updatedAt）
  items.sort((a, b) => {
    const at = a.lastUsedAt || a.updatedAt || a.createdAt
    const bt = b.lastUsedAt || b.updatedAt || b.createdAt
    return bt.localeCompare(at)
  })
  return items
})

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

/**
 * 待输入凭据的会话（null = 没有弹窗）。loading 与 error 都放在这里：弹窗只是表单，
 * 「正在连接 / 上次为什么失败」属于调用方的状态（它才知道 open 的结果）。
 */
const credPrompt = ref<{ session: SessionView; loading: boolean; error: string } | null>(null)

/** 后端要求的「请先输入凭据」（不保存凭据的会话）。 */
function needsCredential(e: unknown): boolean {
  return e instanceof ApiError && e.code === 'credential_required'
}

/**
 * 真正执行打开：openSession（可带凭据）→ 建应用标签 → 恢复/新建终端。
 *
 * 抽出来是因为它要跑两次——第一次不带凭据，收到 credential_required 后带着用户填的凭据再跑
 * 一次。两次的顺序完全一样，复制一份必然有一份会先腐坏。
 */
async function openWith(session: SessionView, credential?: SessionCredential) {
  const state = await home.openSession(session.id, credential)
  tabs.openSession(session.id, sessionLabel(session))
  // ensure 恢复后端已有的终端（并把激活项落到第一个），add 新建即激活 —— 两者都会写
  // sessionUi，这里不需要（也不应该）再去改激活项，否则会和 store 的选择打架。
  terminals.ensure(session.id, state)
  if (!isMobile.value && terminals.countOf(session.id) === 0) terminals.add(session.id)
  toast.show(t('home_open_success', { name: sessionLabel(session) }), 'success')
}

async function onOpen(session: SessionView) {
  if (busyId.value) return
  busyId.value = session.id
  try {
    await openWith(session)
  } catch (e) {
    if (needsCredential(e)) {
      // 不是失败，是「这个会话没存凭据」：别报错，弹输入框，填完带凭据重试
      credPrompt.value = { session, loading: false, error: '' }
      return
    }
    toast.show(t('home_open_failed', { error: describeError(e) }), 'error')
  } finally {
    busyId.value = ''
  }
}

async function onCredentialSubmit(credential: SessionCredential) {
  const pending = credPrompt.value
  if (!pending) return
  pending.loading = true
  pending.error = ''
  try {
    await openWith(pending.session, credential)
    credPrompt.value = null
  } catch (e) {
    // 弹窗留着并把原因显示在里面：用户多半是打错了密码，让他改一个字符重试，
    // 而不是从「打开」按钮从头再来一遍。
    pending.error = describeError(e)
  } finally {
    pending.loading = false
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
  closeTerminalsDialog(session.id)
  toast.show(t('home_close_success', { name: sessionLabel(session) }), 'success')
  busyId.value = ''
}

// ---------- 终端列表弹窗 ----------

function onViewTerminals(session: SessionView) {
  terminalsSid.value = session.id
  terminalsVisible.value = true
}

/**
 * 会话被关掉 / 删掉时收起它的终端清单弹窗。
 * 不收的话弹窗会留在屏幕上显示一个空列表（终端标签已经跟着 forget 清掉了），
 * 用户只能自己点 ×——而那一刻他会以为「终端都消失了」。
 */
function closeTerminalsDialog(sid: string) {
  if (terminalsSid.value === sid) terminalsVisible.value = false
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
  // 表单弹窗自己已 toast。改完名字要同步已打开会话的顶栏标签（契约 13.3：卡片名与标签名
  // 双向同步）——syncTitle 不覆盖别的会话、也不改激活项；用 openSession 会把用户从首页拽走。
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
    closeTerminalsDialog(session.id)
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
    <!-- 工具条固定在顶部：会话多的时候它跟着滚出去就没法用了。
         一行两段（契约 13.2）：左 = 新建 + 刷新，右 = 设置（从顶栏移下来）。 -->
    <div class="shrink-0 border-b border-line px-3 py-3 dark:border-line-dark sm:px-4">
      <div class="flex items-center gap-2 sm:gap-3">
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

        <span class="flex-1"></span>

        <button class="g-btn-ghost !h-10 !px-3" :title="t('settings_title')" @click="emit('settings')">
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
          <span class="hidden sm:inline">{{ t('settings_title') }}</span>
        </button>
      </div>
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
          @terminals="onViewTerminals(session)"
        />
      </div>
    </div>

    <SessionFormDialog v-model:visible="formVisible" :session="editing" @saved="onSaved" />

    <SessionTerminalsDialog v-model:visible="terminalsVisible" :sid="terminalsSid" />

    <!-- 凭据输入：只在「不保存凭据」的会话上出现（第一次 open 回了 credential_required） -->
    <CredentialPromptDialog
      :visible="credPrompt !== null"
      :session="credPrompt?.session ?? null"
      :loading="credPrompt?.loading ?? false"
      :error="credPrompt?.error ?? ''"
      @submit="onCredentialSubmit"
      @update:visible="(v) => { if (!v) credPrompt = null }"
    />

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
