import { createApp } from 'vue'
import { createPinia } from 'pinia'
import App from './App.vue'
// 终端字体（Maple Mono CN）只内置 **Regular 400** 一个字重：终端字号范围 10–26px，
// Regular 是唯一必需的常规字重；粗体交给浏览器合成，不必再塞一个 ~9MB 的字重进来。
import '@automann/maple-mono-cn/regular.css'
import { FONT_FAMILY_KEY } from '@/stores/settings'
import './style.css'
import '@xterm/xterm/css/xterm.css'

// 尽早开始加载终端字体：字体文件按 unicode-range 切片，带中英样例文本可让 latin 与首个
// CJK 切片先在本机（unix socket）取回来。等终端面板真正 open() 时通常已就绪
// （TerminalPane 里另有一道等待与重测兜底）。选「系统字体」时不必预热。
if (localStorage.getItem(FONT_FAMILY_KEY) !== 'system') {
  void document.fonts.load('16px "Maple Mono CN"', 'Aa中0')
}

createApp(App).use(createPinia()).mount('#app')
