// src/stores/terminals.ts — 会话内的终端子标签（一个远程会话可以开多个终端）。
//
// 为什么不并进 home store：home 管的是「远程会话的配置 + 后端实时状态 + 应用级标签」，
// 而终端标签是**会话内部**的东西（一个 sid 一组标签 + 一个激活项）。混在一起会让 home
// 的状态形状变成「会话里再套一层终端」，关闭会话时的清理顺序也难以一眼看清。
// 也不并进 sessionUi：那是纯界面态（只记「当前激活哪个子标签」），没有标签本身的生命周期。
//
// 激活项的**唯一来源**是 sessionUi：本 store 每次改激活项都会同步 setActiveSub(sid, uid)，
// 于是 SessionView / 工具栏都只需要读 sessionUi 就能切面板。反向读取时，
// 激活的是文件管理子标签（FILES_TAB）就说明「当前没有激活的终端」→ activeOf() 返回 null
// （这比「返回最后一个终端」诚实：文件管理面板在前台时，把内容发进隐藏的终端会让人困惑）。
import { defineStore } from 'pinia'
import { ref } from 'vue'
import { api, describeError, type SessionState } from '@/serverapi'
import { t } from '@/i18n'
import { FILES_TAB, useSessionUiStore } from '@/stores/sessionUi'
import { useToastStore } from '@/stores/toast'

export type TermStatus = 'connecting' | 'open' | 'exited' | 'error' | 'detached'

export interface TermTab {
  /** 前端稳定标识（标签状态机的本地主键）；后端只认 tid */
  uid: string
  /** 后端终端 id；新建终端时由 WS 控制帧 `\x1b]tid;<id>\x07` 回填，之前为 null */
  tid: string | null
  /** 用户自定义标题（'' = 用默认「终端 N」） */
  title: string
  status: TermStatus
  /** 是否正在等后端回放历史（`\x1b]ready\x07` 到达后清除；见 TerminalPane） */
  restoring: boolean
  createdAt: string
}

// uid 只在浏览器里用：时间戳 + 自增序号即可（同一毫秒内连开两个也不会撞）。
let uidSeq = 0
function newUid(): string {
  uidSeq += 1
  return `term-${Date.now().toString(36)}-${uidSeq}`
}

export const useTerminalsStore = defineStore('terminals', () => {
  const ui = useSessionUiStore()
  const toast = useToastStore()

  // sid → 该会话的终端标签（顺序即子标签栏的显示顺序）
  const tabs = ref<Record<string, TermTab[]>>({})
  // sid → 激活项的 uid。它记的是「本会话最后一次激活的终端」，即使用户切到文件管理子标签
  // 也不清空（用于关闭标签后挑下一个激活项）；对外读取一律走 activeOf()。
  const activeUid = ref<Record<string, string>>({})

  function listOf(sid: string): TermTab[] {
    return tabs.value[sid] ?? []
  }

  function find(sid: string, uid: string): TermTab | undefined {
    return listOf(sid).find((tab) => tab.uid === uid)
  }

  function tabsOf(sid: string): TermTab[] {
    return listOf(sid)
  }

  function countOf(sid: string): number {
    return listOf(sid).length
  }

  /** 当前激活的终端标签；激活的是文件管理子标签（或激活项已失效且无兜底）时返回 null。 */
  function activeOf(sid: string): TermTab | null {
    const sub = ui.activeSubOf(sid)
    // 文件管理子标签在前台 = 明确表示「当前没有激活的终端」，直接返回 null：
    // 唯一真相在 sessionUi，这里不能再退回「上次激活的终端」（否则文件管理页里发快捷指令
    // 会打进一个看不见的终端）。
    if (!sub || sub === FILES_TAB) return null
    const tab = find(sid, sub)
    if (tab) return tab
    // sessionUi 指向的标签已经不存在（幽灵 uid，比如标签被关掉时 sessionUi 尚未跟上）：
    // 退回本 store 记的激活项，避免出现「有标签却一个也激活不了」的中间态。
    const fallback = activeUid.value[sid]
    return fallback ? find(sid, fallback) ?? null : null
  }

  /** 改激活项：本 store 与 sessionUi 一起写，保证两边不会各说各话。 */
  function setActive(sid: string, uid: string) {
    activeUid.value[sid] = uid
    ui.setActiveSub(sid, uid)
  }

  /**
   * 从后端的实时状态恢复标签（启动/刷新时用）。**幂等**：已有相同 tid 的标签不重复加，
   * 也不再改动已有标签的状态（否则一次刷新就把用户本地状态覆盖掉了）。
   */
  function ensure(sid: string, state: SessionState): void {
    const list = tabs.value[sid] ?? (tabs.value[sid] = [])
    for (const info of state.terminals ?? []) {
      if (list.some((tab) => tab.tid === info.id)) continue
      list.push({
        uid: newUid(),
        tid: info.id,
        title: info.title ?? '',
        // 后端上报 exited 的终端其 PTY 已死：标成 exited（状态点变灰），
        // 而不是假装 open —— 后者会让用户以为「打字没反应」是前端卡住了。
        status: info.exited ? 'exited' : 'open',
        // 恢复出来的标签一律要经历一次历史回放，提示条挂到 READY 为止
        restoring: true,
        createdAt: info.createdAt || new Date().toISOString()
      })
    }
    // 恢复/打开会话时若本会话还没有激活项，落在第一个终端上。
    // 否则桌面端刷新后会停在文件管理子标签，而「刚打开的会话要看的是终端」。
    if (!activeUid.value[sid] && list[0]) setActive(sid, list[0].uid)
  }

  /** 新建一个终端标签（tid 为 null，等后端回填）。新建即激活：用户点 + 就是想用它。 */
  function add(sid: string): TermTab {
    const tab: TermTab = {
      uid: newUid(),
      tid: null,
      title: '',
      status: 'connecting',
      // 新终端没有历史可回放，不必等 READY（后端对新建的终端只回 tid 后直接 attach）
      restoring: false,
      createdAt: new Date().toISOString()
    }
    const list = tabs.value[sid] ?? (tabs.value[sid] = [])
    list.push(tab)
    setActive(sid, tab.uid)
    return tab
  }

  /** 后端回填终端 id（WS 控制帧）。 */
  function adoptTid(sid: string, uid: string, tid: string, createdAt?: string): void {
    const tab = find(sid, uid)
    if (!tab) return
    tab.tid = tid
    // 以后端的创建时间覆盖本地占位时间，标签上的「多久之前」才与历史文件一致
    if (createdAt) tab.createdAt = createdAt
  }

  function setStatus(sid: string, uid: string, status: TermStatus): void {
    const tab = find(sid, uid)
    if (tab) tab.status = status
  }

  /**
   * 重命名终端。先改本地（界面立刻响应），再落盘到后端。
   * 失败**不回滚**本地标题：用户看到的名字就是他刚输入的那个，回滚只会让界面自己跳一下；
   * 后端没记住的话，下次恢复会退回原标题（`renameTerminal` 是唯一持久化入口）。
   */
  function setTitle(sid: string, uid: string, title: string): void {
    const tab = find(sid, uid)
    if (!tab) return
    tab.title = title
    // 新建的终端还没拿到 tid（WS 尚未回填）：没有可改名的对象，只留本地标题
    if (!tab.tid) return
    api.renameTerminal(sid, tab.tid, title).catch((e) => {
      toast.show(t('term_rename_failed', { error: describeError(e) }), 'error')
    })
  }

  /** 标签显示名：自定义标题在前；没有就用「终端 N」（N = 在子标签栏里的序号）。 */
  function titleOf(sid: string, tab: TermTab): string {
    const idx = listOf(sid).findIndex((item) => item.uid === tab.uid)
    const n = idx >= 0 ? idx + 1 : 1
    return tab.title
      ? t('session_terminal_titled', { n, title: tab.title })
      : t('session_terminal_n', { n })
  }

  /**
   * 从列表里摘掉一个标签，并把激活项挪到邻近的一个（没有就退回文件管理子标签）。
   *
   * ⚠️ 激活项必须**每次都能落到一个真实存在的目标上**：sessionUi 里若留着一个已经不在列表里的
   * uid，SessionView 里所有面板的 v-show 都会是 false（终端标签对不上，文件管理标签也不等于它）
   * → 内容区一片空白。不是「没数据」，是「一个面板都没渲染出来」。
   */
  function removeLocal(sid: string, uid: string): void {
    const list = tabs.value[sid]
    if (!list) return
    const idx = list.findIndex((tab) => tab.uid === uid)
    if (idx < 0) return
    // 先记下「被关的是不是当前激活项」——删掉之后就查不出来了
    const wasActive = ui.activeSubOf(sid) === uid
    list.splice(idx, 1)
    if (!list.length) {
      delete tabs.value[sid]
      delete activeUid.value[sid]
      // 最后一个终端关掉 → 激活项交回文件管理子标签：此时没有任何终端标签可激活，
      // 交回文件管理既符合「关完终端回到文件管理」的直觉，也是唯一能保证面板区不空着的做法。
      if (wasActive) ui.setActiveSub(sid, FILES_TAB)
      return
    }
    if (wasActive || activeUid.value[sid] === uid) {
      // 优先选原来位置上的那个（视觉上「后面那个补上来」），没有了就退到最后一个
      const next = list[Math.min(idx, list.length - 1)] ?? list[list.length - 1]
      if (next) setActive(sid, next.uid)
      return
    }
    // 兜底：sessionUi 指向一个**已经不在列表里**的 uid（关闭之外还有别的写入口，比如调用方的
    // 清理顺序），同样会让面板区全空 → 改指第一个真实存在的终端。
    // 指向 FILES_TAB 是合法状态（用户就在文件管理页），不动。
    const cur = ui.activeSubOf(sid)
    if (cur !== FILES_TAB && !list.some((tab) => tab.uid === cur) && list[0]) {
      setActive(sid, list[0].uid)
    }
  }

  /**
   * 关闭终端标签：终止后端终端后从列表移除。
   * 刻意**不 await** 关闭请求：用户点了关闭，标签就该立刻消失；后端关闭失败也只留一个
   * 空闲终端，会在宽限期后回收（下次挂载拿到 gone 再走本地标记）。若 await 后失败就不移除，
   * 用户会看到「点了没反应」。
   */
  function closeTab(sid: string, uid: string): void {
    const tab = find(sid, uid)
    if (!tab) return
    // status === 'error' = 后端已经说过「这个终端不存在」（gone 控制帧），再发一次关闭只会
    // 拿到 not_found，白白在控制台留一条错误。detached 例外：那是「别的设备在用它」，
    // 本端关标签是显式终止（标签上的确认框已经写明会终止远程 shell）。
    if (tab.tid && tab.status !== 'error') {
      api.closeTerminal(sid, tab.tid).catch((e) => console.warn('close terminal:', e))
    }
    removeLocal(sid, uid)
  }

  /** 会话被关闭/后端重启后清空该会话的全部标签状态（不清 sessionUi，那是它的调用方的事）。 */
  function forget(sid: string): void {
    delete tabs.value[sid]
    delete activeUid.value[sid]
  }

  return {
    tabsOf,
    activeOf,
    countOf,
    ensure,
    add,
    adoptTid,
    setStatus,
    setTitle,
    titleOf,
    closeTab,
    forget
  }
})
