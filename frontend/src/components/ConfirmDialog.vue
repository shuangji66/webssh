<script setup lang="ts">
// ConfirmDialog — 通用确认框（契约 6.2 的签名，多个模块在用，不要改 props/emits 名）。
//
// 关闭与确认分成两个 emit：`update:visible` 只同步显隐，`confirm` 才是用户按下确认。
// 参考项目里踩过的坑：调用方把「待处理目标」挂在 `update:visible` 上清空，结果弹窗一关
// 目标就没了，confirm 触发时什么也删不掉。这里保证顺序是 confirm/cancel 先发、再关。
import { ref, watch } from 'vue'
import { t } from '@/i18n'

const props = defineProps<{
  visible: boolean
  title: string
  message: string
  confirmText?: string
  cancelText?: string
  danger?: boolean
}>()

const emit = defineEmits<{
  (e: 'update:visible', v: boolean): void
  (e: 'confirm'): void
  (e: 'cancel'): void
}>()

const open = ref(props.visible)
watch(
  () => props.visible,
  (v) => {
    open.value = v
  },
  { immediate: true }
)

function close() {
  open.value = false
  emit('update:visible', false)
}

function onConfirm() {
  emit('confirm')
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
        <!-- 遮罩铺满全屏（点任意处 = 取消，含顶栏区域） -->
        <div class="absolute inset-0 bg-black/50" @click="onCancel"></div>
        <!-- 面板在「内容区域」内垂直居中（.region-center 已扣掉顶栏与底部键条）。
             不使用 fixed inset-0 居中：那相对布局视口，iOS 键盘弹起时布局视口不缩，
             弹窗会被键盘盖住（本弹窗经常从终端里唤起）。 -->
        <div class="region-center absolute left-0 right-0 flex items-center justify-center px-4">
          <div
            class="pointer-events-auto relative max-h-[90%] w-full max-w-sm overflow-y-auto rounded-xl border border-line bg-surface p-5 shadow-pop dark:border-line-dark dark:bg-surface-dark"
          >
            <h3 class="font-display text-base font-semibold text-ink dark:text-ink-dark">
              {{ title }}
            </h3>
            <p class="mt-2 whitespace-pre-line break-words text-sm text-ink-soft dark:text-ink-soft-dark">
              {{ message }}
            </p>
            <div class="mt-5 flex justify-end gap-2">
              <button class="g-btn-ghost !h-9 !px-4 text-sm" @click="onCancel">
                {{ cancelText || t('confirm_cancel') }}
              </button>
              <button
                class="!h-9 !px-4 text-sm"
                :class="danger ? 'g-btn-danger' : 'g-btn-primary'"
                @click="onConfirm"
              >
                {{ confirmText || t('common_ok') }}
              </button>
            </div>
          </div>
        </div>
      </div>
    </Transition>
  </Teleport>
</template>
