// i18n 一致性检查，两个方向都要查：
//   ① 字典之间：zh.ts 与 en.ts 的 key 必须一一对应（漏翻会在英文界面悄悄回退中文）；
//   ② 代码 → 字典：代码里 `t('x')` 用到的 key 必须存在于字典。
// ② 是必须的：`t()` 在缺 key 时会把 **key 本身**当成文案返回，界面上就会出现
// `files_copy` 这种英文小写串（真发生过大文件菜单里的「复制」变成 `files_copy`），
// 而这种错不会报任何警告。只在字典之间比对是查不出来的。
import { readFileSync, readdirSync, statSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import { dirname, join, relative } from 'node:path'

const here = dirname(fileURLToPath(import.meta.url))
const srcDir = join(here, '..', 'src')
const i18nDir = join(srcDir, 'i18n')

function keysOf(file) {
  const src = readFileSync(join(i18nDir, file), 'utf8')
  const keys = new Set()
  // 字典是扁平的 Record<string, string>：只认「两个空格缩进 + 标识符 + 冒号」这一种形态。
  for (const line of src.split('\n')) {
    const m = /^ {2}([a-zA-Z0-9_]+):/.exec(line)
    if (m) keys.add(m[1])
  }
  return keys
}

function walk(dir, out = []) {
  for (const entry of readdirSync(dir)) {
    const p = join(dir, entry)
    if (statSync(p).isDirectory()) walk(p, out)
    else if (/\.(ts|vue)$/.test(entry)) out.push(p)
  }
  return out
}

const zh = keysOf('zh.ts')
const en = keysOf('en.ts')
let failed = false

const missingInEn = [...zh].filter((k) => !en.has(k))
const missingInZh = [...en].filter((k) => !zh.has(k))
if (missingInEn.length || missingInZh.length) {
  failed = true
  if (missingInEn.length) console.error(`en.ts 缺少 ${missingInEn.length} 个 key：\n  ${missingInEn.join('\n  ')}`)
  if (missingInZh.length) console.error(`zh.ts 缺少 ${missingInZh.length} 个 key：\n  ${missingInZh.join('\n  ')}`)
}

// 代码里静态写死 key 的调用：t('x') / t("x") / t(`x`)
const used = new Map()
for (const file of walk(srcDir)) {
  if (file.startsWith(i18nDir)) continue
  const src = readFileSync(file, 'utf8')
  for (const m of src.matchAll(/\bt\(\s*['"`]([a-zA-Z0-9_]+)['"`]/g)) {
    if (!zh.has(m[1]) && !used.has(m[1])) used.set(m[1], relative(join(srcDir, '..'), file))
  }
}
if (used.size) {
  failed = true
  console.error(`代码引用了 ${used.size} 个字典里不存在的 key（界面上会直接显示 key）：`)
  for (const [k, f] of used) console.error(`  ${k}  <-  ${f}`)
}

if (failed) process.exit(1)
console.log(`i18n OK：zh/en 各 ${zh.size} 个 key 一一对应，代码引用的 key 全部存在`)
