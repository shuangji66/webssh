<script setup lang="ts">
// SessionTerminalsDialog — 首页卡片上「N 个终端」点开的弹窗：列出该会话当前打开的每个终端。
//
// 为什么不复用「+」和子标签栏：卡片上的数字是**已经开着的**终端，用户在首页看不到它们，
// 更没法给它们改名或单独关掉——那要先切进会话、再在很窄的子标签上双击。这个弹窗把这几个
// 动作放回首页。
//
// 两条容易写错的地方：
//  1. **会话名与会话标签名双向同步**（契约 11.1.4）：这里的输入框写的是**会话配置**里的名字，
//     走 `home.renameSession`（`/api/sessions/save`），成功后 store 会同步顶栏标签标题。
//     反过来顶栏标签双击改名也走同一个接口（见 AppTabBar），所以两边永远指向同一份数据。
//  2. 终端名的改动走 `terminals.setTitle`（后端 `/api/terminals/rename`），与会话名无关：
//     终端名是「这一条 shell 会话」的名字，会话名是「这台主机」的名字，混在一起会出现
//     「改了一个终端名，会话名也跟着变」的怪事。
import { computed, onBeforeUnmount, ref, watch } from 'vue'
import { t } from '@/i18n'
import { describeError } from '@/serverapi'
import { useHomeStore } from '@/stores/home'
import { useSessionUiStore } from '@/stores/sessionUi'
import { useTerminalsStore, type TermTab } from '@/stores/terminals'
import { useToastStore } from '@/stores/toast'
import { formatTime, relativeTime, sessionLabel } from '@/utils/format'
import ConfirmDialog from '@/components/ConfirmDialog.vue'
import PromptDialog from '@/components/PromptDialog.vue'

const props = defineProps<{
  visible: boolean
  /** 会话 id（弹窗只在这个会话的终端上操作）。 */
  sid: string
}>()

const emit = defineEmits<{
  (e: 'update:visible', v: boolean): void
}>()

const home = useHomeStore()
const terminals = useTerminalsStore()
const sessionUi = useSessionUiStore()
const toast = useToastStore()

const open = ref(props.visible)
const session = computed(() => home.sessions.find((item) => item.id === props.sid) ?? null)
const tabs = computed<TermTab[]>(() => terminals.tabsOf(props.sid))

const title = computed(() =>
  t('termlist_title', { name: session.value ? sessionLabel(session.value) : props.sid })
)

// 「创建于 X 分钟前」要自己走字：弹窗开着不动时，时间不该冻在打开那一刻
const now = ref(new Date())
let tick = 0

function startTick() {
  stopTick()
  tick = window.setInterval(() => {
    now.value = new Date()
  }, 30_000)
}

function stopTick() {
  if (tick) window.clearInterval(tick)
  tick = 0
}

onBeforeUnmount(stopTick)

// ---------- 会话改名 ----------

const sessionName = ref('')
const renaming = ref(false)

/**
 * 会话名提交（输入框的 change：回车或失焦时各触发一次，值没变时直接返回，天然去重）。
 * 失败要把输入框回滚成原值——留在新值上会让用户以为已经改好了，而顶栏标签没变。
 */
async function commitName() {
  if (renaming.value) return
  const before = session.value?.name ?? ''
  const next = sessionName.value.trim()
  if (next === before) return
  renaming.value = true
  try {
    // 空串 = 后端保留原名，所以回填的是后端返回的 session 而不是我们发出去的值
    const saved = await home.renameSession(props.sid, next)
    sessionName.value = saved.name ?? ''
  } catch (e) {
    sessionName.value = before
    toast.show(t('home_rename_failed', { error: describeError(e) }), 'error')
  } finally {
    renaming.value = false
  }
}

// ---------- 终端：改名 / 关闭 / 新建 ----------

const renameVisible = ref(false)
const renameTarget = ref<TermTab | null>(null)
const renameInitial = ref('')

function startRename(tab: TermTab) {
  renameTarget.value = tab
  renameInitial.value = tab.title
  renameVisible.value = true
}

function onRenamed(value: string) {
  const target = renameTarget.value
  renameTarget.value = null
  // 空值 = 回到默认的「终端 N」（terminals.setTitle 里 title 为空即用默认名），这是清空输入框的
  // 自然语义，所以这里不拦空值。
  if (target) terminals.setTitle(props.sid, target.uid, value.trim())
}

const closeVisible = ref(false)
const closeTarget = ref<TermTab | null>(null)

function requestClose(tab: TermTab) {
  closeTarget.value = tab
  closeVisible.value = true
}

function confirmClose() {
  const target = closeTarget.value
  closeTarget.value = null
  // closeTab 内部会发 /api/terminals/close：远程 shell 就此终止，所以必须先确认（契约 11.1.4）
  if (target) terminals.closeTab(props.sid, target.uid)
}

const closeTitle = computed(() =>
  closeTarget.value ? terminals.titleOf(props.sid, closeTarget.value) : ''
)

/** 状态点（与子标签栏同一套颜色语义：绿=可用、琥珀=连接/恢复/被顶号、灰=已退出、红=出错）。 */
function dotClass(tab: TermTab): string {
  if (tab.status === 'open' && !tab.restoring) return 'bg-success'
  if (tab.status === 'connecting' || tab.restoring || tab.status === 'detached') return 'bg-warning'
  if (tab.status === 'exited') return 'bg-ink-faint dark:bg-ink-faint-dark'
  return 'bg-danger'
}

function addTerminal() {
  // terminals.add 会把这个新终端设为激活项。弹窗不关：用户常常要连开几个。
  terminals.add(props.sid)
  // 顺手把这个会话的主区域切回终端：用户点「新建终端」就是想看到它，不该出现
  // 「新建成功、切回会话却停在文件管理页、看不出终端建在哪」。
  sessionUi.setMainMode(props.sid, 'terminal')
}

// ---------- 打开 / 关闭 ----------

watch(
  () => props.visible,
  (v) => {
    open.value = v
    if (!v) {
      stopTick()
      return
    }
    // 每次打开都重新对齐配置与计时器：弹窗期间会话可能在别处被改了名
    sessionName.value = session.value?.name ?? ''
    now.value = new Date()
    startTick()
  },
  { immediate: true }
)

/** 会话配置在弹窗打开期间被别处改掉（比如顶栏标签改名）也要跟上。 */
watch(session, () => {
  if (open.value && !renaming.value) sessionName.value = session.value?.name ?? ''
})

function close() {
  open.value = false
  emit('update:visible', false)
}
</script>

<template>
  <Teleport to="body">
    <Transition name="modal-fade">
      <div v-if="open" class="fixed inset-0 z-50">
        <div class="absolute inset-0 bg-black/50" @click="close"></div>
        <div class="region-center absolute left-0 right-0 flex items-center justify-center px-4">
          <div
            class="pointer-events-auto relative flex max-h-[90%] w-full max-w-lg flex-col rounded-xl border border-line bg-surface shadow-pop dark:border-line-dark dark:bg-surface-dark"
          >
            <div class="flex shrink-0 items-center justify-between border-b border-line px-5 py-3 dark:border-line-dark">
              <h3 class="min-w-0 truncate font-display text-base font-semibold text-ink dark:text-ink-dark" :title="title">
                {{ title }}
              </h3>
              <button class="g-btn-ghost !h-8 !px-2 text-lg leading-none" @click="close">×</button>
            </div>

            <div class="min-h-0 flex-1 overflow-y-auto px-5 py-4">
              <!-- 会话名称：它就是顶栏标签上的名字，改这里等于改那个标签 -->
              <div>
                <label
                  for="field-sessionterminalsdialog-1"
                  class="mb-1 block text-xs font-medium text-ink-soft dark:text-ink-soft-dark"
                >
                  {{ t('form_name') }}
                </label>
                <input
                  id="field-sessionterminalsdialog-1"
                  v-model="sessionName"
                  class="g-input"
                  :disabled="renaming"
                  :placeholder="t('form_name_placeholder')"
                  @change="commitName"
                />
                <p class="mt-1 text-xs text-ink-faint dark:text-ink-faint-dark">
                  {{ t('termlist_session_name_hint') }}
                </p>
              </div>

              <div class="mt-5 flex items-center justify-between gap-2">
                <span class="text-xs font-medium text-ink-soft dark:text-ink-soft-dark">
                  {{ t('termlist_name_hint') }}
                </span>
                <button class="g-btn-primary !h-8 !px-3 text-xs" @click="addTerminal">
                  <svg
                    viewBox="0 0 24 24"
                    fill="none"
                    stroke="currentColor"
                    stroke-width="2"
                    stroke-linecap="round"
                    class="h-3.5 w-3.5"
                  >
                    <line x1="12" y1="5" x2="12" y2="19" />
                    <line x1="5" y1="12" x2="19" y2="12" />
                  </svg>
                  {{ t('termlist_new') }}
                </button>
              </div>

              <p
                v-if="!tabs.length"
                class="mt-4 rounded-lg border border-line px-3 py-6 text-center text-sm text-ink-soft dark:border-line-dark dark:text-ink-soft-dark"
              >
                {{ t('termlist_empty') }}
              </p>

              <ul v-else class="mt-3 space-y-2">
                <li
                  v-for="tab in tabs"
                  :key="tab.uid"
                  class="flex items-center gap-2 rounded-lg border border-line px-3 py-2 dark:border-line-dark"
                >
                  <span class="h-1.5 w-1.5 shrink-0 rounded-full" :class="dotClass(tab)"></span>
                  <div class="min-w-0 flex-1">
                    <p class="truncate text-sm font-medium text-ink dark:text-ink-dark">
                      {{ terminals.titleOf(sid, tab) }}
                    </p>
                    <!-- 相对时间好读，绝对时间放 title：想知道确切时刻时鼠标一悬停就有 -->
                    <p
                      class="truncate text-xs text-ink-faint dark:text-ink-faint-dark"
                      :title="formatTime(tab.createdAt, now)"
                    >
                      {{ t('termlist_created', { time: relativeTime(tab.createdAt, now) }) }}
                    </p>
                  </div>
                  <button
                    class="g-btn-ghost !h-8 !px-2 shrink-0"
                    :title="t('session_rename_title')"
                    @click="startRename(tab)"
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
                    class="g-btn-ghost !h-8 !px-2 shrink-0 !text-danger hover:!bg-danger/10"
                    :title="t('termlist_close_confirm_title')"
                    @click="requestClose(tab)"
                  >
                    <svg
                      viewBox="0 0 24 24"
                      fill="none"
                      stroke="currentColor"
                      stroke-width="2"
                      stroke-linecap="round"
                      class="h-4 w-4"
                    >
                      <line x1="18" y1="6" x2="6" y2="18" />
                      <line x1="6" y1="6" x2="18" y2="18" />
                    </svg>
                  </button>
                </li>
              </ul>
            </div>
          </div>
        </div>
      </div>
    </Transition>
  </Teleport>

  <PromptDialog
    v-model:visible="renameVisible"
    :title="t('session_rename_title')"
    :label="t('session_rename_prompt')"
    :initial-value="renameInitial"
    @confirm="onRenamed"
    @cancel="renameTarget = null"
  />

  <ConfirmDialog
    :visible="closeVisible"
    :title="t('termlist_close_confirm_title')"
    :message="t('termlist_close_confirm_msg', { title: closeTitle })"
    :danger="true"
    :confirm-text="t('common_close')"
    :cancel-text="t('common_cancel')"
    @confirm="confirmClose"
    @cancel="closeTarget = null"
    @update:visible="(v) => { if (!v) closeTarget = null }"
  />
</template>
