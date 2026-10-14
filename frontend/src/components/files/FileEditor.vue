<script setup lang="ts">
// FileEditor — 编辑页（契约 26.3 / 27）：覆盖在**文件管理面板**上的一整页，不弹窗。
//
// 形态按用户要求：左上角「退出」、右上角「查找 / 还原 / 保存」，中间一行文件名 + 编码/换行元信息，
// 正文是铺满剩余高度的编辑区：等宽、**自动换行（没有横向滚动）**、左侧带行号。
//
// 状态全部在 stores/fileEditor.ts 里（三个入口共用同一份），这个组件只管画与转发动作：
//   - 退出：有未保存的修改时先确认，然后 close()——**不动文件管理器的任何状态**，
//     所以退出后看到的就是进入编辑时的那个目录与视图（从目录树进来的情况同理，见 store 注释）；
//   - 还原：重新从远程读一次（有未保存的修改时先确认）；
//   - 保存：写回远程，失败保留编辑区内容并说明原因。
//
// 编辑区的内容一律是 LF：textarea 的 value 按 HTML 规范把 CRLF 规整成 LF，而写回时按原文件的
// 换行由**后端**换回去（eol 随保存请求带上，见 serverapi 的 fsEditSave）。
//
// 「自动换行 + 左侧行号」是这一页最麻烦的一件事：一行被折成多行之后，行号必须仍然贴在**逻辑行**
// 的首行上，而 textarea 内部的行盒（哪一行折了几行）在外面量不到。做法是让一个**镜像层**承担
// 布局——见 editorStyle 与模板里镜像层的注释。
import { computed, nextTick, reactive, ref, watch, type CSSProperties } from 'vue'
import { t } from '@/i18n'
import { describeError } from '@/serverapi'
import { useFileEditorStore } from '@/stores/fileEditor'
import { useToastStore } from '@/stores/toast'
import { baseName } from '@/utils/format'
import ConfirmDialog from '@/components/ConfirmDialog.vue'

const { sid } = defineProps<{ sid: string }>()

const editor = useFileEditorStore()
const toastStore = useToastStore()

const st = computed(() => editor.stateOf(sid))
const dirty = computed(() => st.value.content !== st.value.original)
const name = computed(() => baseName(st.value.path) || st.value.path)

// ---- 编辑区的几何常量 ----
// ⚠️ 这四个数决定行号列与正文的对齐。镜像层与 textarea 共用 editorStyle（就是为了不让人只改
// 一边），改动前先看那里的注释。
/** 行高（px）：两边都钉死这个**整数**值——相对值（如 1.6）会让两边的行盒按各自的方式取整。 */
const LINE_H = 20
/** 上下内边距（两边相同，行号第一行才会与正文第一行齐平） */
const PAD_Y = 6
const PAD_X = 8
/** 行号列与正文之间的空隙 */
const GAP = 8
/** 行号最多给这么多行：再多就是几万个 DOM 元素，纯属给浏览器添堵（正文照常显示），与预览同一个上限。 */
const MAX_GUTTER_LINES = 20000

/**
 * 标题右边那行元信息：编码（只在不是 UTF-8 时点出来）· CRLF（原文件是 CRLF 时）。
 *
 * 编码与换行都必须显示：它们决定了保存后文件长什么样，而用户看不见这些字节——
 * 「我改的是 UTF-16 的 .ini」这件事只有这里能告诉他。体积与行数按用户要求去掉了（第十七轮）。
 */
const meta = computed(() => {
  const parts: string[] = []
  if (st.value.encoding && st.value.encoding !== 'utf-8') parts.push(st.value.encoding.toUpperCase())
  if (st.value.eol === 'crlf') parts.push(t('files_editor_crlf'))
  return parts.join(' · ')
})

// ---- 行号 ----

/** 逻辑行数。刻意不 split：4 MiB 的文件每敲一个键都 split 一次是白花的时间，这里只数换行符。 */
const lineCount = computed(() => {
  const s = st.value.content
  let n = 1
  for (let i = 0; i < s.length; i++) if (s.charCodeAt(i) === 10) n++
  return n
})

/** 行数超上限就**不画行号**（正文照常显示）——画一半、画错位都是给用户添乱。 */
const gutterOn = computed(() => lineCount.value <= MAX_GUTTER_LINES)

/** 镜像层的行：一行一个元素，元素高度 = 这一行在正文里占的视觉高度（折行了就是多行的高）。 */
const lines = computed(() => (gutterOn.value ? st.value.content.split('\n') : []))

/**
 * 行号列宽（含与正文的间隙）：位数 × `ch` + 空隙。等宽字体里 1ch 就是「0」的宽度，
 * 用 ch 而不是 px 是为了跟着字号走——窄屏下 style.css 把输入控件钉到 16px，字号变了列宽也得变。
 */
const gutterCol = computed(() => `calc(${Math.max(2, String(lineCount.value).length)}ch + ${GAP}px)`)

/**
 * 镜像层与 textarea **共用**的内联样式：内边距 / 行高 / 折行规则必须逐像素相同，否则两边的折行
 * 位置会不一样，行号立刻整体错位。写在一处就是为了不让它们有机会各自漂开。
 *
 * `overflow-wrap: break-word` 两边都要有：textarea 默认就会把超长单词（无空格的路径、URL）撕开
 * 折行，而 `white-space: pre-wrap` 的普通块默认不撕——不显式写上，两者对同一行的折行数就不一样
 * （实测差好几行）。这条是用 `.tmp/edtest/mirror3.html` 的病态用例矩阵量出来的（见契约 27.5）。
 */
const editorStyle = computed<CSSProperties>(() => ({
  paddingTop: `${PAD_Y}px`,
  paddingBottom: `${PAD_Y}px`,
  paddingRight: `${PAD_X}px`,
  paddingLeft: gutterOn.value ? `calc(${gutterCol.value} + ${PAD_X}px)` : `${PAD_X}px`,
  lineHeight: `${LINE_H}px`,
  overflowWrap: 'break-word'
}))

const area = ref<HTMLTextAreaElement | null>(null)
const mirror = ref<HTMLDivElement | null>(null)
const rowHost = ref<HTMLDivElement | null>(null)

/**
 * 编辑区滚动时把镜像层带着一起滚——行号就长在镜像行里，滚镜像等于滚行号。
 *
 * 刻意**不写进响应式状态**：滚动事件每帧都来，写 ref 会让整份行列表重新 diff（两万行 vnode）。
 * 直接改 DOM 的 scrollTop 是这里唯一合理的办法。
 */
function syncScroll() {
  const m = mirror.value
  const el = area.value
  if (m && el) m.scrollTop = el.scrollTop
}

/**
 * 把编辑区归位到开头：光标、视图、行号层一起。
 *
 * 打开与「还原」都要调用它——两次都是「整份内容被 v-model 换掉」，而写 textarea 的 value 会把
 * 光标甩到**末尾**，紧接着浏览器就会把视图滚到最后一行：打开一个长文件看到的是文末，用户根本
 * 不知道自己「已经在文件末尾」。
 */
function resetView() {
  const el = area.value
  if (!el) return
  el.setSelectionRange(0, 0)
  el.scrollTop = 0
  syncScroll()
}

// 打开就把光标放进编辑区（用户点「编辑」就是要打字，不该还要再点一下），并让行号层与编辑区对齐。
// 打开的那一刻组件刚挂载完（v-if 由 FileManager 控制），所以要等 nextTick。
watch(
  () => st.value.open,
  (open) => {
    if (open) {
      void nextTick(() => {
        area.value?.focus()
        resetView()
      })
    }
  },
  { immediate: true }
)

// 内容变了 → 折行与行数都变了 → 重新对齐一次（镜像层高度变化会把它的 scrollTop 夹住，
// 浏览器也可能刚自动滚过编辑区）。查找区的命中表在下面用同步 watcher 单独管。
watch(
  () => st.value.content,
  () => void nextTick(syncScroll)
)

// ---- 查找 / 替换 ----

/** 两行查找区是否展开（顶栏的查找按钮打开，「取消」/Esc 收起）。 */
const findOpen = ref(false)
const query = ref('')
const replacement = ref('')
/** 命中位置（字符下标）：从左到右、互不重叠，随查询词与正文变化重算。 */
const matches = ref<number[]>([])
/** 当前停在哪个命中上（-1 = 还没定位过）；数量显示与「替换」都看它。 */
const cur = ref(-1)
const findInput = ref<HTMLInputElement | null>(null)

/**
 * 扫描命中。**不分大小写**（为了大小写白找一遍更烦人），从左到右、不重叠。
 *
 * toLowerCase 在极少数情况下会改变长度（'İ' 小写化是两个码元），那样 indexOf 的下标就与正文
 * 错位了（选区会画到别处），所以一旦长度变了就退回区分大小写。
 */
function scan(): number[] {
  const q = query.value
  if (!q) return []
  const content = st.value.content
  let hay = content.toLowerCase()
  if (hay.length !== content.length) hay = content
  const needle = q.toLowerCase()
  const out: number[] = []
  let at = hay.indexOf(needle)
  while (at >= 0) {
    out.push(at)
    at = hay.indexOf(needle, at + needle.length)
  }
  return out
}

/** 重算命中并复位当前位置（正文变了、查询词变了、刚展开查找区时都走这里）。 */
function refreshMatches() {
  matches.value = scan()
  cur.value = -1
}

/**
 * 查询词或正文一变就重算命中；查找区没展开时不白扫一遍（大文件一次 toLowerCase 就是 4 MiB）。
 *
 * `flush: 'sync'`：`replaceOne` 改完正文要**立刻**拿到新的命中表才能把选区挪到下一个命中，
 * 默认的 pre 队列会让它先选中、随后又被这里复位。
 */
watch(
  [query, () => st.value.content],
  () => {
    if (findOpen.value) refreshMatches()
  },
  { flush: 'sync' }
)

/** 命中数量：`当前/总数`，没有命中就说「无结果」（还没点过「查找」时当前是 0）。 */
const countText = computed(() => {
  if (!query.value) return ''
  if (!matches.value.length) return t('files_editor_find_none')
  return t('files_editor_find_count', { i: cur.value + 1, n: matches.value.length })
})

function toggleFind() {
  if (findOpen.value) {
    closeFind()
    return
  }
  findOpen.value = true
  refreshMatches()
  // 打开就把光标放进查找框并选中已有内容：接着打字就是新查询词
  void nextTick(() => findInput.value?.select())
}

/** 收起查找区并把焦点还给编辑区（用户按「取消」就是想继续改文件）。 */
function closeFind() {
  findOpen.value = false
  void nextTick(() => area.value?.focus())
}

/** 定位到第 i 个命中（把选区交给编辑区：要能看见，就得先 focus）。 */
function selectMatch(i: number) {
  const el = area.value
  if (!el || i < 0 || i >= matches.value.length) return
  cur.value = i
  const start = matches.value[i]!
  const end = start + query.value.length
  el.focus()
  el.setSelectionRange(start, end)
  reveal(start)
}

/**
 * 点「查找」：按顺序走到下一个命中，走到末尾**回到开头**（循环，用户要求「依次循环显示」）。
 *
 * 还没定位过时从**光标处**往后找第一个（人一般是读到哪儿就从哪儿往下找），找不到再绕回开头；
 * 定位过之后就一格一格往下走。
 */
function findNext() {
  if (!matches.value.length) return
  if (cur.value < 0) {
    const from = area.value?.selectionStart ?? 0
    const at = matches.value.findIndex((s) => s >= from)
    selectMatch(at < 0 ? 0 : at)
    return
  }
  selectMatch((cur.value + 1) % matches.value.length)
}

/** offset 所在的行号（0 起）。 */
function lineIndexAt(offset: number): number {
  const s = st.value.content
  let n = 0
  const end = Math.min(offset, s.length)
  for (let i = 0; i < end; i++) if (s.charCodeAt(i) === 10) n++
  return n
}

/**
 * 把某个下标滚进可视范围。
 *
 * 编辑区不会为了选区自己滚（各内核表现不一致），所以自己算：镜像层与正文的文字盒逐像素重合，
 * 用 Range 在幽灵行里量出的 y 就是它在正文里的 y。行号层关掉（行数超上限）时退化成
 * 「逻辑行 × 行高」估一个位置——只求大致可见。
 */
function reveal(offset: number) {
  const el = area.value
  if (!el) return
  syncScroll() // 量之前先让镜像层与编辑区对上（浏览器可能刚自己滚了一下）
  const line = lineIndexAt(offset)
  let y = line * LINE_H
  const ghost = rowHost.value?.children[line]?.lastElementChild?.firstChild
  if (ghost && ghost.nodeType === Node.TEXT_NODE) {
    const len = ghost.textContent?.length ?? 0
    const col = offset - (st.value.content.lastIndexOf('\n', offset - 1) + 1)
    const range = document.createRange()
    range.setStart(ghost, Math.min(col, len))
    range.setEnd(ghost, Math.min(col + 1, len))
    const rect = range.getBoundingClientRect()
    y = rect.top - (el.getBoundingClientRect().top + PAD_Y) + el.scrollTop
  }
  const top = el.scrollTop
  if (y < top + PAD_Y || y + LINE_H > top + el.clientHeight - PAD_Y) {
    el.scrollTop = Math.max(0, y - Math.round(el.clientHeight / 3))
  }
}

/**
 * 把选区放到 `from` 之后的第一个命中（没有就绕回开头）；没有命中就只把光标放在 `from`。
 *
 * ⚠️ 改正文之后**必须**等 DOM 打过补丁再动选区：v-model 写 textarea 的 value 会把光标甩到
 * 末尾，同步设置的选区会被这一步覆盖掉（实测「替换」之后选区停在文末）。
 */
function selectNextFrom(from: number) {
  if (matches.value.length) {
    const at = matches.value.findIndex((s) => s >= from)
    selectMatch(at < 0 ? 0 : at)
    return
  }
  const el = area.value
  if (el) {
    el.focus()
    el.setSelectionRange(from, from)
  }
}

/**
 * 替换当前命中，然后把选区移到**插入文本之后**的下一个命中。
 *
 * 不回头从刚插进去的内容里再匹配（把 `a` 换成 `aa` 时那样会原地打转）；后面没有了就绕回开头，
 * 与「查找」同一条循环规则。还没定位过时就先按「查找」的规则定位**并立刻替换它**——点了不换、
 * 只是选中，用户会以为按钮坏了。
 */
function replaceOne() {
  if (!query.value || !matches.value.length) return
  let idx = cur.value
  if (idx < 0) {
    const from = area.value?.selectionStart ?? 0
    idx = matches.value.findIndex((s) => s >= from)
    if (idx < 0) idx = 0
  }
  const q = query.value
  const start = matches.value[idx]!
  const content = st.value.content
  // 赋值会触发上面的同步 watcher：命中表在这一行之后已经是新的了
  st.value.content = content.slice(0, start) + replacement.value + content.slice(start + q.length)
  const from = start + replacement.value.length
  void nextTick(() => selectNextFrom(from))
}

/** 全部替换：一次换完（不重叠、从左到右），换了几处必须说出来——不然「点了没反应」最像 bug。 */
function replaceAll() {
  const q = query.value
  const list = matches.value
  if (!q || !list.length) return
  const content = st.value.content
  const parts: string[] = []
  let last = 0
  for (const at of list) {
    parts.push(content.slice(last, at), replacement.value)
    last = at + q.length
  }
  parts.push(content.slice(last))
  // 换完不要动光标与视图：批量替换后视图被甩到文末（v-model 写值的副作用）只会让人以为出错了，
  // 而用户下一步通常是「保存」，得能接着看刚才那一段。
  const caret = area.value?.selectionStart ?? 0
  const scrollTop = area.value?.scrollTop ?? 0
  st.value.content = parts.join('')
  void nextTick(() => {
    const el = area.value
    if (!el) return
    const at = Math.min(caret, el.value.length)
    el.setSelectionRange(at, at)
    el.scrollTop = scrollTop
  })
  toastStore.show(t('files_editor_replace_all_done', { n: list.length }), 'success')
}

const confirm = reactive<{ show: boolean; message: string; run: () => void }>({
  show: false,
  message: '',
  run: () => {}
})

function askConfirm(message: string, run: () => void) {
  confirm.message = message
  confirm.run = run
  confirm.show = true
}

function onConfirmOk() {
  const run = confirm.run
  confirm.show = false
  confirm.run = () => {}
  run()
}

/**
 * 退出。有未保存的修改时先问一句——直接丢掉用户敲进去的东西是这一页最不能犯的错。
 */
function exit() {
  if (dirty.value) {
    askConfirm(t('files_editor_exit_confirm'), () => editor.close(sid))
    return
  }
  editor.close(sid)
}

async function save() {
  if (!dirty.value || st.value.saving) return
  try {
    await editor.save(sid)
    toastStore.show(t('files_editor_saved', { name: name.value }), 'success')
  } catch (e) {
    // 失败时编辑页保持打开、内容原样留着：用户要能重试或者复制出来
    toastStore.show(t('files_editor_save_failed', { error: describeError(e) }), 'error')
  }
}

async function revert() {
  const run = async () => {
    try {
      await editor.revert(sid)
      // 与「打开」同一个道理：读回来的内容会把光标甩到末尾、视图跟着滚到文末，显式归位
      void nextTick(resetView)
    } catch (e) {
      toastStore.show(t('files_editor_load_failed', { error: describeError(e) }), 'error')
    }
  }
  if (dirty.value) {
    askConfirm(t('files_editor_revert_confirm'), () => void run())
    return
  }
  await run()
}

/** Ctrl/Cmd+S 保存：浏览器默认是「保存网页」，必须挡掉（prevent）。 */
function onKeydown(event: KeyboardEvent) {
  if ((event.ctrlKey || event.metaKey) && event.key.toLowerCase() === 's') {
    event.preventDefault()
    void save()
  }
}
</script>

<template>
  <!-- absolute inset-0：**覆盖**文件管理页面（文件管理器保持挂载，退出后目录、视图、滚动都在） -->
  <div class="absolute inset-0 z-30 flex flex-col bg-bg dark:bg-bg-dark">
    <div
      class="flex shrink-0 items-center gap-2 border-b border-line px-2 py-1.5 dark:border-line-dark"
    >
      <!-- 左上角：退出（回到文件管理，不动它的状态） -->
      <button
        type="button"
        class="g-btn-ghost h-8 shrink-0 !px-2"
        :title="t('files_editor_exit')"
        :aria-label="t('files_editor_exit')"
        @click="exit"
      >
        <svg
          class="h-4 w-4"
          viewBox="0 0 24 24"
          fill="none"
          stroke="currentColor"
          stroke-width="1.5"
          stroke-linecap="round"
          stroke-linejoin="round"
        >
          <path d="M19 12H5" />
          <path d="m12 19-7-7 7-7" />
        </svg>
        <span>{{ t('files_editor_exit') }}</span>
      </button>

      <span class="min-w-0 flex-1 truncate font-mono text-xs" :title="st.path">{{ name }}</span>

      <span v-if="meta" class="shrink-0 font-mono text-xs text-ink-faint dark:text-ink-faint-dark">
        {{ meta }}
      </span>
      <!-- 未保存标记：右上角按钮的可用态已经说明了同一件事，但改完一个字符就想确认「存了没」时，
           一个常驻的标记比盯着按钮的灰度看要快 -->
      <span v-if="dirty" class="g-chip shrink-0">{{ t('files_editor_unsaved') }}</span>

      <!-- 查找：点开在编辑区上方出现两行（查找 / 替换），展开时按钮自己变成激活色 -->
      <button
        type="button"
        class="g-btn-ghost h-8 shrink-0 !px-2"
        :class="findOpen ? '!bg-brand-soft !text-brand' : ''"
        :title="t('files_editor_find')"
        :aria-label="t('files_editor_find')"
        :aria-expanded="findOpen"
        @click="toggleFind"
      >
        <svg
          class="h-4 w-4"
          viewBox="0 0 24 24"
          fill="none"
          stroke="currentColor"
          stroke-width="1.5"
          stroke-linecap="round"
          stroke-linejoin="round"
        >
          <circle cx="11" cy="11" r="7" />
          <path d="m20 20-3.6-3.6" />
        </svg>
      </button>

      <!-- 右上角：还原 / 保存 -->
      <button
        type="button"
        class="g-btn-secondary h-8 shrink-0"
        :disabled="st.loading || st.saving"
        :title="t('files_editor_revert_hint')"
        @click="revert"
      >
        {{ t('files_editor_revert') }}
      </button>
      <button
        type="button"
        class="g-btn-primary h-8 shrink-0"
        :disabled="!dirty || st.saving"
        :title="t('files_editor_save_hint')"
        @click="save"
      >
        {{ st.saving ? t('files_editor_saving') : t('common_save') }}
      </button>
    </div>

    <!-- 查找 / 替换两行（用户要求）：第一行「输入框 + 查找 + 数量 + 取消」，第二行「输入框 + 替换 + 全部替换」。
         「查找」没命中时禁用（点了不动的按钮比灰按钮更像坏了），数量跟着当前命中显示 `i/n`。 -->
    <div
      v-if="findOpen"
      class="shrink-0 space-y-1 border-b border-line bg-black/[0.02] px-2 py-1 dark:border-line-dark dark:bg-white/[0.03]"
    >
      <div class="flex items-center gap-1.5">
        <input
          ref="findInput"
          v-model="query"
          type="text"
          class="g-input h-7 min-w-0 flex-1 !py-0 !text-xs"
          :placeholder="t('files_editor_find_ph')"
          :aria-label="t('files_editor_find_ph')"
          spellcheck="false"
          autocapitalize="off"
          autocomplete="off"
          autocorrect="off"
          @keydown.enter.prevent="findNext"
          @keydown.esc.prevent="closeFind"
        />
        <button
          type="button"
          class="g-btn-secondary h-7 shrink-0 !px-3 !text-xs"
          :disabled="!matches.length"
          @click="findNext"
        >
          {{ t('files_editor_find') }}
        </button>
        <span
          class="w-16 shrink-0 truncate text-center font-mono text-xs text-ink-faint dark:text-ink-faint-dark"
          >{{ countText }}</span
        >
        <button type="button" class="g-btn-ghost h-7 shrink-0 !px-2 !text-xs" @click="closeFind">
          {{ t('common_cancel') }}
        </button>
      </div>
      <div class="flex items-center gap-1.5">
        <input
          v-model="replacement"
          type="text"
          class="g-input h-7 min-w-0 flex-1 !py-0 !text-xs"
          :placeholder="t('files_editor_replace_ph')"
          :aria-label="t('files_editor_replace_ph')"
          spellcheck="false"
          autocapitalize="off"
          autocomplete="off"
          autocorrect="off"
          @keydown.enter.prevent="replaceOne"
          @keydown.esc.prevent="closeFind"
        />
        <button
          type="button"
          class="g-btn-secondary h-7 shrink-0 !px-3 !text-xs"
          :disabled="!matches.length"
          @click="replaceOne"
        >
          {{ t('files_editor_replace') }}
        </button>
        <button
          type="button"
          class="g-btn-secondary h-7 shrink-0 !px-3 !text-xs"
          :disabled="!matches.length"
          @click="replaceAll"
        >
          {{ t('files_editor_replace_all') }}
        </button>
      </div>
    </div>

    <!-- 编辑区 = 镜像层 + textarea，两层**同框叠放**（同一个 absolute inset-0、同一份 editorStyle）。
         镜像层每行一个「幽灵块」（正常排版但不可见），它的高度就是这一行在正文里折行后的高度，
         行号作为行内的绝对定位元素贴在行首；整层 visibility: hidden，行号用 visibility: visible
         穿透出来。这样行号天然跟着折行走，不需要 JS 逐行量高度，滚动时也不会漂。
         textarea 在上面（透明背景），负责输入、选区、IME 与滚动。
         ⚠️ 三个必须一起看的点：
           1. 两个容器都得 `overflow-y: scroll`（**常驻**占位）：只让一边有滚动条，另一边在内容
              不足一屏时文字盒会宽 8px，折行位置就与行号对不上了；
           2. 两边的字体/字号/行高/折行规则必须一致（editorStyle + 外层同一套 font-mono 与
              max-md:text-[16px]，否则窄屏下 style.css 把输入控件钉到 16px 时镜像层还是 12px）；
           3. 幽灵层的 span 必须 min-height 一个行高：空行在普通块里高度是 0，而 textarea 里
              空行占一整行。 -->
    <div class="relative min-h-0 flex-1 font-mono text-xs max-md:text-[16px]">
      <div
        v-if="gutterOn"
        ref="mirror"
        class="thin-scroll pointer-events-none absolute inset-0 overflow-x-hidden overflow-y-scroll text-right"
        :style="[editorStyle, { visibility: 'hidden' }]"
        aria-hidden="true"
      >
        <div ref="rowHost">
          <div
            v-for="(line, i) in lines"
            :key="i"
            class="relative"
            :style="{ lineHeight: `${LINE_H}px`, minHeight: `${LINE_H}px` }"
          >
            <span
              class="absolute top-0 text-ink-faint dark:text-ink-faint-dark"
              style="visibility: visible"
              :style="{ right: `calc(100% + ${GAP}px)`, width: gutterCol }"
              >{{ i + 1 }}</span
            >
            <span class="block whitespace-pre-wrap">{{ line }}</span>
          </div>
        </div>
      </div>

      <textarea
        ref="area"
        v-model="st.content"
        class="thin-scroll absolute inset-0 h-full w-full resize-none overflow-x-hidden overflow-y-scroll border-0 bg-transparent text-ink outline-none dark:text-ink-dark"
        :style="editorStyle"
        wrap="soft"
        spellcheck="false"
        autocapitalize="off"
        autocomplete="off"
        autocorrect="off"
        :aria-label="t('files_editor_title', { name })"
        @keydown="onKeydown"
        @scroll="syncScroll"
      />
    </div>

    <ConfirmDialog
      :visible="confirm.show"
      :title="t('common_tip')"
      :message="confirm.message"
      :danger="true"
      @update:visible="confirm.show = $event"
      @confirm="onConfirmOk"
      @cancel="confirm.run = () => {}"
    />
  </div>
</template>
