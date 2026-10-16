<script setup lang="ts">
// NewEntryDialog — 「新建」弹窗：新建目录与新建文件**共用一个输入框**（第二十三轮）。
//
// 为什么另起一个组件而不是给 PromptDialog 加一个按钮：这个弹窗的动作按钮**本身就是
// 「要建什么」的开关**——用户输完名字直接点「新建文件夹」或「新建文件」，不必先在一处选类型
// 再点「确定」。PromptDialog 是「收集一个字符串 + 一个确定」的通用件，重命名等场景在用它，
// 不该为了这一个场景长出一套「多路确定」的语义。
//
// 与 PromptDialog 的两处**刻意差别**：
// 1. 提交**不自我关闭**：名字校验在调用方（规则与原实现完全一致：非空、不含 `/`、不是 `.` / `..`），
//    校验不过时调用方只 toast 原因、弹窗留着——用户不用重新打开再输一遍（原实现在这个场景下
//    是「先关弹窗再报错」）。
// 2. 输入框里按回车 = **新建文件**：合并后保留下来的入口就是「新建文件」那个图标，回车给默认动作；
//    Esc / 点遮罩 / 「取消」都是取消。
import { nextTick, ref, watch } from 'vue'
import { t } from '@/i18n'

const props = defineProps<{
  visible: boolean
  label?: string
  placeholder?: string
}>()

const emit = defineEmits<{
  (e: 'update:visible', v: boolean): void
  (e: 'submit', kind: 'file' | 'dir', value: string): void
  (e: 'cancel'): void
}>()

const open = ref(props.visible)
const value = ref('')
const inputEl = ref<HTMLInputElement | null>(null)

watch(
  () => props.visible,
  (v) => {
    open.value = v
    if (!v) return
    // 每次打开都清空：新建总是从空名字开始（与重命名不同，那里回填原名）。
    // 放在 nextTick 里是因为面板此刻才从 v-if 渲染出来，立即 focus 会落到 null 上。
    value.value = ''
    void nextTick(() => inputEl.value?.focus())
  },
  { immediate: true }
)

function close() {
  open.value = false
  emit('update:visible', false)
}

/** 提交给调用方**不关弹窗**：关不关由调用方按校验结果决定（见文件头的第 1 条）。 */
function submit(kind: 'file' | 'dir') {
  emit('submit', kind, value.value)
}

function onCancel() {
  emit('cancel')
  close()
}
</script>

<template>
  <Teleport to="body">
    <Transition name="modal-fade">
      <div v-if="open" class="fixed inset-0 z-50">
        <div class="absolute inset-0 bg-black/50" @click="onCancel"></div>
        <!-- 与其它弹窗同一条定位带（.region-center）；输入框场景尤其不能退到
             fixed inset-0 居中：iOS 键盘弹起时面板会被键盘盖住。 -->
        <div class="region-center absolute left-0 right-0 flex items-center justify-center px-4">
          <div
            class="pointer-events-auto relative max-h-[90%] w-full max-w-sm overflow-y-auto rounded-xl border border-line bg-surface p-5 shadow-pop dark:border-line-dark dark:bg-surface-dark"
          >
            <h3 class="font-display text-base font-semibold text-ink dark:text-ink-dark">
              {{ t('files_new') }}
            </h3>
            <label
              v-if="label"
              for="new-entry-dialog-input"
              class="mt-3 block text-xs font-medium text-ink-soft dark:text-ink-soft-dark"
            >
              {{ label }}
            </label>
            <!-- id + for 把 label 和输入框真正关联起来（同 PromptDialog）：否则输入框没有可访问名。 -->
            <input
              id="new-entry-dialog-input"
              ref="inputEl"
              v-model="value"
              type="text"
              class="g-input mt-1.5"
              :placeholder="placeholder"
              @keydown.enter.prevent="submit('file')"
              @keydown.esc.prevent="onCancel"
            />
            <div class="mt-5 flex justify-end gap-2">
              <button class="g-btn-ghost !h-9 !px-4 text-sm" @click="onCancel">
                {{ t('common_cancel') }}
              </button>
              <button
                class="g-btn-secondary !h-9 whitespace-nowrap !px-4 text-sm"
                @click="submit('dir')"
              >
                {{ t('files_new_dir') }}
              </button>
              <button
                class="g-btn-primary !h-9 whitespace-nowrap !px-4 text-sm"
                @click="submit('file')"
              >
                {{ t('files_new_file') }}
              </button>
            </div>
          </div>
        </div>
      </div>
    </Transition>
  </Teleport>
</template>
