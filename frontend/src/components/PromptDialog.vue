<script setup lang="ts">
// PromptDialog — 通用输入框（重命名 / 新建文件名/目录，契约 6.2 的签名，文件管理模块也在用）。
//
// 输入校验（是否为空、是否含 `/`）**交给调用方**：不同场景的规则不同（标签名允许空格，
// 文件名不允许 `/`），弹窗只负责收集一个字符串。
import { nextTick, ref, watch } from 'vue'
import { t } from '@/i18n'

const props = defineProps<{
  visible: boolean
  title: string
  label?: string
  placeholder?: string
  initialValue?: string
  confirmText?: string
  cancelText?: string
}>()

const emit = defineEmits<{
  (e: 'update:visible', v: boolean): void
  (e: 'confirm', value: string): void
  (e: 'cancel'): void
}>()

const open = ref(props.visible)
const value = ref(props.initialValue ?? '')
const inputEl = ref<HTMLInputElement | null>(null)

watch(
  () => props.visible,
  (v) => {
    open.value = v
    if (!v) return
    value.value = props.initialValue ?? ''
    // 每次打开都聚焦并全选：重命名几乎总是整体替换，省一次手动全选。
    // 放在 nextTick 里是因为面板此刻才从 v-if 渲染出来，立即 focus 会落到 null 上。
    void nextTick(() => {
      inputEl.value?.focus()
      inputEl.value?.select()
    })
  },
  { immediate: true }
)

function close() {
  open.value = false
  emit('update:visible', false)
}

function onConfirm() {
  emit('confirm', value.value)
  close()
}

function onCancel() {
  emit('cancel')
  close()
}
</script>

<template>
  <Teleport to="body">
    <Transition name="modal-fade">
      <div v-if="open" class="fixed inset-0 z-50">
        <div class="absolute inset-0 bg-black/50" @click="onCancel"></div>
        <!-- 与其它弹窗同一条定位带（.region-center）；输入框场景尤其不能退到
             fixed inset-0 居中：iOS 键盘弹起时面板会被键盘盖住。 -->
        <div class="region-center absolute left-0 right-0 flex items-center justify-center px-4">
          <div
            class="pointer-events-auto relative max-h-[90%] w-full max-w-sm overflow-y-auto rounded-xl border border-line bg-surface p-5 shadow-pop dark:border-line-dark dark:bg-surface-dark"
          >
            <h3 class="font-display text-base font-semibold text-ink dark:text-ink-dark">
              {{ title }}
            </h3>
            <label v-if="label" class="mt-3 block text-xs font-medium text-ink-soft dark:text-ink-soft-dark">
              {{ label }}
            </label>
            <input
              ref="inputEl"
              v-model="value"
              class="g-input mt-1.5"
              :placeholder="placeholder"
              @keydown.enter.prevent="onConfirm"
              @keydown.esc.prevent="onCancel"
            />
            <div class="mt-5 flex justify-end gap-2">
              <button class="g-btn-ghost !h-9 !px-4 text-sm" @click="onCancel">
                {{ cancelText || t('common_cancel') }}
              </button>
              <button class="g-btn-primary !h-9 !px-4 text-sm" @click="onConfirm">
                {{ confirmText || t('common_ok') }}
              </button>
            </div>
          </div>
        </div>
      </div>
    </Transition>
  </Teleport>
</template>
