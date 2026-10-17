// src/stores/settings.ts — 本设备的显示偏好（终端字号/字体、首页卡片排序），只存浏览器 localStorage。
//
// 这些是「本设备的显示偏好」，不是会话数据：换台设备用同一个会话应该有各自的字号，
// 所以不进后端数据目录（与主题、语言一致）。
import { defineStore } from 'pinia'
import { ref } from 'vue'

/**
 * 终端字体家族的存储键。
 *
 * ⚠️ `main.ts`（冻结文件）直接读这个 key 判断要不要预热 Maple Mono 字体文件，
 * 所以名字必须与契约 6.1 一致，且值只能是 'maple' | 'system'。
 */
export const FONT_FAMILY_KEY = 'webssh-font-family'
/** 终端字号存储键（其余地方不需要，故不导出）。 */
const FONT_SIZE_KEY = 'webssh-font-size'

export const FONT_SIZE_MIN = 10
export const FONT_SIZE_MAX = 26
const FONT_SIZE_DEFAULT = 16

export type TerminalFont = 'maple' | 'system'

/**
 * 首页会话卡片的排序方式（本设备的界面偏好）。
 *
 * - `recent`：默认，最近使用优先（`lastUsedAt → updatedAt → createdAt` 降序，见 HomeView）；
 * - `name`：按展示名升序（空名回退 `user@host`，与卡片标题同口径）。
 *
 * 放 localStorage 而不是会话配置：这是「这台设备上怎么看列表」的偏好，换台设备不该跟着变
 * （与字号、主题一致）。
 */
export type HomeSort = 'recent' | 'name'
const HOME_SORT_KEY = 'webssh-home-sort'

/** 字号钳制：越界的输入（手改 localStorage、旧版本留下的脏值）一律夹回合法区间。 */
function clampFont(n: number): number {
  if (!Number.isFinite(n)) return FONT_SIZE_DEFAULT
  return Math.min(FONT_SIZE_MAX, Math.max(FONT_SIZE_MIN, Math.round(n)))
}

export const useSettingsStore = defineStore('settings', () => {
  const fontSize = ref(clampFont(Number(localStorage.getItem(FONT_SIZE_KEY)) || FONT_SIZE_DEFAULT))

  function setFontSize(n: number) {
    fontSize.value = clampFont(n)
    localStorage.setItem(FONT_SIZE_KEY, String(fontSize.value))
  }

  // 默认内置 Maple Mono；只有显式存过 'system' 才用系统等宽字体栈。
  const terminalFont = ref<TerminalFont>(
    localStorage.getItem(FONT_FAMILY_KEY) === 'system' ? 'system' : 'maple'
  )

  function setTerminalFont(v: TerminalFont) {
    terminalFont.value = v === 'system' ? 'system' : 'maple'
    localStorage.setItem(FONT_FAMILY_KEY, terminalFont.value)
  }

  // 脏值（手改 localStorage / 旧版本留下的值）一律当默认，不让界面进入未知状态。
  const homeSort = ref<HomeSort>(localStorage.getItem(HOME_SORT_KEY) === 'name' ? 'name' : 'recent')

  function toggleHomeSort() {
    homeSort.value = homeSort.value === 'name' ? 'recent' : 'name'
    localStorage.setItem(HOME_SORT_KEY, homeSort.value)
  }

  return { fontSize, terminalFont, setFontSize, setTerminalFont, homeSort, toggleHomeSort }
})
