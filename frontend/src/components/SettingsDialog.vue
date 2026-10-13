<script setup lang="ts">
// SettingsDialog — 设置：主题 / 语言 / 终端字体与字号 / 已信任主机 / 数据目录 / 飞牛状态 / 关于。
//
// 主题与语言是**全局立刻生效**的（写 <html> 的 class 与 i18n 的 locale），所以不设「保存」按钮：
// 这里的每一项点下去就是结果。字体字号同理（TerminalPane 监听 settings store 即时重排）。
import { computed, ref, watch } from 'vue'
import { t, useI18n } from '@/i18n'
import { api, describeError, type HostKeyRecord, type TrimStatus } from '@/serverapi'
import { useTheme, type ThemeMode } from '@/composables/useTheme'
import { useHomeStore } from '@/stores/home'
import { FONT_SIZE_MAX, FONT_SIZE_MIN, useSettingsStore, type TerminalFont } from '@/stores/settings'
import { useToastStore } from '@/stores/toast'
import ConfirmDialog from './ConfirmDialog.vue'

const props = defineProps<{ visible: boolean }>()

const emit = defineEmits<{
  (e: 'update:visible', v: boolean): void
}>()

const settings = useSettingsStore()
const home = useHomeStore()
const toast = useToastStore()
const { themeMode, setTheme } = useTheme()
const { locale, setLocale } = useI18n()

const open = ref(props.visible)

const themeOptions: { value: ThemeMode; key: string }[] = [
  { value: 'light', key: 'settings_theme_light' },
  { value: 'dark', key: 'settings_theme_dark' },
  { value: 'system', key: 'settings_theme_system' }
]

const fontOptions: { value: TerminalFont; key: string }[] = [
  { value: 'maple', key: 'settings_font_maple' },
  { value: 'system', key: 'settings_font_system' }
]

// ---------- 已信任主机 ----------

const hosts = ref<HostKeyRecord[]>([])
const hostsLoading = ref(false)
const hostsError = ref('')
const pendingForget = ref<HostKeyRecord | null>(null)

async function loadHosts() {
  hostsLoading.value = true
  hostsError.value = ''
  try {
    const res = await api.hostKeys()
    hosts.value = res.hosts ?? []
  } catch (e) {
    // 读不到要说出来：显示成「还没有信任任何主机」会让用户以为记录被清了
    hostsError.value = describeError(e)
  } finally {
    hostsLoading.value = false
  }
}

function hostLabel(record: HostKeyRecord): string {
  return record.port && record.port !== 22 ? `${record.host}:${record.port}` : record.host
}

async function confirmForget() {
  const record = pendingForget.value
  pendingForget.value = null
  if (!record) return
  try {
    await api.forgetHostKey(record.host, record.port)
    toast.show(t('settings_known_hosts_removed'), 'success')
    await loadHosts()
  } catch (e) {
    toast.show(t('settings_known_hosts_remove_failed', { error: describeError(e) }), 'error')
  }
}

// ---------- 飞牛状态 ----------

const trim = ref<TrimStatus | null>(null)
/** /api/trim/status 失败时的原因（与「飞牛不可用」区分开：后者是 available:false）。 */
const trimError = ref('')

async function loadTrim() {
  trimError.value = ''
  try {
    const res = await api.trimStatus()
    trim.value = res.trim
  } catch (e) {
    trim.value = null
    trimError.value = describeError(e)
  }
}

const dataDir = computed(() => home.runtime?.dataDir || t('common_none'))

watch(
  () => props.visible,
  (v) => {
    open.value = v
    if (!v) return
    // 每次打开都重新读一遍：主机密钥可能在别的页面/设备上刚被信任过，飞牛状态也会变
    void loadHosts()
    void loadTrim()
    if (!home.runtime) void home.loadInfo().catch(() => undefined)
  },
  { immediate: true }
)

function close() {
  open.value = false
  emit('update:visible', false)
}
</script>

<template>
  <Teleport to="body">
    <Transition name="modal-fade">
      <div v-if="open" class="fixed inset-0 z-50">
        <div class="absolute inset-0 bg-black/50" @click="close"></div>
        <div class="region-center absolute left-0 right-0 flex items-center justify-center px-4">
          <div
            class="pointer-events-auto relative max-h-[90%] w-full max-w-lg overflow-y-auto rounded-xl border border-line bg-surface p-5 shadow-pop dark:border-line-dark dark:bg-surface-dark"
          >
            <div class="flex items-center justify-between">
              <h3 class="font-display text-base font-semibold text-ink dark:text-ink-dark">
                {{ t('settings_title') }}
              </h3>
              <button class="g-btn-ghost !h-8 !px-2 text-lg leading-none" @click="close">×</button>
            </div>

            <!-- 主题 -->
            <div class="mt-4">
              <label class="mb-1.5 block text-xs font-medium text-ink-soft dark:text-ink-soft-dark">
                {{ t('settings_theme') }}
              </label>
              <div class="grid grid-cols-3 gap-2">
                <button
                  v-for="opt in themeOptions"
                  :key="opt.value"
                  class="rounded-md border px-3 py-2 text-sm font-medium transition-colors"
                  :class="
                    themeMode === opt.value
                      ? 'border-brand bg-brand/10 text-brand'
                      : 'border-line text-ink hover:border-brand dark:border-line-dark dark:text-ink-dark'
                  "
                  @click="setTheme(opt.value)"
                >
                  {{ t(opt.key) }}
                </button>
              </div>
            </div>

            <!-- 语言 -->
            <div class="mt-4">
              <label class="mb-1.5 block text-xs font-medium text-ink-soft dark:text-ink-soft-dark">
                {{ t('settings_lang') }}
              </label>
              <div class="grid grid-cols-2 gap-2">
                <button
                  class="rounded-md border px-3 py-2 text-sm font-medium transition-colors"
                  :class="
                    locale === 'zh'
                      ? 'border-brand bg-brand/10 text-brand'
                      : 'border-line text-ink hover:border-brand dark:border-line-dark dark:text-ink-dark'
                  "
                  @click="setLocale('zh')"
                >
                  {{ t('settings_lang_zh') }}
                </button>
                <button
                  class="rounded-md border px-3 py-2 text-sm font-medium transition-colors"
                  :class="
                    locale === 'en'
                      ? 'border-brand bg-brand/10 text-brand'
                      : 'border-line text-ink hover:border-brand dark:border-line-dark dark:text-ink-dark'
                  "
                  @click="setLocale('en')"
                >
                  {{ t('settings_lang_en') }}
                </button>
              </div>
            </div>

            <!-- 终端字体 -->
            <div class="mt-4">
              <label class="mb-1.5 block text-xs font-medium text-ink-soft dark:text-ink-soft-dark">
                {{ t('settings_font_family') }}
              </label>
              <div class="grid grid-cols-2 gap-2">
                <button
                  v-for="opt in fontOptions"
                  :key="opt.value"
                  class="rounded-md border px-3 py-2 text-sm font-medium transition-colors"
                  :class="
                    settings.terminalFont === opt.value
                      ? 'border-brand bg-brand/10 text-brand'
                      : 'border-line text-ink hover:border-brand dark:border-line-dark dark:text-ink-dark'
                  "
                  @click="settings.setTerminalFont(opt.value)"
                >
                  {{ t(opt.key) }}
                </button>
              </div>
            </div>

            <!-- 终端字号：− / 数字 / +（10–26，越界由 store 钳制） -->
            <div class="mt-4">
              <label class="mb-1.5 block text-xs font-medium text-ink-soft dark:text-ink-soft-dark">
                {{ t('settings_font_size') }}
              </label>
              <div class="flex items-center gap-2">
                <div class="flex items-center overflow-hidden rounded-md border border-line dark:border-line-dark">
                  <button
                    class="flex h-9 w-9 items-center justify-center text-lg text-ink-soft transition-colors hover:bg-black/5 disabled:opacity-40 dark:text-ink-soft-dark dark:hover:bg-white/5"
                    :disabled="settings.fontSize <= FONT_SIZE_MIN"
                    @click="settings.setFontSize(settings.fontSize - 1)"
                  >
                    −
                  </button>
                  <span
                    class="flex h-9 w-12 items-center justify-center border-x border-line font-mono text-sm text-ink dark:border-line-dark dark:text-ink-dark"
                  >
                    {{ settings.fontSize }}
                  </span>
                  <button
                    class="flex h-9 w-9 items-center justify-center text-lg text-ink-soft transition-colors hover:bg-black/5 disabled:opacity-40 dark:text-ink-soft-dark dark:hover:bg-white/5"
                    :disabled="settings.fontSize >= FONT_SIZE_MAX"
                    @click="settings.setFontSize(settings.fontSize + 1)"
                  >
                    +
                  </button>
                </div>
                <span class="text-xs text-ink-faint dark:text-ink-faint-dark">
                  {{ t('settings_font_size_hint') }}
                </span>
              </div>
            </div>

            <!-- 已信任的主机密钥 -->
            <div class="mt-4 border-t border-line pt-4 dark:border-line-dark">
              <label class="mb-1.5 block text-xs font-medium text-ink-soft dark:text-ink-soft-dark">
                {{ t('settings_known_hosts') }}
              </label>
              <p v-if="hostsLoading" class="text-xs text-ink-soft dark:text-ink-soft-dark">
                {{ t('common_loading') }}
              </p>
              <p v-else-if="hostsError" class="break-all text-xs text-danger">
                {{ t('err_unknown', { error: hostsError }) }}
              </p>
              <p
                v-else-if="!hosts.length"
                class="text-xs text-ink-soft dark:text-ink-soft-dark"
              >
                {{ t('settings_known_hosts_empty') }}
              </p>
              <ul v-else class="divide-y divide-line dark:divide-line-dark">
                <li
                  v-for="record in hosts"
                  :key="`${record.host}:${record.port}`"
                  class="flex items-center gap-3 py-2"
                >
                  <div class="min-w-0 flex-1">
                    <p class="truncate font-mono text-xs text-ink dark:text-ink-dark">
                      {{ hostLabel(record) }}
                      <span class="text-ink-faint dark:text-ink-faint-dark">{{ record.keyType }}</span>
                    </p>
                    <p class="truncate font-mono text-[11px] text-ink-faint dark:text-ink-faint-dark">
                      {{ record.fingerprint }}
                    </p>
                  </div>
                  <button
                    class="g-btn-ghost !h-8 !px-2 shrink-0 !text-danger hover:!bg-danger/10"
                    :title="t('settings_known_hosts_remove')"
                    @click="pendingForget = record"
                  >
                    <svg
                      viewBox="0 0 24 24"
                      fill="none"
                      stroke="currentColor"
                      stroke-width="1.5"
                      stroke-linecap="round"
                      stroke-linejoin="round"
                      class="h-4 w-4"
                    >
                      <path d="M3 6h18" />
                      <path d="M19 6l-1 14a2 2 0 0 1-2 2H8a2 2 0 0 1-2-2L5 6" />
                      <path d="M10 11v6M14 11v6" />
                      <path d="M9 6V4a1 1 0 0 1 1-1h4a1 1 0 0 1 1 1v2" />
                    </svg>
                  </button>
                </li>
              </ul>
            </div>

            <!-- 数据目录 / 飞牛状态：部署信息的唯一入口（用户排查问题时要看） -->
            <div class="mt-4 border-t border-line pt-4 dark:border-line-dark">
              <label class="mb-1.5 block text-xs font-medium text-ink-soft dark:text-ink-soft-dark">
                {{ t('settings_data_dir') }}
              </label>
              <p class="term-container break-all font-mono text-xs text-ink dark:text-ink-dark">
                {{ dataDir }}
              </p>

              <p class="mt-3 text-xs">
                <span v-if="trimError" class="break-all text-danger">
                  {{ t('err_unknown', { error: trimError }) }}
                </span>
                <span v-else-if="trim?.available" class="text-success">{{ t('settings_trim_ok') }}</span>
                <span v-else-if="trim" class="text-warning">
                  {{ t('settings_trim_unavailable', { message: trim.message }) }}
                </span>
                <span v-else class="text-ink-soft dark:text-ink-soft-dark">{{ t('common_loading') }}</span>
              </p>
            </div>

            <!-- 关于 -->
            <div class="mt-4 border-t border-line pt-4 dark:border-line-dark">
              <div class="flex items-center gap-2">
                <h3 class="font-display text-base font-semibold text-ink dark:text-ink-dark">
                  {{ t('about_title') }}
                </h3>
                <span v-if="home.runtime?.version" class="font-mono text-xs text-ink-faint dark:text-ink-faint-dark">
                  v{{ home.runtime.version }}
                </span>
                <!-- 仓库地址来自 i18n 的 about_repo_url：这里没有运行期配置能提供仓库地址，
                     而它是「部署相关的固定信息」，放在两套字典里比烧进组件里更好改。 -->
                <a
                  :href="t('about_repo_url')"
                  target="_blank"
                  rel="noopener noreferrer"
                  class="g-btn-ghost !h-8 !px-2"
                  :title="t('about_github')"
                  :aria-label="t('about_github')"
                >
                  <svg viewBox="0 0 16 16" fill="currentColor" class="h-4 w-4">
                    <path
                      d="M8 0C3.58 0 0 3.58 0 8c0 3.54 2.29 6.53 5.47 7.59.4.07.55-.17.55-.38 0-.19-.01-.82-.01-1.49-2.01.37-2.53-.49-2.69-.94-.09-.23-.48-.94-.82-1.13-.28-.15-.68-.52-.01-.53.63-.01 1.08.58 1.23.82.72 1.21 1.87.87 2.33.66.07-.52.28-.87.51-1.07-1.78-.2-3.64-.89-3.64-3.95 0-.87.31-1.59.82-2.15-.08-.2-.36-1.02.08-2.12 0 0 .67-.21 2.2.82.64-.18 1.32-.27 2-.27s1.36.09 2 .27c1.53-1.04 2.2-.82 2.2-.82.44 1.1.16 1.92.08 2.12.51.56.82 1.27.82 2.15 0 3.07-1.87 3.75-3.65 3.95.29.25.54.73.54 1.48 0 1.07-.01 1.93-.01 2.2 0 .21.15.46.55.38A8.01 8.01 0 0 0 16 8c0-4.42-3.58-8-8-8z"
                    />
                  </svg>
                </a>
              </div>
              <p class="mt-1.5 text-xs leading-relaxed text-ink-soft dark:text-ink-soft-dark">
                {{ t('about_desc') }}
              </p>
            </div>
          </div>
        </div>
      </div>
    </Transition>
  </Teleport>

  <ConfirmDialog
    :visible="pendingForget !== null"
    :title="t('settings_known_hosts_remove')"
    :message="t('settings_known_hosts_confirm', { host: pendingForget ? hostLabel(pendingForget) : '' })"
    :danger="true"
    :confirm-text="t('settings_known_hosts_remove')"
    :cancel-text="t('common_cancel')"
    @confirm="confirmForget"
    @cancel="pendingForget = null"
    @update:visible="(v) => { if (!v) pendingForget = null }"
  />
</template>
