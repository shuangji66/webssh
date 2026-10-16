// src/stores/quickAccess.ts — 快捷访问：**每个会话**一串远程目录路径（第二十轮，契约第 30 节）。
//
// 为什么按 sid 分片、而不是一份全局列表：路径是**某台远程主机上**的绝对路径。会话身份
// （user@host:port）在后端是去重的，所以「按会话」就等于「按主机」——把 A 主机上的
// `/vol1/1000` 挂进 B 会话的弹窗，点开只能报错。
//
// 为什么存后端数据目录而不是 localStorage：快捷访问是**用户数据**（换设备、清浏览器数据
// 都该还在），而且要能随「删除会话」一并清掉；localStorage 里那几份（视图 / 排序 / 权限列）
// 都是「这台设备上的人怎么看」，性质不同。接口见 /api/quickaccess/list|save。
//
// 写入语义与 stores/quickCmds.ts 一致：后端整表覆盖，这里乐观更新 + 失败回滚，
// **快照必须在改动之前拍**（回滚回去的必须不是失败的那一份）。差别是这里的新增/删除都要
// `ensure` 一次：整表覆盖的硬前提是「手上这份就是服务端那份」，没加载就写等于用一条路径
// 覆盖掉服务端已有的全部路径。ensure 内部还做了去重（同一个 sid 同时在飞的请求只发一次），
// 所以「先 ensure 再快照」不会有并发窗口。
import { defineStore } from 'pinia'
import { ref } from 'vue'
import { t } from '@/i18n'
import { api } from '@/serverapi'

/**
 * 规整用户输入的路径，返回 null = 不合法。
 *
 * **与后端 sanitizeQuickPath 同一套规则**（backend/quickaccess.go）：必须是绝对路径、不含 `..`
 * 段；重复斜杠、`.` 段、末尾斜杠都规整掉（同一个目录的两种写法不该变成两条）。后端仍然会
 * 再校验一遍——这一份只是为了让界面立刻给出提示，别让用户白等一次往返。
 *
 * **不 trim**：以空格结尾的目录名是合法的（`/vol1/1000/my dir ` 与 `/vol1/1000/my dir` 是
 * 两个目录），本仓库已经在 `resolveRemotePath` 上被这条坑过一次。用户手输时误带的首尾空白
 * 由输入框那一层处理（见 QuickAccessDialog 的 `submit`）。
 */
export function normalizeQuickPath(raw: string): string | null {
  if (!raw || !raw.startsWith('/')) return null
  const parts: string[] = []
  for (const seg of raw.split('/')) {
    if (!seg || seg === '.') continue
    if (seg === '..') return null
    parts.push(seg)
  }
  return `/${parts.join('/')}`
}

/** 空列表的稳定引用：`?? []` 每次调用都造一个新数组，v-for 会白重渲染一次。 */
const EMPTY: string[] = []

export const useQuickAccessStore = defineStore('quickAccess', () => {
  const lists = ref<Record<string, string[]>>({})
  // 「已加载」「加载中」「加载失败」分开记：ensure 靠第一个幂等，界面靠后两个显示状态
  // （加载失败必须能说原因，不能显示成「还没有快捷访问」——那会让用户以为自己加的被删了）
  const loaded = ref<Record<string, boolean>>({})
  const busy = ref<Record<string, boolean>>({})
  const failed = ref<Record<string, boolean>>({})

  /**
   * 弹窗显隐（按 sid）。
   *
   * 放在 store 里而不是某个组件里：入口有三个（文件管理工具栏、左栏面包屑按钮条、目录树/
   * 列表的「添加快捷访问」只加不弹），而弹窗本体挂在**会话视图**上（左栏与主区域是两个兄弟
   * 分支，见契约 30.3）——显隐只能有一个共享来源。
   */
  const dialog = ref<Record<string, boolean>>({})

  /** 同一个 sid 上正在飞的请求：聚合成一个 promise，免得两个组件同时挂载时发两次。 */
  const inflight = new Map<string, Promise<void>>()

  function listOf(sid: string): string[] {
    return lists.value[sid] ?? EMPTY
  }

  function isLoading(sid: string): boolean {
    return busy.value[sid] === true
  }

  function isFailed(sid: string): boolean {
    return failed.value[sid] === true
  }

  function isDialogOpen(sid: string): boolean {
    return dialog.value[sid] === true
  }

  function setDialog(sid: string, open: boolean) {
    dialog.value[sid] = open
  }

  function fetchList(sid: string): Promise<void> {
    const running = inflight.get(sid)
    if (running) return running
    busy.value[sid] = true
    failed.value[sid] = false
    const p = (async () => {
      try {
        const res = await api.quickAccess(sid)
        lists.value[sid] = res.paths ?? []
        loaded.value[sid] = true
      } catch (e) {
        // 失败必须与「空表」区分开：弹窗据此显示「读取失败：<原因> + 重试」，而不是
        // 「还没有快捷访问路径」——后者会让用户以为自己加的条目被删了（契约 30.3 明令禁止）。
        // loaded 保持 false，所以下次 ensure 会再拉一次，不会永远停在失败态。
        failed.value[sid] = true
        throw e
      }
    })()
    const tracked = p.finally(() => {
      busy.value[sid] = false
      if (inflight.get(sid) === tracked) inflight.delete(sid)
    })
    // 失败的拒绝要能被调用方看到（ensure 不吞异常）：p 自己没被 await，这里补一个
    // 空的 catch 免得出现「未处理的 promise 拒绝」告警——真正的原因由 tracked 带出去。
    p.catch(() => {})
    inflight.set(sid, tracked)
    return tracked
  }

  /** 首次使用：把服务端的表拉下来（已加载过就直接返回）。失败会抛，调用方决定怎么说话。 */
  async function ensure(sid: string): Promise<void> {
    if (loaded.value[sid]) return
    await fetchList(sid)
  }

  /** 强制重拉（弹窗里的「重试」）。 */
  async function reload(sid: string): Promise<void> {
    loaded.value[sid] = false
    await fetchList(sid)
  }

  /** 乐观更新 + 失败回滚：before 是**改动之前**的深拷贝（这里元素是字符串，浅拷贝即可）。 */
  async function persist(sid: string, before: string[]): Promise<void> {
    try {
      const res = await api.saveQuickAccess(sid, listOf(sid))
      lists.value[sid] = res.paths ?? listOf(sid)
    } catch (e) {
      lists.value[sid] = before
      throw e
    }
  }

  /**
   * 加一条。返回 `'added'` / `'exists'`（已经在列表里就不重复添加，也不报错——「再加一次
   * 同一个目录」是正常操作，界面给一句「已经在列表里」即可）。
   *
   * 路径非法时抛 Error（消息已是 i18n 文案，describeError 会原样显示）；其它失败（网络、
   * 会话已被删）原样抛给调用方。
   */
  async function add(sid: string, rawPath: string): Promise<'added' | 'exists'> {
    const path = normalizeQuickPath(rawPath)
    if (!path) throw new Error(t('files_quick_invalid'))
    await ensure(sid)
    const before = [...listOf(sid)]
    if (before.includes(path)) return 'exists'
    lists.value[sid] = [...before, path]
    await persist(sid, before)
    return 'added'
  }

  /** 删一条（不在列表里就什么都不做）。 */
  async function remove(sid: string, path: string): Promise<void> {
    await ensure(sid)
    const before = [...listOf(sid)]
    if (!before.includes(path)) return
    lists.value[sid] = before.filter((item) => item !== path)
    await persist(sid, before)
  }

  /** 会话关闭/被删时丢掉它的状态（记录只增不减会让内存一直涨）。 */
  function forget(sid: string) {
    delete lists.value[sid]
    delete loaded.value[sid]
    delete busy.value[sid]
    delete failed.value[sid]
    delete dialog.value[sid]
  }

  return {
    listOf,
    isLoading,
    isFailed,
    isDialogOpen,
    setDialog,
    ensure,
    reload,
    add,
    remove,
    forget
  }
})
