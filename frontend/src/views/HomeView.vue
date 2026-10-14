<script setup lang="ts">
// HomeView — 首页：会话卡片网格（只平铺，不做列表/表格视图）。
//
// 打开会话的完整接线（契约 6.3）都在这里：home.openSession → appTabs.openSession →
// terminals.ensure → 一个终端都没有就自动建一个（桌面与窄屏一样，第五轮统一）。
// 顺序不能乱：标签要先建好，SessionView 才会挂载，终端面板才有地方登记 paneControls。
//
// 第六轮：卡片上的**子终端列表**由这里接线——「快捷打开」= 切应用标签 + 激活那个子终端 +
// 把键盘焦点交给它（panes.get 在 SessionView 挂载的那一帧就已注册，见 TerminalPane 的 setup）；
// 单个终端的关闭同样从这里走确认框（它会终止远端的 shell）。原来的终端列表弹窗已删除。
//
// 「不保存凭据」的会话会在这里收到 `credential_required`（契约 11.1.3）：那不是失败，
// 而是「请先输入凭据」，所以转成 CredentialPromptDialog，填完带凭据重试一次。
import { computed, nextTick, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { t } from '@/i18n'
import { ApiError, describeError, type SessionView } from '@/serverapi'
import { HOME_TAB_ID, useAppTabsStore } from '@/stores/appTabs'
import { useHomeStore, type SessionCredential } from '@/stores/home'
import { usePaneControlsStore } from '@/stores/paneControls'
import { useSessionStatsStore } from '@/stores/sessionStats'
import { useTerminalsStore } from '@/stores/terminals'
import { useToastStore } from '@/stores/toast'
import { sessionLabel } from '@/utils/format'
import SessionCard from '@/components/home/SessionCard.vue'
import SessionFormDialog from '@/components/home/SessionFormDialog.vue'
import CredentialPromptDialog from '@/components/CredentialPromptDialog.vue'
import ConfirmDialog from '@/components/ConfirmDialog.vue'

const home = useHomeStore()
const tabs = useAppTabsStore()
const terminals = useTerminalsStore()
const panes = usePaneControlsStore()
const sessionStats = useSessionStatsStore()
const toast = useToastStore()

/** 设置弹窗挂在壳层（App.vue），这里只抛事件——它是应用级设置（主题/语言/字体），不是首页的。 */
const emit = defineEmits<{
  (e: 'settings'): void
}>()

const formVisible = ref(false)
const editing = ref<SessionView | null>(null)
/** 正在打开/关闭的会话 id（按钮禁用，防连点）。 */
const busyId = ref('')

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

onMounted(() => {
  if (!home.loaded) void home.load()
})

// ---------- 卡片上的系统信息（第十三轮，契约 23） ----------

/**
 * 已打开会话的 id（只有打开着的才有连接可问，后端对没打开的会话回 `not_open`）。
 *
 * `list` 依赖 lastUsedAt 排序，而打开会话会写 lastUsedAt，所以顺序变化也会让这个 computed
 * 重算——`sync` 只把「集合变了」当成需要立刻重拉，重复一次请求是无害的。
 */
const openIds = computed(() =>
  list.value.filter((s) => home.stateOf(s.id)?.open === true).map((s) => s.id)
)

/**
 * 把「该问哪些会话 + 首页在不在前台」交给 sessionStats store：它负责 5 秒一次的轮询与停止。
 *
 * 首页是 v-show 隐藏的（切到终端标签后卡片仍然挂载着），所以「可见性」必须显式传进去，
 * 否则用户在看终端的时候后端会一直每 5 秒跑一条远程命令。
 */
watch(
  [openIds, () => tabs.activeId === HOME_TAB_ID],
  ([ids, visible]) => sessionStats.sync(ids, visible),
  { immediate: true }
)

// 卸载即停表：这一页当前是常驻挂载（`v-show`），但定时器不该依赖「组件永不卸载」这个前提——
// 一旦将来改成 `v-if`，没有这一句就是后台每 5 秒一条远程命令。
onBeforeUnmount(() => sessionStats.dispose())

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
  // 桌面与窄屏都要自动建一个终端（契约 11.7 / 第五轮用户要求：手机上新开会话也直接落在
  // 「终端 1」，而不是丢进文件管理子标签——新会话的第一件事就是敲命令）
  if (terminals.countOf(session.id) === 0) terminals.add(session.id)
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
    toast.show(t('home_open_failed', { error: openFailedText(e) }), 'error')
  } finally {
    busyId.value = ''
  }
}

/**
 * 打开失败的报错文案：**只有「主机密钥未信任」**这一种失败在后面追加
 * 「测试连接并信任密钥指纹后再试」（第十一轮用户要求）。
 *
 * 为什么不无脑追加：拨号不通、认证失败这些原因跟信任无关——「目标拒绝连接」后面让人去确认
 * 指纹是误导（先得连上才有指纹可确认）。`hostkey_unknown` 是唯一「就是因为没信任才失败」的
 * 错误码，指引只属于它；`hostkey_mismatch` 也不加——那种情况该走会话弹窗里的「移除信任」，
 * 文案 `test_mismatch_hint` 已经把话说了。
 */
function openFailedText(e: unknown): string {
  const text = describeError(e)
  return e instanceof ApiError && e.code === 'hostkey_unknown'
    ? text + t('home_open_hostkey_hint')
    : text
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
  toast.show(t('home_close_success', { name: sessionLabel(session) }), 'success')
  busyId.value = ''
}

// ---------- 卡片上的子终端（第六轮：内联列表 + 快捷打开） ----------

/**
 * 快捷打开某个子终端：切到该会话的应用标签 → 激活那个终端子标签 → 把键盘焦点交给它。
 *
 * 焦点为什么放在 nextTick 里：`openSession` 只是建标签/改激活项，SessionView（以及它里面的
 * TerminalPane）要到下一帧才被创建；而 paneControls 的注册发生在 TerminalPane 的 setup 里，
 * 所以 nextTick 之后 `get()` 一定拿得到。拿不到也不报错——终端本身已经切过去了，
 * 用户点一下就能打字，这里只是顺手把焦点送进去。
 */
function onOpenTerminal(session: SessionView, uid: string) {
  tabs.openSession(session.id, sessionLabel(session))
  terminals.setActive(session.id, uid)
  void nextTick(() => panes.get(session.id, uid)?.focus())
}

/** 关闭某个子终端：先确认（会终止远端的 shell），见 pending 的 kind: 'term'。 */
function onCloseTerminal(session: SessionView, uid: string) {
  pending.value = { kind: 'term', session, uid }
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

const pending = ref<{ kind: 'close' | 'delete' | 'term'; session: SessionView; uid?: string } | null>(
  null
)

const confirmTitle = computed(() => {
  const kind = pending.value?.kind
  if (kind === 'delete') return t('home_delete_confirm_title')
  if (kind === 'term') return t('termlist_close_confirm_title')
  return t('home_close_confirm_title')
})

const confirmMessage = computed(() => {
  const p = pending.value
  if (!p) return ''
  const name = sessionLabel(p.session)
  if (p.kind === 'delete') return t('home_delete_confirm_msg', { name })
  if (p.kind === 'term') {
    const tab = terminals.tabsOf(p.session.id).find((item) => item.uid === p.uid)
    return t('termlist_close_confirm_msg', {
      title: tab ? terminals.titleOf(p.session.id, tab) : ''
    })
  }
  return t('home_close_confirm_msg', { name })
})

function onConfirmPending() {
  const p = pending.value
  pending.value = null
  if (!p) return
  if (p.kind === 'close') void onClose(p.session)
  else if (p.kind === 'term') {
    // closeTab 内部会发 /api/terminals/close：远程那个 shell 就此终止（所以上面先确认）
    if (p.uid) terminals.closeTab(p.session.id, p.uid)
  } else void onDelete(p.session)
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
    <!-- 工具条固定在顶部：会话多的时候它跟着滚出去就没法用了。
         第六轮（用户要求）：删掉「刷新」（会话列表本来每次进首页都会重读，留一个按钮只是噪音）、
         「添加会话」不填充颜色但补上描边（品牌色的图标+文字）、「设置」也是描边、字号统一 14px，
         整栏高度跟着降下来（py-2 + 按钮 h-9）。 -->
    <div class="shrink-0 border-b border-line px-3 py-2 dark:border-line-dark sm:px-4">
      <div class="flex items-center gap-2 sm:gap-3">
        <button class="g-btn-secondary !h-9 !px-3 text-sm !text-brand" @click="onAdd">
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

        <span class="flex-1"></span>

        <button
          class="g-btn-secondary !h-9 !px-3 text-sm"
          :title="t('settings_title')"
          @click="emit('settings')"
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
          :tabs="terminals.tabsOf(session.id)"
          :busy="busyId === session.id"
          @open="onOpen(session)"
          @close="pending = { kind: 'close', session }"
          @edit="onEdit(session)"
          @remove="pending = { kind: 'delete', session }"
          @open-terminal="(uid) => onOpenTerminal(session, uid)"
          @close-terminal="(uid) => onCloseTerminal(session, uid)"
        />
      </div>
    </div>

    <SessionFormDialog v-model:visible="formVisible" :session="editing" @saved="onSaved" />

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
      :danger="true"
      :confirm-text="pending?.kind === 'delete' ? t('common_delete') : t('home_close')"
      :cancel-text="t('common_cancel')"
      @confirm="onConfirmPending"
      @cancel="pending = null"
      @update:visible="(v) => { if (!v) pending = null }"
    />
  </div>
</template>
