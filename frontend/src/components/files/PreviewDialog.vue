<script setup lang="ts">
// PreviewDialog — 文件预览：图片（/api/fs/image）与文本（/api/fs/read）。
//
// 模式按**扩展名**选（后端再按魔术字节确认内容）：
//   - 图片：/api/fs/image 回原始字节 → Blob URL 交给 <img>。上限 4 MiB，超过时后端回
//     too_large，界面**不显示**预览、只说清原因并给「下载」（用户要求）。
//   - 文本：/api/fs/read 回 UTF-8 内容，配行号（不换行时）、行数、编码。
//
// 文本绝不把二进制塞进 <pre> 显示成乱码——乱码会让人以为文件已经损坏，宁可说一句
// 「不是文本」再引导去下载。
//
// 第十六轮：文本预览成功之后，标题栏出现**编辑**按钮（emit `edit`，由调用方进入编辑页）。
// 图片没有编辑按钮——图片编辑器不在本仓库的范围内；能编辑的判据与文件管理器/目录树的
// 「编辑」菜单项共用 `fileKind.ts`（按扩展名算文本），真正的结论由后端给（没写权限回 permission）。
import { computed, onBeforeUnmount, ref, watch } from 'vue'
import { t } from '@/i18n'
import { ApiError, api, describeError, fetchImageBlob } from '@/serverapi'
import { useSftpStore } from '@/stores/sftp'
import { useToastStore } from '@/stores/toast'
import { baseName, humanSize } from '@/utils/format'
import { previewKindOf } from './fileKind'

const props = defineProps<{ visible: boolean; sid: string; path: string; size?: number }>()
const emit = defineEmits<{
  'update:visible': [v: boolean]
  /** 用户点了「编辑」：由调用方决定怎么进编辑页（状态在 stores/fileEditor.ts 里） */
  edit: [path: string]
}>()

const sftp = useSftpStore()
const toastStore = useToastStore()

/** 图片预览上限，**与后端 maxImageBytes 一致**；只用于文案里的数字。 */
const IMAGE_MAX_BYTES = 4 << 20
/** 行号最多给这么多行：再多就是几十万个字符的 gutter，纯属给浏览器添堵（内容照常显示）。 */
const MAX_GUTTER_LINES = 20000

/** 预览形态按扩展名选（后端再按内容确认），判据与文件管理器/目录树的「编辑」共用一份。 */
const kind = computed<'image' | 'text'>(() => previewKindOf(props.path))

// ---- 文本态 ----
const loading = ref(false)
const error = ref('')
const content = ref('')
const size = ref(0)
const tooLarge = ref(false)
const binary = ref(false)
const encoding = ref('')
/**
 * 自动换行：默认**关**（代码/配置文件按行读，行号与缩进才对得上），用户可按需打开。
 *
 * 刻意**不随文件重置**：这是用户的一次性偏好，看第二个文件时还要再点一遍开关很烦。
 * 弹窗本身常驻挂载（FileManager 里只有内容按 v-if 切），所以这个偏好活在本次会话里。
 */
const wrap = ref(false)

// ---- 图片态 ----
const imgUrl = ref('')
const imgLoading = ref(false)
const imgTooLarge = ref(false)
const imgInvalid = ref(false)
/** 浏览器自己解不开（格式不认识）：后端只按魔术字节放行，能不能渲染得看浏览器。 */
const imgBroken = ref(false)
const imgError = ref('')
const imgDims = ref('')

const downloading = ref(false)

/**
 * 一次加载的「代号」与它对应的取消句柄。
 *
 * 为什么必须有：请求是异步的，而弹窗可以被关掉、也可以换一个文件再打开。没有作废机制时，
 * 迟到的响应会写进**新文件**的状态里 —— 标题是新文件名、图却是上一个文件的，`size` 也是错的；
 * 更糟的是那个 Blob URL 只挂在组件上一次的 `imgUrl` 上，直到下次 reset 才被 revoke（≤4 MiB
 * 常驻）。批量切换文件时这是必然发生的，不是理论问题。
 *
 * 两个手段一起用：
 *  - `seq`：响应回来时比对「还是不是我这一次」，不是就整个丢弃（不写任何状态）；
 *  - `AbortController`：把还在飞的请求真的取消掉（4 MiB 的图片不必白传完；后端也会因为
 *    ctx 结束而立刻收手——对 FIFO 那类会让 SFTP 卡住的路径尤其重要）。
 * 刻意**不**放进响应式状态：它只用于控制流，写进 ref 只会让每次加载都触发一次无谓渲染。
 */
let seq = 0
let controller: AbortController | null = null

function revokeImage() {
  if (imgUrl.value) {
    URL.revokeObjectURL(imgUrl.value)
    imgUrl.value = ''
  }
}

/** 作废「当前这一次加载」：取消在飞请求 + 让迟到的响应认不出自己。 */
function invalidate() {
  seq += 1
  controller?.abort()
  controller = null
}

function reset() {
  invalidate()
  loading.value = false
  error.value = ''
  content.value = ''
  size.value = 0
  tooLarge.value = false
  binary.value = false
  encoding.value = ''
  imgLoading.value = false
  imgTooLarge.value = false
  imgInvalid.value = false
  imgBroken.value = false
  imgError.value = ''
  imgDims.value = ''
  revokeImage()
}

async function loadText(my: number, signal: AbortSignal) {
  loading.value = true
  try {
    const res = await api.fsRead(props.sid, props.path, signal)
    if (my !== seq) return // 已经不是这一次了：一个字段都不要写
    size.value = res.size
    tooLarge.value = res.tooLarge
    binary.value = !res.text
    encoding.value = res.encoding || ''
    content.value = res.text ? res.content : ''
  } catch (e) {
    if (my !== seq) return
    error.value = describeError(e)
  } finally {
    if (my === seq) loading.value = false
  }
}

async function loadImage(my: number, signal: AbortSignal) {
  imgLoading.value = true
  try {
    const blob = await fetchImageBlob(props.sid, props.path, signal)
    if (my !== seq) return
    size.value = blob.size
    imgUrl.value = URL.createObjectURL(blob)
  } catch (e) {
    if (my !== seq) return
    if (e instanceof ApiError && e.code === 'too_large') imgTooLarge.value = true
    else if (e instanceof ApiError && e.code === 'invalid') imgInvalid.value = true
    else imgError.value = describeError(e)
  } finally {
    if (my === seq) imgLoading.value = false
  }
}

function load() {
  reset()
  const my = seq
  controller = new AbortController()
  if (kind.value === 'image') void loadImage(my, controller.signal)
  else void loadText(my, controller.signal)
}

// 打开（或换文件）就重新读一次：不做内容缓存——远程文件随时可能被改，缓存只会骗自己。
// 关闭时也 reset：把 Blob URL 释放掉（图片可能有几 MB，留在内存里没道理）。
watch(
  () => [props.visible, props.sid, props.path],
  () => {
    if (props.visible) load()
    else reset()
  },
  { immediate: true }
)

// 卸载时也要收尾：取消在飞请求（否则一个 4 MiB 的图片还会白传完）+ 释放 Blob URL。
onBeforeUnmount(() => {
  invalidate()
  revokeImage()
})

// ---- 文本展示 ----

const lines = computed(() => (content.value === '' ? 0 : content.value.split('\n').length))
/** 行号只在「不换行」时给：换行会让一行占多行，行号与内容对不上，宁可不显示。 */
const showGutter = computed(() => !wrap.value && lines.value > 0 && lines.value <= MAX_GUTTER_LINES)
const lineNumbers = computed(() => {
  if (!showGutter.value) return ''
  const parts: string[] = []
  for (let i = 1; i <= lines.value; i++) parts.push(String(i))
  return parts.join('\n')
})

/** 标题右边那行元信息：体积 + 行数 + 编码（只在不是 UTF-8 时点出来）。 */
const knownSize = computed(() => size.value || props.size || 0)

const meta = computed(() => {
  const parts: string[] = []
  if (kind.value === 'image') {
    if (knownSize.value > 0) parts.push(humanSize(knownSize.value))
    if (imgDims.value) parts.push(imgDims.value)
  } else if (!tooLarge.value && !binary.value) {
    if (size.value > 0) parts.push(humanSize(size.value))
    if (lines.value > 0) parts.push(t('files_preview_lines', { n: lines.value }))
    if (encoding.value && encoding.value !== 'utf-8') parts.push(encoding.value.toUpperCase())
  } else if (size.value > 0) {
    parts.push(humanSize(size.value))
  }
  return parts.join(' · ')
})

/** 图片超限的文案：优先用列表里已知的体积（后端也拦，这里只是为了把话说清楚）。 */
const imgTooLargeText = computed(() =>
  props.size && props.size > 0
    ? t('files_preview_image_too_large', {
        size: humanSize(props.size),
        limit: humanSize(IMAGE_MAX_BYTES)
      })
    : t('files_preview_image_too_large_unknown', { limit: humanSize(IMAGE_MAX_BYTES) })
)

function onImageLoad(e: Event) {
  const img = e.target as HTMLImageElement
  if (img.naturalWidth > 0) imgDims.value = `${img.naturalWidth}×${img.naturalHeight}`
}

async function download() {
  if (downloading.value) return
  downloading.value = true
  try {
    await sftp.download(props.sid, props.path)
    toastStore.show(t('files_download_started', { name: baseName(props.path) }), 'success')
  } catch (e) {
    toastStore.show(t('files_download_failed', { error: describeError(e) }), 'error')
  } finally {
    downloading.value = false
  }
}

function close() {
  emit('update:visible', false)
}
</script>

<template>
  <!-- 与 shell 的弹窗同一套外壳：Teleport 到 body + 铺满全屏的遮罩 + .region-center 定位带 -->
  <Teleport to="body">
    <Transition name="modal-fade">
      <div v-if="visible" class="fixed inset-0 z-50">
        <div class="absolute inset-0 bg-black/50" @click="close" />
        <div class="region-center absolute right-0 left-0 flex items-center justify-center px-4">
          <div
            class="pointer-events-auto flex max-h-[90%] w-full flex-col overflow-hidden rounded-xl border border-line bg-surface shadow-pop dark:border-line-dark dark:bg-surface-dark"
            :class="kind === 'text' ? 'max-w-4xl' : 'max-w-3xl'"
          >
            <div
              class="flex shrink-0 items-center gap-2 border-b border-line px-3 py-2 dark:border-line-dark"
            >
              <h3 class="min-w-0 flex-1 truncate text-sm font-semibold" :title="path">
                {{ t('files_preview_title', { name: baseName(path) }) }}
              </h3>
              <span
                v-if="meta"
                class="shrink-0 font-mono text-xs text-ink-faint dark:text-ink-faint-dark"
              >
                {{ meta }}
              </span>
              <!-- 编辑（第十六轮）：只在**文本读出来了**之后出现——图片没有编辑按钮，
                   不是文本 / 太大 / 读失败时也不该给一个点下去只会报错的入口。
                   权限检查在调用方那一侧（/api/fs/edit-load 先验证登录用户可写，
                   没权限直接报错、不进编辑页，见契约 26.2）。 -->
              <button
                v-if="kind === 'text' && !loading && !error && !tooLarge && !binary"
                type="button"
                class="tree-tool"
                :title="t('files_preview_edit')"
                :aria-label="t('files_preview_edit')"
                @click="emit('edit', path)"
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
                  <path d="M12 20h9" />
                  <path d="M16.5 3.5a2.12 2.12 0 0 1 3 3L7 19l-4 1 1-4z" />
                </svg>
              </button>
              <!-- 换行开关（只对文本有意义）：关 = 不换行 + 显示行号，开 = 长行折回来。
                   按钮名字说的是「这个开关管什么」，开/关由 aria-pressed 表达。 -->
              <button
                v-if="kind === 'text' && !loading && !error && !tooLarge && !binary"
                type="button"
                class="tree-tool"
                :class="wrap ? 'bg-black/5 text-ink dark:bg-white/5 dark:text-white' : ''"
                :title="t('files_preview_wrap')"
                :aria-label="t('files_preview_wrap')"
                :aria-pressed="wrap"
                @click="wrap = !wrap"
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
                  <path d="M3 6h18" />
                  <path d="M3 12h12a3 3 0 0 1 3 3v1" />
                  <path d="m15 15 3 3 3-3" />
                  <path d="M3 18h6" />
                </svg>
              </button>
              <button
                type="button"
                class="tree-tool"
                :title="t('files_download')"
                :aria-label="t('files_download')"
                :disabled="downloading"
                @click="download"
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
                  <path d="M12 3v12" />
                  <path d="m7 10 5 5 5-5" />
                  <path d="M5 21h14" />
                </svg>
              </button>
              <button
                type="button"
                class="tree-tool"
                :title="t('common_close')"
                :aria-label="t('common_close')"
                @click="close"
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

            <!-- 图片：居中铺在正文区，超出可用高度就按比例缩到放得下 -->
            <div v-if="kind === 'image'" class="min-h-0 flex-1 overflow-auto p-3">
              <p v-if="imgLoading" class="py-4 text-center text-sm text-ink-faint">
                {{ t('common_loading') }}
              </p>

              <div v-else-if="imgError" class="py-2 text-sm">
                <p class="leading-relaxed break-all text-danger">
                  {{ t('files_preview_image_failed', { error: imgError }) }}
                </p>
                <button type="button" class="g-btn-ghost mt-1 h-8 text-danger" @click="load">
                  {{ t('common_retry') }}
                </button>
              </div>

              <!-- 超限 / 不是图片 / 浏览器解不开：都说清原因 + 让用户下载自己看 -->
              <div v-else-if="imgTooLarge || imgInvalid || imgBroken" class="space-y-2 py-2 text-sm">
                <p class="text-ink-soft dark:text-ink-soft-dark">
                  {{
                    imgTooLarge
                      ? imgTooLargeText
                      : imgInvalid
                        ? t('files_preview_image_invalid')
                        : t('files_preview_image_unsupported')
                  }}
                  <!-- 超限那句里已经带了体积，别再缀一遍 -->
                  <span v-if="!imgTooLarge && knownSize > 0">· {{ humanSize(knownSize) }}</span>
                </p>
                <button
                  type="button"
                  class="g-btn-secondary h-9"
                  :disabled="downloading"
                  @click="download"
                >
                  {{ t('files_download') }}
                </button>
              </div>

              <div v-else class="flex justify-center">
                <img
                  v-if="imgUrl"
                  :src="imgUrl"
                  :alt="baseName(path)"
                  class="max-h-[70vh] max-w-full object-contain"
                  @load="onImageLoad"
                  @error="imgBroken = true"
                />
              </div>
            </div>

            <!-- 文本 -->
            <div v-else class="thin-scroll min-h-0 flex-1 overflow-auto px-3 py-2">
              <p v-if="loading" class="py-4 text-center text-sm text-ink-faint">
                {{ t('common_loading') }}
              </p>

              <div v-else-if="error" class="py-2 text-sm">
                <p class="leading-relaxed break-all text-danger">
                  {{ t('files_preview_failed', { error }) }}
                </p>
                <button type="button" class="g-btn-ghost mt-1 h-8 text-danger" @click="load">
                  {{ t('common_retry') }}
                </button>
              </div>

              <!-- 太大 / 不是文本：说明原因 + 让用户下载自己看 -->
              <div v-else-if="tooLarge || binary" class="space-y-2 py-2 text-sm">
                <p class="text-ink-soft dark:text-ink-soft-dark">
                  {{ tooLarge ? t('err_too_large') : t('files_preview_binary') }}
                  <span v-if="size > 0">· {{ humanSize(size) }}</span>
                </p>
                <button
                  type="button"
                  class="g-btn-secondary h-9"
                  :disabled="downloading"
                  @click="download"
                >
                  {{ t('files_download') }}
                </button>
              </div>

              <p v-else-if="content === ''" class="py-2 text-sm text-ink-faint">
                {{ t('files_preview_empty') }}
              </p>

              <!-- 行号列 sticky 在左边：不换行时正文会横向滚动，行号得跟着钉住才不会滑走 -->
              <div v-else class="flex min-w-min items-start">
                <pre
                  v-if="showGutter"
                  class="sticky left-0 z-10 shrink-0 select-none border-r border-line bg-surface px-2 text-right font-mono text-xs leading-relaxed text-ink-faint dark:border-line-dark dark:bg-surface-dark dark:text-ink-faint-dark"
                  >{{ lineNumbers }}</pre
                >
                <pre
                  class="flex-1 px-2 font-mono text-xs leading-relaxed text-ink dark:text-ink-dark"
                  :class="wrap ? 'whitespace-pre-wrap break-all' : 'whitespace-pre'"
                  >{{ content }}</pre
                >
              </div>
            </div>
          </div>
        </div>
      </div>
    </Transition>
  </Teleport>
</template>
