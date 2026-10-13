<script setup lang="ts">
// QuickCmdEditDialog — 新增 / 编辑单条快捷指令（由 QuickCmdsDialog 打开）。
//
// 只收集字段并做「不能为空」的校验，保存与持久化交给 quickCmds store（它负责乐观更新与
// 失败回滚）。校验文案用 toast/内联提示，不再另开弹窗。
import { ref, watch } from 'vue'
import { t } from '@/i18n'
import type { QuickCmd } from '@/serverapi'

const props = defineProps<{
  visible: boolean
  /** null = 新增 */
  cmd: QuickCmd | null
}>()

const emit = defineEmits<{
  (e: 'update:visible', v: boolean): void
  (e: 'save', payload: { name: string; content: string; auto: boolean }): void
}>()

const open = ref(props.visible)
const name = ref('')
const content = ref('')
const auto = ref(false)
const error = ref('')

watch(
  () => props.visible,
  (v) => {
    open.value = v
    if (!v) return
    name.value = props.cmd?.name ?? ''
    content.value = props.cmd?.content ?? ''
    auto.value = props.cmd?.auto ?? false
    error.value = ''
  },
  { immediate: true }
)

function close() {
  open.value = false
  emit('update:visible', false)
}

function save() {
  if (!name.value.trim()) {
    error.value = t('qc_name_required')
    return
  }
  if (!content.value.trim()) {
    error.value = t('qc_content_required')
    return
  }
  // 只 trim 名称：命令内容里的空格/换行是有意义的，不能顺手改掉
  emit('save', { name: name.value.trim(), content: content.value, auto: auto.value })
  close()
}
</script>

<template>
  <Teleport to="body">
    <Transition name="modal-fade">
      <div v-if="open" class="fixed inset-0 z-[60]">
        <div class="absolute inset-0 bg-black/50" @click="close"></div>
        <!-- 弹窗层级比快捷指令列表高（z-60）：这个框是从那个列表里开出来的 -->
        <div class="region-center absolute left-0 right-0 flex items-center justify-center px-4">
          <div
            class="pointer-events-auto relative max-h-[90%] w-full max-w-md overflow-y-auto rounded-xl border border-line bg-surface p-5 shadow-pop dark:border-line-dark dark:bg-surface-dark"
          >
            <h3 class="font-display text-base font-semibold text-ink dark:text-ink-dark">
              {{ cmd ? t('qc_edit_title') : t('qc_add_title') }}
            </h3>

            <div class="mt-4 space-y-3">
              <div>
                <label for="field-quickcmdeditdialog-1" class="mb-1 block text-xs font-medium text-ink-soft dark:text-ink-soft-dark">
                  {{ t('qc_name') }}
                </label>
                <input id="field-quickcmdeditdialog-1"
                  v-model="name"
                  class="g-input"
                  :placeholder="t('qc_name_placeholder')"
                  @keydown.enter.prevent="save"
                />
              </div>
              <div>
                <label for="field-quickcmdeditdialog-2" class="mb-1 block text-xs font-medium text-ink-soft dark:text-ink-soft-dark">
                  {{ t('qc_content') }}
                </label>
                <textarea id="field-quickcmdeditdialog-2"
                  v-model="content"
                  class="g-input resize-y font-mono text-xs"
                  rows="3"
                  spellcheck="false"
                  :placeholder="t('qc_content_placeholder')"
                ></textarea>
              </div>
              <label class="flex cursor-pointer select-none items-center gap-2">
                <input v-model="auto" type="checkbox" class="h-4 w-4 accent-brand" />
                <span class="text-sm text-ink dark:text-ink-dark">{{ t('qc_auto') }}</span>
                <span class="text-xs text-ink-faint dark:text-ink-faint-dark">{{ t('qc_auto_hint') }}</span>
              </label>
              <p v-if="error" class="text-xs text-danger">{{ error }}</p>
            </div>

            <div class="mt-5 flex justify-end gap-2">
              <button class="g-btn-ghost !h-9 !px-4 text-sm" @click="close">
                {{ t('common_cancel') }}
              </button>
              <button class="g-btn-primary !h-9 !px-5 text-sm" @click="save">
                {{ t('common_save') }}
              </button>
            </div>
          </div>
        </div>
      </div>
    </Transition>
  </Teleport>
</template>
