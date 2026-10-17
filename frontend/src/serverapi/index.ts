// API 客户端 —— **前后端契约的代码化**（后端实现在 backend/ 下，见 docs/CONTRACT.md）。
//
// 部署形态：前端静态资源与 API 都经 nginx / 飞牛统一网关反代到**后端 unix socket** 上的
// baseurl 前缀（WEBSSH_ADMIN_BASEURL）。baseurl 构建期未知，由后端在响应 index.html 时
// 注入 <base href>，所以这里一律用 document.baseURI 解析路径，绝不烧进产物。
//
// 两个刻意的约定（都来自实践，别改回去）：
//   1. **带参数的接口一律 POST + JSON body**。查询串在网关/反代链路上不保证原样送到后端，
//      而 body 是端到端可靠的；只有无参数的读取（/api/info 等）用 GET。
//   2. 下载走 fetch（POST）拿 blob 再落地，而不是 `<a href>`：失败时后者会把整个文档导航到
//      JSON 错误页，标签页与未保存状态全丢，用户只看到一行错误。
import { t } from '@/i18n'

/** 运行期 baseurl 前缀（'' 表示部署在根路径）。 */
export function runtimeBase(): string {
  if (typeof document !== 'undefined' && document.baseURI) {
    const p = new URL(document.baseURI).pathname
    return p.endsWith('/') ? p.slice(0, -1) : p
  }
  return import.meta.env.BASE_URL.replace(/\/$/, '') || ''
}

function apiURL(path: string): string {
  return runtimeBase() + path
}

/** 后端 {ok:false} 时抛出。code 是稳定的错误码（err_* 文案见 i18n）。 */
export class ApiError extends Error {
  readonly code: string
  readonly status: number

  constructor(status: number, code: string, message: string) {
    super(message)
    this.name = 'ApiError'
    this.status = status
    this.code = code
  }
}

function snippet(text: string, max = 200): string {
  const one = text.replace(/\s+/g, ' ').trim()
  return one.length > max ? `${one.slice(0, max)}…` : one
}

/** 把任意异常转成一句可展示的文案（ApiError 的 code 映射成人话）。 */
export function describeError(e: unknown): string {
  if (e instanceof ApiError) {
    const key = `err_${e.code}`
    const mapped = t(key)
    if (mapped !== key) return mapped
    return e.message
  }
  if (e instanceof Error) return e.message
  return String(e)
}

async function parseBody(res: Response): Promise<unknown> {
  const text = await res.text()
  if (!text) return null
  try {
    return JSON.parse(text)
  } catch {
    return { __raw: snippet(text) }
  }
}

interface RequestOptions {
  method?: 'GET' | 'POST'
  json?: unknown
  signal?: AbortSignal
}

/** 发一个返回 JSON 的请求；后端 {ok:false} 或非 2xx 一律抛 ApiError。 */
async function request<T>(path: string, options: RequestOptions = {}): Promise<T> {
  const init: RequestInit = {
    method: options.method ?? (options.json !== undefined ? 'POST' : 'GET'),
    signal: options.signal
  }
  if (options.json !== undefined) {
    init.headers = { 'Content-Type': 'application/json' }
    init.body = JSON.stringify(options.json)
  }
  const res = await fetch(apiURL(path), init)
  const body = (await parseBody(res)) as Record<string, unknown> | null
  const ok = body && body.ok !== false && res.ok
  if (!ok) {
    const code = (body?.code as string) || (res.ok ? 'internal' : `http_${res.status}`)
    const message =
      (body?.error as string) ||
      (body?.__raw as string) ||
      `${res.status} ${res.statusText}`
    throw new ApiError(res.status, code, message)
  }
  return body as T
}

/**
 * 发一个**流式**（NDJSON：一行一条记录）请求，每读到完整的一行就交给 onRecord。
 *
 * 为什么单独一条路：搜索的响应是**边搜边回**的（见 backend/files_search.go），而 `request()`
 * 会把整个 body 读完再解析——那就等于等整次遍历结束，「搜到一点显示一点」就没了。
 *
 * 分帧：一行一条 JSON（`\n` 结尾）。块边界可能落在行中间，所以按块 decode（`{stream:true}`，
 * 跨块被切开的半个 UTF-8 字符会拼回来）后留在 buffer 里，读到换行才解析。
 *
 * 错误同样分两种（与后端一致）：
 *  - **开流之前**：普通的 JSON 错误响应（状态码非 2xx，或 Content-Type 不是 NDJSON），
 *    这里按 ApiError 抛（判据与 `request()` 完全一致）；
 *  - **开流之后**：流里出现 `{"type":"error", code, error}`，也抛 ApiError——调用方只有一条
 *    错误通道，不必分两处处理。
 */
async function requestStream(
  path: string,
  json: unknown,
  onRecord: (rec: Record<string, unknown>) => void,
  signal?: AbortSignal
): Promise<void> {
  const res = await fetch(apiURL(path), {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(json),
    signal
  })
  const ctype = res.headers.get('Content-Type') ?? ''
  if (!res.ok || !res.body || !ctype.includes('application/x-ndjson')) {
    const body = (await parseBody(res)) as Record<string, unknown> | null
    const code = (body?.code as string) || (res.ok ? 'internal' : `http_${res.status}`)
    const message =
      (body?.error as string) || (body?.__raw as string) || `${res.status} ${res.statusText}`
    throw new ApiError(res.status, code, message)
  }

  const reader = res.body.getReader()
  const decoder = new TextDecoder()
  let buf = ''
  const handle = (line: string) => {
    let rec: Record<string, unknown>
    try {
      rec = JSON.parse(line) as Record<string, unknown>
    } catch {
      // 只在「后端坏了 / 中间层改写了正文」时才会走到：如实报出来，别当成「没有结果」
      throw new Error(t('files_search_bad_response', { line: line.slice(0, 80) }))
    }
    if (rec.type === 'error') {
      throw new ApiError(res.status, String(rec.code ?? 'internal'), String(rec.error ?? ''))
    }
    onRecord(rec)
  }
  // flush = true 时把残留的最后一行也处理掉（后端每行都带换行，这里只是不假设）
  const consume = (chunk: string, flush = false) => {
    buf += chunk
    for (let at = buf.indexOf('\n'); at >= 0; at = buf.indexOf('\n')) {
      const line = buf.slice(0, at).trim()
      buf = buf.slice(at + 1)
      if (line) handle(line)
    }
    if (flush && buf.trim()) {
      handle(buf.trim())
      buf = ''
    }
  }
  try {
    for (;;) {
      const { value, done } = await reader.read()
      if (done) break
      consume(decoder.decode(value, { stream: true }))
    }
  } finally {
    // 中途退出（错误 / 取消）时把 body 交回去，别让连接悬着
    void reader.cancel().catch(() => {})
  }
  consume(decoder.decode(), true)
}

// ===== 类型 =====

export type AuthType = 'password' | 'key'

/** 远程会话的配置（不含任何明文凭据）。 */
export interface SessionView {
  id: string
  name: string
  host: string
  port: number
  user: string
  authType: AuthType
  hasPassword: boolean
  hasPrivateKey: boolean
  /**
   * 是否把凭据加密落盘。为 false（表单里关掉「保存凭据」）时数据目录里不写密码/私钥，
   * 凭据只活在后端内存里：会话关闭或后端重启后，下次 open 必须先输入凭据
   * （后端回 credential_required）。
   */
  credentialSaved: boolean
  /** 私钥对应公钥的 SHA256 指纹（展示用，不是敏感信息） */
  keyFingerprint?: string
  note: string
  createdAt: string
  updatedAt: string
  lastUsedAt?: string
}

/**
 * 新增/编辑会话的输入。密码与私钥留空（或干脆不发）表示「不修改已保存的值」。
 *
 * `host` / `user` / `authType` 在**更新**时也可以省：后端按「部分更新」把缺省字段合并成现有
 * 配置里的值（见 CONTRACT 4.3 与 backend/sessions.go 的 resolveSessionFields）。那是给
 * 「只改名字」这类调用方用的（home.renameSession）——回传一份**本地缓存**的旧值等于把别的
 * 设备刚改过的地址覆盖回去。新增会话时三者必填（否则后端会以 invalid 拒绝）。
 */
export interface SessionInput {
  id?: string
  name?: string
  host?: string
  port?: number
  user?: string
  authType?: AuthType
  password?: string
  privateKey?: string
  passphrase?: string
  note?: string
  /** false = 不落盘凭据（只在本进程内存里用于连接）；缺省 = true。 */
  saveCredential?: boolean
}

/** 打开会话时的可选凭据：会话没保存凭据（credentialSaved=false）时必填。 */
export interface OpenSessionInput {
  id: string
  password?: string
  privateKey?: string
  passphrase?: string
}

export type TestStage = 'ok' | 'resolve' | 'dial' | 'handshake' | 'hostkey' | 'auth' | 'shell'

/** 连接测试结果。success 为假时 code/detail 给出原因，交给 i18n 渲染成人话。 */
export interface TestOutcome {
  success: boolean
  code: string
  detail?: string
  stage?: TestStage
  fingerprint?: string
  keyType?: string
  /** 主机密钥已在 known_hosts 里且与本次一致 */
  trusted?: boolean
  /** 主机密钥与已记录的不一致（可疑，必须用户确认后手动移除旧记录） */
  mismatch?: boolean
  banner?: string
  latencyMs?: number
}

export interface TerminalInfo {
  id: string
  title: string
  createdAt: string
  exited: boolean
}

/** 已打开会话的实时状态（后端内存态，进程重启即消失）。 */
export interface SessionState {
  id: string
  open: boolean
  openedAt?: string
  terminals: TerminalInfo[]
  sftpOpen: boolean
}

/**
 * 已打开会话所在远端主机的系统信息（卡片上的两行数字，契约 23）。
 *
 * `available === false` 表示**远端读不出来**（不是 Linux、没有 /proc 且 df 也没输出）：
 * 这不是错误，界面整块不显示即可，原因在 `reason` 里（只给日志，不给用户看）。
 * 各字段互相独立：macOS 那种没有 /proc 的机器会只给 diskPercent。
 */
export interface SessionStats {
  available: boolean
  reason?: string
  /** CPU 占用率 0–100。 */
  cpuPercent?: number
  /** 已用 / 总内存，单位字节。 */
  memUsed?: number
  memTotal?: number
  /** 家目录所在文件系统的占用率 0–100。 */
  diskPercent?: number
  /** 1 / 5 / 15 分钟平均负载。 */
  load?: number[]
  /**
   * 在线 CPU 核数。负载要除以它才有意义（1.5 在 2 核上是重载、在 32 核上什么都不算），
   * 前端据此折算负载的绿黄红；缺省（非 Linux）时按 1 核处理。
   */
  cpuCount?: number
}

export interface FsEntry {
  name: string
  path: string
  dir: boolean
  size: number
  mode: string
  mtime: string
  symlink?: string
  broken?: boolean
}

/** 单个路径的信息（「信息」弹窗）：FsEntry 的全部字段 + 属主 + 直接子项的计数。 */
export interface FsInfo extends FsEntry {
  /** 属主的数字 uid/gid（SFTP v3 只给数字） */
  uid: number
  gid: number
  /** 在远端查出来的账号名/组名；查不到就没有这两个字段（界面退回显示数字） */
  owner?: string
  group?: string
  /** 直接子项里的目录数（跟随软链接，与列表的目录/文件图标口径一致） */
  dirCount: number
  /** 直接子项里的文件数 */
  fileCount: number
  /** 非空 = 数子项失败的原因（多半是权限）：界面如实显示，**不是**「空目录」 */
  countError?: string
}

/**
 * 编辑页打开时拿到的内容与元信息（契约 26.2 的 `/api/fs/edit-load`）。
 *
 * `encoding` / `bom` / `eol` 是「保存时怎么把文件写回去」的全部依据，前端只负责原样带回：
 * 把 Windows 的 `.ini` 悄悄改成 UTF-8 + LF 是数据损坏，不是「顺手升级」。
 */
export interface FsEditContent {
  /** 解析后的绝对路径（后端回填，前端用它做刷新与标题） */
  path: string
  size: number
  /** 文本内容；换行一律是 LF（CRLF 由后端在保存时按 eol 换回去） */
  content: string
  /** 'utf-8' | 'utf-16le' | 'utf-16be' */
  encoding: string
  /** 原文件是否以 BOM 开头（保存时保持一致） */
  bom: boolean
  /** 'lf' | 'crlf' */
  eol: string
}

/** 批量文件操作（复制/移动/粘贴）的结果。 */
export interface FsOpResult {
  files: { path: string; size: number }[]
  /** 因目标同名而跳过的数量（复制/移动遇到同名一律跳过，不覆盖） */
  skipped: number
}

/**
 * 列目录的响应（`/api/fs/list`，契约 4.2）。
 *
 * `path` 是**后端解析后**的绝对路径、`home` 是登录用户的家目录：请求里给空路径（家目录）时
 * 只有后端知道答案，前端一律以响应为准（不同账号可能是 /root、/home/x，也可能被 chroot 到 /）。
 * 快捷访问的「先探测再跳转」（stores/sftp.ts 的 jumpTo）用的就是这个响应。
 */
export interface FsListing {
  path: string
  home: string
  entries: FsEntry[]
}

/**
 * 一次搜索的收尾统计（`/api/fs/search` 的 `done` 记录，契约 32）。
 *
 * 界面上用来说明「为什么慢 / 结果全不全」：扫描了多少条目、多少个目录、用时多久、有没有触到
 * 上限被截断、有多少个目录因为读不出来被跳过。数值全部来自后端，前端不自己数。
 */
export interface FsSearchSummary {
  /** 结果条数 */
  total: number
  /** 扫描过的条目数 */
  scanned: number
  /** 读过的目录数 */
  dirs: number
  /** 读不出来被跳过的目录数（权限 / 遍历途中被删） */
  skipped: number
  /** 触到上限提前结束：界面必须说「已截断」，不能装作搜完了 */
  truncated: boolean
  elapsedMs: number
}

/** `/api/fs/search` 的流式回调。 */
export interface FsSearchHandlers {
  /** 起点（后端解析后的**绝对**路径）：结果的 path 用它折成相对路径 */
  onStart: (path: string) => void
  /** 每收到一批结果回调一次（「搜到一点显示一点」） */
  onBatch: (entries: FsEntry[]) => void
  /** 扫描进度（后端节流，约 400ms 一次） */
  onProgress?: (scanned: number, dirs: number) => void
  signal?: AbortSignal
}

/** 支持的压缩格式：tar.gz（默认）与 zip。 */
export type ArchiveFormat = 'tar.gz' | 'zip'
/** 压缩结果：压缩包落在 archiveDir 里。 */
export interface ArchiveResult {
  path: string
  size: number
  dir: string
}

/** 导出至飞牛的结果。 */
export interface ExportResult {
  /** 本机（NAS）上的落盘路径 */
  path: string
  size: number
  files: number
}

export interface QuickCmd {
  id: string
  name: string
  content: string
  auto: boolean
}

export interface HostKeyRecord {
  host: string
  port: number
  keyType: string
  fingerprint: string
  addedAt: string
}

export interface TrimStatus {
  available: boolean
  message: string
  app: string
}

export interface AclEntry {
  path: string
  readable: boolean
  writable: boolean
  deletable: boolean
}

export interface GatewayUser {
  uid: number
  username: string
  isAdmin: boolean
  fromGateway: boolean
}

export interface RuntimeInfo {
  version: string
  /** 后端进程实例标识：变了说明进程重启过，旧会话全部失效 */
  bootId: string
  dataDir: string
  adminSock: string
  adminBaseURL: string
  hostname: string
  /** 远程压缩包的固定输出目录（默认 /tmp/webssh-archive） */
  archiveDir: string
  /** 导出至飞牛的目标目录（TRIM_DATA_SHARE_PATHS 取第一个冒号之前的部分）；空 = 不可用 */
  exportRoot: string
  trim: TrimStatus
  gateway: GatewayUser
}

// ===== WebSocket 控制帧 =====
// 客户端 → 服务端（不写入远程 PTY）：resize / 心跳。
export function resizePayload(cols: number, rows: number): string {
  return `\x1b]resize;${cols};${rows}\x07`
}
export const HEARTBEAT_PAYLOAD = '\x1b]ping\x07'

// 服务端 → 客户端：
/** 新建终端时下发的终端 id：`\x1b]tid;<id>\x07` */
export const TID_PAYLOAD_RE = /\x1b\]tid;([0-9a-zA-Z_-]+)\x07/g
/** 历史回放结束、进入实时流 */
export const READY_PAYLOAD = '\x1b]ready\x07'
/** 远程 shell 退出 */
export const EXIT_PAYLOAD = '\x1b]exit\x07'
/** 本端被其他设备顶掉（与 WS_CLOSE_DETACHED 双保险） */
export const DETACHED_PAYLOAD = '\x1b]detached\x07'
/** 「该终端在后端不存在」，附在错误文本帧末尾 */
export const GONE_PAYLOAD = '\x1b]gone\x07'
/** 「已被其他设备接管」的 WebSocket 关闭码 */
export const WS_CLOSE_DETACHED = 4001
/**
 * 「新建子终端失败」的 WebSocket 关闭码（没有可用凭据 / 认证失败 / 远端拒绝 PTY……）。
 *
 * 与 GONE_PAYLOAD 的语义必须分开：gone = 终端/会话在后端不存在（标签该消失），
 * 创建失败 = 这次没建成（标签保留并标红，修正凭据后可以重连）。
 */
export const WS_CLOSE_CREATE_FAILED = 4002

/** 终端 WebSocket 地址：tid 为空 = 新建一个终端（后端回 TID_PAYLOAD_RE 回填 id）。 */
export function wsUrl(sid: string, tid?: string | null): string {
  const proto = location.protocol === 'https:' ? 'wss:' : 'ws:'
  const u = new URL(apiURL('/api/terminal/ws'), location.href)
  u.protocol = proto
  u.searchParams.set('sid', sid)
  if (tid) u.searchParams.set('tid', tid)
  return u.toString()
}

// ===== 上传 / 下载 =====

/** 一个待上传的本地条目：path 是相对路径（目录上传时含子目录，例如 `a/b.txt`）。 */
export interface UploadEntry {
  file: File
  path: string
}

export interface UploadResult {
  files: { path: string; size: number }[]
  /** 因已存在且未开启覆盖而跳过的数量 */
  skipped: number
}

/**
 * 上传本地文件 / 目录（multipart）。用 XHR 是为了拿到上传进度；signal 一 abort 就中断请求，
 * 后端 ctx 随之取消，不会留下写了一半的文件。
 */
export function uploadLocal(
  sid: string,
  dir: string,
  entries: UploadEntry[],
  overwrite: boolean,
  onProgress?: (percent: number) => void,
  signal?: AbortSignal
): Promise<UploadResult> {
  const form = new FormData()
  form.append('sid', sid)
  form.append('path', dir)
  form.append('overwrite', overwrite ? 'true' : 'false')
  for (const entry of entries) form.append('files', entry.file, entry.path)

  return new Promise<UploadResult>((resolve, reject) => {
    const xhr = new XMLHttpRequest()
    xhr.open('POST', apiURL('/api/fs/upload'))
    const onAbort = () => xhr.abort()
    const detach = () => signal?.removeEventListener('abort', onAbort)
    if (signal) {
      if (signal.aborted) {
        reject(new ApiError(0, 'aborted', 'aborted'))
        return
      }
      signal.addEventListener('abort', onAbort, { once: true })
    }
    xhr.upload.addEventListener('progress', (e) => {
      if (e.lengthComputable && onProgress) onProgress(Math.round((e.loaded / e.total) * 100))
    })
    xhr.addEventListener('load', () => {
      detach()
      let parsed: (UploadResult & { ok?: boolean; error?: string; code?: string }) | null = null
      try {
        parsed = JSON.parse(xhr.responseText)
      } catch {
        parsed = null
      }
      if (xhr.status >= 200 && xhr.status < 300 && parsed && parsed.ok !== false) {
        resolve(parsed)
        return
      }
      reject(new ApiError(xhr.status, parsed?.code || 'upload_failed', parsed?.error || `HTTP ${xhr.status}`))
    })
    xhr.addEventListener('error', () => {
      detach()
      reject(new ApiError(0, 'network', 'network error'))
    })
    xhr.addEventListener('abort', () => {
      detach()
      reject(new ApiError(0, 'aborted', 'aborted'))
    })
    xhr.send(form)
  })
}

/** 触发浏览器下载（fetch + blob，失败能把原因报出来）。 */
export async function downloadPath(sid: string, path: string): Promise<void> {
  const res = await fetch(apiURL('/api/fs/download'), {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ sid, path })
  })
  if (!res.ok) {
    const body = (await parseBody(res)) as Record<string, unknown> | null
    throw new ApiError(
      res.status,
      (body?.code as string) || `http_${res.status}`,
      (body?.error as string) || `${res.status} ${res.statusText}`
    )
  }
  const blob = await res.blob()
  const url = URL.createObjectURL(blob)
  try {
    const a = document.createElement('a')
    a.href = url
    a.download = filenameFrom(path, res.headers.get('Content-Disposition'))
    a.rel = 'noopener'
    document.body.appendChild(a)
    a.click()
    document.body.removeChild(a)
  } finally {
    window.setTimeout(() => URL.revokeObjectURL(url), 10_000)
  }
}

function filenameFrom(path: string, disposition: string | null): string {
  const quoted = /filename="([^"]+)"/i.exec(disposition ?? '')
  if (quoted?.[1]) return quoted[1]
  const base = path.slice(path.lastIndexOf('/') + 1)
  return base || 'download'
}

/**
 * 读取远程图片文件（文件管理器预览用）：成功回**原始字节**的 Blob。
 *
 * 失败（不是图片 / 超过 4 MiB / 不存在）后端回 JSON 错误 → ApiError，前端按 code 分别提示。
 * 走 fetch 拿 blob 而不是 `<img src="/api/fs/image?path=…">`：本应用带参数的接口一律
 * POST + JSON body（查询串在反代链路上不保证送达），而且 <img> 遇到错误响应会把整个文档
 * 导航到那段 JSON 上，标签页与界面状态全丢。
 *
 * 调用方拿到 blob 后要 `URL.createObjectURL` 并**负责 revoke**（见 PreviewDialog）。
 */
export async function fetchImageBlob(sid: string, path: string, signal?: AbortSignal): Promise<Blob> {
  const res = await fetch(apiURL('/api/fs/image'), {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ sid, path }),
    // 可选取消：弹窗关掉 / 换文件时把还在飞的请求掐掉（一张 4 MiB 的图不必白传完，
    // 后端的 SFTP 也会因为 ctx 结束立刻收手）。
    signal
  })
  if (!res.ok) {
    const body = (await parseBody(res)) as Record<string, unknown> | null
    throw new ApiError(
      res.status,
      (body?.code as string) || `http_${res.status}`,
      (body?.error as string) || `${res.status} ${res.statusText}`
    )
  }
  return await res.blob()
}

// ===== API =====

export const api = {
  info: () => request<{ ok: boolean; runtime: RuntimeInfo }>('/api/info'),

  /** 会话配置 + 已打开会话的实时状态。 */
  sessions: () =>
    request<{ ok: boolean; sessions: SessionView[]; open: SessionState[] }>('/api/sessions'),

  /** 新增或更新（带 id 即更新）；同一 host+port+user 重复会返回 err_duplicate。 */
  saveSession: (input: SessionInput) =>
    request<{ ok: boolean; session: SessionView }>('/api/sessions/save', { json: input }),

  deleteSession: (id: string) =>
    request<{ ok: boolean }>('/api/sessions/delete', { json: { id } }),

  /** 连接测试：不会建立持久会话，返回指纹/延迟/失败原因。 */
  testSession: (input: SessionInput) =>
    request<{ ok: boolean; result: TestOutcome }>('/api/sessions/test', { json: input }),

  /** 首次信任某主机密钥（TPOU：只信任用户确认过的指纹）。 */
  trustHostKey: (host: string, port: number, fingerprint: string) =>
    request<{ ok: boolean }>('/api/sessions/trust', { json: { host, port, fingerprint } }),

  /**
   * 打开会话。`credentialSaved === false` 的会话必须在这里带上凭据，否则返回
   * `credential_required`（此时前端弹凭据输入框，用户填完再调一次）。
   */
  openSession: (input: OpenSessionInput) =>
    request<{ ok: boolean; state: SessionState }>('/api/sessions/open', { json: input }),

  /** 关闭会话：终止它下面所有终端与 SFTP 连接（配置保留）。 */
  closeSession: (id: string) => request<{ ok: boolean }>('/api/sessions/close', { json: { id } }),

  /**
   * 已打开会话的系统信息（CPU / 负载 / 内存 / 磁盘）。
   *
   * 只有打开着的会话才有：没打开时后端回 `not_open`（409）。远端读不出来时**不是**错误，
   * 而是 `stats.available === false`。
   */
  sessionStats: (id: string) =>
    request<{ ok: boolean; stats: SessionStats }>('/api/sessions/stats', { json: { id } }),

  terminalHistory: (sid: string, tid: string) =>
    request<{ ok: boolean; content: string; size: number }>('/api/terminals/history', {
      json: { sid, tid }
    }),

  clearTerminalHistory: (sid: string, tid: string) =>
    request<{ ok: boolean }>('/api/terminals/clear', { json: { sid, tid } }),

  closeTerminal: (sid: string, tid: string) =>
    request<{ ok: boolean }>('/api/terminals/close', { json: { sid, tid } }),

  renameTerminal: (sid: string, tid: string, title: string) =>
    request<{ ok: boolean }>('/api/terminals/rename', { json: { sid, tid, title } }),

  fsList: (sid: string, path: string) =>
    request<FsListing & { ok: boolean }>('/api/fs/list', {
      json: { sid, path }
    }),

  /**
   * 单个路径的信息（「信息」弹窗）：条目自身属性 + 目录**直接子项**的计数。
   *
   * 列目录（fsList）拿不到「当前目录自己」的属性（它返回的是目录里的东西），所以这里必须
   * 单独问一次；`countError` 非空表示数子项失败（多半是权限），界面要如实说原因，
   * **不能**把 0 当成「空目录」显示。
   */
  fsInfo: (sid: string, path: string) =>
    request<{ ok: boolean; info: FsInfo }>('/api/fs/info', { json: { sid, path } }),

  /**
   * 搜索（第二十二轮，契约 32）：在 `path`（空 = 家目录）的**整棵子树**里，按**条目自己的
   * 名字**做不区分大小写的子串匹配（32.6 起不再匹配路径——按路径匹配时，搜一个常见目录名会把
   * 那个目录下的每一条都倒出来），结果**边搜边回**（流式 NDJSON，见 requestStream）。
   *
   * 上限都在后端（2000 条结果 / 20 万条目 / 10 层 / 15 秒），触顶时 `truncated` 为真——
   * 界面要照实说「已截断」，不能假装搜完了。没命中也**不是错误**：返回一份 total=0 的统计。
   *
   * `signal` 用于「取消」和「换关键词重搜」时中断请求（后端据此停掉遍历）。
   */
  fsSearch: async (sid: string, path: string, query: string, h: FsSearchHandlers) => {
    const summary: { value: FsSearchSummary | null } = { value: null }
    await requestStream(
      '/api/fs/search',
      { sid, path, query },
      (rec) => {
        switch (rec.type) {
          case 'start':
            h.onStart(String(rec.path ?? ''))
            break
          case 'batch':
            h.onBatch((rec.entries as FsEntry[] | undefined) ?? [])
            break
          case 'progress':
            h.onProgress?.(Number(rec.scanned ?? 0), Number(rec.dirs ?? 0))
            break
          case 'done':
            summary.value = {
              total: Number(rec.total ?? 0),
              scanned: Number(rec.scanned ?? 0),
              dirs: Number(rec.dirs ?? 0),
              skipped: Number(rec.skipped ?? 0),
              truncated: rec.truncated === true,
              elapsedMs: Number(rec.elapsedMs ?? 0)
            }
            break
          default:
            // 认不出的记录类型：忽略（前向兼容），但不会静默吞掉错误——error 由 requestStream 抛
            break
        }
      },
      h.signal
    )
    // 流正常结束却没有 done：这次搜索的结论不可信（结果可能只有一半），不能当成功
    if (!summary.value) throw new Error(t('files_search_incomplete'))
    return summary.value
  },

  fsMkdir: (sid: string, path: string) =>
    request<{ ok: boolean }>('/api/fs/mkdir', { json: { sid, path } }),

  fsCreate: (sid: string, path: string) =>
    request<{ ok: boolean }>('/api/fs/create', { json: { sid, path } }),

  fsRename: (sid: string, path: string, to: string) =>
    request<{ ok: boolean }>('/api/fs/rename', { json: { sid, path, to } }),

  fsRemove: (sid: string, path: string, dir: boolean) =>
    request<{ ok: boolean }>('/api/fs/remove', { json: { sid, path, dir } }),

  /** 批量复制到目标目录（远端内部复制；同名跳过）。 */
  fsCopy: (sid: string, paths: string[], toDir: string) =>
    request<FsOpResult & { ok: boolean }>('/api/fs/copy', { json: { sid, paths, toDir } }),

  /** 批量移动到目标目录（粘贴剪切板用；同名跳过）。 */
  fsMove: (sid: string, paths: string[], toDir: string) =>
    request<FsOpResult & { ok: boolean }>('/api/fs/move', { json: { sid, paths, toDir } }),

  /**
   * 压缩：调用远程主机的 tar / zip，产物落在服务端固定的 archiveDir（/tmp/webssh-archive）。
   * `format` 缺省 = `tar.gz`；`zip` 需要远端有 `zip` 命令（缺工具报 `no_archive_tool`）。
   */
  fsCompress: (sid: string, paths: string[], name?: string, format?: ArchiveFormat) =>
    request<{ ok: boolean; archive: ArchiveResult }>('/api/fs/compress', {
      json: { sid, paths, name: name ?? '', format: format ?? 'tar.gz' }
    }),

  /** 解压：调用远程主机的 tar / unzip，在原位解压。 */
  fsExtract: (sid: string, path: string) =>
    request<{ ok: boolean; dir: string }>('/api/fs/extract', { json: { sid, path } }),

  /** 导出至飞牛：把远程文件/目录拉回本机共享目录（TRIM_DATA_SHARE_PATHS 的第一个路径）。 */
  fsExportToNas: (sid: string, path: string, overwrite: boolean) =>
    request<{ ok: boolean; export: ExportResult }>('/api/fs/export-to-nas', {
      json: { sid, path, overwrite }
    }),

  /** 从飞牛（本机 NAS）已有的文件上传到远程主机：服务端读取本地路径后经 SFTP 写入。 */
  fsUploadFromNas: (sid: string, dir: string, source: string, overwrite: boolean) =>
    request<UploadResult & { ok: boolean }>('/api/fs/upload-from-nas', {
      json: { sid, path: dir, source, overwrite }
    }),

  /**
   * 读取远程文本文件（文件管理器预览用，超过上限时后端返回 tooLarge）。
   *
   * `text` 为 false 表示后端判定这不是能展示的文本（二进制 / 认不出的编码），此时
   * `content` 是空串；`encoding` 是认出来的编码（utf-8 / utf-16le / utf-16be，认不出为空）。
   */
  fsRead: (sid: string, path: string, signal?: AbortSignal) =>
    request<{
      ok: boolean
      size: number
      text: boolean
      tooLarge: boolean
      content: string
      encoding: string
    }>('/api/fs/read', { json: { sid, path }, signal }),

  /**
   * 打开编辑器（契约 26.2）：先验证**本会话的登录用户**能不能改这个文件，再回内容。
   *
   * 没有写权限时后端回 `permission`（403）且**一个字节的内容都不给**——调用方据此报错并
   * 拒绝进入编辑页（用户要求）。`encoding` / `bom` / `eol` 是保存时要原样带回去的元信息：
   * 编辑区里的内容一律是 LF（textarea 的 value 按 HTML 规范把 CRLF 规整成 LF），写回时由
   * 后端按 `eol` 换回去，所以前端不自己动换行。
   */
  fsEditLoad: (sid: string, path: string) =>
    request<FsEditContent & { ok: boolean }>('/api/fs/edit-load', { json: { sid, path } }),

  /** 保存编辑内容（契约 26.2）：编码 / BOM / 换行按 `fsEditLoad` 给的原样写回。 */
  fsEditSave: (
    sid: string,
    path: string,
    content: string,
    meta: { encoding: string; bom: boolean; eol: string }
  ) =>
    request<{ ok: boolean; size: number }>('/api/fs/edit-save', {
      json: { sid, path, content, ...meta }
    }),

  // 快捷指令：sid 为空 = 全局；否则是该会话的专属指令。
  quickCmds: (sid?: string) =>
    request<{ ok: boolean; scope: string; commands: QuickCmd[] }>('/api/quickcmds/list', {
      json: { sid: sid ?? '' }
    }),

  saveQuickCmds: (sid: string | undefined, commands: QuickCmd[]) =>
    request<{ ok: boolean; scope: string; commands: QuickCmd[] }>('/api/quickcmds/save', {
      json: { sid: sid ?? '', commands }
    }),

  /**
   * 快捷访问（第二十轮，契约 30）：**按会话**存的一串远程目录路径，没有全局表——
   * 路径是某台远程主机上的绝对路径，跨会话共享只会是一条条点了报错的项。
   *
   * 保存是整表覆盖（与快捷指令同一套）：去重与规整都由后端做，前端发整份列表。
   * 会话不存在（已被删）时回 `session_gone`。
   */
  quickAccess: (sid: string) =>
    request<{ ok: boolean; paths: string[] }>('/api/quickaccess/list', { json: { sid } }),

  saveQuickAccess: (sid: string, paths: string[]) =>
    request<{ ok: boolean; paths: string[] }>('/api/quickaccess/save', { json: { sid, paths } }),

  hostKeys: () => request<{ ok: boolean; hosts: HostKeyRecord[] }>('/api/hostkeys'),

  forgetHostKey: (host: string, port: number) =>
    request<{ ok: boolean }>('/api/hostkeys/forget', { json: { host, port } }),

  trimStatus: () => request<{ ok: boolean; trim: TrimStatus }>('/api/trim/status'),

  trimAcl: (paths: string[]) =>
    request<{ ok: boolean; list: AclEntry[] }>('/api/trim/acl', { json: { paths } })
}

export type Api = typeof api
