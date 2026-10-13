<script setup lang="ts">
// Toast — 全局轻提示（队列渲染）。
//
// 定位带：贴在顶栏下方（`--vvt` 跟随可视视口位移、`--header-h` 是顶栏实测高度，见
// useViewportHeight）。**不用** `fixed top-2`：iOS 键盘弹起/地址栏收缩时可视视口会上移，
// 写死的 top 会把提示顶到屏幕外；也不盖在标签栏上（标签栏里全是可点目标，提示浮在上面
// 会挡住用户正在点的标签）。
import { useToastStore } from '@/stores/toast'

const toast = useToastStore()
</script>

<template>
  <Teleport to="body">
    <div
      class="fixed inset-x-0 z-[100] flex flex-col items-center gap-2 px-4 pointer-events-none"
      style="top: calc(var(--vvt, 0px) + var(--header-h, calc(3rem + env(safe-area-inset-top, 0px))) + 8px)"
    >
      <TransitionGroup
        enter-active-class="transition duration-200 ease-out"
        enter-from-class="opacity-0 -translate-y-2"
        leave-active-class="transition duration-150 ease-in"
        leave-to-class="opacity-0"
      >
        <div
          v-for="tb in toast.toasts"
          :key="tb.id"
          class="max-w-full rounded-lg border px-4 py-2 text-sm font-medium shadow-pop"
          :class="
            tb.type === 'success'
              ? 'border-transparent bg-success text-white'
              : tb.type === 'error'
                ? 'border-transparent bg-danger text-white'
                : 'border-line bg-surface text-ink dark:border-line-dark dark:bg-surface-dark dark:text-ink-dark'
          "
        >
          {{ tb.msg }}
        </div>
      </TransitionGroup>
    </div>
  </Teleport>
</template>
