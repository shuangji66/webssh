<script setup lang="ts">
// InfoDialog — 「信息」弹窗（文件管理器的行菜单/空白处菜单都用它）。
//
// 数据来自 /api/fs/info（契约 15.5）：条目自身属性 + 目录**直接子项**的计数。刻意只数一层
// （用户确认过）：递归统计在大目录上要跑很久，而这个弹窗是「点开就想看一眼」的东西。
//
// 三条如实原则：
//  1. 拿不到就说拿不到：权限不足导致数不出子项时显示后端给的原因，**不显示 0**——
//     「0 个目录、0 个文件」会被读成「这是个空目录」，那是另一回事；
//  2. 大小是**条目自身**的大小（目录就是它自己的 inode 大小，通常 4 KB），所以文案里写清
//     「不含子目录内容」，否则用户会以为这个目录只有 4 KB；
//  3. 权限两种写法都显示（符号 + 八进制）——八进制是 `modeToOctal` 从符号串换算的，
//     认不出来就只显示符号串，不编造一个 0xxx；
//  4. 拥有者同理：远端查到名字就显示 `名字（uid:gid）`，查不到只显示 `uid:gid`。
import { computed, ref, watch } from 'vue'
import { t } from '@/i18n'
import { api, describeError, type FsInfo } from '@/serverapi'
import { formatTime, humanSize, modeToOctal, permText } from '@/utils/format'

const props = defineProps<{ visible: boolean; sid: string; path: string }>()
const emit = defineEmits<{ 'update:visible': [v: boolean] }>()

const loading = ref(false)
const error = ref('')
const info = ref<FsInfo | null>(null)

async function load() {
  loading.value = true
  error.value = ''
  info.value = null
  try {
    const res = await api.fsInfo(props.sid, props.path)
    info.value = res.info
  } catch (e) {
    error.value = describeError(e)
  } finally {
    loading.value = false
  }
}

// 打开（或换目标）就重新读一次：不做缓存——远程文件随时可能被改，缓存只会骗自己
watch(
  () => [props.visible, props.sid, props.path],
  () => {
    if (props.visible && props.path) void load()
  },
  { immediate: true }
)

function close() {
  emit('update:visible', false)
}

/** 类型：目录 / 文件 / 符号链接（→ 目标）/ 失效链接。 */
const typeText = computed(() => {
  const it = info.value
  if (!it) return ''
  if (it.symlink) {
    return it.broken
      ? t('files_broken_symlink')
      : t('files_info_symlink_to', { target: it.symlink })
  }
  return it.dir ? t('files_dir') : t('files_file')
})

/** 权限：符号（-rw-r--r--）与八进制（0644）两种写法都给，八进制换算不出来就只给符号。 */
const modeText = computed(() => {
  const it = info.value
  if (!it) return ''
  const octal = modeToOctal(it.mode)
  const symbolic = permText(it.mode)
  return octal ? `${symbolic}（${octal}）` : symbolic
})

/**
 * 拥有者：优先显示远端查出来的名字，拿不到就退回数字 uid:gid。
 *
 * 两者都有时写成 `demo:demo（1000:1000）`——名字好认、数字能直接对着 `ls -n` 或 chown 用。
 */
const ownerText = computed(() => {
  const it = info.value
  if (!it) return ''
  const num = `${it.uid}:${it.gid}`
  const names = it.owner && it.group ? `${it.owner}:${it.group}` : (it.owner ?? it.group ?? '')
  return names ? `${names}（${num}）` : num
})

/** 内含：目录才有意义；数不出来时给原因，不给 0。 */
const contentsText = computed(() => {
  const it = info.value
  if (!it?.dir) return ''
  if (it.countError) return t('files_info_contents_failed', { error: it.countError })
  return t('files_info_contents_value', { dirs: it.dirCount, files: it.fileCount })
})
</script>

<template>
  <Teleport to="body">
    <Transition name="modal-fade">
      <div v-if="visible" class="fixed inset-0 z-50">
        <div class="absolute inset-0 bg-black/50" @click="close"></div>
        <div class="region-center absolute left-0 right-0 flex items-center justify-center px-4">
          <div
            class="pointer-events-auto relative max-h-[90%] w-full max-w-md overflow-y-auto rounded-xl border border-line bg-surface p-5 shadow-pop dark:border-line-dark dark:bg-surface-dark"
          >
            <div class="flex items-start justify-between gap-3">
              <h3 class="min-w-0 break-all font-display text-base font-semibold text-ink dark:text-ink-dark">
                {{ t('files_info') }}
              </h3>
              <button class="g-btn-ghost shrink-0 !h-8 !px-2 text-lg leading-none" @click="close">×</button>
            </div>

            <p v-if="loading" class="mt-4 text-xs text-ink-soft dark:text-ink-soft-dark">
              {{ t('files_info_loading') }}
            </p>

            <p v-else-if="error" class="mt-4 break-all text-xs text-danger">
              {{ t('files_info_failed', { error }) }}
            </p>

            <dl
              v-else-if="info"
              class="mt-4 grid grid-cols-[4.5rem_1fr] gap-x-3 gap-y-2 text-xs"
            >
              <dt class="text-ink-faint dark:text-ink-faint-dark">{{ t('files_info_type') }}</dt>
              <dd class="break-all text-ink dark:text-ink-dark">{{ typeText }}</dd>

              <dt class="text-ink-faint dark:text-ink-faint-dark">{{ t('files_name') }}</dt>
              <dd class="break-all text-ink dark:text-ink-dark">{{ info.name }}</dd>

              <dt class="text-ink-faint dark:text-ink-faint-dark">{{ t('files_info_path') }}</dt>
              <dd class="break-all font-mono text-ink dark:text-ink-dark">{{ info.path }}</dd>

              <dt class="text-ink-faint dark:text-ink-faint-dark">{{ t('files_size') }}</dt>
              <dd class="text-ink dark:text-ink-dark">
                {{ humanSize(info.size) }}
                <span class="text-ink-faint dark:text-ink-faint-dark">
                  {{ t('files_info_size_hint') }}
                </span>
              </dd>

              <dt class="text-ink-faint dark:text-ink-faint-dark">{{ t('files_modified') }}</dt>
              <dd class="text-ink dark:text-ink-dark">{{ formatTime(info.mtime) }}</dd>

              <dt class="text-ink-faint dark:text-ink-faint-dark">{{ t('files_mode') }}</dt>
              <dd class="break-all font-mono text-ink dark:text-ink-dark">{{ modeText }}</dd>

              <dt class="text-ink-faint dark:text-ink-faint-dark">{{ t('files_info_owner') }}</dt>
              <dd class="break-all font-mono text-ink dark:text-ink-dark">{{ ownerText }}</dd>

              <template v-if="info.dir">
                <dt class="text-ink-faint dark:text-ink-faint-dark">{{ t('files_info_contents') }}</dt>
                <dd class="break-all text-ink dark:text-ink-dark">{{ contentsText }}</dd>
              </template>
            </dl>

            <div class="mt-5 flex justify-end">
              <button class="g-btn-secondary !h-9 !px-4 text-sm" @click="close">
                {{ t('common_close') }}
              </button>
            </div>
          </div>
        </div>
      </div>
    </Transition>
  </Teleport>
</template>
