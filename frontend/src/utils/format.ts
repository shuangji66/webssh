import { t } from '@/i18n'

/** 展示用的小工具（时间 / 体积 / 路径）。 */

export function baseName(path: string): string {
  const at = path.lastIndexOf('/')
  return at < 0 ? path : path.slice(at + 1)
}

export function parentPath(path: string): string {
  const at = path.lastIndexOf('/')
  if (at <= 0) return '/'
  return path.slice(0, at)
}

export function joinPath(...parts: string[]): string {
  const joined = parts
    .filter((p) => p !== '')
    .join('/')
    .replace(/\/{2,}/g, '/')
  return joined || '/'
}

/** 路径是否是绝对路径（SFTP 的路径都以 / 开头）。 */
export function isAbsolutePath(path: string): boolean {
  return path.startsWith('/')
}

export function humanSize(bytes: number): string {
  if (!Number.isFinite(bytes) || bytes < 0) return ''
  if (bytes < 1024) return `${bytes} B`
  const units = ['KB', 'MB', 'GB', 'TB', 'PB']
  let value = bytes / 1024
  let unit = 0
  while (value >= 1024 && unit < units.length - 1) {
    value /= 1024
    unit += 1
  }
  return `${value.toFixed(1)} ${units[unit]}`
}

/** 权限位：把后端给的 `-rw-r--r--` 直接展示，缺省时用 mode 数字兜底。 */
export function permText(mode: string): string {
  return mode || ''
}

/**
 * 绝对时间（紧凑）：今天只留时分，今年内省掉年份。
 *
 * 后端给的是 RFC3339；解析不了就原样返回——宁可显示长一点，也不要显示空白。
 */
export function formatTime(stamp: string, now: Date = new Date()): string {
  if (!stamp) return ''
  const d = new Date(stamp)
  if (Number.isNaN(d.getTime())) {
    const m = /^(\d{4})-(\d{2})-(\d{2})[ T](\d{2}:\d{2})/.exec(stamp)
    if (!m) return stamp
    return `${m[2]}-${m[3]} ${m[4]}`
  }
  const pad = (n: number) => String(n).padStart(2, '0')
  const hhmm = `${pad(d.getHours())}:${pad(d.getMinutes())}`
  const sameDay =
    d.getFullYear() === now.getFullYear() &&
    d.getMonth() === now.getMonth() &&
    d.getDate() === now.getDate()
  if (sameDay) return hhmm
  if (d.getFullYear() === now.getFullYear()) return `${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${hhmm}`
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}`
}

/** 相对时间（首页会话卡片上的「打开于 …」用）。 */
export function relativeTime(stamp: string, now: Date = new Date()): string {
  if (!stamp) return ''
  const d = new Date(stamp)
  if (Number.isNaN(d.getTime())) return formatTime(stamp, now)
  const diff = Math.max(0, now.getTime() - d.getTime())
  const min = Math.floor(diff / 60_000)
  if (min < 1) return t('time_just_now')
  if (min < 60) return t('time_minutes_ago', { n: min })
  const hour = Math.floor(min / 60)
  if (hour < 24) return t('time_hours_ago', { n: hour })
  return t('time_days_ago', { n: Math.floor(hour / 24) })
}

/** 会话默认显示名：登录名@地址。 */
export function defaultSessionName(user: string, host: string): string {
  return `${user}@${host}`
}

/** 会话在界面上的名字（用户填了就用用户的）。 */
export function sessionLabel(s: { name: string; user: string; host: string }): string {
  return s.name?.trim() || defaultSessionName(s.user, s.host)
}

/** 会话连接目标（卡片上的 host:port 一行）。 */
export function sessionTarget(s: { host: string; port: number }): string {
  return s.port && s.port !== 22 ? `${s.host}:${s.port}` : s.host
}

// ---------- 权限 / 排序 / shell 引用（文件管理与「在此目录新建终端」用） ----------

/**
 * 把后端给的符号权限串换算成八进制（`-rwxr-xr-x` → `0755`）。
 *
 * 为什么在前端换算而不是让后端多回一个字段：符号串已经包含了全部信息（含 setuid/setgid/
 * sticky），换算在纯字符串上是确定性的，不必为一个展示开关加接口字段。
 * 兼容三种输入：10 字符符号串、纯数字串（`644` / `0644` / `4755`）、以及认不出来的串（返回空串，
 * 界面显示空白而不是胡说一个权限）。
 */
export function modeToOctal(mode: string): string {
  const m = (mode || '').trim()
  if (!m) return ''
  if (/^[0-7]{3,4}$/.test(m)) return m.padStart(4, '0')
  if (m.length < 10) return ''
  const digit = (r: string, w: string, x: string) => {
    let v = 0
    if (r === 'r') v += 4
    if (w === 'w') v += 2
    // 's'/'t' 出现在执行位上时表示「执行位 + 特殊位」（'S'/'T' 表示只有特殊位）
    if (x === 'x' || x === 's' || x === 't') v += 1
    return v
  }
  const owner = digit(m[1]!, m[2]!, m[3]!)
  const group = digit(m[4]!, m[5]!, m[6]!)
  const other = digit(m[7]!, m[8]!, m[9]!)
  let special = 0
  if (m[3] === 's' || m[3] === 'S') special += 4
  if (m[6] === 's' || m[6] === 'S') special += 2
  if (m[9] === 't' || m[9] === 'T') special += 1
  return `${special}${owner}${group}${other}`
}

export type SortKey = 'name' | 'size' | 'mtime'
export type SortDir = 'asc' | 'desc'

/** 目录永远排在文件前面；同类型再按 key 比较。`key` 为空表示不排序（由调用方保持原顺序）。 */
export function compareEntries(
  a: { name: string; dir: boolean; size: number; mtime: string },
  b: { name: string; dir: boolean; size: number; mtime: string },
  key: SortKey,
  dir: SortDir
): number {
  if (a.dir !== b.dir) return a.dir ? -1 : 1
  let cmp: number
  if (key === 'size' && !a.dir && !b.dir) {
    cmp = a.size - b.size
  } else if (key === 'mtime') {
    cmp = a.mtime.localeCompare(b.mtime)
  } else {
    // 自然序：file2 排在 file10 前面（数字按数值比较）
    cmp = a.name.localeCompare(b.name, undefined, { numeric: true, sensitivity: 'base' })
  }
  return dir === 'desc' ? -cmp : cmp
}

/** POSIX shell 单引号安全引用（远程命令里拼接路径必须走它）。 */
export function shellQuote(s: string): string {
  return `'${s.replace(/'/g, "'\\''")}'`
}
