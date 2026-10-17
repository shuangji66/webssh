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
import { shellQuote } from '@/utils/format'
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
  /**
   * 新终端建立后要**执行一次**的命令（目前只有 openTerminalAt 用：`cd '<目录>'`）。
   *
   * 它由 TerminalPane 在自己的 WS 握手成功（onopen）后取出、发送、随即清空。清空是这条
   * 约定的一半：带着它重挂载（用户点重连、或刷新页面后端重新回放历史）时字段已经为空，
   * 同一条 `cd` 不会被执行第二遍——用户不会在终端里看到一行自己没敲过的命令。
   */
  initialCommand?: string
}

// uid 只在浏览器里用：时间戳 + 自增序号即可（同一毫秒内连开两个也不会撞）。
let uidSeq = 0
function newUid(): string {
  uidSeq += 1
  return `term-${Date.now().toString(36)}-${uidSeq}`
}

/**
 * 终端状态点的配色（子标签栏与首页卡片共用一份）。
 *
 * 放在 store 文件里而不是组件里：它描述的是**终端状态**（open / connecting / exited / error /
 * detached），两个界面（会话子标签栏、首页的终端列表）必须给出同一套颜色，否则同一个终端在
 * 两处显示成不同状态，用户会以为其中一处坏了。参数写成结构化类型，避免和 store 的
 * TermTab 形成循环依赖。
 */
export function termStatusDotClass(tab: { status: TermStatus; restoring: boolean }): string {
  if (tab.status === 'open' && !tab.restoring) return 'bg-success'
  if (tab.status === 'connecting' || tab.restoring || tab.status === 'detached') return 'bg-warning'
  if (tab.status === 'exited') return 'bg-ink-faint dark:bg-ink-faint-dark'
  return 'bg-danger'
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

  /**
   * 本会话**上一次激活**的终端（不关心此刻激活的是不是文件管理子标签），没有就返回 null。
   *
   * 「此刻激活的是终端就直接用它」这一条是关键：切子标签只写 sessionUi（唯一真相在那边），
   * 所以 `activeUid` 只在 add / ensure / 关标签时被写过，光靠它会把「返回终端」固定指到
   * **最后建出来的那个标签**上（第五轮用户报的 bug：从终端 2 切到文件管理，点返回却跳回终端 3）。
   * 现在两头一起保证：切终端标签走 setActive（两个 store 一起写，见上），这里再以 sessionUi 为准。
   *
   * 用途仅限「切回哪个终端」这类**导航**；`activeOf` 仍然是「指令发给谁」的唯一判据
   * （文件管理在前台时必须返回 null，见文件头）。
   */
  function lastActiveOf(sid: string): TermTab | null {
    const cur = activeOf(sid)
    if (cur) return cur
    const uid = activeUid.value[sid]
    return uid ? find(sid, uid) ?? null : null
  }

  /**
   * 改激活项：本 store 与 sessionUi 一起写，保证两边不会各说各话。
   *
   * 切终端子标签**必须**走这里（而不是直接 `sessionUi.setActiveSub`）：否则 `activeUid` 会停在
   * 「最后新建的那个标签」上，`lastActiveOf` 这种「你刚才在哪个终端」的判断就全错了。
   */
  function setActive(sid: string, uid: string) {
    activeUid.value[sid] = uid
    ui.setActiveSub(sid, uid)
  }

  /**
   * 从后端的实时状态恢复标签（启动/刷新时用）。**幂等**：已有相同 tid 的标签不重复加，
   * 也不再改动已有标签的状态（否则一次刷新就把用户本地状态覆盖掉了）。
   */
  function ensure(sid: string, state: SessionState): void {
    // ⚠️ 新键要赋值**再读一次**：`tabs.value[sid] ?? (tabs.value[sid] = [])` 这种写法在键不存在时
    // 求值成的是**原始数组**（赋值表达式的值 = 右值），拿它 push 不会触发响应式——和
    // sftp.stateOf 踩的是同一个坑（见那里的说明）。
    if (!tabs.value[sid]) tabs.value[sid] = []
    const list = tabs.value[sid]!
    for (const info of state.terminals ?? []) {
      if (list.some((tab) => tab.tid === info.id)) continue
      list.push({
        uid: newUid(),
        tid: info.id,
        title: info.title ?? '',
        // 后端上报 exited 的终端其 PTY 已死：标成 exited（状态点变灰），
        // 而不是假装 open —— 后者会让用户以为「打字没反应」是前端卡住了。
        status: info.exited ? 'exited' : 'open',
        // **不**在这里预置 restoring：ensure 只把后端的终端投影成标签，不代表已经发起了挂载。
        // 会话恢复后视图要等用户进入才挂载（见 App.vue），预置 true 会让卡片上的状态点一直
        // 停在「恢复中」的琥珀色。真正的置位在 `TerminalPane` 挂载 / 重连时，按 tid 判定
        // （`onMounted` / `reconnect`：只有已有 tid 的终端才会回放并等 READY）。
        restoring: false,
        createdAt: info.createdAt || new Date().toISOString()
      })
    }
    // 恢复/打开会话时若本会话还没有激活项，落在第一个终端上。
    // 否则桌面端刷新后会停在文件管理子标签，而「刚打开的会话要看的是终端」。
    if (!activeUid.value[sid] && list[0]) setActive(sid, list[0].uid)
  }

  /**
   * 新建一个终端标签（tid 为 null，等后端回填）。新建即激活：用户点 + 就是想用它。
   *
   * `opts.initialCommand` = 这个新终端建立后要执行一次的命令（见 TermTab.initialCommand）。
   *
   * 为什么命令要**挂在标签上**、由 TerminalPane 在握手成功后自己取，而不是「先 add()，
   * 再由调用方等连接好了 send」：
   * 1) 调用方（左栏 / 文件管理器）手里没有 socket。持有 WS 的 TerminalPane 是 Vue 在**下一次
   *    渲染**才创建的，此刻它连 xterm 都还没建；调用方只能走 paneControls.get(sid, uid).send()，
   *    而 send 在未连接时只返回 false（不排队、不重试）——命令静默丢失。想不丢就得自己轮询
   *    「连上没有」，而握手的耗时取决于网络与后端建 PTY，轮询次数纯属猜。
   * 2) 标签的 status 也不是个可靠的「可以发」事件：它是**面板自己**在 onopen/onclose 里写的
   *    状态（open → 掉线 → connecting → 重连后再 open），盯它发命令会在重连时再发一遍，
   *    而且 watch 与 onopen 回调的先后顺序没有约定。
   * 挂在标签上则与这批时序无关：命令跟着标签走，面板在**自己这条连接**的 onopen 取一次、
   * 发一次、清一次，天然是「一次挂载最多一次」。
   */
  function add(sid: string, opts?: { initialCommand?: string }): TermTab {
    const tab: TermTab = {
      uid: newUid(),
      tid: null,
      title: '',
      status: 'connecting',
      // 新终端没有历史可回放，不必等 READY（后端对新建的终端只回 tid 后直接 attach）
      restoring: false,
      createdAt: new Date().toISOString()
    }
    // 只在真的有命令时挂字段：留一个空串会让「非空才发」的判断多一层无意义的分支
    if (opts?.initialCommand) tab.initialCommand = opts.initialCommand
    if (!tabs.value[sid]) tabs.value[sid] = []
    const list = tabs.value[sid]!
    list.push(tab)
    setActive(sid, tab.uid)
    return tab
  }

  /**
   * 在指定远程目录里新开一个终端（左栏 / 文件管理器的「在此目录新建终端」）。
   *
   * 路径**必须**过 shellQuote：这里的目录名来自远程文件系统，含空格（`My Files`）、单引号
   * （`it's`）、`$` / 反引号（`$(rm -rf ~)`）、中文都很常见。直接拼成 `cd <dir>` 会被 shell
   * 按空白拆词、按 `$`/反引号做命令替换——轻则 cd 到别的目录，重则执行了目录名里藏的命令。
   * shellQuote 用单引号把整串包住、把内部单引号转义成 `'\''`，是 POSIX shell 下唯一安全的写法。
   * （dir 是 /api/fs/list 给的绝对路径，不以 `-` 开头，所以无需 `cd --`。）
   */
  function openTerminalAt(sid: string, dir: string): TermTab {
    return add(sid, { initialCommand: 'cd ' + shellQuote(dir) })
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
    lastActiveOf,
    setActive,
    countOf,
    ensure,
    add,
    openTerminalAt,
    adoptTid,
    setStatus,
    setTitle,
    titleOf,
    closeTab,
    forget
  }
})
