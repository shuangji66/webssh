// src/stores/paneControls.ts — 各终端面板暴露给界面的命令出口（粘贴/清屏/重连/搜索/聚焦/发送）。
//
// 为什么用注册表而不是「让工具栏直接找组件」：工具栏在 SessionView（子标签栏右侧）里，
// 与 TerminalPane 没有父子关系，只能靠一份注册表把命令转过去。
//
// 为什么按 **(sid, uid) 两级**登记：uid 是浏览器本地生成的，两个会话并行创建时理论上可能
// 撞名；只用 uid 会让后注册的标签覆盖前一个 —— 表现为「点清屏清掉的是另一个会话的终端」。
// 加一层 sid 前缀就完全不会互相干扰（关掉整个会话时按 sid 一次清空）。
//
// ⚠️ `connected` 的写法有坑：它必须**通过 get() 拿到的对象**去改。注册表是 reactive 的，
// get() 返回的是内层对象的代理，只有改代理才会通知依赖它的 computed 重算；组件自己持有的
// 原始对象直接赋值不会触发任何更新（工具栏的按钮就会一直停在旧状态）。
import { defineStore } from 'pinia'
import { reactive } from 'vue'

export interface PaneControls {
  paste: () => void
  clear: () => void
  reconnect: () => void
  search: () => void
  focus: () => void
  /** 发送一段内容到 PTY；未连接返回 false（调用方据此提示「终端未连接」） */
  send: (data: string) => boolean
  connected: boolean
}

export const usePaneControlsStore = defineStore('paneControls', () => {
  // sid → (uid → 控制句柄)
  const registry = reactive<Record<string, Record<string, PaneControls>>>({})

  function register(sid: string, uid: string, ctl: PaneControls): void {
    const bucket = registry[sid] ?? (registry[sid] = {})
    bucket[uid] = ctl
  }

  function unregister(sid: string, uid: string): void {
    const bucket = registry[sid]
    if (!bucket) return
    delete bucket[uid]
    // 空桶不留下：会话关掉后注册表不该慢慢长大
    if (Object.keys(bucket).length === 0) delete registry[sid]
  }

  function get(sid: string, uid: string | null | undefined): PaneControls | undefined {
    if (!uid) return undefined
    return registry[sid]?.[uid]
  }

  return { registry, register, unregister, get }
})
