// src/stores/sessionStats.ts — 首页卡片上那两行「系统信息」的取数与轮询。
//
// 后端接口 `/api/sessions/stats` 是**每次现问远端**的（一条只读命令），所以这里必须自己管
// 生命周期，不能挂上去就一直问：
//   - 只有「已打开」的会话才问（关掉会话后端会回 not_open，而且连接已经没了）；
//   - 只有首页在前台时才问（用户在看终端时，卡片根本不可见，每 5 秒一条远程命令纯属浪费）；
//   - 不再「已打开」的会话立刻把数字清掉（留着会在下次打开时先闪一眼旧数据）。
//
// 谁负责告诉它这两件事：`HomeView`（它才知道当前打开了哪些会话、首页是不是前台），
// 见那里的 `sync()` 调用点。组件不自己起定时器——卡片会被反复挂载/卸载，
// 定时器活在组件里就会出现「卸载了还在问」或者「重挂载了多问一份」。
import { defineStore } from 'pinia'
import { ref } from 'vue'
import { api, type SessionStats } from '@/serverapi'

/** 刷新间隔：用户要求 5 秒一次。 */
export const STATS_INTERVAL_MS = 5000

// ---------------------------------------------------------------------------
// 数值的绿 / 黄 / 红（阈值只写在这里，改颜色规则改这一处）
// ---------------------------------------------------------------------------

/** 三档：正常 / 偏高 / 过高。 */
export type StatLevel = 'ok' | 'warn' | 'danger'

/**
 * 阈值（见 CONTRACT 23.6）。
 *
 * 取值理由：内存比 CPU 更早影响使用（开始换页就卡了，所以 70% 就转黄）；磁盘在 NAS 上后果最重
 * （写满会直接写失败，所以 75% 就转黄）；负载是「排队长度」，必须**除以核数**才有意义，
 * 1.0 = 每个核上都有一个任务在等，也就是刚好跑满。
 */
export const STATS_THRESHOLDS = {
  cpu: { warn: 60, danger: 85 },
  load: { warn: 0.7, danger: 1 },
  mem: { warn: 70, danger: 90 },
  disk: { warn: 75, danger: 90 }
} as const

function levelOf(value: number, warn: number, danger: number): StatLevel {
  if (!Number.isFinite(value)) return 'ok'
  if (value >= danger) return 'danger'
  if (value >= warn) return 'warn'
  return 'ok'
}

/** 颜色类名只在这里定义一份（与 `terminals.termStatusDotClass` 同一套做法）。 */
export function statLevelClass(level: StatLevel): string {
  if (level === 'danger') return 'text-danger'
  if (level === 'warn') return 'text-warning'
  return 'text-success'
}

/** CPU 占用率（0–100）。 */
export function cpuLevel(percent: number): StatLevel {
  return levelOf(percent, STATS_THRESHOLDS.cpu.warn, STATS_THRESHOLDS.cpu.danger)
}

/** 平均负载：按核数折算成「每个核的排队长度」（核数缺失时按 1 核算，宁可偏严也不偏松）。 */
export function loadLevel(load1: number, cores: number | undefined): StatLevel {
  const n = cores && cores > 0 ? cores : 1
  return levelOf(load1 / n, STATS_THRESHOLDS.load.warn, STATS_THRESHOLDS.load.danger)
}

/** 内存占用率（已用 / 总量，0–100）。 */
export function memLevel(used: number, total: number): StatLevel {
  if (!total || total <= 0) return 'ok'
  return levelOf((used / total) * 100, STATS_THRESHOLDS.mem.warn, STATS_THRESHOLDS.mem.danger)
}

/** 磁盘占用率（0–100）。 */
export function diskLevel(percent: number): StatLevel {
  return levelOf(percent, STATS_THRESHOLDS.disk.warn, STATS_THRESHOLDS.disk.danger)
}

export const useSessionStatsStore = defineStore('sessionStats', () => {
  /** sid → 最近一次读到的系统信息（没有条目 = 不显示）。 */
  const byId = ref<Record<string, SessionStats>>({})

  /** 定时器句柄（null = 没在轮询）。 */
  let timer: number | null = null
  /** 当前「该问」的会话 id 列表。 */
  let wanted: string[] = []
  /** 每个会话各自记录「上一请求还没回来」：远端很慢时不要把它堆成一串并发请求。 */
  const inFlight = new Set<string>()

  /** 某个会话的系统信息（undefined = 这一块不显示）。 */
  function statsOf(sid: string): SessionStats | undefined {
    return byId.value[sid]
  }

  async function pollOne(sid: string): Promise<void> {
    if (inFlight.has(sid)) return
    inFlight.add(sid)
    try {
      const out = await api.sessionStats(sid)
      // ⚠️ 写回前先确认「这个会话还在被问」：请求从发出到回来之间，用户可能已经把会话关了
      // （sync 会把它的条目删掉，定时器也会停）。那时再写回去，卡片上就会挂着一份**不再更新**
      // 的旧数字（而卡片已经显示「未打开」）——因为它已经不在 wanted 里，没人会再清它。
      if (!wanted.includes(sid)) return
      if (out.stats?.available) byId.value[sid] = out.stats
      // 远端读不出来（不是 Linux / 连接断了 / 会话刚被关掉）：整块不显示，**不保留上一次的数字**
      // ——卡片上挂一份不知道多久以前的占用率比不显示更容易误导人。
      else delete byId.value[sid]
    } catch {
      // 同上：接口失败（409 会话已关、网络抖动、超时）一律当作「没有数据」。
      // 不弹 toast：这是每 5 秒一次的后台轮询，为它弹提示会淹没真正的错误。
      // （会话已经被关掉的情况条目早就被 sync 删了，这一句只是把它按住不让它回来。）
      delete byId.value[sid]
    } finally {
      inFlight.delete(sid)
    }
  }

  function pollAll(): void {
    for (const sid of wanted) void pollOne(sid)
  }

  function stop(): void {
    if (timer !== null) {
      window.clearInterval(timer)
      timer = null
    }
  }

  /**
   * 停表并把「该问谁」清空（组件卸载时调）。
   *
   * `stop()` 单独导出是为了不再把定时器的生死托付给「HomeView 永不卸载」：
   * 现在它确实是常驻挂载（`v-show`），但将来谁把它改成 `v-if`（或者再加一个首页入口），
   * 定时器就会在后台一直每 5 秒问一次远端——而那时没人会注意到。
   */
  function dispose(): void {
    stop()
    wanted = []
    byId.value = {}
  }

  /**
   * 同步「该问哪些会话 + 首页在不在前台」。HomeView 在打开集合或激活标签变化时调用它。
   *
   * 立刻拉一次（而不是等第一个 5 秒 tick）：刚打开的会话应当马上看到数字；从别的标签切回
   * 首页时也顺手刷新一次，免得先显示上一次离开时的旧值。
   */
  function sync(sids: string[], visible: boolean): void {
    const changed = sids.length !== wanted.length || sids.some((sid, i) => sid !== wanted[i])
    wanted = [...sids]
    for (const sid of Object.keys(byId.value)) {
      if (!wanted.includes(sid)) delete byId.value[sid]
    }
    if (!visible || !wanted.length) {
      stop()
      return
    }
    if (timer === null) {
      pollAll()
      timer = window.setInterval(pollAll, STATS_INTERVAL_MS)
      return
    }
    if (changed) pollAll()
  }

  return { statsOf, sync, stop, dispose }
})