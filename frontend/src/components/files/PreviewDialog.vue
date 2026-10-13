<script setup lang="ts">
// PreviewDialog — 文本预览（用 /api/fs/read）。
//
// 只预览文本：后端会判定内容是不是文本、超过上限时回 tooLarge。两种情况都给一句说明 +
// 「下载」按钮，绝不把二进制塞进 <pre> 里显示成乱码——乱码会让人以为文件已经损坏。
import { ref, watch } from 'vue'
import { t } from '@/i18n'
import { api, describeError } from '@/serverapi'
import { useSftpStore } from '@/stores/sftp'
import { useToastStore } from '@/stores/toast'
import { baseName, humanSize } from '@/utils/format'

const props = defineProps<{ visible: boolean; sid: string; path: string }>()
const emit = defineEmits<{ 'update:visible': [v: boolean] }>()

const sftp = useSftpStore()
const toastStore = useToastStore()

const loading = ref(false)
const error = ref('')
const content = ref('')
const size = ref(0)
const tooLarge = ref(false)
const binary = ref(false)
const downloading = ref(false)

async function load() {
  loading.value = true
  error.value = ''
  content.value = ''
  size.value = 0
  tooLarge.value = false
  binary.value = false
  try {
    const res = await api.fsRead(props.sid, props.path)
    size.value = res.size
    tooLarge.value = res.tooLarge
    binary.value = !res.text
    content.value = res.text ? res.content : ''
  } catch (e) {
    error.value = describeError(e)
  } finally {
    loading.value = false
  }
}

// 打开（或换文件）就重新读一次：不做内容缓存——远程文件随时可能被改，缓存只会骗自己
watch(
  () => [props.visible, props.sid, props.path],
  () => {
    if (props.visible) void load()
  },
  { immediate: true }
)

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
            class="pointer-events-auto flex max-h-[90%] w-full max-w-3xl flex-col overflow-hidden rounded-xl border border-line bg-surface shadow-pop dark:border-line-dark dark:bg-surface-dark"
          >
            <div
              class="flex shrink-0 items-center gap-2 border-b border-line px-3 py-2 dark:border-line-dark"
            >
              <h3 class="min-w-0 flex-1 truncate text-sm font-semibold" :title="path">
                {{ t('files_preview_title', { name: baseName(path) }) }}
              </h3>
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

            <div class="thin-scroll min-h-0 flex-1 overflow-auto px-3 py-2">
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

              <pre
                v-else
                class="font-mono text-xs leading-relaxed break-all whitespace-pre-wrap text-ink dark:text-ink-dark"
              >{{ content }}</pre>
            </div>
          </div>
        </div>
      </div>
    </Transition>
  </Teleport>
</template>
