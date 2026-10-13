<script setup lang="ts">
// UploadDialog — 上传弹窗（三个来源：本地文件 / 本地目录 / 飞牛文件）。
//
// 上传动作放在弹窗里而不是调用方：三个来源共用同一套进度、取消与失败展示，散到
// FileManager / FileTree 两处各写一遍，迟早有一边漏掉取消或漏掉失败原因。
//
// 进度与取消的差别是有原因的：
//   - 本地文件 / 本地目录：走 XHR（serverapi.uploadLocal），能拿真实进度，AbortSignal 一
//     abort 会连带取消后端 ctx，不会留下写了一半的文件；
//   - 飞牛文件：后端接口 /api/fs/upload-from-nas 没有 signal（一次读一个本机文件推过去），
//     只能按「已完成 / 总数」显示进度，也不提供取消按钮——给了按钮却取消不掉更糟。
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { t } from '@/i18n'
import { describeError, type UploadEntry, type UploadResult } from '@/serverapi'
import { useSftpStore } from '@/stores/sftp'
import { useToastStore } from '@/stores/toast'
import { isHostAvailable, pickUserFiles, TrimError, TrimUnavailableError } from '@/trim/sdk'

const props = defineProps<{ visible: boolean; sid: string; dir: string }>()
const emit = defineEmits<{
  'update:visible': [v: boolean]
  done: [result: UploadResult]
}>()

const sftp = useSftpStore()
const toastStore = useToastStore()

type Tab = 'local' | 'dir' | 'nas'

const tab = ref<Tab>('local')
const localFiles = ref<File[]>([])
const dirFiles = ref<File[]>([])
const nasPaths = ref<string[]>([])
/** 默认不覆盖：同名被跳过会明确报出来，而静默覆盖是不可恢复的。 */
const overwrite = ref(false)
const uploading = ref(false)
const percent = ref(0)
const error = ref('')

const localInput = ref<HTMLInputElement | null>(null)
const dirInput = ref<HTMLInputElement | null>(null)

/** 目录上传靠 webkitdirectory（Chromium / Safari / Firefox 都支持，但不保证）。 */
const dirSupported = 'webkitdirectory' in document.createElement('input')
const hostAvailable = isHostAvailable()

const tabs = computed<{ id: Tab; label: string }[]>(() => [
  { id: 'local', label: t('upload_tab_local') },
  { id: 'dir', label: t('upload_tab_dir') },
  { id: 'nas', label: t('upload_tab_nas') }
])

const tabHint = computed(() => {
  if (tab.value === 'local') return t('upload_local_hint')
  if (tab.value === 'dir') return t('upload_dir_hint')
  return t('upload_nas_hint')
})

function toast(msg: string, type: 'success' | 'error' | 'info' = 'info') {
  toastStore.show(msg, type)
}

// 每次打开都从干净状态开始：上一次失败留下的红字、选一半的文件不该跟过来
watch(
  () => props.visible,
  (visible) => {
    if (!visible) return
    tab.value = 'local'
    localFiles.value = []
    dirFiles.value = []
    nasPaths.value = []
    percent.value = 0
    error.value = ''
  }
)

// ---- 选择来源 ----

function onLocalPick(event: Event) {
  const input = event.target as HTMLInputElement
  localFiles.value = Array.from(input.files ?? [])
  // 清空 input 的 value：同一个文件连选两次也要能触发 change
  input.value = ''
}

function onDirPick(event: Event) {
  const input = event.target as HTMLInputElement
  dirFiles.value = Array.from(input.files ?? [])
  input.value = ''
}

async function pickNas() {
  error.value = ''
  try {
    const paths = await pickUserFiles(t('upload_tab_nas'))
    // 空数组 = 用户在宿主选择器里点了取消，不是错误
    if (!paths.length) {
      toast(t('upload_nas_canceled'))
      return
    }
    nasPaths.value = paths
  } catch (e) {
    if (e instanceof TrimUnavailableError) {
      toast(t('upload_nas_unavailable'), 'error')
      return
    }
    if (e instanceof TrimError) {
      toast(t('upload_nas_pick_failed', { error: e.message }), 'error')
      return
    }
    toast(t('upload_nas_pick_failed', { error: describeError(e) }), 'error')
  }
}

// ---- 上传 ----

let controller: AbortController | null = null

/** 待上传的条目：目录上传必须带相对路径（含顶层目录名），否则整个目录会被拍到目标目录下。 */
function entriesOf(files: File[], keepRelativePath: boolean): UploadEntry[] {
  return files.map((file) => ({
    file,
    path: keepRelativePath ? file.webkitRelativePath || file.name : file.name
  }))
}

function requestClose() {
  // 上传中不许直接关：要么等它跑完，要么点「取消上传」（abort 掉请求、服务端也不会留半截文件）
  if (uploading.value) return
  emit('update:visible', false)
}

function cancel() {
  controller?.abort()
}

function startUpload() {
  if (uploading.value) return
  if (tab.value === 'nas') {
    if (!nasPaths.value.length) {
      toast(t('upload_nas_need_path'))
      return
    }
    void run(() => uploadNas(), false)
    return
  }
  const entries =
    tab.value === 'local' ? entriesOf(localFiles.value, false) : entriesOf(dirFiles.value, true)
  if (!entries.length) {
    toast(t('upload_none'))
    return
  }
  const ctl = new AbortController()
  controller = ctl
  void run(
    () =>
      sftp.uploadLocal(
        props.sid,
        props.dir,
        entries,
        overwrite.value,
        (p) => {
          percent.value = p
        },
        ctl.signal
      ),
    true
  )
}

/** 飞牛文件逐个上传（接口一次只吃一个 source），进度按「已完成 / 总数」推进。 */
async function uploadNas(): Promise<UploadResult> {
  const acc: UploadResult = { files: [], skipped: 0 }
  const total = nasPaths.value.length
  for (let i = 0; i < total; i += 1) {
    percent.value = Math.round((i / total) * 100)
    const res = await sftp.uploadFromNas(props.sid, props.dir, nasPaths.value[i], overwrite.value)
    acc.files.push(...res.files)
    acc.skipped += res.skipped
  }
  percent.value = 100
  return acc
}

/** 成功 / 取消 / 失败的收敛点：三种结局的收尾（状态复位、提示、关弹窗）只在这里写一次。 */
async function run(task: () => Promise<UploadResult>, abortable: boolean) {
  uploading.value = true
  percent.value = 0
  error.value = ''
  const ctl = controller
  try {
    const res = await task()
    uploading.value = false
    toast(t('upload_done', { n: res.files.length }), 'success')
    // 关闭覆盖时被跳过的文件要说清楚，否则用户以为传上去了
    if (res.skipped > 0) toast(t('upload_skipped', { n: res.skipped }))
    emit('done', res)
    emit('update:visible', false)
  } catch (e) {
    uploading.value = false
    if (abortable && ctl?.signal.aborted) {
      toast(t('upload_canceled'))
      emit('update:visible', false)
      return
    }
    error.value = describeError(e)
    toast(t('upload_failed', { error: error.value }), 'error')
  } finally {
    if (controller === ctl) controller = null
  }
}

// ---- 键盘：Esc 关闭（上传中不响应，避免误关掉正在进行的请求） ----

function onKeydown(event: KeyboardEvent) {
  if (event.key === 'Escape' && props.visible) requestClose()
}

onMounted(() => window.addEventListener('keydown', onKeydown))
onBeforeUnmount(() => {
  window.removeEventListener('keydown', onKeydown)
  controller?.abort()
})
</script>

<template>
  <!-- 与 shell 的弹窗同一套外壳：Teleport 到 body + 铺满全屏的遮罩 + .region-center 定位带。
       面板不用 fixed inset-0 居中：那相对布局视口，iOS 键盘弹起时会被键盘盖住。 -->
  <Teleport to="body">
    <Transition name="modal-fade">
      <div v-if="visible" class="fixed inset-0 z-50">
        <div class="absolute inset-0 bg-black/50" @click="requestClose" />
        <div class="region-center absolute right-0 left-0 flex items-center justify-center px-4">
          <div
            class="pointer-events-auto flex max-h-[90%] w-full max-w-lg flex-col overflow-hidden rounded-xl border border-line bg-surface shadow-pop dark:border-line-dark dark:bg-surface-dark"
          >
            <div
              class="flex shrink-0 items-center gap-2 border-b border-line px-3 py-2 dark:border-line-dark"
            >
              <h3 class="min-w-0 flex-1 truncate text-sm font-semibold" :title="dir">
                {{ t('upload_title', { path: dir }) }}
              </h3>
              <button
                type="button"
                class="tree-tool"
                :title="t('common_close')"
                :aria-label="t('common_close')"
                @click="requestClose"
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
                  <path d="M6 6l12 12M18 6 6 18" />
                </svg>
              </button>
            </div>

            <div
              class="flex shrink-0 items-center gap-1 border-b border-line px-2 py-1.5 dark:border-line-dark"
            >
              <button
                v-for="item in tabs"
                :key="item.id"
                type="button"
                class="subtab"
                :class="tab === item.id ? 'subtab-active' : 'subtab-idle'"
                @click="tab = item.id"
              >
                {{ item.label }}
              </button>
            </div>

            <div class="thin-scroll min-h-0 flex-1 space-y-2 overflow-auto px-3 py-2">
              <p class="text-xs leading-relaxed text-ink-faint dark:text-ink-faint-dark">
                {{ tabHint }}
              </p>
              <!-- 目标目录单独再说一遍：标题长了会被截断，而「传到哪儿」是上传前必须看清的事 -->
              <p class="text-xs leading-relaxed break-all text-ink-soft dark:text-ink-soft-dark">
                {{ t('upload_target') }}：<span class="font-mono">{{ dir }}</span>
              </p>

              <!-- 本地文件 -->
              <template v-if="tab === 'local'">
                <input ref="localInput" type="file" multiple class="hidden" @change="onLocalPick" />
                <button
                  type="button"
                  class="g-btn-secondary h-9"
                  :disabled="uploading"
                  @click="localInput?.click()"
                >
                  {{ localFiles.length ? t('upload_pick_again') : t('upload_choose_files') }}
                </button>
                <p v-if="localFiles.length" class="text-xs text-ink-soft dark:text-ink-soft-dark">
                  {{ t('upload_ready', { n: localFiles.length }) }}
                </p>
                <ul
                  v-if="localFiles.length"
                  class="thin-scroll max-h-40 overflow-auto rounded-md border border-line p-1 text-xs dark:border-line-dark"
                >
                  <li v-for="file in localFiles" :key="file.name" class="px-1 py-0.5">
                    <span class="block truncate" :title="file.name">{{ file.name }}</span>
                  </li>
                </ul>
                <p v-else class="text-xs text-ink-faint dark:text-ink-faint-dark">
                  {{ t('upload_none') }}
                </p>
              </template>

              <!-- 本地目录 -->
              <template v-else-if="tab === 'dir'">
                <template v-if="dirSupported">
                  <input
                    ref="dirInput"
                    type="file"
                    webkitdirectory
                    multiple
                    class="hidden"
                    @change="onDirPick"
                  />
                  <button
                    type="button"
                    class="g-btn-secondary h-9"
                    :disabled="uploading"
                    @click="dirInput?.click()"
                  >
                    {{ dirFiles.length ? t('upload_pick_again') : t('upload_choose_dir') }}
                  </button>
                  <p v-if="dirFiles.length" class="text-xs text-ink-soft dark:text-ink-soft-dark">
                    {{ t('upload_selected_count', { n: dirFiles.length }) }}
                  </p>
                  <ul
                    v-if="dirFiles.length"
                    class="thin-scroll max-h-40 overflow-auto rounded-md border border-line p-1 text-xs dark:border-line-dark"
                  >
                    <li
                      v-for="file in dirFiles"
                      :key="file.webkitRelativePath || file.name"
                      class="px-1 py-0.5"
                    >
                      <span class="block truncate" :title="file.webkitRelativePath || file.name">
                        {{ file.webkitRelativePath || file.name }}
                      </span>
                    </li>
                  </ul>
                  <p v-else class="text-xs text-ink-faint dark:text-ink-faint-dark">
                    {{ t('upload_none') }}
                  </p>
                </template>
                <p v-else class="rounded-md bg-warning/10 px-3 py-2 text-xs text-warning">
                  {{ t('upload_no_local_files') }}
                </p>
              </template>

              <!-- 飞牛文件 -->
              <template v-else>
                <template v-if="hostAvailable">
                  <button
                    type="button"
                    class="g-btn-secondary h-9"
                    :disabled="uploading"
                    @click="pickNas"
                  >
                    {{ nasPaths.length ? t('upload_pick_again') : t('upload_choose_nas') }}
                  </button>
                  <ul
                    v-if="nasPaths.length"
                    class="thin-scroll max-h-40 overflow-auto rounded-md border border-line p-1 text-xs dark:border-line-dark"
                  >
                    <li v-for="path in nasPaths" :key="path" class="px-1 py-0.5">
                      <span class="block truncate" :title="path">{{ path }}</span>
                    </li>
                  </ul>
                  <p v-else class="text-xs text-ink-faint dark:text-ink-faint-dark">
                    {{ t('upload_none') }}
                  </p>
                </template>
                <!-- 宿主不可用：说明原因，而不是给一个点了就报错的按钮 -->
                <p v-else class="rounded-md bg-warning/10 px-3 py-2 text-xs text-warning">
                  {{ t('upload_nas_unavailable') }}
                </p>
              </template>

              <p
                v-if="error"
                class="rounded-md bg-danger/10 px-3 py-2 text-xs leading-relaxed break-all text-danger"
              >
                {{ t('upload_failed', { error }) }}
              </p>
            </div>

            <div
              v-if="uploading"
              class="flex shrink-0 items-center gap-2 border-t border-line px-3 py-2 dark:border-line-dark"
            >
              <div
                class="h-1.5 min-w-0 flex-1 overflow-hidden rounded-full bg-black/10 dark:bg-white/10"
              >
                <div
                  class="h-full rounded-full bg-brand transition-[width] duration-150"
                  :style="{ width: `${percent}%` }"
                />
              </div>
              <span class="shrink-0 text-[11px] text-ink-soft dark:text-ink-soft-dark">
                {{ t('upload_uploading', { percent }) }}
              </span>
              <!-- 飞牛分组没有取消：接口不带 AbortSignal，按了也停不下来 -->
              <button
                v-if="tab !== 'nas'"
                type="button"
                class="g-btn-ghost h-7 shrink-0 text-danger"
                @click="cancel"
              >
                {{ t('upload_cancel') }}
              </button>
            </div>

            <div
              class="flex shrink-0 items-center gap-2 border-t border-line px-3 py-2 dark:border-line-dark"
            >
              <label
                class="flex min-w-0 items-center gap-2 text-xs"
                :title="t('upload_overwrite_hint')"
              >
                <input
                  v-model="overwrite"
                  type="checkbox"
                  class="h-4 w-4 accent-brand"
                  :disabled="uploading"
                />
                <span class="min-w-0 truncate">
                  {{ t('upload_overwrite') }}
                  <span class="text-ink-faint dark:text-ink-faint-dark">
                    · {{ t('upload_overwrite_hint') }}
                  </span>
                </span>
              </label>
              <span class="flex-1" />
              <button
                type="button"
                class="g-btn-secondary h-9 px-3 text-sm"
                :disabled="uploading"
                @click="requestClose"
              >
                {{ t('common_cancel') }}
              </button>
              <button
                type="button"
                class="g-btn-primary h-9 px-3 text-sm"
                :disabled="uploading"
                @click="startUpload"
              >
                {{ t('upload_start') }}
              </button>
            </div>
          </div>
        </div>
      </div>
    </Transition>
  </Teleport>
</template>
