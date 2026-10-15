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
// 写操作（新建 / 重命名 / 删除 / 上传 / 下载 / 复制粘贴 / 压缩解压 / 导出）一律把 ApiError
// 抛给调用方：它们各有各的提示位置（toast 文案要带文件名），store 里不该替它们决定怎么说话。
//
// 第二轮新增的公开成员（左栏 FileTree 与 FileManager 共用，两个组件都从这里取）：
//   state.mode / toggleMode                权限列显示形态（符号 / 八进制，持久化）
//   state.sort / cycleSort / sorted        列表排序（三态循环，持久化；sorted 返回排好的副本）
//   clipboard / copyToClipboard / cutToClipboard / paste / clipboardFor / clipboardIsForeign /
//   clearClipboard
//                                          剪贴板（全局一份、不跨会话；清不清由入口决定：
//                                          右键菜单的粘贴粘完就清，常驻剪贴板栏的
//                                          「粘贴到此处」不清，点「取消」才清）
//   paste / compress / extract / exportToNas
//                                          批量/远程操作（compress 支持 tar.gz 与 zip 两种格式）
//   expandPath / refreshTree               树：展开到某条链路（家目录）、刷新某个节点
//   state.treeFileTotal / TREE_FILE_LIMIT  树里每个目录的文件总数（第十六轮起树也列文件：
//                                          每个目录只显示前 TREE_FILE_LIMIT 个，多出来的靠
//                                          「···更多文件」那一行去文件管理里看，见契约 26.4）
//   runtime / ensureRuntime / exportRoot / archiveDir
//                                          运行信息（导出目标目录、压缩包输出目录）
//   jumpTo（第二十轮）                       「先探测、成功了才切过去」的跳转（快捷访问的点击）：
//                                          列目录失败时**不动任何展示状态**，把异常抛给调用方去报错，
//                                          所以它不取代 navigate（普通导航仍然「先切、再显示错误」）
import { ref } from 'vue'
import { defineStore } from 'pinia'
import { t } from '@/i18n'
import {
  api,
  describeError,
  downloadPath,
  uploadLocal as uploadLocalRequest,
  type ArchiveFormat,
  type ArchiveResult,
  type ExportResult,
  type FsEntry,
  type FsListing,
  type RuntimeInfo,
  type UploadEntry,
  type UploadResult
} from '@/serverapi'
import {
  baseName,
  compareEntries,
  joinPath,
  parentPath,
  type SortDir,
  type SortKey
} from '@/utils/format'
import { useHomeStore } from '@/stores/home'

/** 权限显示模式：符号串（`-rw-r--r--`）还是八进制（`0644`）。 */
export type PermMode = 'symbolic' | 'octal'

export interface SortState {
  /** null = 不排序（回到服务端给的顺序） */
  key: SortKey | null
  dir: SortDir
}

/** 剪贴板：内容 + 它来自哪个会话 + 是复制还是剪切。 */
export interface Clipboard {
  sid: string
  op: 'copy' | 'cut'
  paths: string[]
}

/** 粘贴结果：真贴上去了几项、因为目标同名被跳过几项（`conflicts` 是被挡下的名字）。 */
export interface PasteOutcome {
  count: number
  skipped: number
  conflicts: string[]
}

export interface SftpState {
  dir: string
  home: string
  entries: FsEntry[]
  loading: boolean
  error: string
  back: string[]
  forward: string[]
  view: 'list' | 'grid'
  /** 权限列显示成什么样子（全局偏好，跟浏览器走，不落后端） */
  mode: PermMode
  /** 列表排序（全局偏好） */
  sort: SortState
  treeChildren: Record<string, FsEntry[]>
  /**
   * 目录树里每个目录的**文件总数**（第十六轮）。
   *
   * 与 `treeChildren` 分开记：树里只显示前 TREE_FILE_LIMIT 个文件，而「还有多少个没显示」
   * 决定要不要渲染「···更多文件」那一行——只看 treeChildren 是数不出来的。
   */
  treeFileTotal: Record<string, number>
  expanded: string[]
  treeLoading: Record<string, boolean>
  selected: string[]
  /**
   * 是否处于**多选模式**（第九轮）。
   *
   * 与 `selected` 分开：`selected` 是「选了哪些」，这个是「现在是点选模式的吗」——用户先点
   * 「多选」进入模式（此时一个都没选），再逐个点选；执行复制/剪切/删除/压缩/导出之后退出模式
   * （见 FileManager 的 exitMulti 调用点）。之前那个「全选」按钮是一次性把当前目录全选上，
   * 用户想选三个文件还得先全选再逐个取消。
   */
  multi: boolean
}

/**
 * 目录树里**每个目录**最多显示多少个文件（用户要求 20，按文件名）。
 *
 * 上限按「单个目录」算，整棵树没有总数上限（展开多少层就有多少行）；超过这个数的目录在树里
 * 多一行「···更多文件」，点它去文件管理打开该目录（契约 26.4）。
 */
export const TREE_FILE_LIMIT = 20

/** 视图偏好（列表 / 网格）的持久化键。纯界面偏好：跟随浏览器，不落后端、不随会话走。 */
const VIEW_KEY = 'webssh-files-view'
/** 权限显示模式与排序同理：是「这台设备上的人怎么看文件」的偏好，不是会话的属性。 */
const MODE_KEY = 'webssh-files-mode'
const SORT_KEY = 'webssh-files-sort'

const SORT_KEYS: SortKey[] = ['name', 'size', 'mtime']

/** localStorage 在隐私模式下会直接抛：读不到就用默认值，不值得为此打断页面。 */
function readStored(key: string): string {
  try {
    return localStorage.getItem(key) ?? ''
  } catch {
    return ''
  }
}

function writeStored(key: string, value: string) {
  try {
    localStorage.setItem(key, value)
  } catch {
    // 写不进去只意味着下次进来看不到这个偏好，不影响功能
  }
}

function readStoredView(): 'list' | 'grid' {
  return readStored(VIEW_KEY) === 'grid' ? 'grid' : 'list'
}

function readStoredMode(): PermMode {
  return readStored(MODE_KEY) === 'octal' ? 'octal' : 'symbolic'
}

/**
 * 读回排序偏好。
 *
 * 存储里的内容**不可信**（用户手改过、旧版本写过别的形状）：key/dir 都要逐字段校验，
 * 认不出来就退回「不排序」。直接 JSON.parse 出来的对象塞进 state，会让排序函数读到
 * undefined 而静默按名字排——用户明明选了「取消排序」，界面却还在排。
 */
function readStoredSort(): SortState {
  const raw = readStored(SORT_KEY)
  if (!raw) return { key: null, dir: 'asc' }
  try {
    const parsed = JSON.parse(raw) as Partial<SortState>
    const key = SORT_KEYS.find((k) => k === parsed.key) ?? null
    return { key, dir: parsed.dir === 'desc' ? 'desc' : 'asc' }
  } catch {
    return { key: null, dir: 'asc' }
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
    mode: readStoredMode(),
    sort: readStoredSort(),
    treeChildren: {},
    treeFileTotal: {},
    expanded: [],
    treeLoading: {},
    selected: [],
    multi: false
  }
}

export const useSftpStore = defineStore('sftp', () => {
  const states = ref<Record<string, SftpState>>({})

  /**
   * 剪贴板只有一份，**刻意不放进 SftpState**。
   *
   * 它是全局的（用户在一个会话里复制，然后切到另一个会话去看——这正是要拦住的情形），
   * 而 SftpState 是按 sid 分片的。放进分片里，「跨会话」这件事在数据结构上就不存在了，
   * 也就没法在目标会话里给出「不能跨会话粘贴」的提示。
   */
  const clipboard = ref<Clipboard | null>(null)

  /** /api/info 的运行信息（导出至飞牛的目标目录、压缩包输出目录）。 */
  const runtime = ref<RuntimeInfo | null>(null)
  let runtimePending: Promise<void> | null = null

  // 下面两份账本刻意留在闭包里、不进 SftpState：SftpState 是与契约逐字对应的展示状态，
  // 而「这个会话初始化过了没」「这个目录拉过了没」是请求记账，塞进 SftpState 就得给它加字段。
  //
  // 键必须是 sid + 路径：两个会话连的是不同主机，同一个路径在不同主机上完全是两个目录，
  // 只按路径记账会让 B 会话误以为「这个目录已经加载过」而拿去 A 会话的内容。
  const inited = new Set<string>()
  const loadedDirs = new Set<string>()

  const dirKey = (sid: string, path: string) => `${sid}\u0000${path}`

  /**
   * 取某个会话的展示状态（没有就建一个）。
   *
   * ⚠️ 新键必须**从 ref 上再读一次**再返回：`blankState()` 造出来的是**原始对象**，直接把它
   * 返回给「第一个调用方」，那个组件拿到的就是一层没有响应式的壳——它自己的模板与 watch 全都
   * 收不到后续改动（读到的值是对的，界面永远不刷新）。这正是第八轮用户报的
   * 「新打开会话时面包屑没有展开至家目录」的根因：左栏里的 BreadcrumbBar 不再读 `state.home`
   * 之后，FileTree 成了第一个调用方，于是它的 `watch(() => state.home)` 从注册那天起就没触发过。
   * （谁先调用是会随模板改动变化的，所以这里不能靠「反正有别的组件先建」来兜。）
   */
  function stateOf(sid: string): SftpState {
    if (!states.value[sid]) states.value[sid] = blankState()
    return states.value[sid]!
  }

  /** 把某个已加载过的路径从账本里清掉（删除 / 重命名之后调用）。 */
  function forgetLoaded(sid: string, path: string) {
    const prefix = `${sid}\u0000${path}`
    for (const key of [...loadedDirs]) {
      if (key === prefix || key.startsWith(`${prefix}/`)) loadedDirs.delete(key)
    }
  }

  /**
   * 树里一个目录要显示的子项：**全部目录 + 按名字排序的前 TREE_FILE_LIMIT 个文件**。
   *
   * 第十六轮起树也列文件（用户要求：能直接在树里找到配置文件去编辑），但每个目录只列前
   * TREE_FILE_LIMIT 个——一个 `node_modules` 或 `/usr/bin` 铺进树里会把整栏变成几千行的列表，
   * 而树是「快速跳转」用的。多出来的部分由树里的「···更多文件」那一行引导到文件管理去看
   * （契约 26.4）。**整棵树显示的文件总数不设上限**：每个目录各算各的。
   *
   * 三个写入点（ensure / loadTreeChildren / loadDir 的同步）都过这里，免得某一处漏过滤，
   * 表现是「树里有、列表里没有」或者文件总数对不上、该出现的「更多文件」行不出现。
   */
  function treeRows(st: SftpState, path: string, entries: FsEntry[]): FsEntry[] {
    const dirs = entries.filter((entry) => entry.dir)
    // 后端已经按名字排好，这里再排一次是为了不依赖「调用方给的是哪一份列表」（loadDir 的同步
    // 用的是文件管理器那一份，它可能来自排序后的视图）
    const files = entries
      .filter((entry) => !entry.dir)
      .sort((a, b) => compareEntries(a, b, 'name', 'asc'))
    st.treeFileTotal[path] = files.length
    return [...dirs, ...files.slice(0, TREE_FILE_LIMIT)]
  }

  /**
   * 从远程根 `/` 到 path 的完整链路（含 path 自己）：`/a/b` → `['/', '/a', '/a/b']`。
   *
   * 只做字符串切分，不发请求：路径是不是真的目录由后面逐层 loadTreeChildren 的结果回答，
   * 猜错（比如路径里有两层但中间那层不是目录）的表现只是「展开停在某层」，不会出错数据。
   */
  function ancestors(path: string): string[] {
    const out = ['/']
    if (!path || path === '/') return out
    let acc = ''
    for (const part of path.split('/').filter(Boolean)) {
      acc += `/${part}`
      out.push(acc)
    }
    return out
  }

  /**
   * 把一次列目录的结果装进状态：**成功路径唯一的写入点**（loadDir 与 jumpTo 都走它）。
   *
   * 分开的原因见 jumpTo：快捷访问的点击要「先探测、成功了才改状态」，而探测成功的那个响应
   * 就是这一次加载的结果——如果不抽出来，jumpTo 只能先探一次、再让 loadDir 请求第二次，
   * 目录一大的时候这一下是肉眼可见的。
   */
  function applyListing(sid: string, st: SftpState, res: FsListing) {
    st.home = res.home || st.home
    // 家目录请求（path 为空）时以响应里的解析结果为准
    st.dir = res.path || st.dir
    st.entries = res.entries
    loadedDirs.add(dirKey(sid, st.dir))
    // 树和列表是同一份数据：这个目录在树里已经展开过的话，一并换掉它的缓存，
    // 免得「列表里删掉的文件，树里还挂在那儿」。
    if (st.treeChildren[st.dir]) st.treeChildren[st.dir] = treeRows(st, st.dir, res.entries)
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
      applyListing(sid, st, res)
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
      st.treeChildren[path] = treeRows(st, path, res.entries)
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
    // 文件总数与 treeChildren 同生命周期：留着它，「更多文件」那一行会在缓存已经丢掉之后
    // 还挂在那儿（点进去才发现目录早就收起/删掉了）
    for (const key of Object.keys(st.treeFileTotal)) {
      if (key === path || key.startsWith(childPrefix)) delete st.treeFileTotal[key]
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
    for (const key of Object.keys(st.treeFileTotal)) {
      if (key !== from && !key.startsWith(childPrefix)) continue
      st.treeFileTotal[`${to}${key.slice(from.length)}`] = st.treeFileTotal[key]
      delete st.treeFileTotal[key]
    }
    st.expanded = st.expanded.map((key) =>
      key === from || key.startsWith(childPrefix) ? `${to}${key.slice(from.length)}` : key
    )
  }

  /** 首次进入会话：定位家目录并加载它的内容（树的根由 expandPath 按需拉，见下）。 */
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
    // 家目录这一份列表已经到手，顺手灌进树缓存，树里展开家目录时不用再请求一次。
    //
    // 树的根 `/` 不在这里拉：`ensure` 只回答「登录用户的家在哪」，而 `/` 那一层由
    // expandPath（首次定位到家目录时）按需拉——移动端根本没有树，不该替它多发一次 listing。
    st.treeChildren[st.home || st.dir] = treeRows(st, st.home || st.dir, st.entries)
  }

  /**
   * 展开到某个目录：收起其它展开项，只保留从根到 `path` 的那条链路。
   *
   * 两处用到：「家目录」按钮（11.3 要求全折叠 + 只展开家目录链路），以及首次进入会话时
   * 把登录用户的家目录露出来。链上每一层都要把子目录拉出来（懒加载），否则 rows 走到那个
   * 节点就断了——用户看到的是「展开了但底下空空」。
   */
  async function expandPath(sid: string, path: string): Promise<void> {
    const st = stateOf(sid)
    const chain = ancestors(path)
    const keep = new Set(chain)
    // 收起不在链上的节点，并丢掉它们的子树缓存。理由同 toggleExpand：树只在「没缓存」时
    // 才请求，留着缓存就再没有失效路径——重新展开看到的是几分钟前的目录内容。
    for (const node of [...st.expanded]) {
      if (keep.has(node)) continue
      if (st.treeChildren[node]) dropTreeKeys(st, node)
      else st.expanded = st.expanded.filter((item) => item !== node)
    }
    st.expanded = [...chain]
    // 逐层补缓存；某一层拉不到（权限不足、目录已被删）就停在上一层：留一串展开的空节点
    // 只会让人以为「这些目录都是空的」。
    for (const node of chain) {
      if (st.treeChildren[node]) continue
      try {
        await loadTreeChildren(sid, node)
      } catch {
        break
      }
    }
  }

  /** 强制重拉某个树节点的子目录（树的「刷新」）。当前目录就是它时，列表一起刷新。 */
  async function refreshTree(sid: string, path: string): Promise<void> {
    const st = stateOf(sid)
    // 没缓存说明这个节点没展开过，也就没东西需要刷新——但根节点例外（它总是显示着）
    if (path === '/' || st.treeChildren[path]) await loadTreeChildren(sid, path, true)
    // 树与列表是同一个目录的两份视图，只刷一份会出现「树里有、列表里没有」
    if (path === st.dir) await loadDir(sid, path, true)
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

  /**
   * 跳到某个目录，但**先探测**：列目录成功才动状态，失败（权限不足 / 目录不存在 / 会话没开）
   * 原样抛给调用方去报错。
   *
   * 给快捷访问的点击用（契约 30.3）。为什么不像 navigate 那样「先切过去、再在面板里显示错误」：
   *  - 入口在左栏按钮条与弹窗里，用户可能压根没在看文件管理——切过去只看到一屏错误；
   *  - 更实际的是它会毁掉现场：`back` 历史会记下「刚才那个目录」，当前目录也变成了一个打不开
   *    的路径，用户想的是「打开另一个目录」，不是「离开现在这个目录」。
   *
   * 探测的那次响应**直接当成本次加载的结果**（`applyListing`）：一次点击只发一次请求。
   * 探测失败时不动任何展示状态（`entries` / `error` / `back` 全部保持原样）。
   */
  async function jumpTo(sid: string, path: string): Promise<void> {
    const st = stateOf(sid)
    const target = path || st.home
    if (!target) throw new Error(t('files_quick_invalid'))
    const res = await api.fsList(sid, target)
    if (res.path && res.path !== st.dir) {
      if (st.dir) st.back.push(st.dir)
      st.forward = []
    }
    st.error = ''
    applyListing(sid, st, res)
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
    writeStored(VIEW_KEY, v)
  }

  /** 权限列的显示形态（符号 / 八进制）切换：两种形态来回切，所以只需要「切换」。 */
  function toggleMode(sid: string) {
    const mode: PermMode = stateOf(sid).mode === 'symbolic' ? 'octal' : 'symbolic'
    stateOf(sid).mode = mode
    writeStored(MODE_KEY, mode)
  }

  /**
   * 表头点击：`升序 → 降序 → 取消`（取消 = 回到服务端顺序）。
   *
   * 循环的第三态是刻意的：远程目录常见的顺序（后端按名字给）本身就有意义，用户试了两种
   * 排序之后必须有路回到它，否则只能刷新页面。
   */
  function cycleSort(sid: string, key: SortKey) {
    const st = stateOf(sid)
    if (st.sort.key !== key) st.sort = { key, dir: 'asc' }
    else if (st.sort.dir === 'asc') st.sort = { key, dir: 'desc' }
    else st.sort = { key: null, dir: 'asc' }
    writeStored(SORT_KEY, JSON.stringify(st.sort))
  }

  /**
   * 排好序的条目（模板用）。
   *
   * 不直接在 `entries` 上原地排序：`entries` 是服务端给的原始顺序，取消排序时要能原样回去；
   * 排序是对「怎么显示」的加工，改了就回不来了。目录恒在文件前由 compareEntries 保证。
   */
  function sorted(sid: string): FsEntry[] {
    const st = stateOf(sid)
    const { key, dir } = st.sort
    if (!key) return st.entries
    return [...st.entries].sort((a, b) => compareEntries(a, b, key, dir))
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

  // ---------- 复制 / 剪切 / 粘贴 ----------

  function copyToClipboard(sid: string, paths: string[]) {
    if (!paths.length) return
    clipboard.value = { sid, op: 'copy', paths: [...paths] }
  }

  function cutToClipboard(sid: string, paths: string[]) {
    if (!paths.length) return
    clipboard.value = { sid, op: 'cut', paths: [...paths] }
  }

  /** 剪贴板里能粘贴到 sid 的内容；空或来自别的会话时返回 null。 */
  function clipboardFor(sid: string): Clipboard | null {
    return clipboard.value && clipboard.value.sid === sid ? clipboard.value : null
  }

  /** 剪贴板有内容但属于别的会话（界面据此「禁用 + 说明原因」）。 */
  function clipboardIsForeign(sid: string): boolean {
    return clipboard.value !== null && clipboard.value.sid !== sid
  }

  /**
   * 清空剪贴板（文件管理里那行「剪贴板」的「取消」按钮）。
   *
   * 不走 sid：剪贴板全局只有一份，而「取消」的语义就是「把手上这份东西放下」，
   * 来自别的会话的那份也一并丢掉——否则它会在那个会话里一直挂着、点不掉。
   */
  function clearClipboard() {
    clipboard.value = null
  }

  /**
   * 粘贴到目标目录。
   *
   * **剪切和复制的清空语义不同**（第十一轮用户要求），判据分两层：
   * - **剪切**：真的贴上至少一项就**清空剪贴板**，与入口无关。剪切是「搬走」，这一批东西已经
   *   落到新位置，剪贴板（以及地址栏下面那行常驻栏）就该随之退出——留着它只有两种下场：要么
   *   指着一批已经不存在的源路径（下一次粘贴报 not_found），要么变成「再把这东西搬一次」，
   *   都不是用户点「剪切」时想要的。
   * - **复制**：清不清由**入口**决定（`keepClipboard`，第五轮定的）——右键/空白菜单里的
   *   「粘贴」（缺省）粘完就清（一次性动作）；文件管理里那行常驻「剪贴板」的「粘贴到此处」
   *   （keepClipboard=true）**不清空**，只有点「取消」才清。那一行的意义就是「同一批东西想粘到
   *   好几个目录去」，粘一次就清掉等于把它的用处抹掉一半。
   *
   * 失败、以及「一项都没贴上」（全被同名挡下）时一律**保留**剪贴板：那两种情况下什么都没发生，
   * 用户可以直接重试或换个目录粘——失败后还要重新去复制一遍是很烦的。（剪切也只按「真的贴上了
   * 至少一项」清空：一项都没贴上时那次剪切还没完成，剪贴板得留着。）
   *
   * 不跨会话在这里是**硬约束**：剪贴板里的路径属于另一个主机的文件系统，拿过来粘贴轻则
   * 「文件不存在」，重则删/改错了同名文件，必须由调用方先拦住（这里再兜一次底）。
   */
  async function paste(sid: string, toDir: string, keepClipboard = false): Promise<PasteOutcome> {
    const clip = clipboardFor(sid)
    if (!clip) {
      // 调用方按「同一会话才显示粘贴项」过滤过，走到这里说明状态被别处改了。明确抛错而不是
      // 静默返回：静默失败在界面上就是「点了没反应」。
      throw new Error(
        clipboardIsForeign(sid) ? t('files_paste_cross_session') : t('files_clipboard_empty')
      )
    }
    const op = clip.op
    const paths = [...clip.paths]
    const res =
      op === 'copy' ? await api.fsCopy(sid, paths, toDir) : await api.fsMove(sid, paths, toDir)
    const outcome: PasteOutcome = { count: res.files.length, skipped: res.skipped, conflicts: [] }
    if (res.skipped > 0) {
      // 后端只回「跳过了几项」，名字得自己对：源里的名字没出现在结果里 = 被同名挡下了。
      // （res.files 是**目标路径**；万一后端回的是源路径，差集就是空的，下面兜底。）
      const pastedNames = new Set(res.files.map((file) => baseName(file.path)))
      outcome.conflicts = paths.map((path) => baseName(path)).filter((n) => !pastedNames.has(n))
      if (!outcome.conflicts.length) outcome.conflicts = paths.map((path) => baseName(path))
    }
    if (outcome.count > 0) {
      // 剪切：粘完就清（不管从哪个入口粘的，剪切是「搬走」，搬完这次就结束）；
      // 复制：只有一次性入口（右键/空白菜单）粘完才清，常驻栏的「粘贴到此处」保留。
      if (op === 'cut' || !keepClipboard) clipboard.value = null
    }
    await refreshDir(sid, toDir)
    if (op === 'cut') {
      // 剪切是「搬走」：源目录里那一项已经没了，不刷新的话用户切回去还能看到它
      for (const dir of new Set(paths.map((p) => parentPath(p)))) {
        if (dir !== toDir) await refreshDir(sid, dir)
      }
    }
    return outcome
  }

  // ---------- 压缩 / 解压 / 导出至飞牛 ----------

  /**
   * 归档名（传后端的 `name`，后缀由后端按 format 补）。
   *
   * 单个条目用它自己的名字（`photos.tar.gz` 比 `webssh-20260214-101530.tar.gz` 好认太多）；
   * 多选同一个目录下的条目用父目录名；跨目录多选给不出有意义的名字，留空让后端用时间戳。
   * 名字是会被拼进**远程命令**的输入，所以只取基名，绝不带路径分隔符。
   */
  function archiveNameFor(paths: string[]): string {
    if (paths.length === 1) return baseName(paths[0]!)
    const parents = new Set(paths.map((path) => parentPath(path)))
    if (parents.size === 1) return baseName([...parents][0]!)
    return ''
  }

  /**
   * 压缩（tar.gz / zip）。
   *
   * 产物落在**服务端固定目录**（默认 /tmp/webssh-archive，见 /api/info 的 archiveDir），
   * 因此成功后把当前目录切过去：用户点「压缩」的直接目的就是拿到那个包，停在原目录里
   * 只看到一条 toast、还得自己去找路径，等于没完成。
   *
   * 后缀由**后端**按 format 补（archive.go 的 archiveName），所以这里只给不带后缀的名字。
   * zip 与 tar.gz 的差别只有「远端要装 zip 命令」——缺工具时后端回 no_archive_tool，
   * 由调用方把原文 toast 出来，前端不自己猜。
   */
  async function compress(
    sid: string,
    paths: string[],
    format: ArchiveFormat = 'tar.gz'
  ): Promise<ArchiveResult> {
    const res = await api.fsCompress(sid, paths, archiveNameFor(paths), format)
    await navigate(sid, res.archive.dir)
    // navigate 对「已经加载过且还在缓存里」的目录会直接返回（同目录不重复请求），而这里
    // 必须看到刚生成的那个包，所以强制再拉一次。
    await loadDir(sid, res.archive.dir, true)
    return res.archive
  }

  /** 解压（原位）。返回解压出来的目录，供调用方提示「已解压到 …」。 */
  async function extract(sid: string, path: string): Promise<string> {
    const res = await api.fsExtract(sid, path)
    const dir = res.dir || parentPath(path)
    await refreshDir(sid, dir)
    return dir
  }

  /** 导出到飞牛共享目录。同名冲突由后端回 `duplicate`，调用方确认后带 overwrite 重试。 */
  async function exportToNas(sid: string, path: string, overwrite = false): Promise<ExportResult> {
    const res = await api.fsExportToNas(sid, path, overwrite)
    return res.export
  }

  // ---------- 运行信息（导出目标 / 压缩包输出目录） ----------

  /**
   * 拿到 /api/info 的运行信息。
   *
   * 先看 home store：应用启动时它已经拉过一次 /api/info，两个 store 再各拉一次没有意义。
   * 它还没拉到时（比如直接进文件管理），才自己补一次。失败就保持 null——调用方按
   * 「拿不到运行信息 = 导出不可用」处理（后端没配 TRIM_DATA_SHARE_PATHS 时本来也是空的）。
   *
   * ⚠️ 这里对 home store 是**惰性**引用（只在函数体里调用 useHomeStore()）：home.ts 反过来
   * import 了本 store（关闭会话时要 forget），模块层再互相 import 会形成环，惰性调用把环
   * 推迟到「两个 store 都已经建好」之后。
   */
  async function ensureRuntime(): Promise<void> {
    if (runtime.value) return
    const cached = useHomeStore().runtime
    if (cached) {
      runtime.value = cached
      return
    }
    if (!runtimePending) {
      runtimePending = (async () => {
        try {
          runtime.value = (await api.info()).runtime
        } catch {
          // 拿不到就不重试了：同一个页面里反复拉只会刷控制台，导出项保持禁用并给出原因
          runtime.value = null
        } finally {
          runtimePending = null
        }
      })()
    }
    await runtimePending
  }

  /** 导出目标是本机的共享目录，未配置时为空 → 菜单项禁用。 */
  function exportRoot(): string {
    return runtime.value?.exportRoot ?? ''
  }

  /** 压缩包输出目录（文件管理器用它提示「你现在看到的为什么在 /tmp」）。 */
  function archiveDir(): string {
    return runtime.value?.archiveDir ?? ''
  }

  // ---------- 选择 ----------

  function select(sid: string, paths: string[]) {
    stateOf(sid).selected = [...paths]
  }

  function clearSelection(sid: string) {
    stateOf(sid).selected = []
  }

  /**
   * 进/出多选模式。两个方向都**清空已选**：
   *  - 进入时本来就没选（干净起点）；
   *  - 退出时清掉高亮，否则「退出了但东西还亮着」会让人以为选择还在（下次进模式又冒出来）。
   */
  function setMulti(sid: string, on: boolean) {
    const st = stateOf(sid)
    st.multi = on
    st.selected = []
  }

  /** 结束多选：做完一个批量操作（复制/剪切/删除/压缩/导出）之后收敛回普通浏览状态。 */
  function exitMulti(sid: string) {
    setMulti(sid, false)
  }

  /** 会话关闭时丢掉它的全部浏览状态（否则记录只增不减）。 */
  function forget(sid: string) {
    delete states.value[sid]
    inited.delete(sid)
    for (const key of [...loadedDirs]) {
      if (key.startsWith(`${sid}\u0000`)) loadedDirs.delete(key)
    }
    // 剪贴板里的路径属于这个会话的远程主机：会话没了，路径也就没有意义了。
    if (clipboard.value?.sid === sid) clipboard.value = null
  }

  return {
    clipboard,
    runtime,
    stateOf,
    ensure,
    ensureRuntime,
    exportRoot,
    archiveDir,
    navigate,
    jumpTo,
    back,
    forward,
    refresh,
    loadDir,
    setView,
    toggleMode,
    cycleSort,
    sorted,
    expandPath,
    refreshTree,
    toggleExpand,
    createFile,
    createDir,
    rename,
    remove,
    uploadLocal,
    uploadFromNas,
    download,
    copyToClipboard,
    cutToClipboard,
    clipboardFor,
    clipboardIsForeign,
    clearClipboard,
    paste,
    compress,
    extract,
    exportToNas,
    select,
    clearSelection,
    setMulti,
    exitMulti,
    forget
  }
})
