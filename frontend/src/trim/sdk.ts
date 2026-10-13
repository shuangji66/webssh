/**
 * 飞牛（fnOS）宿主能力的薄封装。
 *
 * 为什么要包一层：`@trimjs/web-app` 是宿主注入的微应用 SDK，页面既可能跑在飞牛的应用
 * iframe 里，也可能被人直接用浏览器打开（本项目的后端只监听 unix socket，很多人是桥出来
 * 自己点的）。两种情况差别只在「有没有宿主桥」：
 *
 *   - 跑在宿主里（`isStandaloneWeb === false`）：文件选择、授权、读主题都能直接调；
 *   - 独立浏览器页面（`isStandaloneWeb === true`）：这些能力一律没有，调了只会抛
 *     「Host bridge is not available」。所以这里统一先判环境，调用方只需要处理
 *     「不可用」这一个分支——界面上给一句「当前不在飞牛宿主里」，而不是摆一个点了就报错的按钮。
 *
 * 官方推荐的另一条路是 `openAppAuth`（打开系统授权页 + redirectUri 回调），它要求应用挂在
 * 统一网关下、回调页与原页面同源。本项目的部署形态是「反代到 unix socket」，没有这条链路，
 * 所以独立浏览器里干脆不提供入口，明确告诉用户去飞牛里打开。
 *
 * 后端接口（trim.file.checkUserACL / convertPath / getUserAccessibleFolders）不在这里调：
 * 它们只能由服务端经 unix socket 调，前端一律走 /api/trim/*（token 不能进浏览器）。
 *
 * 本文件只提供「选文件」这一条能力，因为本项目只有上传飞牛文件用得到它。不用的能力
 * （打开文件管理器、读宿主主题）不在这里留占位实现：主题由 useTheme 自己按
 * prefers-color-scheme 处理。
 */
import { TrimApp } from '@trimjs/web-app'
import { t } from '@/i18n'

/** 文件选择器左侧的分组（官方枚举）。 */
export type SidebarGroup = 'myFiles' | 'otherShare' | 'external' | 'remote' | 'favorites' | 'team'

/**
 * 选择器要显示的分组：**除了远程挂载和他人分享，其余都列出来**。
 *
 * 「远程挂载」里常常是没挂好的网盘 / SMB，「他人分享」不是当前用户自己的文件，
 * 拿来上传到远程主机都容易选错。
 */
export const SIDEBAR_GROUPS: SidebarGroup[] = ['myFiles', 'external', 'favorites', 'team']

/** 宿主能力调用失败（业务码非 0）。 */
export class TrimError extends Error {
  readonly code: number

  constructor(code: number, message: string) {
    super(message)
    this.name = 'TrimError'
    this.code = code
  }
}

/** 宿主能力用不了：压根没有宿主桥（standalone），或者宿主桥迟迟不完备（notReady）。 */
export class TrimUnavailableError extends Error {
  readonly reason: 'standalone' | 'notReady'

  constructor(reason: 'standalone' | 'notReady' = 'standalone') {
    super(reason === 'standalone' ? 'not running inside the fnOS host' : 'host bridge is not ready')
    this.name = 'TrimUnavailableError'
    this.reason = reason
  }
}

/** 等宿主桥就绪的上限。正常宿主里初始化是百毫秒级，这里给足余量。 */
const HOST_READY_TIMEOUT_MS = 10_000

const sdk = new TrimApp()

/**
 * 宿主能力是否可用。
 *
 * 只判 `isStandaloneWeb`：移动端 App 内嵌页面（`isWeb === false`）同样支持文件选择与授权，
 * 把它一起挡掉没有道理。
 */
export function isHostAvailable(): boolean {
  return !sdk.isStandaloneWeb
}

/**
 * 确认宿主桥真的可用。
 *
 * 只判 `isStandaloneWeb` 是不够的：页面被塞进一个普通 iframe（比如有人在自建页面上嵌了本
 * 应用）时，`isStandaloneWeb` 是 false，但对面根本不回应握手，SDK 的初始化 Promise 永远不
 * settle——文件选择器调用会一直挂着，界面只能一直转圈，也不报错。所以要主动等一个上限。
 */
async function ensureHostReady(): Promise<void> {
  if (!isHostAvailable()) throw new TrimUnavailableError('standalone')

  let timer: number | undefined
  const timeout = new Promise<never>((_, reject) => {
    timer = window.setTimeout(
      () => reject(new TrimUnavailableError('notReady')),
      HOST_READY_TIMEOUT_MS
    )
  })
  try {
    await Promise.race([sdk.ready(), timeout])
  } finally {
    if (timer !== undefined) window.clearTimeout(timer)
  }
}

interface PickParams {
  title?: string
  okText?: string
}

/**
 * 拉起用户文件选择器，并把选中的路径授权给本应用。
 *
 * 返回的 `data` 是「当前用户授权给本应用的路径」。
 *
 * **空数组代表用户取消了**（没选、也没授权），不是错误。宿主在用户直接关掉选择器时返回的是
 * `code: 1` + `msg: "Operation failed"`——一个既没有业务码、也没有可读原因的信号，当错误抛
 * 出去只会让界面弹一句「调用飞牛接口失败（1）：Operation failed」，而用户明明只是点了取消。
 * 所以 `code: 1`、以及「成功但没返回内容」都归一成空数组，由调用方给一句「已取消」。
 *
 * 只把 `code: 1` 当取消是安全的：真正没有权限的情况走业务码（例如 `1003201`
 * 「管理员已关闭该应用的普通用户授权能力」），仍然是错误。
 */
async function pickUserPaths(params: PickParams): Promise<string[]> {
  await ensureHostReady()

  const res = await sdk.pickUserFile({
    directory: false,
    sidebarGroup: SIDEBAR_GROUPS,
    title: params.title,
    okText: params.okText
  })

  // 宿主没回内容（undefined）与 code 1 是同一件事：这次选择什么也没得到。
  if (!res) return []
  if (res.code === 1) return []
  if (res.code !== 0) throw new TrimError(res.code, res.msg || `code ${res.code}`)
  return res.data ?? []
}

/**
 * 选文件（暂不支持目录：后端 /api/fs/upload-from-nas 只按单个文件路径读）。
 *
 * 返回授权结果里的文件路径（本机绝对路径），交给 `sftp.uploadFromNas` 由服务端读出来推送
 * 到远程主机。确认按钮的文案用本应用当前语言的「确定」——宿主弹窗的按钮由我们给文案，
 * 写死中文会在英文界面里露馅。
 */
export function pickUserFiles(title: string): Promise<string[]> {
  return pickUserPaths({ title, okText: t('common_ok') })
}
