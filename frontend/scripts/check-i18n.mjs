// i18n 字典一致性检查：zh.ts 与 en.ts 的 key 必须完全一致。
//
// 为什么需要脚本：`t()` 在缺 key 时会回退中文、再回退 key 本身，所以漏翻不会报错，
// 只会在英文界面里悄悄冒出中文（或一串 key）。这个检查把它变成一次显式失败。
import { readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import { dirname, join } from 'node:path'

const here = dirname(fileURLToPath(import.meta.url))
const dir = join(here, '..', 'src', 'i18n')

function keysOf(file) {
  const src = readFileSync(join(dir, file), 'utf8')
  const keys = new Set()
  // 只认「两个空格缩进 + 标识符 + 冒号」这一种形态：字典就是扁平的 Record<string, string>。
  for (const line of src.split('\n')) {
    const m = /^ {2}([a-zA-Z0-9_]+):/.exec(line)
    if (m) keys.add(m[1])
  }
  return keys
}

const zh = keysOf('zh.ts')
const en = keysOf('en.ts')

const missingInEn = [...zh].filter((k) => !en.has(k))
const missingInZh = [...en].filter((k) => !zh.has(k))

if (missingInEn.length || missingInZh.length) {
  if (missingInEn.length) console.error(`en.ts 缺少 ${missingInEn.length} 个 key：\n  ${missingInEn.join('\n  ')}`)
  if (missingInZh.length) console.error(`zh.ts 缺少 ${missingInZh.length} 个 key：\n  ${missingInZh.join('\n  ')}`)
  process.exit(1)
}

console.log(`i18n OK：zh/en 各 ${zh.size} 个 key，一一对应`)
