// src/stores/appTabs.ts — 应用级标签（首页固定 + 每个「已打开」的会话一个）。
//
// 为什么会话标签的 id 直接用 sid：应用标签就是「会话被打开」这件事在界面上的投影，
// 而 sid 是这件事的唯一标识。再包一层 `session:<sid>` 只会让每个调用点（打开 / 关闭 /
// 改标题 / 判断是否已存在）都做一次前缀拼接与还原；sid 由后端随机生成，不可能等于 'home'。
//
// 这里**只**管标签本身：关闭会话标签时「终止后端会话 + 清终端/SFTP/界面状态」属于
// 业务动作，由入口（AppTabBar）按契约 6.3 的顺序完成，store 不替调用方做决定。
import { defineStore } from 'pinia'
import { computed, ref } from 'vue'
import { t } from '@/i18n'

/** 首页标签的固定 id（契约 6.1）。 */
export const HOME_TAB_ID = 'home'

export interface AppTab {
  id: string
  kind: 'home' | 'session'
  title: string
  /** 首页 false（不可关闭），会话 true。 */
  closable: boolean
}

// 用户双击改过的标签名记在浏览器本地：标签名是「我在这台设备上给它起的简称」，
// 不应该写回会话配置（那是会话的真实名字）。按 sid 存，删掉会话标签也不清，
// 重开同一个会话还能看到自己起过的名字。
const TITLES_KEY = 'webssh-tab-titles'

function readTitles(): Record<string, string> {
  try {
    const raw = localStorage.getItem(TITLES_KEY)
    const parsed: unknown = raw ? JSON.parse(raw) : null
    if (parsed && typeof parsed === 'object' && !Array.isArray(parsed)) {
      return parsed as Record<string, string>
    }
  } catch {
    // 解析失败按「没有改名记录」处理：这只是展示偏好，不能因为它让整个应用起不来
  }
  return {}
}

export const useAppTabsStore = defineStore('appTabs', () => {
  const renamed = ref<Record<string, string>>(readTitles())

  // 首页标题这里给的是初始值；语言切换后由 AppTabBar 用 t('app_tab_home') 覆盖渲染，
  // 否则切到英文后标签上的「首页」会留在中文。
  const tabs = ref<AppTab[]>([
    { id: HOME_TAB_ID, kind: 'home', title: t('app_tab_home'), closable: false }
  ])
  const activeId = ref<string>(HOME_TAB_ID)

  const sessionTabs = computed(() => tabs.value.filter((tab) => tab.kind === 'session'))

  function openHome() {
    activeId.value = HOME_TAB_ID
  }

  /** 打开（或切回）一个会话标签。已存在时只激活，并按需更新标题。 */
  function openSession(sid: string, title?: string) {
    const existing = tabs.value.find((tab) => tab.id === sid)
    const wanted = title?.trim() || ''
    const stored = renamed.value[sid] || ''
    if (existing) {
      // 用户手动改过名就不再被配置名覆盖：否则「首页点打开 → 标签名又变回去」，
      // 用户会以为重命名没保存。
      if (!stored && wanted) existing.title = wanted
      activeId.value = sid
      return
    }
    tabs.value.push({
      id: sid,
      kind: 'session',
      closable: true,
      title: stored || wanted || sid
    })
    activeId.value = sid
  }

  function activate(id: string) {
    if (tabs.value.some((tab) => tab.id === id)) activeId.value = id
  }

  /**
   * 关闭一个标签（首页不可关闭）。**不**动后端会话，调用方负责先
   * `home.closeSession` 再 `home.forgetSession`。
   */
  function close(id: string) {
    const index = tabs.value.findIndex((tab) => tab.id === id)
    if (index < 0 || !tabs.value[index].closable) return
    const wasActive = activeId.value === id
    tabs.value.splice(index, 1)
    if (wasActive) {
      // 关掉当前标签后落到相邻的一个（先看右边，没有就看左边，最后兜底首页）
      const next = tabs.value[index] ?? tabs.value[index - 1] ?? tabs.value[0]
      if (next) activeId.value = next.id
    }
  }

  /** 关闭除 `id` 以外的所有会话标签，返回被关掉的 sid 列表（调用方据此清会话状态）。 */
  function closeOthers(id: string): string[] {
    const removed = tabs.value
      .filter((tab) => tab.kind === 'session' && tab.id !== id)
      .map((tab) => tab.id)
    tabs.value = tabs.value.filter((tab) => tab.kind === 'home' || tab.id === id)
    activate(id)
    return removed
  }

  /** 改标签名（空字符串 = 放弃改名，保持原样）。 */
  function setTitle(sid: string, title: string) {
    const value = title.trim()
    if (!value) return
    renamed.value[sid] = value
    try {
      localStorage.setItem(TITLES_KEY, JSON.stringify(renamed.value))
    } catch {
      // 存储满/隐私模式下写不进去：名字这一次仍然生效，只是刷新后丢，不必打扰用户
    }
    const tab = tabs.value.find((item) => item.id === sid)
    if (tab) tab.title = value
  }

  /**
   * 会话配置改名后同步标签标题（**不**改变激活项，也不覆盖用户手动改过的名字）。
   *
   * 与 openSession 分开：改完配置还在首页 / 别的会话里，不该被拽到那个会话的标签上。
   */
  function syncTitle(sid: string, title: string) {
    const tab = tabs.value.find((item) => item.id === sid)
    if (!tab || renamed.value[sid]) return
    const value = title.trim()
    if (value) tab.title = value
  }

  return {
    tabs,
    activeId,
    sessionTabs,
    openHome,
    openSession,
    activate,
    close,
    closeOthers,
    setTitle,
    syncTitle
  }
})
