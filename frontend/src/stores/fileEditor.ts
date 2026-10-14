// src/stores/fileEditor.ts — 每个会话一份的「文件编辑」状态（编辑页与三个入口共用）。
//
// 为什么放在 store 里而不是编辑页组件里：入口有三个（预览弹窗的编辑按钮、文件管理的行菜单、
// 左栏目录树的文件菜单），而编辑页渲染在**文件管理器**里（覆盖它）。三者分属不同组件，
// 状态留在任何一处都会变成「点得动但页面不出现」或者「两个入口各开一份」。
//
// 三条与契约 26.3 对应的硬约束：
//  1. **没权限不进编辑页**：`open()` 先调 `/api/fs/edit-load`（后端以登录用户身份验证可写），
//     失败就**原样抛出**、状态与标签页都不动，由调用方 toast 原因；
//  2. **编辑页一定在文件管理标签页下**：加载**成功之后**才把激活子标签切到 FILES_TAB
//     （先切标签页再加载的话，一次「没有权限」会把用户从终端视图拽到文件管理里）；
//  3. **不动文件管理器的状态**：进入与退出都不导航、不换视图——「退出编辑后回到进入时的状态」
//     靠的就是「从头到尾没碰它」，而不是退出时再恢复一次（后者在「从面包屑进来」那条路上会
//     把用户带到一个他没去过的目录，用户明确要求那种情况回到文件管理的初始状态）。
import { ref } from 'vue'
import { defineStore } from 'pinia'
import { api } from '@/serverapi'
import { parentPath } from '@/utils/format'
import { FILES_TAB, useSessionUiStore } from '@/stores/sessionUi'
import { useSftpStore } from '@/stores/sftp'

export interface FileEditorState {
  /** 编辑页是否打开（覆盖在文件管理面板上） */
  open: boolean
  /** 正在加载（打开 / 还原）：两个动作都在读远程内容 */
  loading: boolean
  saving: boolean
  /** 远程绝对路径（后端回填的那一个） */
  path: string
  /** 编辑区里的内容 */
  content: string
  /** 打开时（或上次保存后）的内容：脏判据与「还原」的基准 */
  original: string
  /** 'utf-8' | 'utf-16le' | 'utf-16be'（保存时原样带回） */
  encoding: string
  /** 原文件是否以 BOM 开头（保存时原样带回） */
  bom: boolean
  /** 'lf' | 'crlf'（保存时原样带回） */
  eol: string
}

function blankState(): FileEditorState {
  return {
    open: false,
    loading: false,
    saving: false,
    path: '',
    content: '',
    original: '',
    encoding: '',
    bom: false,
    eol: 'lf'
  }
}

/**
 * 把远程文本规整成**编辑区**用的那一份：CRLF 与孤立的 CR 一律变成 LF。
 *
 * 为什么必须归一化（这是编辑页最容易踩空的一处）：`<textarea>` 的 `value` 按 HTML 规范会
 * 把 CRLF / 单个 CR 换成 LF，而 store 里的字符串如果把 `\r` 原样留着，两边就成了**两个
 * 坐标系**——命中表（`scan()`）算出的下标、`setSelectionRange` 的选区、以及
 * `selectionStart` 与命中表之间的比较，全都差「前面出现过多少个 CRLF」（每行累积 1）。
 * 用户看到的症状：CRLF 文件里「查找」的高亮每行**左偏一个字符**（第 100 行偏 100 个），
 * 「从光标处往后找」会跳过一个本应命中的位置；最严重的是在错位的选区上直接打字会改到
 * **另一处字符**，保存后文件内容损坏。
 *
 * 后端只在保存时按 `eol` 把 LF 换回 CRLF（见 `encodeForSave`），所以「编辑区里一律是 LF」
 * 这条口径（契约 26.2 / 26.3）必须由这里真正做出来——只写在注释里就会出上面那些症状。
 */
function toEditorText(text: string): string {
  return text.replace(/\r\n?/g, '\n')
}

export const useFileEditorStore = defineStore('fileEditor', () => {
  const states = ref<Record<string, FileEditorState>>({})

  /**
   * 取某个会话的编辑状态（没有就建一个）。
   *
   * ⚠️ 与 sftp store 同一条坑：新键必须**赋值之后再从 ref 上读一次**再返回。直接返回
   * `blankState()` 造出来的**原始对象**，第一个调用方（谁的模板先跑是不确定的）拿到的就是
   * 一层没有响应式的壳——读到的值是对的，界面永远不刷新。
   */
  function stateOf(sid: string): FileEditorState {
    if (!states.value[sid]) states.value[sid] = blankState()
    return states.value[sid]!
  }

  /** 编辑页是不是开着（FileManager 据此渲染覆盖层）。 */
  function isOpen(sid: string): boolean {
    return states.value[sid]?.open === true
  }

  /** 有未保存的修改（退出 / 还原前要确认，按钮的可用态也看它）。 */
  function isDirty(sid: string): boolean {
    const st = states.value[sid]
    return !!st && st.open && st.content !== st.original
  }

  /**
   * 打开编辑页。**加载成功之后**才切到文件管理标签页；失败原样抛出（状态不变、标签页不动）。
   *
   * 重复点击（加载中）直接忽略：同一个文件连点两下不该发两个请求，更不该让后一个请求的
   * 响应覆盖前一个（两个响应可能来自不同的文件版本）。
   */
  async function open(sid: string, path: string): Promise<void> {
    const st = stateOf(sid)
    if (st.loading) return
    st.loading = true
    try {
      const res = await api.fsEditLoad(sid, path)
      // 归一成 LF 再进 store：与 textarea 的 value 同一坐标系（见 toEditorText）
      const text = toEditorText(res.content)
      st.open = true
      st.path = res.path
      st.content = text
      st.original = text
      st.encoding = res.encoding
      st.bom = res.bom
      st.eol = res.eol
      // 编辑页在文件管理标签页下（用户要求）：切换放在成功之后，见文件头第 2 条。
      useSessionUiStore().setActiveSub(sid, FILES_TAB)
    } finally {
      st.loading = false
    }
  }

  /**
   * 还原：重新读一次远程内容（丢弃本地修改）。
   *
   * 刻意**重新请求**而不是把 `original` 塞回编辑区：还原的语义是「回到磁盘上的当前内容」，
   * 编辑期间别人改过这个文件的话，只把内存里的旧快照放回去再保存会把别人的改动覆盖掉。
   */
  async function revert(sid: string): Promise<void> {
    const st = stateOf(sid)
    // saving 期间也不许还原：两个请求同时在飞时，回来的先后会决定 content/original 的最终值
    // （可能变成「屏幕显示干净、磁盘是旧正文」，见 save 的说明）。
    if (!st.open || st.loading || st.saving) return
    st.loading = true
    try {
      const res = await api.fsEditLoad(sid, st.path)
      // 与 open() 同一个口径：归一成 LF，否则「还原」之后查找/选区又开始错位
      const text = toEditorText(res.content)
      st.content = text
      st.original = text
      st.encoding = res.encoding
      st.bom = res.bom
      st.eol = res.eol
    } finally {
      st.loading = false
    }
  }

  /**
   * 保存。失败**原样抛出**且保留编辑区内容（调用方 toast 原因）——用户敲进去的东西不能因为
   * 一次失败就没了。
   *
   * 成功后刷新**文件所在的那一层**（`refreshTree` 会同时更新文件管理器的列表与树里展开过的
   * 节点，见契约 26.3）：文件大小 / 修改时间要立刻变，否则用户会以为没保存上。
   *
   * ⚠️ 把要写的内容**先固化成 payload**：保存期间 textarea 并没有被禁用（只有按钮禁用），
   * 用户完全可能接着敲字；如果 `await` 之后再读 `st.content` 赋给 `original`，那个
   * `original` 指的是一份**没写进磁盘**的内容——界面显示「已保存」、脏标记消失，而磁盘上是
   * 旧的一版，用户接着点「退出」连确认都不会弹，后来的输入就无声丢了。
   * 固化之后若用户又改了内容，`content !== original` 自然为真，脏标记保持。
   */
  async function save(sid: string): Promise<void> {
    const st = stateOf(sid)
    // loading 期间不许保存：还原在飞时点保存，写的是一份马上要被覆盖的旧正文
    if (!st.open || st.saving || st.loading || st.content === st.original) return
    const payload = st.content
    st.saving = true
    try {
      // 回执里的 size 不再用了（第十七轮起编辑页不显示体积）：要的是「写成功」这件事本身。
      await api.fsEditSave(sid, st.path, payload, {
        encoding: st.encoding,
        bom: st.bom,
        eol: st.eol
      })
      st.original = payload
      try {
        await useSftpStore().refreshTree(sid, parentPath(st.path))
      } catch {
        // 刷新失败不该把「已保存」说成失败：文件已经写进去了，列表下一次进目录时会拉新的
      }
    } finally {
      st.saving = false
    }
  }

  /** 关掉编辑页（丢弃未保存的修改由调用方先确认）。 */
  function close(sid: string) {
    states.value[sid] = blankState()
  }

  /** 会话关闭时丢掉它的编辑状态（否则记录只增不减）。 */
  function forget(sid: string) {
    delete states.value[sid]
  }

  return { stateOf, isOpen, isDirty, open, revert, save, close, forget }
})
