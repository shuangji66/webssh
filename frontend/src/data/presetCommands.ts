// src/data/presetCommands.ts —— 内置的「常用指令」清单（快捷指令弹窗的第一页）。
//
// 为什么放在前端而不是后端：这是一份**纯展示内容**（发什么命令、是否自动回车），不涉及
// 任何凭据或主机信息，放进后端只会多一个接口和一些没人读的持久化文件。清单按用途分组，
// 名字与分组名都走 i18n（`qc_p_<id>` / `qc_g_<group>`），因此中英文都能显示。
//
// **auto 的取舍**：只读命令一律 `auto: true`（点一下就把结果打出来，这是这个页面存在的
// 意义）；会改变系统状态的命令 `auto: false`——用户点一下只把命令**填进终端**，回车由他
// 自己按（现存唯一一条是「重启 SSH 服务」）。
//
// 第十一轮删掉了原先的「危险操作」分组（`reboot` / `poweroff`）与配套的警示色渲染：关机/重启
// 这种命令不该出现在「点一下就发」的面板里（见 CONTRACT 21.2）。分组类型、分组顺序、以及
// 弹窗里那套 `isDanger` 判据一并删掉——留一个永远为空的危险组就是死代码。
export type PresetGroup = 'sys' | 'proc' | 'disk' | 'net' | 'log' | 'svc' | 'user' | 'docker'

export interface PresetCmd {
  /** 稳定 id：i18n key 是 `qc_p_<id>` */
  id: string
  group: PresetGroup
  /** 命令原文（可能是复合命令，用 `2>/dev/null`、`||` 兜住不同发行版的差异） */
  content: string
  /** 点击后是否自动回车执行 */
  auto: boolean
}

/** 分组顺序即界面上的显示顺序。 */
export const PRESET_GROUPS: PresetGroup[] = [
  'sys',
  'proc',
  'disk',
  'net',
  'log',
  'svc',
  'user',
  'docker'
]

export const PRESET_COMMANDS: PresetCmd[] = [
  // ---- 系统信息 ----
  { id: 'uname', group: 'sys', content: 'uname -a', auto: true },
  { id: 'os_release', group: 'sys', content: 'cat /etc/os-release', auto: true },
  { id: 'uptime', group: 'sys', content: 'uptime', auto: true },
  { id: 'lscpu', group: 'sys', content: 'lscpu', auto: true },
  { id: 'free', group: 'sys', content: 'free -h', auto: true },
  { id: 'lsblk', group: 'sys', content: 'lsblk -f', auto: true },
  { id: 'df', group: 'sys', content: 'df -h', auto: true },

  // ---- 资源与进程 ----
  { id: 'top', group: 'proc', content: 'top -bn1 | head -25', auto: true },
  { id: 'ps_mem', group: 'proc', content: 'ps aux --sort=-%mem | head -20', auto: true },
  { id: 'ps_cpu', group: 'proc', content: 'ps aux --sort=-%cpu | head -20', auto: true },

  // ---- 磁盘与目录 ----
  { id: 'du', group: 'disk', content: 'du -sh * 2>/dev/null | sort -h', auto: true },
  { id: 'du_depth', group: 'disk', content: 'du -h --max-depth=1 2>/dev/null | sort -h', auto: true },
  {
    id: 'big_files',
    group: 'disk',
    content: "find . -xdev -type f -size +100M -exec ls -lh {} + 2>/dev/null | head -20",
    auto: true
  },

  // ---- 网络 ----
  { id: 'ip_addr', group: 'net', content: 'ip -br a', auto: true },
  { id: 'ss_listen', group: 'net', content: 'ss -tulnp', auto: true },
  { id: 'ping', group: 'net', content: 'ping -c 4 223.5.5.5', auto: true },
  { id: 'route', group: 'net', content: 'ip route', auto: true },

  // ---- 日志 ----
  { id: 'journal_tail', group: 'log', content: 'journalctl -n 100 --no-pager', auto: true },
  { id: 'journal_err', group: 'log', content: 'journalctl -p err -n 50 --no-pager', auto: true },
  { id: 'dmesg_tail', group: 'log', content: 'dmesg | tail -50', auto: true },

  // ---- 服务 ----
  {
    id: 'systemd_running',
    group: 'svc',
    content: 'systemctl list-units --type=service --state=running --no-pager',
    auto: true
  },
  { id: 'systemd_failed', group: 'svc', content: 'systemctl --failed --no-pager', auto: true },
  {
    id: 'systemd_status_ssh',
    group: 'svc',
    content: 'systemctl status ssh --no-pager 2>/dev/null || systemctl status sshd --no-pager',
    auto: true
  },
  {
    id: 'systemd_restart_ssh',
    group: 'svc',
    content: 'systemctl restart ssh 2>/dev/null || systemctl restart sshd',
    auto: false
  },

  // ---- 用户 ----
  { id: 'who', group: 'user', content: 'who', auto: true },
  { id: 'last', group: 'user', content: 'last -n 10', auto: true },
  { id: 'id', group: 'user', content: 'id', auto: true },

  // ---- Docker ----
  { id: 'docker_ps', group: 'docker', content: 'docker ps -a', auto: true },
  { id: 'docker_stats', group: 'docker', content: 'docker stats --no-stream', auto: true },
  { id: 'docker_df', group: 'docker', content: 'docker system df', auto: true }
]

/** 按分组切好、且保持 PRESET_GROUPS 顺序，便于模板直接渲染。 */
export function presetByGroup(): { group: PresetGroup; items: PresetCmd[] }[] {
  return PRESET_GROUPS.map((group) => ({
    group,
    items: PRESET_COMMANDS.filter((c) => c.group === group)
  })).filter((g) => g.items.length > 0)
}

export const presetNameKey = (id: string) => `qc_p_${id}`
export const presetGroupKey = (group: PresetGroup) => `qc_g_${group}`
