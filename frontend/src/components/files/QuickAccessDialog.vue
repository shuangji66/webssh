<script setup lang="ts">
// QuickAccessDialog — 快捷访问弹窗（第二十轮，契约第 30 节）。
//
// 弹窗里**不预设任何路径**（用户要求）：列表来自 /api/quickaccess/list，一开始是空的，用户
// 要么在目录的「更多」菜单里点「添加快捷访问」（不弹这个弹窗，直接加），要么在这里手动输入。
//
// 点一条 = 在文件管理里进入那个目录。顺序是**先探测、再切过去**（用户要求「没权限就报错并且
// 不打开文件管理或不进入目录」）：
//   1. `sftp.jumpTo` 会先列一次目录，失败（权限 / 目录不在了 / 会话没打开）就只是报错——激活
//      子标签不动、当前目录不动（从面包屑按钮进来时主区域还停在终端上）；
//   2. 成功之后才把激活子标签切到文件管理（用户在左栏点了书签，主区域该跟着走），然后关弹窗
//      ——留着它挡在刚打开的目录上面没有意义。
//    失败时弹窗**留着**：用户多半想顺手把这条失效的路径删掉。
//
// 删除不弹确认框：删掉一条书签没有任何破坏性（重新加一条的成本是点两下），而多一次确认
// 反而与「点一下移除」的预期不符。
import { computed, ref, watch } from 'vue'
import { t } from '@/i18n'
import { describeError } from '@/serverapi'
import { normalizeQuickPath, useQuickAccessStore } from '@/stores/quickAccess'
import { FILES_TAB, useSessionUiStore } from '@/stores/sessionUi'
import { useSftpStore } from '@/stores/sftp'
import { useToastStore } from '@/stores/toast'

const props = defineProps<{ sid: string; visible: boolean }>()

const emit = defineEmits<{
  (e: 'update:visible', v: boolean): void
}>()

const qa = useQuickAccessStore()
const sftp = useSftpStore()
const sessionUi = useSessionUiStore()
const toastStore = useToastStore()

const paths = computed(() => qa.listOf(props.sid))
const loading = computed(() => qa.isLoading(props.sid))
const failed = computed(() => qa.isFailed(props.sid))

/** 上次加载失败的原因：界面必须说清是什么失败了（「加载失败」四个字对用户没有任何用）。 */
const loadError = ref('')

const input = ref('')
const inputEl = ref<HTMLInputElement | null>(null)
const adding = ref(false)
/** 正在打开的那一条（点一下要等一次列目录：期间标出来，避免连点两次）。 */
const opening = ref('')

/**
 * 拉一次列表（弹窗每次打开、以及点「重试」）。
 *
 * `force` = 用户点的「重试」：走 `reload` 绕过「已加载」的幂等判断强制重拉，否则走 `ensure`
 * （已成功加载过就不发请求）。两条路都从这里走，**原因与失败标志各归一处**：失败标志在 store
 * 里（`isFailed` → 上面那个 failed，界面据此走「加载失败」分支而不是空态），原因留在这个
 * `loadError` 里（store 不该管界面文案）。每次尝试前先清空它，避免重试成功后还挂着上一次的话。
 */
async function load(force = false) {
  loadError.value = ''
  try {
    if (force) await qa.reload(props.sid)
    else await qa.ensure(props.sid)
  } catch (e) {
    loadError.value = describeError(e)
  }
}

watch(
  () => props.visible,
  (v) => {
    if (!v) return
    void load()
    // 打开时**刻意不聚焦**输入框（用户要求）：打开这个弹窗有两个用途——挑一条已有的路径跳过去、
    // 或者加一条新的，而前者更常见；自动聚焦会让移动端键盘立刻弹起来盖住半屏列表，桌面端则会
    // 把光标停在输入框里等着，用户点列表还要先挪一次焦点。输入框就在页脚，想加的时候直接点它
    // 或者按 Tab 进去（加完之后焦点**留**在输入框里，连着加几条不用反复点，见 submit）。
  },
  { immediate: true }
)

function close() {
  emit('update:visible', false)
}

/**
 * 添加（输入框回车或「添加」按钮）。
 *
 * 手输的路径**首尾空白当误触**去掉（粘贴时常带来一个尾随空格）。注意这是**输入框这一层**的
 * 处理：从目录菜单/列表加进来的路径一个字都不动——以空格结尾的目录名是合法的，而本仓库已经
 * 被 `resolveRemotePath` 那条坑过一次（见 stores/quickAccess.ts 的说明）。
 */
async function submit() {
  const raw = input.value.trim()
  if (!raw || adding.value) return
  const path = normalizeQuickPath(raw)
  if (!path) {
    toastStore.show(t('files_quick_invalid'), 'error')
    return
  }
  adding.value = true
  try {
    const result = await qa.add(props.sid, raw)
    input.value = ''
    if (result === 'exists') toastStore.show(t('files_quick_exists'), 'info')
    else toastStore.show(t('files_quick_added', { path }), 'success')
    // 焦点留在输入框：连着加好几条是常见操作
    inputEl.value?.focus()
  } catch (e) {
    toastStore.show(t('files_quick_add_failed', { error: describeError(e) }), 'error')
  } finally {
    adding.value = false
  }
}

async function remove(path: string) {
  try {
    await qa.remove(props.sid, path)
  } catch (e) {
    toastStore.show(t('files_quick_remove_failed', { error: describeError(e) }), 'error')
  }
}

/** 点一条：先探测（jumpTo），成功了才切子标签并关弹窗。 */
async function open(path: string) {
  if (opening.value) return
  opening.value = path
  try {
    await sftp.jumpTo(props.sid, path)
  } catch (e) {
    toastStore.show(t('files_quick_open_failed', { error: describeError(e) }), 'error')
    return
  } finally {
    opening.value = ''
  }
  // 切到文件管理：从面包屑按钮进来时主区域还在终端上（这是「在文件管理中进入该目录」的
  // 那一半）。已经在前台时这一句是无副作用的重复写。
  sessionUi.setActiveSub(props.sid, FILES_TAB)
  close()
}
</script>

<template>
  <Teleport to="body">
    <Transition name="modal-fade">
      <div v-if="visible" class="fixed inset-0 z-50">
        <div class="absolute inset-0 bg-black/50" @click="close"></div>
        <!-- 与其它弹窗同一条定位带（.region-center，扣掉顶栏与底部键条）。 -->
        <div class="region-center absolute left-0 right-0 flex items-center justify-center px-4">
          <div
            class="pointer-events-auto relative flex max-h-[90%] w-full max-w-md flex-col rounded-xl border border-line bg-surface shadow-pop dark:border-line-dark dark:bg-surface-dark"
          >
            <!-- 头部固定：路径列表可能很长，标题与关闭按钮不能跟着滚出去 -->
            <div
              class="flex shrink-0 items-center gap-2 border-b border-line px-4 py-3 dark:border-line-dark"
            >
              <h3 class="min-w-0 flex-1 font-display text-base font-semibold text-ink dark:text-ink-dark">
                {{ t('files_quick_access') }}
              </h3>
              <button
                type="button"
                class="tree-tool h-7 w-7"
                :title="t('common_close')"
                :aria-label="t('common_close')"
                @click="close"
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
                  <path d="M6 6l12 12M18 6L6 18" />
                </svg>
              </button>
            </div>

            <div class="thin-scroll min-h-0 flex-1 overflow-y-auto px-2 py-2">
              <div v-if="loading" class="px-2 py-4 text-center text-sm text-ink-faint">
                {{ t('common_loading') }}
              </div>

              <div v-else-if="failed" class="px-2 py-3 text-sm">
                <p class="leading-relaxed break-all text-danger">
                  {{ t('files_quick_load_failed', { error: loadError }) }}
                </p>
                <!-- 显式传 true：写成 @click="load" 会把 MouseEvent 当作 force 传进去，
                     虽然结果一样，但没人看得出这是「强制重拉」。 -->
                <button type="button" class="g-btn-ghost mt-1 h-8 text-danger" @click="load(true)">
                  {{ t('common_retry') }}
                </button>
              </div>

              <div v-else-if="!paths.length" class="px-2 py-3 text-sm">
                <p class="text-ink-soft dark:text-ink-soft-dark">{{ t('files_quick_empty') }}</p>
                <p class="mt-1 text-xs leading-relaxed text-ink-faint">
                  {{ t('files_quick_empty_hint') }}
                </p>
              </div>

              <template v-else>
                <div
                  v-for="path in paths"
                  :key="path"
                  class="flex items-center gap-1 rounded px-1 hover:bg-black/5 dark:hover:bg-white/5"
                >
                  <!-- 点路径 = 进入该目录。删除是同一行里另一个按钮（不能嵌套 button：
                       HTML 里 button 里再放 button 会被浏览器拆开）。 -->
                  <button
                    type="button"
                    class="min-w-0 flex-1 truncate py-1.5 text-left font-mono text-xs text-ink hover:text-brand dark:text-ink-dark"
                    :title="path"
                    :disabled="opening === path"
                    @click="open(path)"
                  >
                    {{ path }}
                  </button>
                  <button
                    type="button"
                    class="tree-tool h-6 w-6 shrink-0 text-danger"
                    :title="`${t('files_quick_remove')} ${path}`"
                    :aria-label="`${t('files_quick_remove')} ${path}`"
                    @click="remove(path)"
                  >
                    <svg
                      class="h-3.5 w-3.5"
                      viewBox="0 0 24 24"
                      fill="none"
                      stroke="currentColor"
                      stroke-width="1.5"
                      stroke-linecap="round"
                      stroke-linejoin="round"
                    >
                      <path d="M4 7h16" />
                      <path d="M9 7V5h6v2" />
                      <path d="M6 7l1 13h10l1-13" />
                    </svg>
                  </button>
                </div>
              </template>
            </div>

            <!-- 输入区固定：手输一条路径是这个弹窗最常用的动作 -->
            <div class="shrink-0 border-t border-line px-4 py-3 dark:border-line-dark">
              <label
                for="quick-access-input"
                class="block text-xs font-medium text-ink-soft dark:text-ink-soft-dark"
              >
                {{ t('files_quick_input') }}
              </label>
              <div class="mt-1.5 flex items-center gap-2">
                <!-- id + for 关联 label：否则输入框没有可访问名（屏幕阅读器与自动化只能猜
                     placeholder），点 label 也不会聚焦过来。 -->
                <input
                  id="quick-access-input"
                  ref="inputEl"
                  v-model="input"
                  type="text"
                  class="g-input min-w-0 flex-1 font-mono text-xs"
                  :placeholder="t('files_quick_input_ph')"
                  :disabled="adding"
                  @keydown.enter.prevent="submit"
                  @keydown.esc.prevent="close"
                />
                <button
                  type="button"
                  class="g-btn-primary h-9 shrink-0 !px-3 text-sm"
                  :disabled="adding"
                  @click="submit"
                >
                  {{ t('files_quick_add') }}
                </button>
              </div>
            </div>
          </div>
        </div>
      </div>
    </Transition>
  </Teleport>
</template>
