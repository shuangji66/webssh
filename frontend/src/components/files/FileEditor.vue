<script setup lang="ts">
// FileEditor — 编辑页（契约 26.3）：覆盖在**文件管理面板**上的一整页，不弹窗。
//
// 形态按用户要求：左上角「退出」、右上角「还原 / 保存」，中间一行文件名 + 元信息，正文是
// 铺满剩余高度的 <textarea>。
//
// 状态全部在 stores/fileEditor.ts 里（三个入口共用同一份），这个组件只管画与转发动作：
//   - 退出：有未保存的修改时先确认，然后 close()——**不动文件管理器的任何状态**，
//     所以退出后看到的就是进入编辑时的那个目录与视图（从目录树进来的情况同理，见 store 注释）；
//   - 还原：重新从远程读一次（有未保存的修改时先确认）；
//   - 保存：写回远程，失败保留编辑区内容并说明原因。
//
// 编辑区的内容一律是 LF：textarea 的 value 按 HTML 规范把 CRLF 规整成 LF，而写回时按原文件的
// 换行由**后端**换回去（eol 随保存请求带上，见 serverapi 的 fsEditSave）。
import { computed, nextTick, reactive, ref, watch } from 'vue'
import { t } from '@/i18n'
import { describeError } from '@/serverapi'
import { useFileEditorStore } from '@/stores/fileEditor'
import { useToastStore } from '@/stores/toast'
import { baseName, humanSize } from '@/utils/format'
import ConfirmDialog from '@/components/ConfirmDialog.vue'

const { sid } = defineProps<{ sid: string }>()

const editor = useFileEditorStore()
const toastStore = useToastStore()

const st = computed(() => editor.stateOf(sid))
const dirty = computed(() => st.value.content !== st.value.original)
const name = computed(() => baseName(st.value.path) || st.value.path)

/**
 * 标题右边那行元信息：体积 · 行数 · 编码（只在不是 UTF-8 时点出来）· CRLF（原文件是 CRLF 时）。
 *
 * 编码与换行都必须显示：它们决定了保存后文件长什么样，而用户看不见这些字节——
 * 「我改的是 UTF-16 的 .ini」这件事只有这里能告诉他。
 */
const meta = computed(() => {
  const parts: string[] = []
  if (st.value.size > 0) parts.push(humanSize(st.value.size))
  const lines = st.value.content === '' ? 0 : st.value.content.split('\n').length
  if (lines > 0) parts.push(t('files_preview_lines', { n: lines }))
  if (st.value.encoding && st.value.encoding !== 'utf-8') parts.push(st.value.encoding.toUpperCase())
  if (st.value.eol === 'crlf') parts.push(t('files_editor_crlf'))
  return parts.join(' · ')
})

const area = ref<HTMLTextAreaElement | null>(null)

// 打开就把光标放进编辑区（用户点「编辑」就是要打字，不该还要再点一下）。
// 打开的那一刻组件刚挂载完（v-if 由 FileManager 控制），所以要等 nextTick。
watch(
  () => st.value.open,
  (open) => {
    if (open) void nextTick(() => area.value?.focus())
  },
  { immediate: true }
)

const confirm = reactive<{ show: boolean; message: string; run: () => void }>({
  show: false,
  message: '',
  run: () => {}
})

function askConfirm(message: string, run: () => void) {
  confirm.message = message
  confirm.run = run
  confirm.show = true
}

function onConfirmOk() {
  const run = confirm.run
  confirm.show = false
  confirm.run = () => {}
  run()
}

/**
 * 退出。有未保存的修改时先问一句——直接丢掉用户敲进去的东西是这一页最不能犯的错。
 */
function exit() {
  if (dirty.value) {
    askConfirm(t('files_editor_exit_confirm'), () => editor.close(sid))
    return
  }
  editor.close(sid)
}

async function save() {
  if (!dirty.value || st.value.saving) return
  try {
    await editor.save(sid)
    toastStore.show(t('files_editor_saved', { name: name.value }), 'success')
  } catch (e) {
    // 失败时编辑页保持打开、内容原样留着：用户要能重试或者复制出来
    toastStore.show(t('files_editor_save_failed', { error: describeError(e) }), 'error')
  }
}

async function revert() {
  const run = async () => {
    try {
      await editor.revert(sid)
    } catch (e) {
      toastStore.show(t('files_editor_load_failed', { error: describeError(e) }), 'error')
    }
  }
  if (dirty.value) {
    askConfirm(t('files_editor_revert_confirm'), () => void run())
    return
  }
  await run()
}

/** Ctrl/Cmd+S 保存：浏览器默认是「保存网页」，必须挡掉（prevent）。 */
function onKeydown(event: KeyboardEvent) {
  if ((event.ctrlKey || event.metaKey) && event.key.toLowerCase() === 's') {
    event.preventDefault()
    void save()
  }
}
</script>

<template>
  <!-- absolute inset-0：**覆盖**文件管理页面（文件管理器保持挂载，退出后目录、视图、滚动都在） -->
  <div class="absolute inset-0 z-30 flex flex-col bg-bg dark:bg-bg-dark">
    <div
      class="flex shrink-0 items-center gap-2 border-b border-line px-2 py-1.5 dark:border-line-dark"
    >
      <!-- 左上角：退出（回到文件管理，不动它的状态） -->
      <button
        type="button"
        class="g-btn-ghost h-8 shrink-0 !px-2"
        :title="t('files_editor_exit')"
        :aria-label="t('files_editor_exit')"
        @click="exit"
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
          <path d="M19 12H5" />
          <path d="m12 19-7-7 7-7" />
        </svg>
        <span>{{ t('files_editor_exit') }}</span>
      </button>

      <span class="min-w-0 flex-1 truncate font-mono text-xs" :title="st.path">{{ name }}</span>

      <span v-if="meta" class="shrink-0 font-mono text-xs text-ink-faint dark:text-ink-faint-dark">
        {{ meta }}
      </span>
      <!-- 未保存标记：右上角按钮的可用态已经说明了同一件事，但改完一个字符就想确认「存了没」时，
           一个常驻的标记比盯着按钮的灰度看要快 -->
      <span v-if="dirty" class="g-chip shrink-0">{{ t('files_editor_unsaved') }}</span>

      <!-- 右上角：还原 / 保存 -->
      <button
        type="button"
        class="g-btn-secondary h-8 shrink-0"
        :disabled="st.loading || st.saving"
        :title="t('files_editor_revert_hint')"
        @click="revert"
      >
        {{ t('files_editor_revert') }}
      </button>
      <button
        type="button"
        class="g-btn-primary h-8 shrink-0"
        :disabled="!dirty || st.saving"
        :title="t('files_editor_save_hint')"
        @click="save"
      >
        {{ st.saving ? t('files_editor_saving') : t('common_save') }}
      </button>
    </div>

    <textarea
      ref="area"
      v-model="st.content"
      class="thin-scroll min-h-0 flex-1 resize-none border-0 bg-transparent px-2 py-1.5 font-mono text-xs leading-relaxed text-ink outline-none dark:text-ink-dark"
      spellcheck="false"
      autocapitalize="off"
      autocomplete="off"
      autocorrect="off"
      :aria-label="t('files_editor_title', { name })"
      @keydown="onKeydown"
    />

    <ConfirmDialog
      :visible="confirm.show"
      :title="t('common_tip')"
      :message="confirm.message"
      :danger="true"
      @update:visible="confirm.show = $event"
      @confirm="onConfirmOk"
      @cancel="confirm.run = () => {}"
    />
  </div>
</template>
