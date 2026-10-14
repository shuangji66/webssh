<script setup lang="ts">
// QuickCmdsDialog — 快捷指令弹窗，三页：常用指令 / 全局 / 专属（契约 11.6）。
//
// 点击一条命令（内置清单里的或用户自己建的）= 发到「当前激活的终端」：
//   1. 优先 sessionUi 指的那个终端（唯一来源，见冻结的 stores/sessionUi.ts）；
//   2. 它指向文件管理子标签（或已经不存在的 uid）时，退到本会话最后激活的终端 ——
//      否则用户在看文件管理时点指令只会得到「终端未连接」，而终端其实好好开着；
//   3. 一个终端都没有（首页打开弹窗、或移动端还没建终端）才算真的没有目标，toast。
//
// 「常用指令」是**内置清单**（`data/presetCommands.ts`，前端纯展示数据）：只读、不能增删改，
// 所以那一页不显示「+」，也不走后端的整体保存；名字与分组名都走 i18n（`qc_p_*` / `qc_g_*`），
// 中英文都能显示。清单里 auto: false 的命令点一下只填进终端，回车由用户自己按。
//
// 「专属」页只在会话里出现：首页没有会话上下文，藏掉比让它指向某个会话更诚实。
//
// 第十一轮删掉了面板底部的**页脚**（原来左边写「会改变系统状态的命令不会自动回车」、右边写
// 「发送到 <终端名>」）：前者随「危险操作」分组（reboot / poweroff）一起删掉了；后者用户明确
// 说不需要（原话「不会发送错的」）——指令发进哪个终端，由工具条上那个按钮所在的位置就已经决定。
// 页脚没了之后弹窗只剩「头部 + 页签 + 列表」三段，列表自己滚（契约 21.2 / 21.4）。
import { computed, ref, watch } from 'vue'
import { t } from '@/i18n'
import { describeError, type QuickCmd } from '@/serverapi'
import {
  presetByGroup,
  presetGroupKey,
  presetNameKey,
  type PresetCmd
} from '@/data/presetCommands'
import { FILES_TAB, useSessionUiStore } from '@/stores/sessionUi'
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
  /** 会话 id；首页打开时为空（没有「专属」页，命令也发不到任何终端）。 */
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

/**
 * 面板高度**固定**为「视口 − 应用顶栏（会话标签栏）」的 90%（契约 21.3）。
 *
 * 为什么用固定高度而不是 max-h：max-h 只是「不超过」，内容少的面板会缩水 —— 三个页签的内容
 * 长短差很多（内置清单很长、自建列表可能为空），来回切就会看到面板忽高忽低。固定高度下切换
 * 页签时窗口边框不动，只有列表在滚。
 *
 * **不减底部辅助键条**（第十一轮用户要求）：定位带就是「顶栏以下到视口底部」，面板取它的 90%
 * 并在带内垂直居中。触屏上弹窗因此会比以前高约 50px，底部可能压到辅助键条上——模态盖住键条
 * 是正常的（键条在弹窗底下点不到，本来也不该点）。
 *
 * 算式用 `--vvh` / `--vvt`（已被虚拟键盘修正）而不是 `100vh`：iOS 上键盘弹起时 vh 会算错。
 * 这两个常量必须与下面的 `BAND_STYLE` 一致，否则面板会在带内溢出（居中的 flex 会把溢出的
 * 一半顶到顶栏里去）。
 */
const BAND_HEIGHT = 'calc(var(--vvh, 100dvh) - var(--header-h, 0px))'
const BAND_STYLE = {
  top: 'calc(var(--vvt, 0px) + var(--header-h, 0px))',
  height: BAND_HEIGHT
}
const PANEL_HEIGHT = `calc(${BAND_HEIGHT} * 0.9)`

const open = ref(props.visible)
const sessionId = computed(() => props.sid ?? null)

/** 三个页签。常用指令排第一：它是这一轮新增的主入口。 */
type Page = 'preset' | 'global' | 'session'
const page = ref<Page>('global')

const pages = computed<{ id: Page; label: string }[]>(() => {
  const out: { id: Page; label: string }[] = [
    { id: 'preset', label: t('qc_tab_preset') },
    { id: 'global', label: t('qc_tab_global') }
  ]
  // 「专属」只有会话里才有（首页打开时没有会话上下文），少一个页签比让它指向某个会话诚实
  if (sessionId.value) out.push({ id: 'session', label: t('qc_tab_session') })
  return out
})

/** 当前页对应的「作用域 sid」：null = 全局（与后端 `sid=""` 的约定一致）。 */
const scopeSid = computed<string | null>(() => (page.value === 'session' ? sessionId.value : null))

const list = computed<QuickCmd[]>(() => {
  if (page.value === 'session' && sessionId.value) return qc.session[sessionId.value] ?? []
  return qc.global
})

/** 内置清单：按用途分好组，顺序由 PRESET_GROUPS 决定。 */
const presetGroups = computed(() => presetByGroup())

/**
 * 只有「已成功加载」的分组才允许增删改：保存是整体覆盖，未加载就改会把服务端那份覆盖掉。
 * 常用指令页的数据是内置常量（没有加载过程），所以它的 loading/failed 恒为假。
 */
const loading = computed(() => page.value !== 'preset' && qc.isLoading(scopeSid.value))
const failed = computed(() => page.value !== 'preset' && qc.isFailed(scopeSid.value))
const ready = computed(() => page.value !== 'preset' && !loading.value && !failed.value)

/** 上次加载失败的原因（弹窗里要显示出来，不能只说「加载失败」）。 */
const loadError = ref('')

const sessionName = computed(() => {
  const session = home.sessions.find((item) => item.id === sessionId.value)
  return session ? sessionLabel(session) : ''
})

async function loadCurrent() {
  // 内置清单不需要加载，也不必因为它去打扰后端
  if (page.value === 'preset') return
  try {
    if (page.value === 'session' && sessionId.value) await qc.ensureSession(sessionId.value)
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
    page.value = 'global'
    void loadCurrent()
  },
  { immediate: true }
)

watch(page, (p) => {
  if (props.visible && p) void loadCurrent()
})

// 会话在弹窗打开期间被关掉 / 关标签：专属页签随之消失，页签必须退回全局 ——
// 否则会停在一个「当前页签没有按钮」的状态上，而列表还在按错的 scope 读取（显示全局那份）。
watch(sessionId, (sid) => {
  if (!sid && page.value === 'session') page.value = 'global'
})

// ---------- 发送到当前激活终端 ----------

/**
 * 弹窗把指令发到哪个终端：**就是当前激活子标签对应的那个终端**（契约 13.1）。
 *
 * 用户的原话是「每个子终端标签页都要有快捷指令按钮，否则在弹窗里点指令都不知道输入到了哪个
 * 终端」——所以这里不再「文件管理在前台时退回本会话最后用过的终端」：那正是他会点错的那种
 * 情况。文件管理子标签在前台时没有激活的终端，就是没有目标，点击时如实报 `qc_not_connected`
 * （工具条上的按钮此时也已经是禁用态，正常路径根本进不来，这里是兜底）。
 */
function currentControls() {
  const sid = sessionId.value
  if (!sid) return undefined
  const sub = sessionUi.activeSubOf(sid)
  if (sub === FILES_TAB) return undefined
  const tabs = terminals.tabsOf(sid)
  const uid = tabs.some((tab) => tab.uid === sub)
    ? sub
    : (terminals.activeOf(sid)?.uid ?? tabs.at(-1)?.uid ?? null)
  return paneControls.get(sid, uid)
}

/**
 * 发给当前激活终端；返回是否真的发出去了（false = 没有可用终端）。
 * 内置清单与用户自建指令共用这一条出口：`auto` 决定要不要替用户按回车。
 */
function sendToActive(content: string, auto: boolean): boolean {
  const ctl = currentControls()
  if (!ctl?.connected) {
    toast.show(t('qc_not_connected'), 'error')
    return false
  }
  // 多行命令要按回车逐行执行：直接把 \n 灌进 PTY 只会得到一行乱掉的输入
  ctl.send(content.replace(/\r?\n/g, '\r') + (auto ? '\r' : ''))
  ctl.focus()
  return true
}

function runPreset(cmd: PresetCmd) {
  if (sendToActive(cmd.content, cmd.auto)) close()
}

function run(cmd: QuickCmd) {
  if (sendToActive(cmd.content, cmd.auto)) close()
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
        <!-- 定位带：顶栏以下到视口底部（**不**用 .region-center——那条带会再减掉底部辅助键条，
             而本弹窗的高度按用户要求只减顶栏，带与面板必须同一个算式，见 BAND_STYLE）。 -->
        <div
          class="absolute left-0 right-0 flex items-center justify-center md:px-4"
          :style="BAND_STYLE"
        >
          <div
            class="pointer-events-auto relative flex w-[95%] flex-col rounded-xl border border-line bg-surface shadow-pop md:w-full md:max-w-lg dark:border-line-dark dark:bg-surface-dark"
            :style="{ height: PANEL_HEIGHT }"
          >
            <!-- 标题 + 新增 / 关闭。常用指令是内置清单，不能增删改，所以那一页没有「+」 -->
            <div class="flex shrink-0 items-center justify-between border-b border-line px-5 py-3 dark:border-line-dark">
              <h3 class="font-display text-base font-semibold text-ink dark:text-ink-dark">
                {{ t('qc_title') }}
              </h3>
              <div class="flex items-center gap-2">
                <button
                  v-if="page !== 'preset'"
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

            <!-- 页签：常用指令 / 全局 / 专属（专属只在会话里出现） -->
            <div class="shrink-0 px-5 pt-3">
              <div
                class="flex items-center gap-1 rounded-md bg-black/5 p-1 dark:bg-white/5"
                :title="t('qc_scope_switch')"
              >
                <button
                  v-for="item in pages"
                  :key="item.id"
                  class="min-w-0 flex-1 truncate rounded px-2 py-1.5 text-xs font-medium transition-colors"
                  :class="
                    page === item.id
                      ? 'bg-surface text-brand shadow-sm dark:bg-surface-dark'
                      : 'text-ink-soft dark:text-ink-soft-dark'
                  "
                  @click="page = item.id"
                >
                  {{ item.label }}
                </button>
              </div>
              <p class="mt-2 text-xs text-ink-faint dark:text-ink-faint-dark">
                {{
                  page === 'preset'
                    ? t('qc_preset_hint')
                    : page === 'session'
                      ? t('qc_session_hint', { name: sessionName })
                      : t('qc_global_hint')
                }}
              </p>
            </div>

            <!-- 列表 -->
            <div class="min-h-0 flex-1 overflow-y-auto px-5 py-4">
              <!-- 常用指令页：内置清单按用途分组 -->
              <template v-if="page === 'preset'">
                <div
                  v-for="group in presetGroups"
                  :key="group.group"
                  class="mb-4 last:mb-0"
                >
                  <p class="mb-2 flex items-center gap-1.5 text-xs font-semibold text-ink-soft dark:text-ink-soft-dark">
                    {{ t(presetGroupKey(group.group)) }}
                  </p>

                  <div class="space-y-2">
                    <button
                      v-for="cmd in group.items"
                      :key="cmd.id"
                      type="button"
                      class="block w-full rounded-lg border border-line px-3.5 py-2.5 text-left transition-colors hover:border-brand/50 dark:border-line-dark"
                      :title="cmd.content"
                      @click="runPreset(cmd)"
                    >
                      <div class="flex min-w-0 items-center gap-2">
                        <span class="min-w-0 truncate text-sm font-semibold text-ink dark:text-ink-dark">
                          {{ t(presetNameKey(cmd.id)) }}
                        </span>
                        <span
                          v-if="cmd.auto"
                          class="inline-flex shrink-0 items-center rounded-full bg-brand-soft px-1.5 py-0.5 text-[10px] font-medium text-brand"
                        >
                          {{ t('qc_auto_tag') }}
                        </span>
                      </div>
                      <p class="mt-1 truncate font-mono text-xs text-ink-soft dark:text-ink-soft-dark">
                        {{ cmd.content }}
                      </p>
                    </button>
                  </div>
                </div>
              </template>

              <template v-else>
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
              </template>
            </div>
          </div>
        </div>
      </div>
    </Transition>
  </Teleport>

  <QuickCmdEditDialog v-model:visible="editVisible" :cmd="editing" @save="onSave" />

  <!-- 删除快捷指令前确认。`visible` 必须用 v-model 接回去：漏接的话弹窗关一次之后就再也
       弹不出来（症状与原因见 ConfirmDialog 头部注释）。 -->
  <ConfirmDialog
    v-model:visible="deleteVisible"
    :title="t('qc_delete_confirm_title')"
    :message="t('qc_delete_confirm_msg', { name: deleteTarget?.name ?? '' })"
    :danger="true"
    :confirm-text="t('common_delete')"
    :cancel-text="t('common_cancel')"
    @confirm="confirmDelete"
    @cancel="deleteTarget = null"
  />
</template>
