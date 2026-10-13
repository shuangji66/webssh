// src/stores/quickCmds.ts — 快捷指令（全局 + 每个会话的专属）。
//
// 后端接口是「整体保存」（`/api/quickcmds/save` 收一整份列表），所以这里统一按
// 「乐观更新 + 失败回滚」实现：先改本地让界面立刻响应，保存失败再整体恢复成改动前的快照
// 并把异常抛给调用方去 toast。**快照必须在改动之前拍**——否则回滚回去的正是保存失败的那一份，
// 界面看起来是成功的（这条是参考项目里踩过的坑）。
import { defineStore } from 'pinia'
import { ref } from 'vue'
import { api, type QuickCmd } from '@/serverapi'

export interface QuickCmdPayload {
  name: string
  content: string
  auto: boolean
}

/**
 * crypto.randomUUID 是 SecureContext-only：经 http 反代访问（isSecureContext=false）时它是
 * undefined，直接调用会抛 TypeError（表现为「新增快捷指令失败」）。兜底用 crypto.getRandomValues
 * 自己拼一个 v4 UUID（非安全上下文同样可用）。
 */
function newCmdId(): string {
  if (typeof crypto !== 'undefined' && typeof crypto.randomUUID === 'function') {
    return crypto.randomUUID()
  }
  const bytes = new Uint8Array(16)
  crypto.getRandomValues(bytes)
  bytes[6] = (bytes[6] & 0x0f) | 0x40 // version 4
  bytes[8] = (bytes[8] & 0x3f) | 0x80 // variant 10xx
  const hex = Array.from(bytes, (b) => b.toString(16).padStart(2, '0')).join('')
  return `${hex.slice(0, 8)}-${hex.slice(8, 12)}-${hex.slice(12, 16)}-${hex.slice(16, 20)}-${hex.slice(20)}`
}

/** 作用域的键：全局用空串（与后端的 `sid=""` 约定一致），专属用 sid。 */
function scopeKey(sid: string | null): string {
  return sid ?? ''
}

export const useQuickCmdsStore = defineStore('quickCmds', () => {
  const global = ref<QuickCmd[]>([])
  const session = ref<Record<string, QuickCmd[]>>({})
  // 「已加载」与「加载中」分开记：ensure 靠前者幂等，界面靠后者显示加载态
  const loadedScopes = ref<Record<string, boolean>>({})
  const busyScopes = ref<Record<string, boolean>>({})
  // 加载失败标志：对话框据此显示「加载失败 + 重试」，而不是「暂无快捷指令」——
  // 后者会让用户以为自己的指令被删了，转头又去重建一份（把服务端那份覆盖掉）。
  const failedScopes = ref<Record<string, boolean>>({})

  function listOf(sid: string | null): QuickCmd[] {
    return sid ? (session.value[sid] ?? []) : global.value
  }

  function setList(sid: string | null, list: QuickCmd[]): void {
    if (sid) session.value[sid] = list
    else global.value = list
  }

  function isLoading(sid: string | null): boolean {
    return busyScopes.value[scopeKey(sid)] === true
  }

  function isFailed(sid: string | null): boolean {
    return failedScopes.value[scopeKey(sid)] === true
  }

  async function fetchScope(sid: string | null): Promise<void> {
    const key = scopeKey(sid)
    if (busyScopes.value[key]) return
    busyScopes.value[key] = true
    failedScopes.value[key] = false
    try {
      const res = await api.quickCmds(sid ?? undefined)
      setList(sid, res.commands ?? [])
      loadedScopes.value[key] = true
    } catch (e) {
      failedScopes.value[key] = true
      throw e
    } finally {
      busyScopes.value[key] = false
    }
  }

  async function ensureGlobal(): Promise<void> {
    if (loadedScopes.value['']) return
    await fetchScope(null)
  }

  async function ensureSession(sid: string): Promise<void> {
    if (loadedScopes.value[sid]) return
    await fetchScope(sid)
  }

  /** 强制重拉（对话框的「重试」按钮）。 */
  async function reload(sid: string | null): Promise<void> {
    loadedScopes.value[scopeKey(sid)] = false
    await fetchScope(sid)
  }

  /** 改动前的深拷贝快照（必须在改动之前调用）。 */
  function snapshot(sid: string | null): QuickCmd[] {
    return listOf(sid).map((cmd) => ({ ...cmd }))
  }

  async function persist(sid: string | null, before: QuickCmd[]): Promise<void> {
    try {
      const res = await api.saveQuickCmds(sid ?? undefined, listOf(sid))
      setList(sid, res.commands ?? listOf(sid))
    } catch (e) {
      setList(sid, before) // 回滚到改动前
      throw e
    }
  }

  /**
   * 新增。
   *
   * ⚠️ 调用方必须保证该作用域**已经加载成功**（对话框只在 ready 时允许新增）：
   * 保存是整体覆盖，未加载就新增等于用一条指令覆盖掉服务端已有的全部指令。
   */
  async function add(sid: string | null, payload: QuickCmdPayload): Promise<void> {
    const before = snapshot(sid)
    setList(sid, [...listOf(sid), { id: newCmdId(), ...payload }])
    await persist(sid, before)
  }

  async function update(
    sid: string | null,
    cmd: QuickCmd,
    payload: QuickCmdPayload
  ): Promise<void> {
    const before = snapshot(sid)
    setList(
      sid,
      listOf(sid).map((item) => (item.id === cmd.id ? { ...item, ...payload } : item))
    )
    await persist(sid, before)
  }

  async function remove(sid: string | null, cmd: QuickCmd): Promise<void> {
    const before = snapshot(sid)
    setList(
      sid,
      listOf(sid).filter((item) => item.id !== cmd.id)
    )
    await persist(sid, before)
  }

  /** 按 id 顺序重排（上移/下移）。不在列表里的 id 会被忽略，本地多出来的项排在末尾。 */
  async function reorder(sid: string | null, orderedIds: string[]): Promise<void> {
    const before = snapshot(sid)
    const current = listOf(sid)
    const ordered: QuickCmd[] = []
    for (const id of orderedIds) {
      const found = current.find((item) => item.id === id)
      if (found) ordered.push(found)
    }
    for (const item of current) {
      if (!ordered.includes(item)) ordered.push(item)
    }
    setList(sid, ordered)
    await persist(sid, before)
  }

  return {
    global,
    session,
    isLoading,
    isFailed,
    ensureGlobal,
    ensureSession,
    reload,
    add,
    update,
    remove,
    reorder
  }
})
