import { onMounted, onBeforeUnmount } from 'vue'

// useViewportHeight — 移动端虚拟键盘（可视视口）适配：把「可视视口」几何写进 <html> 的
// CSS 变量，由样式层消费。只在 App 壳层调用一次。
//
// 为什么需要：键盘弹起时，Chrome/Firefox for Android 按 meta viewport 的
// `interactive-widget=resizes-content` 收缩布局视口，CSS 高度单位随之变小；但 Safari 至今
// 不支持该指令，iOS 上布局视口与 100dvh 都不变、只有「可视视口」收缩。若壳层高度写死
// 100dvh，键盘就会盖住底部辅助键条与终端区域。
//
// 写入的变量（未设置时样式自行回退，见 style.css）：
//   --vvh            可视视口高度 → .app-shell 高度（键盘弹起时随之变矮）
//   --vvt            可视视口相对布局视口的位移 → .app-shell 上内边距（并等量补偿高度）
//   --kb-safe-bottom 键盘弹起时为 0px、其余情况移除 → 底部辅助键条的安全区内边距
//   --header-h       顶栏实测高度 → 需要「与内容区域对齐」的浮层用
const VVH = '--vvh'
const VVT = '--vvt'
const KB_SAFE_BOTTOM = '--kb-safe-bottom'
const HEADER_H = '--header-h'

// viewport 收缩超过该值即认为键盘弹起（避开 iOS 底部地址栏收合约 50~60px 的噪声）。
const KEYBOARD_MIN_INSET = 80

// 捏合缩放（scale > 1）时可视视口按比例变小而布局视口未变，此时应退回布局视口高度。
const MAX_SCALE = 1.01

export function useViewportHeight(): void {
  const root = document.documentElement
  let frame = 0

  // 顶栏实测高度：与 visualViewport 无关，必须先于下面的 early-return 挂上，
  // 否则不支持 visualViewport 的环境里 --header-h 永远缺失（浮层会按 0 处理顶栏）。
  let headerRo: ResizeObserver | null = null
  const measureHeader = () => {
    const header = document.querySelector('header.app-header')
    if (header) root.style.setProperty(HEADER_H, `${Math.round(header.getBoundingClientRect().height)}px`)
  }
  onMounted(() => {
    measureHeader()
    const headerEl = document.querySelector('header.app-header')
    if (window.ResizeObserver && headerEl) {
      headerRo = new ResizeObserver(() => measureHeader())
      headerRo.observe(headerEl)
    }
  })
  onBeforeUnmount(() => {
    headerRo?.disconnect()
    root.style.removeProperty(HEADER_H)
  })

  const vv = window.visualViewport
  // 不支持 visualViewport（老浏览器 / 老旧 WebView）：不写可视视口变量，样式层回退到
  // 100dvh，行为与改动前一致（顶栏变量已在上方写好）。
  if (!vv) return

  const apply = () => {
    frame = 0
    const zoomed = (vv.scale || 1) > MAX_SCALE
    const layoutHeight = window.innerHeight
    // 取两者较小值做防御：某些实现会在事件时点给出「比布局视口还大」的瞬时值。
    const height = zoomed ? layoutHeight : Math.min(vv.height, layoutHeight)
    const offset = zoomed ? 0 : Math.max(0, vv.offsetTop || 0)
    root.style.setProperty(VVH, `${Math.round(height)}px`)
    root.style.setProperty(VVT, `${Math.round(offset)}px`)
    // 键盘弹起时底部安全区（home indicator）已被键盘覆盖：交还给键盘，
    // 避免键条与键盘之间多出一条安全区的空隙。
    const inset = zoomed ? 0 : Math.max(0, layoutHeight - height)
    if (inset >= KEYBOARD_MIN_INSET) root.style.setProperty(KB_SAFE_BOTTOM, '0px')
    else root.style.removeProperty(KB_SAFE_BOTTOM)
  }

  // 键盘起落期间 resize/scroll 逐帧触发，用 rAF 合并为每帧一次样式写入。
  const schedule = () => {
    if (frame === 0) frame = requestAnimationFrame(apply)
  }

  apply() // 挂载前先写一次，避免首帧用错高度
  vv.addEventListener('resize', schedule)
  vv.addEventListener('scroll', schedule)

  onBeforeUnmount(() => {
    vv.removeEventListener('resize', schedule)
    vv.removeEventListener('scroll', schedule)
    if (frame) cancelAnimationFrame(frame)
    root.style.removeProperty(VVH)
    root.style.removeProperty(VVT)
    root.style.removeProperty(KB_SAFE_BOTTOM)
  })
}
