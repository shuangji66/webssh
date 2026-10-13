// src/stores/appTabs.ts — 应用级标签（首页固定 + 每个「已打开」的会话一个）。
//
// 为什么会话标签的 id 直接用 sid：应用标签就是「会话被打开」这件事在界面上的投影，
// 而 sid 是这件事的唯一标识。再包一层 `session:<sid>` 只会让每个调用点（打开 / 关闭 /
// 改标题 / 判断是否已存在）都做一次前缀拼接与还原；sid 由后端随机生成，不可能等于 'home'。
//
// 这里**只**管标签本身：关闭会话标签时「终止后端会话 + 清终端/SFTP/界面状态」属于
// 业务动作，由入口（AppTabBar）按契约 6.3 的顺序完成，store 不替调用方做决定。
//
// **标签标题不是本地偏好，而是会话配置名字的投影**（契约 13.3）：双击改标签会写回
// `/api/sessions/save`，编辑表单改名也会同步过来（`syncTitle`）。这里曾经存过一份
// localStorage 的「本机改名表」，用来让标签名不被配置名覆盖——现在两个方向都要同步，那份
// 覆盖表只会让「表单改了名、标签不动」，所以删掉了：配置是唯一真相。
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

export const useAppTabsStore = defineStore('appTabs', () => {
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

  /** 打开（或切回）一个会话标签。已存在时只激活，并按配置名更新标题。 */
  function openSession(sid: string, title?: string) {
    const wanted = title?.trim() || ''
    const existing = tabs.value.find((tab) => tab.id === sid)
    if (existing) {
      if (wanted) existing.title = wanted
      activeId.value = sid
      return
    }
    tabs.value.push({
      id: sid,
      kind: 'session',
      closable: true,
      title: wanted || sid
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

  /**
   * 改标签名（空字符串 = 放弃改名，保持原样）。**只改展示**：写回配置由调用方（AppTabBar）
   * 走 `home.renameSession`，这样「配置 ↔ 标签」两个方向用的是同一份数据。
   */
  function setTitle(sid: string, title: string) {
    const value = title.trim()
    if (!value) return
    const tab = tabs.value.find((item) => item.id === sid)
    if (tab) tab.title = value
  }

  /**
   * 会话配置改名后同步标签标题（**不**改变激活项）。
   *
   * 与 openSession 分开：改完配置还在首页 / 别的会话里，不该被拽到那个会话的标签上。
   * 无条件覆盖：配置是标签名的唯一真相（契约 13.3），编辑表单改名后标签必须跟着变。
   */
  function syncTitle(sid: string, title: string) {
    const tab = tabs.value.find((item) => item.id === sid)
    const value = title.trim()
    if (tab && value) tab.title = value
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
