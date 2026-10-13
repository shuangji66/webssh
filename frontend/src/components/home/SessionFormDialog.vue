<script setup lang="ts">
// SessionFormDialog — 新增 / 编辑远程会话 + 连接测试（契约 6.2 的签名，首页在用）。
//
// 两条容易写错的规则（都来自契约 4.3）：
//   1. **编辑时密码/私钥留空 = 不修改**：不能发空串（空串在契约里是「清除该字段」）。
//      所以只在该字段有内容时才放进 SessionInput。
//   2. 主机密钥未信任时后端回 `hostkey_unknown` + 指纹：必须让用户看到指纹并显式确认，
//      确认（home.trust）成功后自动重测一次——否则用户要自己再点一次「测试连接」，
//      很多人会以为信任没生效。
import { computed, ref, watch } from 'vue'
import { t } from '@/i18n'
import {
  describeError,
  type AuthType,
  type SessionInput,
  type SessionView,
  type TestOutcome
} from '@/serverapi'
import { useHomeStore } from '@/stores/home'
import { useToastStore } from '@/stores/toast'
import { sessionLabel } from '@/utils/format'
import ConfirmDialog from '@/components/ConfirmDialog.vue'

const props = defineProps<{
  visible: boolean
  session?: SessionView | null
}>()

const emit = defineEmits<{
  (e: 'update:visible', v: boolean): void
  (e: 'saved', session: SessionView): void
}>()

const home = useHomeStore()
const toast = useToastStore()

const open = ref(props.visible)
const isEdit = computed(() => !!props.session)

const name = ref('')
const host = ref('')
const port = ref('22')
const user = ref('')
const authType = ref<AuthType>('password')
const password = ref('')
const privateKey = ref('')
const passphrase = ref('')
const note = ref('')
/**
 * 是否把凭据加密落盘（契约 11.1.3）。新增会话默认「保存」——多数人期望的是「下次点开就能连」；
 * 编辑已有会话时按配置里的 `credentialSaved` 初始化（用户上次的选择要能看见，而不是被重置）。
 *
 * 关掉它不等于这次不用凭据：本次拨号仍然用表单里填的密码/私钥（后端把它们留在内存里），
 * 只是不写进数据目录，所以会话关闭/后端重启后需要重新输入。
 */
const saveCredential = ref(true)

/** 认证方式选项：模板里用常量而不是内联断言，避免依赖模板对 TS 表达式的支持。 */
const authOptions: AuthType[] = ['password', 'key']

const formError = ref('')
const saving = ref(false)
const testing = ref(false)
const trusting = ref(false)
const testResult = ref<TestOutcome | null>(null)
const testHost = ref('')
/** 打开弹窗时的字段快照：用来判断「有未保存的改动」。 */
const baseline = ref('')

function fields() {
  return {
    name: name.value,
    host: host.value,
    port: port.value,
    user: user.value,
    authType: authType.value,
    password: password.value,
    privateKey: privateKey.value,
    passphrase: passphrase.value,
    note: note.value,
    saveCredential: saveCredential.value
  }
}

function reset() {
  const s = props.session
  name.value = s?.name ?? ''
  host.value = s?.host ?? ''
  port.value = s ? String(s.port) : '22'
  user.value = s?.user ?? ''
  authType.value = s?.authType ?? 'password'
  password.value = ''
  privateKey.value = ''
  passphrase.value = ''
  note.value = ''
  saveCredential.value = s ? s.credentialSaved : true
  formError.value = ''
  testResult.value = null
  testHost.value = ''
  baseline.value = JSON.stringify(fields())
}

watch(
  () => props.visible,
  (v) => {
    open.value = v
    if (v) reset()
  },
  { immediate: true }
)

// 编辑目标在弹窗打开期间被换掉（首页点了另一张卡片的「编辑」）也要重置
watch(
  () => props.session,
  () => {
    if (props.visible) reset()
  }
)

// ---------- 输入校验 / 组装 ----------

function buildInput(): SessionInput | null {
  formError.value = ''
  const h = host.value.trim()
  const u = user.value.trim()
  const p = Number(port.value)
  if (!h) {
    formError.value = t('form_required_host')
    return null
  }
  if (!u) {
    formError.value = t('form_required_user')
    return null
  }
  if (!Number.isInteger(p) || p < 1 || p > 65535) {
    formError.value = t('form_port_invalid')
    return null
  }

  const editing = isEdit.value
  const switching = editing && props.session?.authType !== authType.value
  // 「保存凭据」是打开的，但这份配置里现在没有任何可保留的凭据（新增会话，或上次就是「不保存」）：
  // 此时留空提交，后端只能按「留空 = 不修改」处理（配置里的 credentialSaved 仍是 false），
  // 而用户看到的是开关自己弹回去了——说不清的中间态。当场要他把凭据填上，别留静默失败。
  const needCredential = saveCredential.value && (!editing || !props.session?.credentialSaved)
  const input: SessionInput = { host: h, port: p, user: u, authType: authType.value }
  if (props.session) input.id = props.session.id
  // 名称与备注不是密钥：总是整份提交，这样「清空名称」也能生效（留空 = 界面回退成 user@host）
  input.name = name.value.trim()
  input.note = note.value.trim()
  input.saveCredential = saveCredential.value

  if (authType.value === 'password') {
    if (password.value) input.password = password.value
    // 新增会话（或从密钥切回密码）必须给密码：否则后端没有可用凭据，报 no_credential
    else if (!editing || switching || needCredential) {
      formError.value = t('form_required_password')
      return null
    }
  } else {
    if (privateKey.value) {
      input.privateKey = privateKey.value
      // 给了私钥就一起给口令（空串 = 这个私钥没有口令）。不能「口令留空就不发」：
      // 那样会保留上一把私钥的口令，解析新私钥时就会失败。
      input.passphrase = passphrase.value
    } else if (!editing || switching || needCredential) {
      formError.value = t('form_required_key')
      return null
    }
  }
  return input
}

const authChangedHint = computed(
  () => isEdit.value && props.session?.authType !== authType.value && authType.value === 'password'
)

/**
 * 「留空表示不修改已保存的密码/私钥」这句提示只在**确实有东西可保留**时才显示（契约 11.1.3）：
 *  - 配置里 credentialSaved 为假（这张会话从来不存凭据）→ 那三个字段本来就是空的，
 *    显示这句话会让用户以为自己存过密码、现在留空就能沿用；
 *  - 表单里切换过认证方式 → 当前显示的字段与已保存的凭据不是同一个（那种情况由
 *    authChangedHint 说明「保存后会使用新的凭据」）。
 */
const keepHint = computed(() => {
  const s = props.session
  if (!s || !s.credentialSaved) return false
  if (s.authType !== authType.value) return false
  return s.authType === 'password' ? s.hasPassword : s.hasPrivateKey
})

// ---------- 测试连接 ----------

/** 结果码 → 人话（i18n 里没有对应 err_<code> 时退回后端给的 detail）。 */
function outcomeText(res: TestOutcome): string {
  if (res.code === 'unknown') return res.detail || t('test_failed')
  const key = `err_${res.code}`
  const mapped = t(key)
  if (mapped !== key) return mapped
  return res.detail || res.code
}

async function runTest() {
  const input = buildInput()
  if (!input) return
  testing.value = true
  testResult.value = null
  testHost.value = input.host
  try {
    testResult.value = await home.test(input)
  } catch (e) {
    // 参数不合法之类的接口级错误：当成一条失败结果显示，别把弹窗搞崩
    testResult.value = { success: false, code: 'unknown', detail: describeError(e) }
  } finally {
    testing.value = false
  }
}

// 主机密钥未知且还没信任：给出指纹与「信任并继续」
const needTrust = computed(() => {
  const res = testResult.value
  return !!res && !res.success && !res.trusted && !res.mismatch && !!res.fingerprint
})

async function trustAndRetest() {
  const res = testResult.value
  if (!res?.fingerprint) return
  trusting.value = true
  try {
    await home.trust(host.value.trim(), Number(port.value), res.fingerprint)
    toast.show(t('test_trust_ok'), 'success')
    trusting.value = false
    await runTest() // 信任后自动重测
  } catch (e) {
    toast.show(t('test_trust_failed', { error: describeError(e) }), 'error')
  } finally {
    trusting.value = false
  }
}

// ---------- 保存 / 关闭 ----------

async function submit() {
  const input = buildInput()
  if (!input) return
  saving.value = true
  try {
    const saved = await home.save(input)
    toast.show(t('form_saved', { name: sessionLabel(saved) }), 'success')
    emit('saved', saved)
    close()
  } catch (e) {
    formError.value = t('form_save_failed', { error: describeError(e) })
  } finally {
    saving.value = false
  }
}

const discardVisible = ref(false)

function close() {
  open.value = false
  emit('update:visible', false)
}

function requestClose() {
  if (saving.value) return
  // 有未保存的改动先问一声：表单里可能已经粘好一大段私钥，直接关掉就白粘了
  if (JSON.stringify(fields()) !== baseline.value) {
    discardVisible.value = true
    return
  }
  close()
}

function onDiscardConfirm() {
  discardVisible.value = false
  close()
}

function onDiscardVisible(v: boolean) {
  if (!v) discardVisible.value = false
}
</script>

<template>
  <Teleport to="body">
    <Transition name="modal-fade">
      <div v-if="open" class="fixed inset-0 z-50">
        <div class="absolute inset-0 bg-black/50" @click="requestClose"></div>
        <div class="region-center absolute left-0 right-0 flex items-center justify-center px-4">
          <div
            class="pointer-events-auto relative max-h-[90%] w-full max-w-lg overflow-y-auto rounded-xl border border-line bg-surface p-5 shadow-pop dark:border-line-dark dark:bg-surface-dark"
          >
            <div class="flex items-center justify-between">
              <h3 class="font-display text-base font-semibold text-ink dark:text-ink-dark">
                {{ isEdit ? t('form_edit_title') : t('form_new_title') }}
              </h3>
              <button class="g-btn-ghost !h-8 !px-2 text-lg leading-none" @click="requestClose">×</button>
            </div>

            <div class="mt-4 space-y-3">
              <div>
                <label for="field-sessionformdialog-1" class="mb-1 block text-xs font-medium text-ink-soft dark:text-ink-soft-dark">
                  {{ t('form_name') }}
                </label>
                <input id="field-sessionformdialog-1" v-model="name" class="g-input" :placeholder="t('form_name_placeholder')" />
              </div>

              <div class="flex gap-2">
                <div class="min-w-0 flex-1">
                  <label for="field-sessionformdialog-2" class="mb-1 block text-xs font-medium text-ink-soft dark:text-ink-soft-dark">
                    {{ t('form_host') }}
                  </label>
                  <input id="field-sessionformdialog-2"
                    v-model="host"
                    class="g-input font-mono"
                    :placeholder="t('form_host_placeholder')"
                    autocapitalize="off"
                    autocorrect="off"
                    spellcheck="false"
                  />
                </div>
                <div class="w-24 shrink-0">
                  <label for="field-sessionformdialog-3" class="mb-1 block text-xs font-medium text-ink-soft dark:text-ink-soft-dark">
                    {{ t('form_port') }}
                  </label>
                  <input id="field-sessionformdialog-3" v-model="port" class="g-input font-mono" inputmode="numeric" placeholder="22" />
                </div>
              </div>

              <div>
                <label for="field-sessionformdialog-4" class="mb-1 block text-xs font-medium text-ink-soft dark:text-ink-soft-dark">
                  {{ t('form_user') }}
                </label>
                <input id="field-sessionformdialog-4"
                  v-model="user"
                  class="g-input font-mono"
                  :placeholder="t('form_user_placeholder')"
                  autocapitalize="off"
                  autocorrect="off"
                  spellcheck="false"
                />
                <p class="mt-1 text-xs text-ink-faint dark:text-ink-faint-dark">
                  {{ t('form_identity_hint') }}
                </p>
              </div>

              <div>
                <label class="mb-1 block text-xs font-medium text-ink-soft dark:text-ink-soft-dark">
                  {{ t('form_auth_type') }}
                </label>
                <div class="grid grid-cols-2 gap-2">
                  <button
                    v-for="opt in authOptions"
                    :key="opt"
                    class="rounded-md border px-3 py-2 text-sm font-medium transition-colors"
                    :class="
                      authType === opt
                        ? 'border-brand bg-brand/10 text-brand'
                        : 'border-line text-ink hover:border-brand dark:border-line-dark dark:text-ink-dark'
                    "
                    @click="authType = opt"
                  >
                    {{ opt === 'password' ? t('form_auth_password') : t('form_auth_key') }}
                  </button>
                </div>
                <p
                  v-if="authChangedHint"
                  class="mt-1 text-xs text-warning"
                >
                  {{ t('form_auth_changed_hint') }}
                </p>
              </div>

              <div v-if="authType === 'password'">
                <label for="field-sessionformdialog-6" class="mb-1 block text-xs font-medium text-ink-soft dark:text-ink-soft-dark">
                  {{ t('form_password') }}
                </label>
                <input id="field-sessionformdialog-6"
                  v-model="password"
                  class="g-input"
                  type="password"
                  autocomplete="new-password"
                  :placeholder="t('form_password_placeholder')"
                />
                <p v-if="keepHint" class="mt-1 text-xs text-ink-faint dark:text-ink-faint-dark">
                  {{ t('form_password_keep') }}
                </p>
              </div>

              <template v-else>
                <div>
                  <label for="field-sessionformdialog-7" class="mb-1 block text-xs font-medium text-ink-soft dark:text-ink-soft-dark">
                    {{ t('form_private_key') }}
                  </label>
                  <textarea id="field-sessionformdialog-7"
                    v-model="privateKey"
                    class="g-input resize-y font-mono text-xs"
                    rows="4"
                    spellcheck="false"
                    :placeholder="t('form_private_key_placeholder')"
                  ></textarea>
                  <p v-if="keepHint" class="mt-1 text-xs text-ink-faint dark:text-ink-faint-dark">
                    {{ t('form_private_key_keep') }}
                  </p>
                </div>
                <div>
                  <label for="field-sessionformdialog-8" class="mb-1 block text-xs font-medium text-ink-soft dark:text-ink-soft-dark">
                    {{ t('form_passphrase') }}
                  </label>
                  <input id="field-sessionformdialog-8"
                    v-model="passphrase"
                    class="g-input"
                    type="password"
                    autocomplete="new-password"
                    :placeholder="t('form_passphrase_placeholder')"
                  />
                </div>
              </template>

              <!-- 凭据保存开关（契约 11.1.3）：关掉 = 不写入数据目录，凭据只活在后端内存里，
                   该会话关闭或后端重启后要重新输入。文案按认证方式分（保存密码 / 保存私钥）。 -->
              <div class="rounded-md border border-line px-3 py-2.5 dark:border-line-dark">
                <label class="flex cursor-pointer items-start gap-2.5" :title="t('form_save_credential')">
                  <input v-model="saveCredential" type="checkbox" class="mt-0.5 h-4 w-4 shrink-0 accent-brand" />
                  <span class="min-w-0">
                    <span class="block text-sm font-medium text-ink dark:text-ink-dark">
                      {{
                        authType === 'password'
                          ? t('form_save_credential_password')
                          : t('form_save_credential_key')
                      }}
                    </span>
                    <span class="mt-0.5 block text-xs text-ink-faint dark:text-ink-faint-dark">
                      {{ t('form_save_credential_hint') }}
                    </span>
                  </span>
                </label>
              </div>

              <div>
                <label for="field-sessionformdialog-9" class="mb-1 block text-xs font-medium text-ink-soft dark:text-ink-soft-dark">
                  {{ t('form_note') }}
                </label>
                <input id="field-sessionformdialog-9" v-model="note" class="g-input" :placeholder="t('form_note_placeholder')" />
              </div>

              <p v-if="formError" class="break-words text-xs text-danger">{{ formError }}</p>

              <!-- 测试连接结果 -->
              <div
                v-if="testing"
                class="rounded-md border border-line px-3 py-2 text-xs text-ink-soft dark:border-line-dark dark:text-ink-soft-dark"
              >
                {{ t('test_pending', { host: testHost }) }}
              </div>
              <div
                v-else-if="testResult"
                class="space-y-1.5 rounded-md border border-line px-3 py-2 text-xs dark:border-line-dark"
              >
                <div class="flex items-center gap-2">
                  <span
                    class="h-2 w-2 shrink-0 rounded-full"
                    :class="testResult.success ? 'bg-success' : 'bg-danger'"
                  ></span>
                  <span
                    class="font-medium"
                    :class="testResult.success ? 'text-success' : 'text-danger'"
                  >
                    {{
                      testResult.success
                        ? t('test_ok', { ms: testResult.latencyMs ?? 0 })
                        : t('test_failed')
                    }}
                  </span>
                </div>
                <p v-if="testResult.success" class="text-ink-soft dark:text-ink-soft-dark">
                  {{ t('test_ok_detail') }}
                </p>
                <p v-else class="break-words text-danger">{{ outcomeText(testResult) }}</p>

                <dl
                  v-if="testResult.fingerprint || testResult.keyType || testResult.banner"
                  class="grid grid-cols-[5.5rem_1fr] gap-x-2 gap-y-1"
                >
                  <template v-if="testResult.fingerprint">
                    <dt class="text-ink-faint dark:text-ink-faint-dark">{{ t('test_fingerprint') }}</dt>
                    <dd class="break-all font-mono">{{ testResult.fingerprint }}</dd>
                  </template>
                  <template v-if="testResult.keyType">
                    <dt class="text-ink-faint dark:text-ink-faint-dark">{{ t('test_key_type') }}</dt>
                    <dd class="font-mono">{{ testResult.keyType }}</dd>
                  </template>
                  <template v-if="testResult.banner">
                    <dt class="text-ink-faint dark:text-ink-faint-dark">{{ t('test_banner') }}</dt>
                    <dd class="break-all font-mono">{{ testResult.banner }}</dd>
                  </template>
                </dl>

                <p
                  v-if="testResult.fingerprint && !testResult.mismatch"
                  class="text-ink-soft dark:text-ink-soft-dark"
                >
                  {{ testResult.trusted ? t('test_trusted') : t('test_untrusted') }}
                </p>

                <div v-if="needTrust" class="space-y-1.5 pt-1">
                  <p class="text-warning">{{ t('test_need_trust_first') }}</p>
                  <button
                    class="g-btn-secondary !h-8 !px-3 text-xs"
                    :disabled="trusting"
                    @click="trustAndRetest"
                  >
                    {{ t('test_trust') }}
                  </button>
                </div>

                <p v-if="testResult.mismatch" class="text-danger">
                  {{ t('test_mismatch_hint') }}
                </p>
              </div>
            </div>

            <div class="mt-5 flex justify-end gap-2">
              <button class="g-btn-ghost !h-9 !px-4 text-sm" @click="requestClose">
                {{ t('common_cancel') }}
              </button>
              <button
                class="g-btn-secondary !h-9 !px-4 text-sm"
                :disabled="testing || saving"
                @click="runTest"
              >
                {{ testing ? t('form_testing') : t('form_test') }}
              </button>
              <button
                class="g-btn-primary !h-9 !px-5 text-sm"
                :disabled="saving || testing"
                @click="submit"
              >
                {{ saving ? t('form_saving') : t('common_save') }}
              </button>
            </div>
          </div>
        </div>
      </div>
    </Transition>
  </Teleport>

  <ConfirmDialog
    :visible="discardVisible"
    :title="t('common_tip')"
    :message="t('form_unsaved_confirm')"
    :confirm-text="t('common_ok')"
    :cancel-text="t('common_cancel')"
    @confirm="onDiscardConfirm"
    @cancel="discardVisible = false"
    @update:visible="onDiscardVisible"
  />
</template>
