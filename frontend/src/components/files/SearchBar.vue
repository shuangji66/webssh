<script setup lang="ts">
// SearchBar — 文件管理器的搜索栏（第二十二轮，契约 32.2）。
//
// 一排是：输入框 + 「结果：x 项」按钮 + 搜索 + 取消。左边**没有**放大镜图标（同轮返工：用户
// 要求删掉——输入框自己就说明了这里能搜，那个图标只是占地方）。
//
// 两个动作各有各的控件，这是这一轮返工的关键（原来挤在「搜索」一个按钮上，语义要按处境猜）：
//   - **搜索**（与回车同一个动作）：永远在当前目录搜一次，从不管你现在在看什么；
//   - **结果：x 项**：回到上一次的那批结果（缓存，不重新请求）。进入结果里的目录之后再点它
//     就回到结果，与「在当前目录重新搜索」互不干扰。没有可回的结果时这个按钮不出现。
//
// 状态全在 stores/sftp.ts 的 `state.search` 里：搜索栏与「结果 / 那个结果属于哪个目录」是
// 同一次搜索的生命周期，拆成两处（组件一份、store 一份）就会出现「栏还在、结果没了」这种
// 半死不活的状态。这个组件与 BreadcrumbBar 同一套写法：不 emit 事件，动作直接落到 store。
//
// 另一个刻意的取舍：搜索栏一出现就**聚焦输入框**（用户点搜索图标就是为了打字）。这与快捷访问
// 弹窗刻意相反——那个打开后更常见的用途是挑一条已有路径，抢焦点会在移动端立刻弹起键盘盖住列表。
import { computed, nextTick, onMounted, ref } from 'vue'
import { t } from '@/i18n'
import { useSftpStore } from '@/stores/sftp'
import { useToastStore } from '@/stores/toast'
import { describeError } from '@/serverapi'

const { sid } = defineProps<{ sid: string }>()

const sftp = useSftpStore()
const toastStore = useToastStore()

const search = computed(() => sftp.stateOf(sid).search)
const inputEl = ref<HTMLInputElement | null>(null)

// 这个组件只在搜索栏显示时挂载（父组件用 v-if），所以「挂载即聚焦」就够：
// 用户点搜索图标就是为了打字，不必再点一次输入框。
onMounted(() => void nextTick(() => inputEl.value?.focus()))

/**
 * 有没有可以「回去」的结果：这次搜索已经开始过（在跑或已结束）。
 *
 * 没有它（刚打开搜索栏、还没搜过）时不显示那个按钮——一个「结果：0 项」的按钮点下去什么都
 * 没有，不如不出现（与剪贴板那一行同一个口径）。
 */
const hasResults = computed(() => search.value.running || search.value.done)

/** 「结果：x 项」按钮的说明：搜索中顺带说清「还在动、扫了多少」。 */
const resultsTitle = computed(() =>
  search.value.running
    ? t('files_search_scanning', { scanned: search.value.scanned })
    : t('files_search_results_title')
)

async function submit() {
  try {
    await sftp.runSearch(sid)
  } catch (e) {
    // 空关键词这类问题必须说清原因，不能「点了没反应」
    toastStore.show(describeError(e), 'error')
  }
}
</script>

<template>
  <div
    class="flex shrink-0 items-center gap-2 border-b border-line bg-brand-soft/60 px-2 py-1 dark:border-line-dark dark:bg-white/5"
  >
    <!-- 输入框本身没有可见 label（搜索栏的形态约定俗成），用 aria-label 给它一个可访问名 -->
    <input
      ref="inputEl"
      v-model="search.query"
      type="text"
      class="g-input h-8 min-w-0 flex-1 !py-0 text-xs"
      :placeholder="t('files_search_placeholder')"
      :aria-label="t('files_search')"
      @keydown.enter.prevent="submit"
    />
    <!-- 失败原因必须说清（不能「点了没反应」）；完整原因在结果区里，这里给一句截断的 -->
    <span
      v-if="search.error"
      class="max-w-[40%] shrink-0 truncate text-xs text-danger"
      :title="t('files_search_failed', { error: search.error })"
    >
      {{ t('files_search_failed', { error: search.error }) }}
    </span>
    <!-- 回到上次的结果（缓存，不重新请求）。搜索中它也在长，点它就是看已经搜到的那部分。 -->
    <button
      v-if="hasResults"
      type="button"
      class="g-btn-ghost h-7 shrink-0 !px-2 tabular-nums"
      :title="resultsTitle"
      :aria-label="resultsTitle"
      @click="sftp.showSearchResults(sid)"
    >
      {{ t('files_search_results', { n: search.results.length }) }}
    </button>
    <button
      type="button"
      class="g-btn-ghost h-7 shrink-0 !px-2"
      :title="t('files_search_run_title')"
      :aria-label="t('files_search_run_title')"
      @click="submit"
    >
      {{ t('files_search_run') }}
    </button>
    <button
      type="button"
      class="g-btn-ghost h-7 shrink-0 !px-2"
      :title="t('files_search_cancel')"
      @click="sftp.closeSearch(sid)"
    >
      {{ t('files_search_cancel') }}
    </button>
  </div>
</template>
