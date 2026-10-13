// src/stores/sessionUi.ts — 会话视图的界面状态（子标签激活项、左栏显隐）。
//
// 为什么不放在 terminals / sftp store 里：这两份状态是「会话内多个面板共同关心的」——
// 终端标签与文件管理标签都要知道自己是不是当前激活项，左栏（面包屑 + 文件树）也要由
// 文件树组件来切换。放进任一业务 store 都会造成两个 store 互相依赖，所以单独放这里。
//
// 约定：**子标签激活项只有这一个来源**。终端标签激活时写自己的 uid，文件管理标签激活时
// 写 FILES_TAB；键盘/工具栏等所有入口都通过 setActiveSub 切换。
import { defineStore } from 'pinia'
import { ref } from 'vue'

/** 固定的文件管理子标签 id（不参与终端标签的水平滚动）。 */
export const FILES_TAB = 'files'

export const useSessionUiStore = defineStore('sessionUi', () => {
  // sid → 激活的子标签 id（'files' 或某个终端标签的 uid）
  const activeSub = ref<Record<string, string>>({})
  // sid → 左栏（面包屑 + 文件树）是否展开；桌面端默认展开，移动端不渲染左栏
  const leftVisible = ref<Record<string, boolean>>({})

  function setActiveSub(sid: string, id: string) {
    activeSub.value[sid] = id
  }

  function activeSubOf(sid: string): string {
    return activeSub.value[sid] ?? FILES_TAB
  }

  function isFilesActive(sid: string): boolean {
    return activeSubOf(sid) === FILES_TAB
  }

  /** 左栏默认展开（桌面端）；显式关掉过就一直关着，直到用户再点开。 */
  function leftIsVisible(sid: string): boolean {
    return leftVisible.value[sid] !== false
  }

  function setLeftVisible(sid: string, v: boolean) {
    leftVisible.value[sid] = v
  }

  function toggleLeft(sid: string) {
    leftVisible.value[sid] = !leftIsVisible(sid)
  }

  /** 会话被关闭时丢掉它的界面状态（否则记录只增不减）。 */
  function forget(sid: string) {
    delete activeSub.value[sid]
    delete leftVisible.value[sid]
  }

  return {
    activeSub,
    leftVisible,
    setActiveSub,
    activeSubOf,
    isFilesActive,
    leftIsVisible,
    setLeftVisible,
    toggleLeft,
    forget
  }
})
