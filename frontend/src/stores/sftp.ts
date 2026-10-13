// src/stores/sftp.ts — 每个会话一份的 SFTP 文件浏览状态（文件管理器 + 左栏文件树共用）。
//
// 为什么按 sid 分片、并且放在 store 里：文件管理器与左栏文件树是同一批文件的两个入口
// （在列表里新建完文件，树里必须立刻能看到），而它们分挂在 SessionView 的两个分支上、
// 组件之间没有父子关系。状态留在组件里就会变成两份内容不同的缓存，用户看到的是
// 「树里有、列表里没有」这种自相矛盾的界面。
//
// 路径语义：全部是**远程主机上的绝对路径**（后端 FsEntry.path 也是绝对路径），前端只做
// 字符串拼接，不猜测目录是否存在——远程文件系统随时可能被别人改动，猜错比多一次请求更糟。
//
// 错误分工：只有 `state.error` 是「当前目录读取失败」的展示位（文件管理器整块显示它）。
// 写操作（新建 / 重命名 / 删除 / 上传 / 下载）一律把 ApiError 抛给调用方：它们各有各的
// 提示位置（toast 文案要带文件名），store 里不该替它们决定怎么说话。
import { ref } from 'vue'
import { defineStore } from 'pinia'
import {
  api,
  describeError,
  downloadPath,
  uploadLocal as uploadLocalRequest,
  type FsEntry,
  type UploadEntry,
  type UploadResult
} from '@/serverapi'
import { joinPath, parentPath } from '@/utils/format'

export interface SftpState {
  dir: string
  home: string
  entries: FsEntry[]
  loading: boolean
  error: string
  back: string[]
  forward: string[]
  view: 'list' | 'grid'
  treeChildren: Record<string, FsEntry[]>
  expanded: string[]
  treeLoading: Record<string, boolean>
  selected: string[]
}

/** 视图偏好（列表 / 网格）的持久化键。纯界面偏好：跟随浏览器，不落后端、不随会话走。 */
const VIEW_KEY = 'webssh-files-view'

function readStoredView(): 'list' | 'grid' {
  try {
    return localStorage.getItem(VIEW_KEY) === 'grid' ? 'grid' : 'list'
  } catch {
    // 隐私模式下 localStorage 会直接抛：读不到就用默认视图，不值得为此打断页面。
    return 'list'
  }
}

function blankState(): SftpState {
  return {
    dir: '',
    home: '',
    entries: [],
    loading: false,
    error: '',
    back: [],
    forward: [],
    view: readStoredView(),
    treeChildren: {},
    expanded: [],
    treeLoading: {},
    selected: []
  }
}

export const useSftpStore = defineStore('sftp', () => {
  const states = ref<Record<string, SftpState>>({})

  // 下面两份账本刻意留在闭包里、不进 SftpState：SftpState 是与契约逐字对应的展示状态，
  // 而「这个会话初始化过了没」「这个目录拉过了没」是请求记账，塞进 SftpState 就得给它加字段。
  //
  // 键必须是 sid + 路径：两个会话连的是不同主机，同一个路径在不同主机上完全是两个目录，
  // 只按路径记账会让 B 会话误以为「这个目录已经加载过」而拿去 A 会话的内容。
  const inited = new Set<string>()
  const loadedDirs = new Set<string>()

  const dirKey = (sid: string, path: string) => `${sid}\u0000${path}`

  function stateOf(sid: string): SftpState {
    let st = states.value[sid]
    if (!st) {
      st = blankState()
      states.value[sid] = st
    }
    return st
  }

  /** 把某个已加载过的路径从账本里清掉（删除 / 重命名之后调用）。 */
  function forgetLoaded(sid: string, path: string) {
    const prefix = `${sid}\u0000${path}`
    for (const key of [...loadedDirs]) {
      if (key === prefix || key.startsWith(`${prefix}/`)) loadedDirs.delete(key)
    }
  }

  /**
   * 树节点只保留目录：文件树的语义是「跳到某个目录」，文件一律在文件管理面板里操作。
   * 三个写入点（ensure / loadTreeChildren / loadDir 的同步）都过这里，免得某一处漏过滤，
   * 表现是「逛过一圈之后文件树里冒出文件」。
   */
  function dirsOnly(entries: FsEntry[]): FsEntry[] {
    return entries.filter((entry) => entry.dir)
  }

  /**
   * 拉一个目录的内容到当前视图。
   *
   * `path` 为空表示「家目录」——后端会解析出真实家目录并在响应里回填（响应体的 path/home），
   * 前端不自己猜家目录长什么样（不同账号可能是 /root、/home/x，也可能被 chroot 到 /）。
   */
  async function loadDir(sid: string, path: string, force = false): Promise<void> {
    const st = stateOf(sid)
    const target = path || st.home || ''
    // 同目录已加载就不重复请求：切标签 / 切视图会反复触发同一次加载，每次都发请求的话
    // 目录一变就会看到旧内容闪回。force 用于刷新与写操作之后。
    if (!force && st.dir === target && loadedDirs.has(dirKey(sid, target))) return

    st.dir = target
    st.loading = true
    st.error = ''
    // 立刻清空上一层的条目：面包屑此时已经指向新目录，还留着旧目录的列表会让人对着
    // 「路径 A + 内容 B」操作，误删 / 误读都从这儿来。
    st.entries = []
    try {
      const res = await api.fsList(sid, target)
      st.home = res.home || st.home
      // 家目录请求（path 为空）时以响应里的解析结果为准
      st.dir = res.path || target
      st.entries = res.entries
      loadedDirs.add(dirKey(sid, st.dir))
      // 树和列表是同一份数据：这个目录在树里已经展开过的话，一并换掉它的缓存，
      // 免得「列表里删掉的文件，树里还挂在那儿」。
      if (st.treeChildren[st.dir]) st.treeChildren[st.dir] = dirsOnly(res.entries)
    } catch (e) {
      st.error = describeError(e)
    } finally {
      st.loading = false
    }
  }

  /** 懒加载树节点：只往 treeChildren 里写，不动当前目录（树和列表是两个视图）。 */
  async function loadTreeChildren(sid: string, path: string, force = false): Promise<void> {
    const st = stateOf(sid)
    if (!force && st.treeChildren[path]) return
    st.treeLoading[path] = true
    try {
      const res = await api.fsList(sid, path)
      st.treeChildren[path] = dirsOnly(res.entries)
    } finally {
      delete st.treeLoading[path]
    }
  }

  /** 写操作之后刷新「被改动的那一层」：当前目录走 loadDir，树里展开过的节点走树缓存。 */
  async function refreshDir(sid: string, dir: string): Promise<void> {
    const st = stateOf(sid)
    if (!dir || dir === st.dir) {
      await loadDir(sid, st.dir, true)
      return
    }
    if (st.treeChildren[dir]) {
      try {
        await loadTreeChildren(sid, dir, true)
      } catch {
        // 树节点刷新失败不打断已经成功的写操作；下次展开时还会再拉一次
      }
    }
  }

  /** 删除 / 重命名之后丢掉树里那段路径的缓存（否则会留下指向不存在路径的孤儿缓存）。 */
  function dropTreeKeys(st: SftpState, path: string) {
    const childPrefix = `${path}/`
    for (const key of Object.keys(st.treeChildren)) {
      if (key === path || key.startsWith(childPrefix)) delete st.treeChildren[key]
    }
    for (const key of Object.keys(st.treeLoading)) {
      if (key === path || key.startsWith(childPrefix)) delete st.treeLoading[key]
    }
    st.expanded = st.expanded.filter((key) => key !== path && !key.startsWith(childPrefix))
  }

  /** 重命名目录后把树里那一段缓存搬到新路径下（不搬的话旧键变孤儿、新路径又要重新展开）。 */
  function migrateTreeKeys(st: SftpState, from: string, to: string) {
    const childPrefix = `${from}/`
    for (const key of Object.keys(st.treeChildren)) {
      if (key !== from && !key.startsWith(childPrefix)) continue
      st.treeChildren[`${to}${key.slice(from.length)}`] = st.treeChildren[key]
      delete st.treeChildren[key]
    }
    st.expanded = st.expanded.map((key) =>
      key === from || key.startsWith(childPrefix) ? `${to}${key.slice(from.length)}` : key
    )
  }

  /** 首次进入会话：定位家目录并加载。幂等——左右栏两个组件挂载时都会调它。 */
  async function ensure(sid: string): Promise<void> {
    if (inited.has(sid)) return
    // 先占位：两个组件同时挂载时都会调 ensure，不占位就会并发发两次家目录请求。
    inited.add(sid)
    const st = stateOf(sid)
    await loadDir(sid, '', true)
    if (st.error) {
      // 失败就不算初始化过：组件上的「重试」会再走一遍 ensure。
      inited.delete(sid)
      return
    }
    // 家目录这一份列表已经到手，顺手灌进树缓存，树根节点不用再请求一次。
    st.treeChildren[st.home || st.dir] = dirsOnly(st.entries)
  }

  /** 跳转到一个目录并记入后退历史（forward 栈作废：这是用户走了新分支）。 */
  async function navigate(sid: string, path: string): Promise<void> {
    const st = stateOf(sid)
    const target = path || st.home
    if (!target) return
    if (target !== st.dir) {
      // 只有真的换了目录才记历史：点当前目录那一段面包屑不该往栈里塞重复项，
      // 否则用户要按好几次「后退」才能回到上一层。
      if (st.dir) st.back.push(st.dir)
      st.forward = []
    }
    await loadDir(sid, target)
  }

  async function back(sid: string): Promise<void> {
    const st = stateOf(sid)
    const prev = st.back.pop()
    if (prev === undefined) return
    if (st.dir) st.forward.push(st.dir)
    await loadDir(sid, prev)
  }

  async function forward(sid: string): Promise<void> {
    const st = stateOf(sid)
    const next = st.forward.pop()
    if (next === undefined) return
    if (st.dir) st.back.push(st.dir)
    await loadDir(sid, next)
  }

  /** 强制重新拉当前目录（外部的改动、别的设备写的文件靠它看到）。 */
  async function refresh(sid: string): Promise<void> {
    await loadDir(sid, stateOf(sid).dir, true)
  }

  function setView(sid: string, v: 'list' | 'grid') {
    stateOf(sid).view = v
    try {
      localStorage.setItem(VIEW_KEY, v)
    } catch {
      // 写不进去只意味着下次进来看不到这个偏好，不影响功能
    }
  }

  /** 展开 / 收起树节点；展开时才懒加载子目录。 */
  async function toggleExpand(sid: string, path: string): Promise<void> {
    const st = stateOf(sid)
    const at = st.expanded.indexOf(path)
    if (at >= 0) {
      st.expanded.splice(at, 1)
      // 收起时把整棵子树的缓存一起丢掉：展开只在「没有缓存」时才请求，留着缓存就再也没有
      // 失效路径——重新展开看到的是几分钟前的目录内容。代价是重新展开多一次 listing。
      dropTreeKeys(st, path)
      return
    }
    st.expanded.push(path)
    if (!st.treeChildren[path]) {
      try {
        await loadTreeChildren(sid, path)
      } catch (e) {
        // 展开失败就收回去：留一个空节点在那里，用户只会以为「这个目录里什么都没有」
        st.expanded = st.expanded.filter((key) => key !== path)
        throw e
      }
    }
  }

  async function createFile(sid: string, dir: string, name: string): Promise<void> {
    await api.fsCreate(sid, joinPath(dir, name))
    await refreshDir(sid, dir)
  }

  async function createDir(sid: string, dir: string, name: string): Promise<void> {
    await api.fsMkdir(sid, joinPath(dir, name))
    await refreshDir(sid, dir)
  }

  /**
   * 重命名。
   *
   * ⚠️ `to` 必须是**完整目标路径**（与后端 /api/fs/rename 的字段一致），不是新名字——传相对名
   * 会被后端当成非法路径。调用方用 `joinPath(parentPath(path), 新名字)` 拼出来。让这里只认
   * 全路径，是为了顺手支持「改名时换个目录」。
   */
  async function rename(sid: string, path: string, to: string): Promise<void> {
    await api.fsRename(sid, path, to)
    const st = stateOf(sid)
    migrateTreeKeys(st, path, to)
    st.selected = st.selected.map((p) => (p === path ? to : p))
    forgetLoaded(sid, path)
    await refreshDir(sid, parentPath(path))
  }

  async function remove(sid: string, path: string, dir: boolean): Promise<void> {
    await api.fsRemove(sid, path, dir)
    const st = stateOf(sid)
    dropTreeKeys(st, path)
    st.selected = st.selected.filter((p) => p !== path)
    forgetLoaded(sid, path)
    await refreshDir(sid, parentPath(path))
  }

  async function uploadLocal(
    sid: string,
    dir: string,
    entries: UploadEntry[],
    overwrite: boolean,
    onProgress?: (p: number) => void,
    signal?: AbortSignal
  ): Promise<UploadResult> {
    // 直接转 serverapi：进度与取消都在那一层（XHR + AbortSignal），store 只负责写完之后
    // 把目录拉新，让调用方拿到结果时界面已经是新内容。
    const res = await uploadLocalRequest(sid, dir, entries, overwrite, onProgress, signal)
    await refreshDir(sid, dir)
    return res
  }

  async function uploadFromNas(
    sid: string,
    dir: string,
    source: string,
    overwrite: boolean
  ): Promise<UploadResult> {
    const res = await api.fsUploadFromNas(sid, dir, source, overwrite)
    await refreshDir(sid, dir)
    return res
  }

  async function download(sid: string, path: string): Promise<void> {
    await downloadPath(sid, path)
  }

  function select(sid: string, paths: string[]) {
    stateOf(sid).selected = [...paths]
  }

  function clearSelection(sid: string) {
    stateOf(sid).selected = []
  }

  /** 会话关闭时丢掉它的全部浏览状态（否则记录只增不减）。 */
  function forget(sid: string) {
    delete states.value[sid]
    inited.delete(sid)
    for (const key of [...loadedDirs]) {
      if (key.startsWith(`${sid}\u0000`)) loadedDirs.delete(key)
    }
  }

  return {
    stateOf,
    ensure,
    navigate,
    back,
    forward,
    refresh,
    loadDir,
    setView,
    toggleExpand,
    createFile,
    createDir,
    rename,
    remove,
    uploadLocal,
    uploadFromNas,
    download,
    select,
    clearSelection,
    forget
  }
})
