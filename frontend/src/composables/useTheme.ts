// src/composables/useTheme.ts — 主题（浅色 / 深色 / 跟随系统），localStorage 持久化。
import { ref, computed, onMounted, onBeforeUnmount } from 'vue'

export type ThemeMode = 'light' | 'dark' | 'system'

const THEME_KEY = 'webssh-theme'

const themeMode = ref<ThemeMode>(
  ['light', 'dark', 'system'].includes(localStorage.getItem(THEME_KEY) || '')
    ? (localStorage.getItem(THEME_KEY) as ThemeMode)
    : 'system'
)
const systemPrefersDark = ref(
  typeof window !== 'undefined' && window.matchMedia('(prefers-color-scheme: dark)').matches
)

/** 当前是否深色（供 xterm 配色切换）。 */
const isDark = computed(
  () => themeMode.value === 'dark' || (themeMode.value === 'system' && systemPrefersDark.value)
)

function applyTheme(mode: ThemeMode) {
  const dark = mode === 'dark' || (mode === 'system' && systemPrefersDark.value)
  const root = document.documentElement
  if (dark) {
    root.classList.add('dark')
    root.style.colorScheme = 'dark'
  } else {
    root.classList.remove('dark')
    root.style.colorScheme = 'light'
  }
}

function setTheme(mode: ThemeMode) {
  themeMode.value = mode
  localStorage.setItem(THEME_KEY, mode)
  applyTheme(mode)
}

function cycleTheme() {
  const modes: ThemeMode[] = ['light', 'dark', 'system']
  setTheme(modes[(modes.indexOf(themeMode.value) + 1) % modes.length]!)
}

export function useTheme() {
  onMounted(() => {
    const saved = localStorage.getItem(THEME_KEY) as ThemeMode | null
    themeMode.value = saved && ['light', 'dark', 'system'].includes(saved) ? saved : 'system'
    applyTheme(themeMode.value)

    // 用**闭包里的** mql 注册与移除：模块级变量会被每个调用者覆盖，
    // 从它上面 remove 实际上移除的是「别人」的监听（身份不匹配）。
    const mql = window.matchMedia('(prefers-color-scheme: dark)')
    const handler = (e: MediaQueryListEvent) => {
      systemPrefersDark.value = e.matches
      if (themeMode.value === 'system') applyTheme('system')
    }
    mql.addEventListener('change', handler)
    onBeforeUnmount(() => mql.removeEventListener('change', handler))
  })

  return { themeMode, isDark, setTheme, cycleTheme, applyTheme }
}
