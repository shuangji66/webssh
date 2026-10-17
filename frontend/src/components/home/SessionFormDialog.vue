<script setup lang="ts">
// SessionFormDialog — 新增 / 编辑远程会话 + 连接测试（契约 6.2 的签名，首页在用）。
//
// 两条容易写错的规则（都来自契约 4.3）：
//   1. **编辑时密码/私钥留空 = 不修改**：不能发空串（空串在契约里是「清除该字段」）。
//      所以只在该字段有内容时才放进 SessionInput。
//   2. 主机密钥未信任时后端回 `hostkey_unknown` + 指纹：必须让用户看到指纹并显式确认，
//      确认（home.trust）成功后自动重测一次——否则用户要自己再点一次「测试连接」，
//      很多人会以为信任没生效。
//
// 第十一轮新增：**已信任的主机密钥**那一行（标题 + 删除按钮，见 CONTRACT 21.5）。它是从设置页
// 挪过来的，判据只有一条：**当前表单里的 host:port 在 known_hosts 里有没有记录**——
//   - 新增会话且这台主机还没被信任 → 不显示（没有东西可删，留个点了没反应的按钮更糟）；
//   - 点过「信任并继续」→ 重新拉一次列表，那一行立刻出现；
//   - 编辑一个已信任的会话 → 一打开就在。
// 所以「测试连接」不会让按钮出现：测试只回 `hostkey_unknown` + 指纹，**不写** known_hosts，
// 写记录只发生在 `/api/sessions/trust`（也就是「信任并继续」）。
//
// 同一个判据还管着**「保存」按钮显不显示**：指纹没确认过（新增会话、或刚把信任删了）时先藏起来，
// 页脚留一句提示把人引导到「测试连接 → 信任并继续」，走完那一步保存才出现（`canSave`）。
import { computed, ref, watch } from 'vue'
import { t } from '@/i18n'
import {
  api,
  describeError,
  type AuthType,
  type HostKeyRecord,
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

// ---------- 已信任的主机密钥（第十一轮：从设置页挪进来） ----------

/** known_hosts 里的记录（只用来判断「当前 host:port 有没有」）。 */
const trustedHosts = ref<HostKeyRecord[]>([])

/**
 * 当前表单里的 host:port 在 known_hosts 里的那条记录；没有就返回 null。
 *
 * 键的构造必须与后端 `hostKeyID` 一致（`host:port`，端口空/非法按 22）——所以这里用表单里的
 * 字段而不是 `props.session`：用户在编辑时改了主机名，那一行要跟着反映**新主机**的信任状态。
 */
const trustedRecord = computed<HostKeyRecord | null>(() => {
  const h = host.value.trim()
  if (!h) return null
  const p = Number(port.value)
  const wanted = Number.isInteger(p) && p >= 1 && p <= 65535 ? p : 22
  return trustedHosts.value.find((item) => item.host === h && item.port === wanted) ?? null
})

/** 确认框里点名的那台主机（`host:port`，22 端口省略——与设置页原来那句文案一致）。 */
const trustedTarget = computed(() => {
  const record = trustedRecord.value
  if (!record) return ''
  return record.port && record.port !== 22 ? `${record.host}:${record.port}` : record.host
})

/**
 * 拉一遍信任列表。
 *
 * 拉不到就**当成没有记录**（那一行不显示），也不弹错：它只是个附带的开关，读失败不影响
 * 保存/测试这些主流程；而「不显示」是保守方向——绝不会让用户对着一条没确认过的记录点删除。
 */
async function loadTrusted() {
  try {
    const res = await api.hostKeys()
    trustedHosts.value = res.hosts ?? []
    hostsLoaded.value = true
  } catch {
    trustedHosts.value = []
  }
}

/**
 * 这一份列表**读到过**没有（读成功才置真，读失败保持原值）。
 *
 * 与下面 `needsTrust` 的兜底有关：读失败时 `trustedHosts` 是空的，如果直接拿它当判据，一次接口
 * 抖动就会把按钮变成「仅保存」并挂上一句没头没脑的提示。所以「不知道」要能与「确实没有」分开。
 */
const hostsLoaded = ref(false)

/**
 * 按钮文案与那句提示的判据（第十一轮用户要求）：**确认过没有信任记录**才算「还没信任」。
 *
 *   - 新增会话（这台主机没确认过指纹）、刚删掉信任、编辑时把地址改成没信任过的主机 → 真：
 *     按钮写「仅保存」，旁边一句提示建议先「测试连接」确认指纹；
 *   - 编辑一个已信任的会话 → 假：按钮就是普通的「保存」，提示不出现；
 *   - 读不到列表（还在读 / 读失败）→ 假：那是我们自己的接口出了问题，不该让用户的按钮莫名
 *     变成「仅保存」还多一句提示。
 *
 * 注意**不再隐藏按钮**：隐藏等于「连不上主机就存不了配置」，而用户可能只是先把会话存下来、
 * 以后才连（原来的实现就是这么卡住的，用户当场指出）。「仅保存」走的是同一条 `submit()`，
 * 区别只在文案与那句提示——指纹留到第一次连接时再确认。
 */
const needsTrust = computed(() => hostsLoaded.value && trustedRecord.value === null)

const forgetVisible = ref(false)

/** 「移除信任」：先确认（下次连接要重新认指纹，用户得知道这一点）。 */
function askForget() {
  if (!trustedRecord.value) return
  forgetVisible.value = true
}

async function confirmForget() {
  const record = trustedRecord.value
  forgetVisible.value = false
  if (!record) return
  try {
    await api.forgetHostKey(record.host, record.port)
    toast.show(t('form_trusted_key_removed'), 'success')
    await loadTrusted()
  } catch (e) {
    toast.show(t('form_trusted_key_remove_failed', { error: describeError(e) }), 'error')
  }
}


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
  // 备注必须**回填已有值**：它是整份提交的普通字段（见 buildInput），留空提交等于把配置里的
  // 备注清掉——用户点开「编辑」只是想改个名字，顺手保存就把备注弄没了（第四轮用户报的）。
  note.value = s?.note ?? ''
  saveCredential.value = s ? s.credentialSaved : true
  formError.value = ''
  testResult.value = null
  testHost.value = ''
  baseline.value = JSON.stringify(fields())
  // 信任列表可能在别处变过（另一个会话弹窗里刚信任/移除，或另一台设备），每次打开重拉一遍。
  // hostsLoaded 先归零：重开一次就要重新判「保存」显不显示，不能沿用上一次的结论。
  hostsLoaded.value = false
  void loadTrusted()
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
  testHost.value = input.host ?? host.value
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
    await loadTrusted() // 信任记录刚写进去：那一行「已信任的主机密钥」要立刻出现
    await runTest() // 信任后自动重测
  } catch (e) {
    toast.show(t('test_trust_failed', { error: describeError(e) }), 'error')
  } finally {
    trusting.value = false
  }
}

// ---------- 保存 / 关闭 ----------

/**
 * 保存成功的提示文案。
 *
 * 会话**正开着**时改凭据，普通「已保存」会让用户以为新凭据已经在所有地方生效。实际语义是：
 *   - 新开的终端 / SFTP 重连用新凭据；
 *   - 已经开着的终端还是旧连接（不能也不该偷偷断掉它）；
 *   - 关掉「保存凭据」只影响持久化，当前会话在关闭前仍然可用（后端把当前凭据留在内存里，
 *     见 sessions.go 的 carry-over）。
 * 所以这里换一句说清楚「什么时候需要重新打开会话」的话；没改凭据（只改名/备注/端口）时
 * 保持原来的「已保存」。
 */
function savedToast(saved: SessionView): string {
  const prev = props.session
  const name = sessionLabel(saved)
  if (!prev || !home.isOpen(saved.id)) return t('form_saved', { name })
  if (prev.credentialSaved && !saved.credentialSaved) {
    return t('form_cred_off_open', { name })
  }
  const credentialTouched =
    (authType.value === 'password' ? !!password.value : !!privateKey.value) ||
    prev.authType !== authType.value
  if (credentialTouched) return t('form_cred_changed_open', { name })
  return t('form_saved', { name })
}

async function submit() {
  const input = buildInput()
  if (!input) return
  saving.value = true
  try {
    const saved = await home.save(input)
    toast.show(savedToast(saved), 'success')
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
            class="pointer-events-auto relative flex max-h-[90%] w-full max-w-lg flex-col rounded-xl border border-line bg-surface shadow-pop dark:border-line-dark dark:bg-surface-dark"
          >
            <!-- 标题与关闭按钮固定在顶部、不参与滚动（第七轮用户要求）：表单很长时中间那段
                 自己滚，标题和 × 一直在——否则要滚回顶部才能关掉弹窗。页脚同理固定在底部。 -->
            <div
              class="flex shrink-0 items-center justify-between border-b border-line px-5 py-3 dark:border-line-dark"
            >
              <h3 class="font-display text-base font-semibold text-ink dark:text-ink-dark">
                {{ isEdit ? t('form_edit_title') : t('form_new_title') }}
              </h3>
              <button class="g-btn-ghost !h-8 !px-2 text-lg leading-none" @click="requestClose">×</button>
            </div>

            <div class="min-h-0 flex-1 overflow-y-auto px-5 py-4">
              <div class="space-y-3">
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

                <!-- 已信任的主机密钥（第十一轮从设置页挪进来）：**只有当前 host:port 已有记录时
                     才出现**（没有记录就没东西可删；用户问过「创建会话时要不要隐藏」——就是这里：
                     隐藏，点过「信任并继续」后立刻出现）。只留标题 + 删除按钮，不显示 keyType /
                     指纹：这台主机的密钥细节在上面「测试连接」的结果里已经给过了，日常没人看。 -->
                <div
                  v-if="trustedRecord"
                  class="flex items-center gap-2 border-t border-line pt-3 dark:border-line-dark"
                >
                  <span class="min-w-0 flex-1 text-xs font-medium text-ink-soft dark:text-ink-soft-dark">
                    {{ t('form_trusted_key') }}
                  </span>
                  <button
                    class="g-btn-ghost !h-8 !px-2 shrink-0 !text-danger hover:!bg-danger/10"
                    :title="t('form_trusted_key_remove')"
                    :aria-label="t('form_trusted_key_remove')"
                    @click="askForget"
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
                </div>
              </div>
            </div>

            <!-- 页脚：三个按钮都「有边框、不填色」（第七轮用户要求），靠文字区分主次。
                 指纹没确认过时按钮写「仅保存」，并在**三个按钮之上**单独占一行放提示（第十一轮
                 用户要求：改文案 + 不要挤在同一行）——先测一次确认指纹是最顺的路，但不能因此
                 挡住只想先存下来的人（主机暂时连不上、或就想先建好配置），所以「仅保存」仍在。
                 有信任记录时它就是普通的「保存」（见 needsTrust）。 -->
            <div
              class="flex shrink-0 flex-col gap-2 border-t border-line px-5 py-3 dark:border-line-dark"
            >
              <p v-if="needsTrust" class="text-xs text-warning">
                {{ t('form_trust_first') }}
              </p>
              <div class="flex items-center justify-end gap-2">
                <button class="g-btn-secondary !h-9 !px-4 text-sm" @click="requestClose">
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
                  class="g-btn-secondary !h-9 !px-5 text-sm"
                  :disabled="saving || testing"
                  @click="submit"
                >
                  {{ saving ? t('form_saving') : needsTrust ? t('form_save_only') : t('common_save') }}
                </button>
              </div>
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

  <!-- 移除信任前确认：下次连接要重新认一遍指纹，用户得知道这件事（这条记录不是凭据，
       但它承担的是「防中间人」那一步）。 -->
  <ConfirmDialog
    :visible="forgetVisible"
    :title="t('form_trusted_key_remove')"
    :message="t('form_trusted_key_remove_confirm', { host: trustedRecord ? trustedTarget : '' })"
    :danger="true"
    :confirm-text="t('form_trusted_key_remove')"
    :cancel-text="t('common_cancel')"
    @confirm="confirmForget"
    @cancel="forgetVisible = false"
    @update:visible="(v) => { if (!v) forgetVisible = false }"
  />
</template>
