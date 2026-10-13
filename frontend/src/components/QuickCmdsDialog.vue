<script setup lang="ts">
// QuickCmdsDialog — 快捷指令列表（全局 / 会话专属两个分组），从顶栏或会话工具条打开。
//
// 点击一条命令 = 发到「当前激活的终端」：
//   1. 优先 sessionUi 指的那个终端（唯一来源，见冻结的 stores/sessionUi.ts）；
//   2. 它指向文件管理子标签（或已经不存在的 uid）时，退到本会话最后激活的终端 ——
//      否则用户在看文件管理时点指令只会得到「终端未连接」，而终端其实好好开着；
//   3. 一个终端都没有（首页打开弹窗、或移动端还没建终端）才算真的没有目标，toast。
// 「专属」分组只在会话里可选：首页没有会话上下文，把它藏掉比让它指向某个会话更诚实。
import { computed, ref, watch } from 'vue'
import { t } from '@/i18n'
import { describeError, type QuickCmd } from '@/serverapi'
import { useSessionUiStore } from '@/stores/sessionUi'
import { useHomeStore } from '@/stores/home'
import { usePaneControlsStore } from '@/stores/paneControls'
import { useQuickCmdsStore } from '@/stores/quickCmds'
import { useTerminalsStore } from '@/stores/terminals'
import { useToastStore } from '@/stores/toast'
import { sessionLabel } from '@/utils/format'
import ConfirmDialog from './ConfirmDialog.vue'
import QuickCmdEditDialog from './QuickCmdEditDialog.vue'

const props = defineProps<{
  visible: boolean
  /** 会话 id；首页打开时为空（只显示全局分组）。 */
  sid?: string | null
}>()

const emit = defineEmits<{
  (e: 'update:visible', v: boolean): void
}>()

const qc = useQuickCmdsStore()
const home = useHomeStore()
const terminals = useTerminalsStore()
const paneControls = usePaneControlsStore()
const sessionUi = useSessionUiStore()
const toast = useToastStore()

const open = ref(props.visible)
const sessionId = computed(() => props.sid ?? null)
const scope = ref<'global' | 'session'>('global')

/** 当前分组对应的「作用域 sid」：null = 全局（与后端 `sid=""` 的约定一致）。 */
const scopeSid = computed(() => (scope.value === 'session' ? sessionId.value : null))

const list = computed<QuickCmd[]>(() => {
  if (scope.value === 'session' && sessionId.value) return qc.session[sessionId.value] ?? []
  return qc.global
})

/** 只有「已成功加载」的分组才允许增删改：保存是整体覆盖，未加载就改会把服务端那份覆盖掉。 */
const loading = computed(() => qc.isLoading(scopeSid.value))
const failed = computed(() => qc.isFailed(scopeSid.value))
const ready = computed(() => !loading.value && !failed.value)

/** 上次加载失败的原因（弹窗里要显示出来，不能只说「加载失败」）。 */
const loadError = ref('')

const sessionName = computed(() => {
  const session = home.sessions.find((item) => item.id === sessionId.value)
  return session ? sessionLabel(session) : ''
})

async function loadCurrent() {
  try {
    if (scope.value === 'session' && sessionId.value) await qc.ensureSession(sessionId.value)
    else await qc.ensureGlobal()
    loadError.value = ''
  } catch (e) {
    loadError.value = describeError(e)
    toast.show(t('qc_load_failed', { error: loadError.value }), 'error')
  }
}

async function retry() {
  try {
    await qc.reload(scopeSid.value)
    loadError.value = ''
  } catch (e) {
    loadError.value = describeError(e)
    toast.show(t('qc_load_failed', { error: loadError.value }), 'error')
  }
}

watch(
  () => props.visible,
  (v) => {
    open.value = v
    if (!v) return
    // 每次打开都回到「全局」：会话组是新用户最容易误建的地方（以为建的是全局指令）
    scope.value = 'global'
    void loadCurrent()
  },
  { immediate: true }
)

watch(scope, (s) => {
  if (props.visible && s) void loadCurrent()
})

// ---------- 发送到当前激活终端 ----------

function currentControls() {
  const sid = sessionId.value
  if (!sid) return undefined
  const tabs = terminals.tabsOf(sid)
  const sub = sessionUi.activeSubOf(sid)
  const uid = tabs.some((tab) => tab.uid === sub)
    ? sub
    : (terminals.activeOf(sid)?.uid ?? tabs.at(-1)?.uid ?? null)
  return paneControls.get(sid, uid)
}

function run(cmd: QuickCmd) {
  const ctl = currentControls()
  const sid = sessionId.value
  if (!ctl?.connected || !sid) {
    toast.show(t('qc_not_connected'), 'error')
    return
  }
  // 多行命令要按回车逐行执行：直接把 \n 灌进 PTY 只会得到一行乱掉的输入
  ctl.send(cmd.content.replace(/\r?\n/g, '\r') + (cmd.auto ? '\r' : ''))
  ctl.focus()
  close()
}

// ---------- 增删改 ----------

const editVisible = ref(false)
const editing = ref<QuickCmd | null>(null)

function openAdd() {
  if (!ready.value) return
  editing.value = null
  editVisible.value = true
}

function openEdit(cmd: QuickCmd) {
  editing.value = cmd
  editVisible.value = true
}

async function onSave(payload: { name: string; content: string; auto: boolean }) {
  const sid = scopeSid.value
  const target = editing.value
  editing.value = null
  try {
    if (target) await qc.update(sid, target, payload)
    else await qc.add(sid, payload)
    toast.show(t('qc_saved'), 'success')
  } catch (e) {
    // store 已回滚到改动前的快照
    toast.show(t('qc_save_failed', { error: describeError(e) }), 'error')
  }
}

const deleteVisible = ref(false)
const deleteTarget = ref<QuickCmd | null>(null)

function openDelete(cmd: QuickCmd) {
  deleteTarget.value = cmd
  deleteVisible.value = true
}

async function confirmDelete() {
  const target = deleteTarget.value
  deleteTarget.value = null
  if (!target) return
  try {
    await qc.remove(scopeSid.value, target)
    toast.show(t('qc_deleted'), 'success')
  } catch (e) {
    toast.show(t('qc_save_failed', { error: describeError(e) }), 'error')
  }
}

async function move(index: number, delta: -1 | 1) {
  const next = index + delta
  const ids = list.value.map((cmd) => cmd.id)
  if (next < 0 || next >= ids.length) return
  const current = ids[index]!
  ids[index] = ids[next]!
  ids[next] = current
  try {
    await qc.reorder(scopeSid.value, ids)
  } catch (e) {
    toast.show(t('qc_save_failed', { error: describeError(e) }), 'error')
  }
}

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
            <!-- 标题 + 新增 / 关闭 -->
            <div class="flex shrink-0 items-center justify-between border-b border-line px-5 py-3 dark:border-line-dark">
              <h3 class="font-display text-base font-semibold text-ink dark:text-ink-dark">
                {{ t('qc_title') }}
              </h3>
              <div class="flex items-center gap-2">
                <button
                  class="g-btn-primary !h-8 !px-2.5 text-lg leading-none"
                  :disabled="!ready"
                  :title="t('qc_add_title')"
                  @click="openAdd"
                >
                  +
                </button>
                <button class="g-btn-ghost !h-8 !px-2 text-lg leading-none" @click="close">×</button>
              </div>
            </div>

            <!-- 分组切换：专属分组只在会话里出现 -->
            <div class="shrink-0 px-5 pt-3">
              <div
                class="flex items-center gap-1 rounded-md bg-black/5 p-1 dark:bg-white/5"
                :title="t('qc_scope_switch')"
              >
                <button
                  class="flex-1 rounded px-3 py-1.5 text-xs font-medium transition-colors"
                  :class="
                    scope === 'global'
                      ? 'bg-surface text-brand shadow-sm dark:bg-surface-dark'
                      : 'text-ink-soft dark:text-ink-soft-dark'
                  "
                  @click="scope = 'global'"
                >
                  {{ t('qc_scope_global') }}
                </button>
                <button
                  v-if="sessionId"
                  class="flex-1 rounded px-3 py-1.5 text-xs font-medium transition-colors"
                  :class="
                    scope === 'session'
                      ? 'bg-surface text-brand shadow-sm dark:bg-surface-dark'
                      : 'text-ink-soft dark:text-ink-soft-dark'
                  "
                  @click="scope = 'session'"
                >
                  {{ t('qc_scope_session') }}
                </button>
              </div>
              <p class="mt-2 text-xs text-ink-faint dark:text-ink-faint-dark">
                {{
                  scope === 'session'
                    ? t('qc_session_hint', { name: sessionName })
                    : t('qc_global_hint')
                }}
              </p>
            </div>

            <!-- 列表 -->
            <div class="min-h-0 flex-1 overflow-y-auto px-5 py-4">
              <p v-if="loading" class="py-8 text-center text-sm text-ink-soft dark:text-ink-soft-dark">
                {{ t('common_loading') }}
              </p>
              <div v-else-if="failed" class="py-8 text-center">
                <!-- 加载失败不能落到空态：用户会以为指令被删了，转头重建一份覆盖掉服务端 -->
                <p class="break-all text-sm text-danger">{{ t('qc_load_failed', { error: loadError }) }}</p>
                <button class="g-btn-ghost !h-8 !px-3 mt-3 text-xs" @click="retry">
                  {{ t('common_retry') }}
                </button>
              </div>
              <p
                v-else-if="!list.length"
                class="py-8 text-center text-sm text-ink-soft dark:text-ink-soft-dark"
              >
                {{ t('qc_empty') }}
              </p>

              <div v-else class="space-y-3">
                <div
                  v-for="(cmd, idx) in list"
                  :key="cmd.id"
                  class="group cursor-pointer rounded-lg border border-line transition-colors hover:border-brand/50 dark:border-line-dark"
                  :title="cmd.content"
                  @click="run(cmd)"
                >
                  <div class="px-3.5 py-2.5">
                    <div class="flex min-w-0 items-center gap-2">
                      <span class="min-w-0 truncate text-sm font-semibold text-ink dark:text-ink-dark">
                        {{ cmd.name }}
                      </span>
                      <span
                        v-if="cmd.auto"
                        class="inline-flex shrink-0 items-center rounded-full bg-brand-soft px-1.5 py-0.5 text-[10px] font-medium text-brand"
                      >
                        {{ t('qc_auto_tag') }}
                      </span>
                      <div class="flex-1"></div>
                      <div class="flex shrink-0 items-center gap-1">
                        <button
                          class="g-btn-ghost !h-7 !px-2"
                          :title="t('common_edit')"
                          @click.stop="openEdit(cmd)"
                        >
                          <svg
                            viewBox="0 0 24 24"
                            fill="none"
                            stroke="currentColor"
                            stroke-width="2"
                            stroke-linecap="round"
                            stroke-linejoin="round"
                            class="h-4 w-4"
                          >
                            <path d="M17 3a2.828 2.828 0 1 1 4 4L7.5 20.5 2 22l1.5-5.5L17 3z" />
                          </svg>
                        </button>
                        <button
                          class="g-btn-ghost !h-7 !px-2 !text-danger hover:!bg-danger/10"
                          :title="t('common_delete')"
                          @click.stop="openDelete(cmd)"
                        >
                          <svg
                            viewBox="0 0 24 24"
                            fill="none"
                            stroke="currentColor"
                            stroke-width="2"
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
                        <button
                          v-if="idx > 0"
                          class="g-btn-ghost !h-7 !px-2"
                          :title="t('qc_move_up')"
                          @click.stop="move(idx, -1)"
                        >
                          <svg
                            viewBox="0 0 24 24"
                            fill="none"
                            stroke="currentColor"
                            stroke-width="2"
                            stroke-linecap="round"
                            stroke-linejoin="round"
                            class="h-4 w-4"
                          >
                            <path d="M18 15l-6-6-6 6" />
                          </svg>
                        </button>
                        <button
                          v-if="idx < list.length - 1"
                          class="g-btn-ghost !h-7 !px-2"
                          :title="t('qc_move_down')"
                          @click.stop="move(idx, 1)"
                        >
                          <svg
                            viewBox="0 0 24 24"
                            fill="none"
                            stroke="currentColor"
                            stroke-width="2"
                            stroke-linecap="round"
                            stroke-linejoin="round"
                            class="h-4 w-4"
                          >
                            <path d="M6 9l6 6 6-6" />
                          </svg>
                        </button>
                      </div>
                    </div>
                    <p class="mt-1 truncate font-mono text-xs text-ink-soft dark:text-ink-soft-dark">
                      {{ cmd.content }}
                    </p>
                  </div>
                </div>
              </div>
            </div>

            <div class="shrink-0 border-t border-line px-5 py-2 dark:border-line-dark">
              <p class="text-[11px] text-ink-faint dark:text-ink-faint-dark">{{ t('qc_send') }}</p>
            </div>
          </div>
        </div>
      </div>
    </Transition>
  </Teleport>

  <QuickCmdEditDialog v-model:visible="editVisible" :cmd="editing" @save="onSave" />

  <ConfirmDialog
    :visible="deleteVisible"
    :title="t('qc_delete_confirm_title')"
    :message="t('qc_delete_confirm_msg', { name: deleteTarget?.name ?? '' })"
    :danger="true"
    :confirm-text="t('common_delete')"
    :cancel-text="t('common_cancel')"
    @confirm="confirmDelete"
    @cancel="deleteTarget = null"
    @update:visible="(v) => { if (!v) deleteTarget = null }"
  />
</template>
