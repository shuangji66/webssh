<script setup lang="ts">
// BreadcrumbBar — 左栏顶部的路径条（面包屑 + 家目录快捷入口 + 「完整文件管理」）。
//
// 为什么和 FileManager 里那条面包屑分开写：两者在不同的位置上、承担的事也不一样。左栏很窄
// （可能只有 11rem），这里必须把分段收成一行横向滚动，并提供「切回完整文件管理」的入口
// （左栏被关掉之后，用户得有路回到文件管理子标签）；文件管理面板里那条是主视图的导航，
// 分段要逐个可见、还要和「已选 n 项」共用同一行位置。
import { computed, nextTick, ref, watch } from 'vue'
import { t } from '@/i18n'
import { useSftpStore } from '@/stores/sftp'
import { revealActive, wheelToHorizontal } from '@/utils/scroll'

const { sid } = defineProps<{ sid: string }>()
const emit = defineEmits<{ 'show-full': [] }>()

const sftp = useSftpStore()
const state = computed(() => sftp.stateOf(sid))

const rowEl = ref<HTMLElement | null>(null)

/** 分段：第一段固定是根目录，随后每进一层加一段（末段就是当前目录）。 */
const crumbs = computed(() => {
  const dir = state.value.dir || state.value.home || ''
  const out = [{ label: t('session_crumb_root'), path: '/' }]
  let acc = ''
  for (const part of dir.split('/').filter(Boolean)) {
    acc += `/${part}`
    out.push({ label: part, path: acc })
  }
  return out
})

// 进入深层目录后末段会滚出可视区：切片一变就把它滚回来，否则用户看到的永远是开头那段
watch(
  () => state.value.dir,
  () => void nextTick(() => revealActive(rowEl.value, '.crumb-current'))
)

function go(path: string) {
  if (!path) return
  void sftp.navigate(sid, path)
}
</script>

<template>
  <div class="shrink-0 border-b border-line dark:border-line-dark">
    <div
      ref="rowEl"
      class="thin-scroll flex items-center overflow-x-auto px-2 py-1 text-xs"
      @wheel="wheelToHorizontal"
    >
      <template v-for="(seg, i) in crumbs" :key="seg.path">
        <span v-if="i" class="shrink-0 px-0.5 text-ink-faint">/</span>
        <button
          type="button"
          class="max-w-[9rem] shrink-0 truncate rounded px-1 py-0.5 text-ink-soft hover:bg-black/5 hover:text-ink dark:text-ink-soft-dark dark:hover:bg-white/5 dark:hover:text-white"
          :class="i === crumbs.length - 1 ? 'crumb-current font-medium text-ink dark:text-ink-dark' : ''"
          :title="seg.path"
          @click="go(seg.path)"
        >
          {{ seg.label }}
        </button>
      </template>
    </div>

    <div class="flex items-center gap-1 px-2 pb-1">
      <button
        type="button"
        class="g-btn-ghost h-7 gap-1 px-1.5 text-xs"
        :disabled="!state.home || state.dir === state.home"
        :title="t('files_home')"
        @click="go(state.home)"
      >
        <svg
          class="h-3.5 w-3.5"
          viewBox="0 0 24 24"
          fill="none"
          stroke="currentColor"
          stroke-width="1.5"
          stroke-linecap="round"
          stroke-linejoin="round"
        >
          <path d="m3 10 9-7 9 7v10a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2z" />
          <path d="M9 22V12h6v10" />
        </svg>
        <span class="truncate">{{ t('files_home') }}</span>
      </button>
      <button
        type="button"
        class="g-btn-ghost ml-auto h-7 gap-1 px-1.5 text-xs"
        :title="t('session_full_files')"
        @click="emit('show-full')"
      >
        <svg
          class="h-3.5 w-3.5"
          viewBox="0 0 24 24"
          fill="none"
          stroke="currentColor"
          stroke-width="1.5"
          stroke-linecap="round"
          stroke-linejoin="round"
        >
          <path d="M4 20a2 2 0 0 1-2-2V6a2 2 0 0 1 2-2h4l2 2h6a2 2 0 0 1 2 2v12a2 2 0 0 1-2 2z" />
          <path d="m10 13 2 2 4-4" />
        </svg>
        <span class="truncate">{{ t('session_full_files') }}</span>
      </button>
    </div>
  </div>
</template>
