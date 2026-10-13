// src/stores/sessionUi.ts — 会话视图的界面状态（主区域模式 + 移动端子标签激活项）。
//
// 为什么不放在 terminals / sftp store 里：这些状态是「会话内多个面板共同关心的」——
// 终端标签与文件管理面板都要知道自己是不是当前显示的那个。放进任一业务 store 都会造成
// 两个 store 互相依赖，所以单独放这里。
//
// 两种形态（见 docs/CONTRACT.md 第 11.2 节）：
//   - 桌面：子标签栏只有终端标签；主区域在「终端」与「完整文件管理」之间切换（mainMode），
//     切换入口是左栏顶部的图标按钮。左栏常驻（用户明确要求去掉显隐开关）。
//   - 移动（窄屏）：没有左栏可用，因此保留一个固定的「文件管理」子标签（FILES_TAB），
//     用 activeSub 记激活项；mainMode 在移动端不参与渲染。
import { defineStore } from 'pinia'
import { ref } from 'vue'

/** 移动端固定的文件管理子标签 id（桌面端不再有常驻的文件管理标签）。 */
export const FILES_TAB = 'files'

/** 主区域显示什么：终端面板，还是完整文件管理。 */
export type MainMode = 'terminal' | 'files'

export const useSessionUiStore = defineStore('sessionUi', () => {
  // sid → 激活的子标签 id（移动端：'files' 或某个终端标签的 uid）
  const activeSub = ref<Record<string, string>>({})
  // sid → 桌面端主区域模式；缺省 = 终端
  const mainMode = ref<Record<string, MainMode>>({})

  function setActiveSub(sid: string, id: string) {
    activeSub.value[sid] = id
  }

  function activeSubOf(sid: string): string {
    return activeSub.value[sid] ?? FILES_TAB
  }

  function isFilesActive(sid: string): boolean {
    return activeSubOf(sid) === FILES_TAB
  }

  // ---------- 桌面：主区域双模式 ----------

  function mainModeOf(sid: string): MainMode {
    return mainMode.value[sid] ?? 'terminal'
  }

  function setMainMode(sid: string, mode: MainMode) {
    mainMode.value[sid] = mode
  }

  /** 切换并返回切换后的模式（调用方常需要据此决定要不要聚焦终端）。 */
  function toggleMainMode(sid: string): MainMode {
    const next: MainMode = mainModeOf(sid) === 'files' ? 'terminal' : 'files'
    mainMode.value[sid] = next
    return next
  }

  /** 会话被关闭时丢掉它的界面状态（否则记录只增不减）。 */
  function forget(sid: string) {
    delete activeSub.value[sid]
    delete mainMode.value[sid]
  }

  return {
    activeSub,
    mainMode,
    setActiveSub,
    activeSubOf,
    isFilesActive,
    mainModeOf,
    setMainMode,
    toggleMainMode,
    forget
  }
})
