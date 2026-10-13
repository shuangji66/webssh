<script setup lang="ts">
// CredentialPromptDialog — 「不保存凭据」的会话在打开时索要凭据（契约 11.1.3/11.6 的入口）。
//
// 为什么必须存在：这类会话的密码/私钥**不落盘**，后端内存里也没有（会话关闭或后端重启即失效），
// 所以 `POST /api/sessions/open` 会回 `credential_required`。用户看到的不该是一句错误文案，
// 而是「输入凭据 → 重试一次」的入口。
//
// 职责边界：本组件只负责**收集**凭据（以及本地必填校验），真正的 open 与错误处理在调用方
// （HomeView）——它才知道拿到凭据后要接终端标签、要 toast 什么，以及重试失败时把原因回填到
// `error` 属性上。这样这个弹窗不依赖任何业务 store，只依赖一份 session 配置。
import { ref, watch } from 'vue'
import { t } from '@/i18n'
import type { SessionView } from '@/serverapi'
import type { SessionCredential } from '@/stores/home'
import { sessionTarget } from '@/utils/format'

const props = defineProps<{
  visible: boolean
  /** 要连接的会话（提供 user/host/authType：决定标题、提示与要显示的输入框）。 */
  session: SessionView | null
  /** 调用方正在重试 open（按钮转圈并禁用，避免连点重复拨号）。 */
  loading?: boolean
  /** 上一次重试失败的原因（由调用方写入，展示后由下一次提交覆盖）。 */
  error?: string
}>()

const emit = defineEmits<{
  (e: 'update:visible', v: boolean): void
  (e: 'submit', credential: SessionCredential): void
}>()

const open = ref(props.visible)
const password = ref('')
const privateKey = ref('')
const passphrase = ref('')
/** 本地必填校验的错误（与 props.error 分开：一个是「你还没填」，一个是「填了但没连上」）。 */
const formError = ref('')

function reset() {
  password.value = ''
  privateKey.value = ''
  passphrase.value = ''
  formError.value = ''
}

watch(
  () => props.visible,
  (v) => {
    open.value = v
    // 每次打开都清空：凭据是敏感内容，留在输入框里（尤其是上一次失败的）既没必要也不安全
    if (v) reset()
  },
  { immediate: true }
)

// 换了一个会话（用户在弹窗开着时点了另一张卡片的「打开」）也要清空
watch(
  () => props.session,
  () => {
    if (props.visible) reset()
  }
)

const isPassword = () => props.session?.authType !== 'key'

function submit() {
  const session = props.session
  if (!session || props.loading) return
  formError.value = ''
  if (isPassword()) {
    if (!password.value) {
      formError.value = t('form_required_password')
      return
    }
    emit('submit', { password: password.value })
    return
  }
  if (!privateKey.value.trim()) {
    formError.value = t('form_required_key')
    return
  }
  // 私钥与口令一起提交：口令为空串表示「这把私钥没有口令」。只发私钥的话，后端会沿用上一份
  // 凭据的口令（如果内存里还留着），解析新私钥就会失败。
  emit('submit', { privateKey: privateKey.value, passphrase: passphrase.value })
}

function cancel() {
  open.value = false
  emit('update:visible', false)
}
</script>

<template>
  <Teleport to="body">
    <Transition name="modal-fade">
      <div v-if="open" class="fixed inset-0 z-50">
        <div class="absolute inset-0 bg-black/50" @click="cancel"></div>
        <div class="region-center absolute left-0 right-0 flex items-center justify-center px-4">
          <div
            class="pointer-events-auto relative max-h-[90%] w-full max-w-md overflow-y-auto rounded-xl border border-line bg-surface p-5 shadow-pop dark:border-line-dark dark:bg-surface-dark"
          >
            <div class="flex items-center justify-between gap-2">
              <h3 class="min-w-0 truncate font-display text-base font-semibold text-ink dark:text-ink-dark">
                {{ isPassword() ? t('cred_title_password') : t('cred_title_key') }}
              </h3>
              <button class="g-btn-ghost !h-8 !px-2 text-lg leading-none" @click="cancel">×</button>
            </div>

            <p v-if="session" class="mt-2 text-xs text-ink-soft dark:text-ink-soft-dark">
              {{ t('cred_hint', { user: session.user, host: sessionTarget(session) }) }}
            </p>

            <div class="mt-4 space-y-3">
              <div v-if="isPassword()">
                <label
                  for="field-credentialpromptdialog-1"
                  class="mb-1 block text-xs font-medium text-ink-soft dark:text-ink-soft-dark"
                >
                  {{ t('cred_password') }}
                </label>
                <input
                  id="field-credentialpromptdialog-1"
                  v-model="password"
                  class="g-input"
                  type="password"
                  autocomplete="off"
                  :placeholder="t('form_password_placeholder')"
                  @keydown.enter="submit"
                />
              </div>

              <template v-else>
                <div>
                  <label
                    for="field-credentialpromptdialog-2"
                    class="mb-1 block text-xs font-medium text-ink-soft dark:text-ink-soft-dark"
                  >
                    {{ t('cred_private_key') }}
                  </label>
                  <textarea
                    id="field-credentialpromptdialog-2"
                    v-model="privateKey"
                    class="g-input resize-y font-mono text-xs"
                    rows="4"
                    spellcheck="false"
                    :placeholder="t('form_private_key_placeholder')"
                  ></textarea>
                </div>
                <div>
                  <label
                    for="field-credentialpromptdialog-3"
                    class="mb-1 block text-xs font-medium text-ink-soft dark:text-ink-soft-dark"
                  >
                    {{ t('cred_passphrase') }}
                  </label>
                  <input
                    id="field-credentialpromptdialog-3"
                    v-model="passphrase"
                    class="g-input"
                    type="password"
                    autocomplete="off"
                    :placeholder="t('form_passphrase_placeholder')"
                  />
                </div>
              </template>

              <!-- 本次重试失败的原因（后端错误码已由调用方翻成人话） -->
              <p v-if="error" class="break-words text-xs text-danger">{{ error }}</p>
              <p v-if="formError" class="text-xs text-danger">{{ formError }}</p>
            </div>

            <div class="mt-5 flex justify-end gap-2">
              <button class="g-btn-ghost !h-9 !px-4 text-sm" :disabled="loading" @click="cancel">
                {{ t('common_cancel') }}
              </button>
              <button
                class="g-btn-primary !h-9 !px-5 text-sm"
                :disabled="loading"
                @click="submit"
              >
                {{ loading ? t('cred_connecting') : t('cred_connect') }}
              </button>
            </div>
          </div>
        </div>
      </div>
    </Transition>
  </Teleport>
</template>
