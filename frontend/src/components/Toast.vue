<script setup lang="ts">
// Toast — 全局轻提示（队列渲染）。
//
// 定位带：**就浮在顶栏（标签栏）那一条里，水平居中**（第七轮用户要求）。`--vvt` 跟随可视视口
// 位移、`--header-h` 是顶栏实测高度（见 useViewportHeight），所以 iOS 键盘弹起/地址栏收缩时
// 提示会跟着顶栏走，不会跑到屏幕外。
//
// 会不会挡住标签？不会——容器是 `pointer-events-none`，底下那条标签栏照样能点、能拖。
// （第五轮之前这里刻意贴在顶栏**下方**，第七轮按用户要求改成居中放进标签栏。）
//
// 多条提示叠起来时**向下溢出**，不向上：所以这里用「按单条高度算出来的上内边距」而不是把整列
// 竖直居中（`justify-center`）——整列居中时，两条提示会比顶栏还高，上面那条会被视口顶端切掉。
import { useToastStore } from '@/stores/toast'

const toast = useToastStore()
</script>

<template>
  <Teleport to="body">
    <div
      class="pointer-events-none fixed inset-x-0 z-[100] flex flex-col items-center gap-2 px-4"
      style="top: var(--vvt, 0px); padding-top: max(2px, calc((var(--header-h, calc(3rem + env(safe-area-inset-top, 0px))) - 2.25rem) / 2))"
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
