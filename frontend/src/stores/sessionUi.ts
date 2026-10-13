// src/stores/sessionUi.ts — 会话视图的界面状态（哪个子标签是激活的）。
//
// 为什么不放在 terminals / sftp store 里：这份状态是「会话内多个面板共同关心的」——
// 终端面板与文件管理面板都要知道自己是不是当前显示的那个。放进任一业务 store 都会造成
// 两个 store 互相依赖，所以单独放这里。
//
// **单一真相**：`activeSubOf(sid)` 是「当前激活子标签」的唯一来源。
//   - 值的形态：某个终端标签的 uid，或 `FILES_TAB`（固定的「文件管理」子标签）。
//   - 桌面端：`FILES_TAB` 与「左栏面包屑/目录树」是**二选一**的关系——切到文件管理子标签时
//     左栏收起（文件管理器自带面包屑），切回终端标签时左栏展开。规则写在 SessionView 里，
//     这里只提供状态（早先那版把「主区域模式」也放进来，结果两套开关会互相打架，已删掉）。
//   - 移动端：没有左栏，`FILES_TAB` 就是一个占据全区域的子页面。
import { defineStore } from 'pinia'
import { ref } from 'vue'

/** 固定的「文件管理」子标签 id（不参与终端标签的水平滚动）。 */
export const FILES_TAB = 'files'

export const useSessionUiStore = defineStore('sessionUi', () => {
  // sid → 激活的子标签 id（'files' 或某个终端标签的 uid）
  const activeSub = ref<Record<string, string>>({})

  function setActiveSub(sid: string, id: string) {
    activeSub.value[sid] = id
  }

  /** 当前激活的子标签；没有记录时默认「文件管理」（与「一个终端都没有」时的展示一致）。 */
  function activeSubOf(sid: string): string {
    return activeSub.value[sid] ?? FILES_TAB
  }

  function isFilesActive(sid: string): boolean {
    return activeSubOf(sid) === FILES_TAB
  }

  /** 会话被关闭时丢掉它的界面状态（否则记录只增不减）。 */
  function forget(sid: string) {
    delete activeSub.value[sid]
  }

  return {
    activeSub,
    setActiveSub,
    activeSubOf,
    isFilesActive,
    forget
  }
})
