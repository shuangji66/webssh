<script setup lang="ts">
// SessionTerminalsDialog — 首页卡片上「N 个终端」点开的弹窗：列出该会话当前打开的每个终端，
// 可以单独关掉其中某一个（契约 13.3）。
//
// 为什么只剩「列表 + 关闭」：
//   - 改会话名已经有两条路（编辑表单 ↔ 顶栏标签双击），两者与服务端配置双向同步；再放一个
//     输入框就是同一个名字的第三个入口，三处一致性没法维护；
//   - 新建终端在子标签栏的「+」上更自然（点完立刻看到新标签，也能直接在里面打字）；
//   - 终端改名在子标签上双击即可。
// 所以这个弹窗的职责收窄成「看一眼这个会话下开了哪些终端、把不想要的那个关掉」。
import { computed, onBeforeUnmount, ref, watch } from 'vue'
import { t } from '@/i18n'
import { useHomeStore } from '@/stores/home'
import { useTerminalsStore, type TermTab } from '@/stores/terminals'
import { formatTime, relativeTime, sessionLabel } from '@/utils/format'
import ConfirmDialog from '@/components/ConfirmDialog.vue'

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

// ---------- 关闭单个终端 ----------

const closeVisible = ref(false)
const closeTarget = ref<TermTab | null>(null)

function requestClose(tab: TermTab) {
  closeTarget.value = tab
  closeVisible.value = true
}

function confirmClose() {
  const target = closeTarget.value
  closeTarget.value = null
  // closeTab 内部会发 /api/terminals/close（远程 shell 就此终止），所以必须先确认
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

watch(
  () => props.visible,
  (v) => {
    open.value = v
    if (!v) {
      stopTick()
      return
    }
    now.value = new Date()
    startTick()
  },
  { immediate: true }
)

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
              <h3
                class="min-w-0 truncate font-display text-base font-semibold text-ink dark:text-ink-dark"
                :title="title"
              >
                {{ title }}
              </h3>
              <button class="g-btn-ghost !h-8 !px-2 text-lg leading-none" @click="close">×</button>
            </div>

            <div class="min-h-0 flex-1 overflow-y-auto px-5 py-4">
              <p
                v-if="!tabs.length"
                class="rounded-lg border border-line px-3 py-6 text-center text-sm text-ink-soft dark:border-line-dark dark:text-ink-soft-dark"
              >
                {{ t('termlist_empty') }}
              </p>

              <ul v-else class="space-y-2">
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
