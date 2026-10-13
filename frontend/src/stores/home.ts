// src/stores/home.ts — 远程会话配置 + 实时状态 + 连接测试 + 启动恢复。
//
// 「会话配置」（磁盘，活过重启）与「已打开的会话」（后端内存，进程重启即消失）是两件事，
// 这个 store 同时持有两者：
//   sessions    ← /api/sessions 的 sessions 字段（配置）
//   openStates  ← /api/sessions 的 open 字段（实时状态，按 sid 索引）
// 判据只有一处：`isOpen(id)` = 后端返回的 state.open。不要用「标签是否存在」当判据——
// 标签是界面意愿，后端状态才是事实（会话可能已在别的设备上被关掉）。
import { defineStore } from 'pinia'
import { ref } from 'vue'
import {
  ApiError,
  api,
  describeError,
  type OpenSessionInput,
  type RuntimeInfo,
  type SessionInput,
  type SessionState,
  type SessionView,
  type TestOutcome
} from '@/serverapi'
import { t } from '@/i18n'
import { sessionLabel } from '@/utils/format'
import { useAppTabsStore } from '@/stores/appTabs'
import { useSessionUiStore } from '@/stores/sessionUi'
import { useSftpStore } from '@/stores/sftp'
import { useTerminalsStore } from '@/stores/terminals'
import { useToastStore } from '@/stores/toast'

/**
 * 「后端已经没有这个会话」类的错误码。
 *
 * 收到它们说明我们本地状态落后于后端（会话在别处被关掉了 / 后端换过进程），
 * 这时**不能**给用户报错：把本地状态对齐才是正确处置。其它错误（网络、internal）
 * 才是真失败，必须原样抛给调用方去提示。
 */
const GONE_CODES = ['not_found', 'conflict', 'not_open', 'session_gone']

function isGoneError(e: unknown): boolean {
  return e instanceof ApiError && GONE_CODES.includes(e.code)
}

/**
 * 打开会话时可携带的凭据（`credentialSaved === false` 的会话必须带，否则后端回
 * `credential_required`）。id 由调用方传，所以这里把 id 摘掉——避免出现「credential 里的 id
 * 与参数里的 id 不一致」这种能编译通过、但只有运行时才看得出的错。
 */
export type SessionCredential = Omit<OpenSessionInput, 'id'>

export const useHomeStore = defineStore('home', () => {
  const sessions = ref<SessionView[]>([])
  const openStates = ref<Record<string, SessionState>>({})
  const loading = ref(false)
  const loaded = ref(false)
  /** 列表加载失败的原因（首页据此显示「加载失败 + 重试」而不是「还没有会话」）。 */
  const error = ref('')
  /** 后端进程实例标识：变了说明后端重启过，内存态会话全部失效。 */
  const bootId = ref('')
  /** /api/info 的运行信息（设置页展示数据目录与飞牛状态）。 */
  const runtime = ref<RuntimeInfo | null>(null)

  function stateOf(id: string): SessionState | undefined {
    return openStates.value[id]
  }

  function isOpen(id: string): boolean {
    return openStates.value[id]?.open === true
  }

  function labelOf(id: string): string | undefined {
    const s = sessions.value.find((item) => item.id === id)
    return s ? sessionLabel(s) : undefined
  }

  /** 拉一次会话配置 + 实时状态。`force` 用于手动刷新与「本地状态可能落后」时的对齐。 */
  async function load(force = false): Promise<void> {
    // 并发去重：启动流程与首页挂载都会调，打两次没有意义（后到的请求结果还会互相覆盖）
    if (loading.value) return
    if (loaded.value && !force) return
    loading.value = true
    error.value = ''
    try {
      const res = await api.sessions()
      sessions.value = res.sessions ?? []
      const next: Record<string, SessionState> = {}
      for (const state of res.open ?? []) next[state.id] = state
      openStates.value = next
      loaded.value = true
    } catch (e) {
      // 这里不抛：调用方是启动流程与首页，两者都要「显示错误但继续渲染」
      error.value = describeError(e)
    } finally {
      loading.value = false
    }
  }

  /** 运行信息（版本 / bootId / 数据目录 / 飞牛状态）。 */
  async function loadInfo(): Promise<void> {
    const res = await api.info()
    runtime.value = res.runtime
    bootId.value = res.runtime.bootId
  }

  /** 新增或更新会话配置（带 id = 更新）。`duplicate` 等错误原样抛出，由表单展示。 */
  async function save(input: SessionInput): Promise<SessionView> {
    const res = await api.saveSession(input)
    const saved = res.session
    const index = sessions.value.findIndex((item) => item.id === saved.id)
    if (index >= 0) sessions.value[index] = saved
    else sessions.value.push(saved)
    return saved
  }

  /**
   * 删除会话配置，并**一并清掉这台主机的信任记录**（第十一轮用户要求）。
   *
   * 为什么在这里做：known_hosts 按 `host:port` 存，会话配置是它唯一的「引用者」——会话没了
   * 还留着记录，下次新建一个同主机同端口的会话会「一上来就是已信任」，用户看不到自己是什么
   * 时候信过它（安全相关的状态不该这么无声无息）。
   *
   * 代价说清楚：**同一台主机同一端口上如果还有别的会话**（会话身份是 user@host:port，所以这
   * 只发生在换了用户名的场景），它们的信任也会一起没了——下次连接要重新确认指纹。这是用户
   * 明确要的行为；若要改成「删掉最后一个用它的会话时才删」，判据就加在这里。
   *
   * 顺序是先删会话、再删信任：反过来一旦删会话失败，会留下一台「没有会话但已被信任」的主机；
   * 这个顺序最坏也只是留一条没人引用的记录（下面会如实报出来）。
   */
  async function remove(id: string): Promise<void> {
    // host/port 必须从删除前的快照里取：配置删掉之后就查不到了
    const target = sessions.value.find((item) => item.id === id)
    await api.deleteSession(id)
    sessions.value = sessions.value.filter((item) => item.id !== id)
    delete openStates.value[id]
    if (!target) return
    try {
      await api.forgetHostKey(target.host, target.port)
    } catch (e) {
      // 会话**已经删掉了**，这里失败不能反过来报「删除失败」——如实说清是哪一步没做完
      useToastStore().show(t('home_delete_trust_failed', { error: describeError(e) }), 'error')
    }
  }

  /** 连接测试（不建立持久会话）。返回 TestOutcome；参数不合法等会抛 ApiError。 */
  async function test(input: SessionInput): Promise<TestOutcome> {
    const res = await api.testSession(input)
    return res.result
  }

  /** 首次信任主机密钥（TOFU）。只在用户确认过指纹后调用。 */
  async function trust(host: string, port: number, fingerprint: string): Promise<void> {
    await api.trustHostKey(host, port, fingerprint)
  }

  /**
   * 打开会话（后端 /open 幂等：已打开就返回现有状态，不会重建终端）。
   *
   * `credential` 只对「不保存凭据」的会话有意义：后端内存里没有凭据时，不带凭据的 open 会回
   * `credential_required`（409），调用方（首页）据此弹输入框，填完再带凭据调一次。
   * 带上凭据且拨号成功时，后端会把凭据留在**内存**里供该会话后续的终端与 SFTP 复用。
   */
  async function openSession(id: string, credential?: SessionCredential): Promise<SessionState> {
    const res = await api.openSession({ id, ...credential })
    openStates.value[id] = res.state
    // 后端会顺手更新 lastUsedAt；本地也跟一下，否则卡片上的「最近使用」要等下次刷新才变
    const s = sessions.value.find((item) => item.id === id)
    if (s) s.lastUsedAt = new Date().toISOString()
    return res.state
  }

  /**
   * 会话改名（弹窗里的名称输入框与顶栏标签双击共用这一条路径）。
   *
   * 为什么必须走 `/api/sessions/save` 而不是只改本地展示：名字是**会话配置的一部分**，
   * 只改本地的话刷新换台设备就没了。而 save 是整份提交，缺了 host/user/authType 后端会当成
   * 新增或改错对象，所以这里用本地配置里的原值补齐；**绝不**带 password/privateKey/passphrase
   * ——这既没必要（名字与凭据无关），也会把「留空 = 不修改」的语义搅浑。
   *
   * ⚠️ **port 必须一起发**（契约 11.1.4 的示例载荷里没有它，但漏了它改名在已打开的会话上会
   * 直接失败）：后端的 port 是非指针 int，缺省被 `validateSessionFields` 归一成 22，于是
   * 「当前 2222 ≠ 收到 22」被判成改了身份 —— 会话开着时后端会回 `conflict`（
   * 「会话已打开，请先关闭再修改地址 / 端口 / 登录名」）。实测：带 port 后改名成功，且配置里的
   * 凭据与打开状态都不受影响。
   *
   * 名字为空串时后端保留原名（契约 11.1.4），所以这里回填的是后端返回的 session 而不是入参。
   */
  async function renameSession(id: string, name: string): Promise<SessionView> {
    if (!sessions.value.some((item) => item.id === id)) {
      // 本地没有这份配置（配置被删/被别处刷新掉了）：抛给调用方 toast，不做静默失败。
      throw new Error(t('err_not_found'))
    }
    // **只发名字**：后端按部分更新合并其余字段。回传本地缓存的 host/user/authType 在
    // 「另一台设备刚改过地址」时会把它覆盖掉（第六轮用户报的「把缓存写回」就是这类问题）。
    const saved = await save({ id, name })
    // 标签标题跟着配置走（openSession 之外的入口不能把用户拽到那个会话上）。
    // syncTitle 不覆盖用户手动起过的名字，这里也不需要改激活项。
    useAppTabsStore().syncTitle(id, sessionLabel(saved))
    return saved
  }

  /** 关闭会话：后端终止它下面所有终端与 SFTP（配置保留）。 */
  async function closeSession(id: string): Promise<void> {
    try {
      await api.closeSession(id)
    } catch (e) {
      // 后端已经没这个会话了也照常往下走（本地状态对齐）；其它错误抛给调用方提示
      if (!isGoneError(e)) throw e
    }
    delete openStates.value[id]
  }

  /**
   * 把一个会话从前端状态里彻底摘掉：应用标签 + 终端标签 + 文件管理 + 会话内界面状态。
   *
   * 只清前端状态，**不**动后端（是否要 close 由调用方决定：关标签要 close，后端已重启就不能
   * 再 close）。四个 store 的清理必须一起做且顺序无关，所以收在一个地方——漏掉一个的表现是
   * 「会话关了，但重新打开还看到上次的终端标签/文件列表」。
   */
  function forgetSession(sid: string): void {
    useAppTabsStore().close(sid)
    useTerminalsStore().forget(sid)
    useSftpStore().forget(sid)
    useSessionUiStore().forget(sid)
    delete openStates.value[sid]
  }

  /**
   * 启动恢复：把后端仍在运行的会话恢复成应用标签，返回 sid 列表（App 据此接线终端标签）。
   *
   * 依据只有后端返回的 open 列表：标签在刷新前是否打开过是本地信息，不可靠也不该持久化
   * （本地记着但后端已经关了的情况必然出现，比如在另一台设备上关掉了）。
   */
  async function restore(): Promise<string[]> {
    if (!loaded.value) await load()
    const tabs = useAppTabsStore()
    const sids = Object.keys(openStates.value).filter((sid) => openStates.value[sid]?.open === true)
    for (const sid of sids) tabs.openSession(sid, labelOf(sid))
    return sids
  }

  let probing = false

  function handleBackendRestart(): void {
    // 后端换了进程：内存态会话全没了（配置还在）。把所有会话标签收掉，否则点进去只会得到
    // 「终端不存在」——用户看到一堆死标签，还以为是自己操作错了。
    for (const sid of [...Object.keys(openStates.value)]) forgetSession(sid)
    openStates.value = {}
    useToastStore().show(t('app_backend_restarted'), 'error')
  }

  /**
   * WS 掉线后探测后端是否换了进程（bootId 变化）。
   *
   * 由终端面板在连接断开时调用：WS 断开既可能是网络抖动，也可能是后端重启，前端凭一次断连
   * 分不出来，所以这里去问一次 /api/info。bootId 为空（启动时 /api/info 就失败过）时不当成
   * 重启：宁可漏报一次，也不能把「网络抖动」误报成「后端重启」并清掉用户所有会话标签。
   */
  function watchBackendRestart(): void {
    if (probing) return
    probing = true
    void (async () => {
      try {
        const res = await api.info()
        const next = res.runtime.bootId
        runtime.value = res.runtime
        const changed = !!bootId.value && !!next && next !== bootId.value
        bootId.value = next
        if (changed) handleBackendRestart()
      } catch {
        // 探测失败（后端还没起来 / 网络不通）：保持现状，下一次断线会再探
      } finally {
        probing = false
      }
    })()
  }

  /**
   * 某个终端在后端已不存在（WS 回了 `gone` 控制帧）——由终端面板调用。
   *
   * 单个终端消失不等于是整个会话没了（用户可能只是关掉了那个终端，远程 shell 退出也会走到
   * 类似路径），所以先只摘掉这个终端的标签与状态，再重拉一次 /api/sessions 对齐：
   * 会话确实不在 open 列表里了，才把会话标签一起收掉。
   */
  function handleTerminalGone(sid: string, tid: string): void {
    const terminals = useTerminalsStore()
    const target = terminals.tabsOf(sid).find((tab) => tab.tid === tid)
    if (target) {
      // 先标成 error（它的语义就是「后端明确说过这个终端不存在」）再关标签：
      // terminals.closeTab 对 error 标签不会再发 /api/terminals/close —— 那个请求只会拿到
      // not_found（后端本来就说它不存在），白白在控制台留一条警告。
      terminals.setStatus(sid, target.uid, 'error')
      terminals.closeTab(sid, target.uid)
    }
    const state = openStates.value[sid]
    if (state) state.terminals = state.terminals.filter((info) => info.id !== tid)
    useToastStore().show(t('app_session_gone'), 'error')
    void load(true).then(() => {
      if (!openStates.value[sid]) forgetSession(sid)
    })
  }

  return {
    sessions,
    openStates,
    loading,
    loaded,
    error,
    bootId,
    runtime,
    load,
    loadInfo,
    stateOf,
    isOpen,
    save,
    remove,
    test,
    trust,
    openSession,
    renameSession,
    closeSession,
    forgetSession,
    restore,
    watchBackendRestart,
    handleTerminalGone
  }
})
