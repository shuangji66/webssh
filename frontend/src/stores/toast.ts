// src/stores/toast.ts — 轻提示（右下/顶部浮层用的队列）。
//
// 为什么是队列而不是单条：一次操作可能同时产生「已关闭会话」+「后端已重启」两条提示，
// 单条实现下后一条会立刻盖掉前一条，用户永远看不到前一条。定时器挂在 show() 上而不是
// 组件里：组件（Toast.vue）只负责渲染，切页面/重挂载不会让提示提前消失。
import { defineStore } from 'pinia'
import { ref } from 'vue'

export interface ToastItem {
  id: number
  msg: string
  type: 'success' | 'error' | 'info'
}

const DEFAULT_DURATION = 2600

let seq = 0

export const useToastStore = defineStore('toast', () => {
  const toasts = ref<ToastItem[]>([])

  function show(msg: string, type: ToastItem['type'] = 'info') {
    const id = ++seq
    toasts.value.push({ id, msg, type })
    window.setTimeout(() => dismiss(id), DEFAULT_DURATION)
  }

  function dismiss(id: number) {
    toasts.value = toasts.value.filter((tb) => tb.id !== id)
  }

  return { toasts, show, dismiss }
})
