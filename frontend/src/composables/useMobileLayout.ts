// useMobileLayout.ts — 「是否按移动端（触屏）布局渲染」的判定。
//
// 用途：触屏专属 UI 的显隐判据（底部辅助键条、会话视图的单列布局）。
//
// 为什么不能按视口宽度判定：iPad 的 CSS 宽度是 768 / 834 / 1024px，全部 ≥ Tailwind 的 md
// 断点，用 `md:hidden` 会把 iPad 判成桌面 —— 辅助键条被隐藏，触屏上就再也没有 ESC/Tab/
// Ctrl/Alt/方向键可用。屏幕大不等于有键盘：大屏触屏必须按「输入能力」判定，宽度只作为
// 「窄窗口也当移动端用」的补充。
import { readonly, ref, type Ref } from 'vue'

// 与 Tailwind md 断点互补：md 是 (width >= 768px)，这里取 (max-width: 767px)。
const MOBILE_LAYOUT_QUERY = '(max-width: 767px), (hover: none) and (pointer: coarse)'

// 移动 / 平板平台标识（iPadOS 的 Safari UA 是 "Macintosh; Intel Mac OS X"，
// 与 macOS 只能靠 maxTouchPoints 区分）。
const TOUCH_PLATFORM_RE = /Android|iPhone|iPad|iPod|Macintosh/i

function isTouchPlatform(): boolean {
  if (typeof navigator === 'undefined') return false
  return (navigator.maxTouchPoints ?? 0) > 0 && TOUCH_PLATFORM_RE.test(navigator.userAgent || '')
}

const query =
  typeof window !== 'undefined' && window.matchMedia
    ? window.matchMedia(MOBILE_LAYOUT_QUERY)
    : null

const isMobileLayout = ref((query?.matches ?? false) || isTouchPlatform())

query?.addEventListener('change', () => {
  isMobileLayout.value = query.matches || isTouchPlatform()
})

/** 只读：当前是否按移动端（触屏）布局渲染。全局设备属性，所有调用者共享一份结果。 */
export function useMobileLayout(): Readonly<Ref<boolean>> {
  return readonly(isMobileLayout)
}

// ---------- 「宽布局」判定（平板档，用于键条双页并排） ----------
// 判据就是 Tailwind 的 md 断点（>=768px），与「桌面功能行」用的是同一条线：
// 手机竖屏 <768 → 单页 + 左右切换；平板（iPad 768/834/1024）及以上 → 两页并排同时显示。
const WIDE_LAYOUT_QUERY = '(min-width: 768px)'

const wideQuery =
  typeof window !== 'undefined' && window.matchMedia ? window.matchMedia(WIDE_LAYOUT_QUERY) : null

const isWideLayout = ref(wideQuery?.matches ?? false)

wideQuery?.addEventListener('change', () => {
  isWideLayout.value = wideQuery.matches
})

/** 只读：当前是否宽布局（>=md 断点）。键条据此决定「两页并排」还是「单页 + 切换」。 */
export function useWideLayout(): Readonly<Ref<boolean>> {
  return readonly(isWideLayout)
}
