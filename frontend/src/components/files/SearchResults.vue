<script setup lang="ts">
// SearchResults — 搜索结果区（第二十二轮，契约 32.3）。
//
// 它不是独立页面，而是**列表区的一种形态**（搜索栏还开着时，列表区显示结果还是目录内容由
// `search.viewing` 决定），所以这里只渲染结果区自己的那几行，滚动容器归 FileManager。
//
// 四条刻意的规矩：
//  1. **跟随「视图」偏好**（列表 / 网格，与目录内容同一套渲染形态）：每行的第一列是**名字 +
//     灰色的所在目录**（递归搜索的结果散在不同层的目录里，只给一个文件名等于没有上下文）。
//     名字在前，截断时先截目录，所以第一列仍然是「名称」——排序与表头标签都不用为结果单开一套。
//     **表头不在这里**：列表的表头（可点排序的三列 + 权限切换）在 FileManager 里，就在滚动容器
//     外面、目录与结果共用一份——用户要求「列表视图时标题栏不参与滚动，复用排序和权限显示切换」。
//  2. **行内没有「···」菜单**：行菜单的动作（删除 / 重命名 / 压缩 / 导出…）都建立在「这一项在
//     当前目录的列表里」这个前提上（见 FileManager 的 removePaths / menuItems 都拿
//     `state.entries` 查表），而结果里的项可能在别的目录下。用户要的交互只有两条：文件 → 预览，
//     目录 → 进入该目录。别的动作请先进入那个目录（点那一行就是进入）。
//  3. **排序复用目录那一份偏好**（`sftp.sortedResults`）：点表头排的就是结果，没点之前保持
//     **到达顺序**（后端 BFS 先近后远，边搜边追加——「搜到一点显示一点」要的就是这个顺序）。
//  4. 所在目录由 `scopeDir` 前缀裁出来（后端给的 path 是绝对路径）；**不 trim**——名字以空格
//     结尾是合法的（与 resolveRemotePath 同一条纪律）。
import { computed } from 'vue'
import { t } from '@/i18n'
import { useSftpStore } from '@/stores/sftp'
import { formatTime, humanSize, modeToOctal, parentPath, permText } from '@/utils/format'
import { type FsEntry } from '@/serverapi'

const { sid, mobile = false } = defineProps<{ sid: string; mobile?: boolean }>()
const emit = defineEmits<{ open: [entry: FsEntry] }>()

const sftp = useSftpStore()
const state = computed(() => sftp.stateOf(sid))
const search = computed(() => state.value.search)
/** 结果条目：排序（若启用）在 store 里算，与目录列表共用同一份排序偏好。 */
const entries = computed(() => sftp.sortedResults(sid))
/** 视图形态与目录内容一致（列表 / 网格）。 */
const grid = computed(() => state.value.view === 'grid')

/**
 * 命中项**所在目录**的相对形式（相对搜索起点，末尾带 `/`；命中项就在起点下时是空串）。
 *
 * 递归搜索必须给出这个上下文（只给一个文件名，用户不知道它藏在哪一层）。渲染成
 * 「名字 + 灰色目录后缀」而不是整条相对路径：名字在前，长了先截目录——用户搜的那个名字
 * 永远看得见；而且第一列仍然是「名称」，表头那一列的**排序与标签都不用改**（复用目录那份）。
 *
 * 起点对不上（后端回的 path 不在搜索结果起点下，理论上不会发生）时退回绝对父目录：
 * 显示成什么样都比显示一个被截错位置、看起来像在别处的路径好。
 */
function relDir(entry: FsEntry): string {
  const root = search.value.scopeDir
  const parent = parentPath(entry.path)
  if (!root) return parent === '/' ? '' : `${parent}/`
  if (parent === root) return ''
  if (root === '/' || parent.startsWith(`${root}/`)) {
    return `${parent.slice(root === '/' ? 1 : root.length + 1)}/`
  }
  return `${parent}/`
}

/**
 * 结果来自**别的目录**时的说明（为空 = 不显示）。
 *
 * 什么时候算「别的目录」：当前目录已经不在搜索起点那棵子树里（用户从结果跳进某个子目录、
 * 或者干脆用了后退/面包屑去了别处）。这时面包屑上是 A、结果却来自 B，不说一句就会以为是 bug。
 * 在同一棵子树里不显示——那时结果仍然和眼前这个目录相关，多一行字反而是噪音。
 */
const scopeNote = computed(() => {
  const root = search.value.scopeDir
  const dir = state.value.dir
  if (!root || !dir) return ''
  const prefix = root === '/' ? '/' : `${root}/`
  if (dir === root || dir.startsWith(prefix)) return ''
  return t('files_search_scope', { dir: root })
})

/** 收尾统计：扫了多少、用时多久；被截断 / 有目录被跳过时一并说明（用户要能判断结果全不全）。 */
const summary = computed(() => {
  const s = search.value
  if (s.running || !s.done || !s.results.length) return ''
  let out = t('files_search_summary', {
    scanned: s.scanned,
    dirs: s.dirs,
    seconds: (s.elapsedMs / 1000).toFixed(1)
  })
  if (s.skipped) out += t('files_search_summary_skipped', { n: s.skipped })
  if (s.truncated) out += t('files_search_summary_truncated')
  return out
})

/** 权限列：与目录列表同一个口径（符号串 / 八进制由表头的「权限」列切换）。 */
function modeText(entry: FsEntry): string {
  return state.value.mode === 'octal' ? modeToOctal(entry.mode) : permText(entry.mode)
}
</script>

<template>
  <div v-if="scopeNote" class="px-2 py-1 text-[11px] text-ink-faint dark:text-ink-faint-dark">
    {{ scopeNote }}
  </div>

  <!-- 搜索失败：已经搜到的部分照常列出来（下面），这里只说失败原因 -->
  <div v-if="search.error" class="px-2 py-2 text-xs break-all text-danger">
    {{ t('files_search_failed', { error: search.error }) }}
  </div>

  <!-- 空态：搜索中说「搜索中」（结果还在路上，别让用户以为搜完了），搜完说「没找到」。 -->
  <div
    v-if="!entries.length && !search.error"
    class="px-3 py-6 text-center text-sm text-ink-faint"
  >
    {{ search.running ? t('files_search_scanning', { scanned: search.scanned }) : t('files_search_empty') }}
  </div>

  <!-- 列表：形态与 FileManager 的目录列表一致（图标 / 相对路径 / 大小 / 修改时间 / 权限），
       末尾那个 w-6 空档是**对齐表头**用的（表头最后一列是行内「···」的位置，结果行没有菜单，
       但列宽必须一样，否则 flex-1 那列一宽一窄、后面几列整体错位）。 -->
  <template v-if="!grid">
    <div
      v-for="entry in entries"
      :key="entry.path"
      class="flex cursor-pointer items-center gap-2 border-b border-line/60 px-2 py-1.5 text-sm hover:bg-black/5 dark:border-line-dark/60 dark:hover:bg-white/5"
      @click="emit('open', entry)"
    >
      <svg
        v-if="entry.dir"
        class="h-4 w-4 shrink-0 text-warning"
        viewBox="0 0 24 24"
        fill="none"
        stroke="currentColor"
        stroke-width="1.5"
        stroke-linecap="round"
        stroke-linejoin="round"
        :title="t('files_dir')"
      >
        <path d="M4 20a2 2 0 0 1-2-2V6a2 2 0 0 1 2-2h4l2 2h6a2 2 0 0 1 2 2v12a2 2 0 0 1-2 2z" />
      </svg>
      <svg
        v-else
        class="h-4 w-4 shrink-0 text-ink-faint"
        viewBox="0 0 24 24"
        fill="none"
        stroke="currentColor"
        stroke-width="1.5"
        stroke-linecap="round"
        stroke-linejoin="round"
        :title="t('files_file')"
      >
        <path d="M14 3H7a2 2 0 0 0-2 2v14a2 2 0 0 0 2 2h10a2 2 0 0 0 2-2V8z" />
        <path d="M14 3v5h5" />
      </svg>
      <!-- 名字 + 灰色所在目录（悬停给完整绝对路径）：名字在前，截断时先截目录 -->
      <span class="min-w-0 flex-1 truncate" :title="entry.path">
        {{ entry.name }}<!-- 左边距用 pl-1 而不是空格：模板里插值节点旁边的空格会被编译器裁掉，
             结果就是「APP.lognotes/」这种粘在一起的显示（浏览器实测抓到过）。 -->
        <span v-if="relDir(entry)" class="pl-1 text-ink-faint">{{ relDir(entry) }}</span>
      </span>
      <span class="w-16 shrink-0 text-right text-xs text-ink-faint">
        {{ entry.dir ? '' : humanSize(entry.size) }}
      </span>
      <span class="w-24 shrink-0 text-right text-xs text-ink-faint">
        {{ formatTime(entry.mtime) }}
      </span>
      <span
        v-if="!mobile"
        class="w-20 shrink-0 truncate text-right font-mono text-[11px] text-ink-faint"
      >
        {{ modeText(entry) }}
      </span>
      <span class="w-6 shrink-0" />
    </div>
  </template>

  <!-- 网格：与目录内容的网格同一套形态（格子里的文字换成相对路径 + 大小/目录）。
       content-start 不能删：网格默认会拉伸自动行，只有一行条目时那一行会被拉成整屏高，
       悬停高亮就变成贯穿上下的一整列（见 FileManager 与契约 14.5）。 -->
  <div
    v-else
    class="grid min-h-full content-start gap-1 p-2"
    style="grid-template-columns: repeat(auto-fill, minmax(6.5rem, 1fr))"
  >
    <div
      v-for="entry in entries"
      :key="entry.path"
      class="group relative flex cursor-pointer flex-col items-center gap-1 rounded-md border border-transparent px-2 py-2 text-center hover:border-line hover:bg-black/5 dark:hover:border-line-dark dark:hover:bg-white/5"
      @click="emit('open', entry)"
    >
      <svg
        v-if="entry.dir"
        class="h-6 w-6 text-warning"
        viewBox="0 0 24 24"
        fill="none"
        stroke="currentColor"
        stroke-width="1.5"
        stroke-linecap="round"
        stroke-linejoin="round"
      >
        <path d="M4 20a2 2 0 0 1-2-2V6a2 2 0 0 1 2-2h4l2 2h6a2 2 0 0 1 2 2v12a2 2 0 0 1-2 2z" />
      </svg>
      <svg
        v-else
        class="h-6 w-6 text-ink-faint"
        viewBox="0 0 24 24"
        fill="none"
        stroke="currentColor"
        stroke-width="1.5"
        stroke-linecap="round"
        stroke-linejoin="round"
      >
        <path d="M14 3H7a2 2 0 0 0-2 2v14a2 2 0 0 0 2 2h10a2 2 0 0 0 2-2V8z" />
        <path d="M14 3v5h5" />
      </svg>
      <!-- 名字最多两行（悬停给完整绝对路径），下面一行是灰色的所在目录 -->
      <span class="line-clamp-2 w-full break-all text-xs leading-tight" :title="entry.path">
        {{ entry.name }}
      </span>
      <span v-if="relDir(entry)" class="w-full truncate text-[11px] text-ink-faint">
        {{ relDir(entry) }}
      </span>
      <span class="text-[11px] text-ink-faint">
        {{ entry.dir ? t('files_dir') : humanSize(entry.size) }}
      </span>
    </div>
  </div>

  <div v-if="summary" class="px-2 py-1 text-[11px] text-ink-faint dark:text-ink-faint-dark">
    {{ summary }}
  </div>
</template>
