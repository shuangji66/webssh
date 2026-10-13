<script setup lang="ts">
// BreadcrumbBar — 左栏顶部的**按钮条**（不显示路径，见契约 11.3 / 13.1 / 14.3）。
//
// 为什么去掉路径：左栏宽度只有 11~20rem，一串「根目录 / vol1 / 1000 / docker / …」在这里
// 只能靠横向滚动看，而它想回答的「我在哪」在文件管理器的面包屑里已经回答过了（那里有整行
// 宽度）。左栏这一行的价值在于**动作**：
//   [刷新] [隐藏面包屑]
//
// 第四轮：家目录按钮简化成纯图标。
// 第六轮：删掉「在此目录新建终端」（工具栏/文件树菜单里都有，重复）；新增「隐藏面包屑」——
// 左栏整条收起，收起后子标签栏左侧会出现「展开面包屑」按钮（见 SessionView）。
// 第八轮：
//   - 删掉「家目录」按钮：树本来就会自动展开到登录用户的家目录（新开会话时、以及重新展开
//     面包屑时各一次，见 FileTree 的 revealHome），再放一个按钮是重复的；
//   - 删掉「切换到文件管理」：子标签栏里那个「文件管理」子标签是常驻的，不需要第二个入口
//     （注意：文件管理器里的「返回终端」**不是**它的反面，那个要留着）。
//
// 本组件不 emit 任何事件：动作全部直接落到 store（sftp / sessionUi）上。
import { computed } from 'vue'
import { t } from '@/i18n'
import { useSessionUiStore } from '@/stores/sessionUi'
import { useSftpStore } from '@/stores/sftp'

const { sid } = defineProps<{ sid: string }>()

const sftp = useSftpStore()
const ui = useSessionUiStore()

const state = computed(() => sftp.stateOf(sid))

function refresh() {
  void (state.value.dir ? sftp.refresh(sid) : sftp.ensure(sid))
}

/** 收起左栏（面包屑 + 目录树）。再展开的入口在子标签栏左侧那个「展开面包屑」按钮上。 */
function hideLeft() {
  ui.setLeftHidden(sid, true)
}
</script>

<template>
  <div
    class="flex shrink-0 items-center gap-0.5 border-b border-line px-1 py-1 dark:border-line-dark"
  >
    <button
      type="button"
      class="tree-tool"
      :title="t('files_refresh')"
      :aria-label="t('files_refresh')"
      @click="refresh"
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
        <path d="M21 12a9 9 0 1 1-3-6.7" />
        <path d="M21 4v5h-5" />
      </svg>
    </button>

    <!-- 隐藏面包屑（第六轮）：整条左栏收起来，把横向空间全让给主区域。
         展开入口在子标签栏最左侧（收起后才出现，见 SessionView）。 -->
    <button
      type="button"
      class="tree-tool"
      :title="t('session_hide_left')"
      :aria-label="t('session_hide_left')"
      @click="hideLeft"
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
        <rect x="3" y="4" width="18" height="16" rx="2" />
        <path d="M9 4v16" />
        <path d="m16 9-2 3 2 3" />
      </svg>
    </button>
  </div>
</template>
