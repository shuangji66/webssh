package main

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
	"webssh/backend/internal/store"
)

// ---------------------------------------------------------------------------
// known_hosts.json 的 TOFU（首次信任即记录）
//
//	{ "version": 1,
//	  "hosts": { "10.0.0.1:22": { "keyType": "ssh-ed25519", "fingerprint": "SHA256:…",
//	                             "publicKey": "<base64 wire format>",
//	                             "addedAt": "2026-01-01T00:00:00Z" } } }
//
// 匹配以 host:port 为键。**不匹配就报错，绝不覆盖**：主机密钥变了既可能是服务器重装，
// 也可能是中间人；静默覆盖等于把中间人攻击变成「一次无感的成功连接」。
// ---------------------------------------------------------------------------

type hostKeyRecord struct {
	KeyType     string `json:"keyType"`
	Fingerprint string `json:"fingerprint"`
	// PublicKey 是 base64 编码的 SSH wire format 公钥。存整把公钥而不是只存指纹：
	// 指纹算法（SHA256）将来若换了、或要支持多密钥算法协商，手上得有原始材料。
	PublicKey string `json:"publicKey"`
	AddedAt   string `json:"addedAt"`
}

type knownHostsFile struct {
	Version int                      `json:"version"`
	Hosts   map[string]hostKeyRecord `json:"hosts"`
}

// hostKeyView 是 /api/hostkeys 与 HostKeyRecord 的响应形态（host/port 分开给前端用）。
type hostKeyView struct {
	Host        string `json:"host"`
	Port        int    `json:"port"`
	KeyType     string `json:"keyType"`
	Fingerprint string `json:"fingerprint"`
	AddedAt     string `json:"addedAt"`
}

type hostKeyStore struct {
	mu    sync.Mutex
	st    *store.Store
	hosts map[string]hostKeyRecord
}

// hostKeyID 是 known_hosts 的键：`host:port`。IPv6 用方括号，与 ssh 客户端一致。
func hostKeyID(host string, port int) string {
	if port == 0 {
		port = 22
	}
	return net.JoinHostPort(strings.TrimSpace(host), strconv.Itoa(port))
}

// splitHostKeyID 把 `host:port` 拆回来（给前端展示用）；拆不出来时按整串当 host、端口 22。
func splitHostKeyID(id string) (string, int) {
	host, portStr, err := net.SplitHostPort(id)
	if err != nil {
		return id, 22
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port <= 0 {
		port = 22
	}
	return host, port
}

func newHostKeyStore(st *store.Store) (*hostKeyStore, error) {
	h := &hostKeyStore{st: st, hosts: map[string]hostKeyRecord{}}
	var f knownHostsFile
	err := st.ReadJSON(st.KnownHostsPath(), &f)
	switch {
	case err == nil:
		if f.Hosts != nil {
			h.hosts = f.Hosts
		}
	case errors.Is(err, os.ErrNotExist):
		// 第一次运行：还没有任何信任记录，正常状态，不记日志。
	default:
		// 文件存在但读不了 / 解析失败：**不能**当成「没有信任记录」继续跑，
		// 否则下次连接会把 hostkey_mismatch 降级成 hostkey_unknown，用户被引导着
		// 重新信任一把可疑的密钥。这里直接把错误抛给启动流程。
		return nil, fmt.Errorf("读取 %s 失败：%w", st.KnownHostsPath(), err)
	}
	return h, nil
}

func (h *hostKeyStore) lookup(id string) (hostKeyRecord, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	rec, ok := h.hosts[id]
	return rec, ok
}

// add 记录（或覆盖）一条信任。覆盖只在用户显式移除旧记录后重新信任时发生，
// 由上层（/api/sessions/trust）保证——store 这一层不做「是否可疑」的判断。
func (h *hostKeyStore) add(id string, rec hostKeyRecord) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.hosts[id] = rec
	return h.flushLocked()
}

func (h *hostKeyStore) forget(host string, port int) error {
	id := hostKeyID(host, port)
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.hosts[id]; !ok {
		return os.ErrNotExist
	}
	delete(h.hosts, id)
	return h.flushLocked()
}

func (h *hostKeyStore) list() []hostKeyView {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]hostKeyView, 0, len(h.hosts))
	for id, rec := range h.hosts {
		host, port := splitHostKeyID(id)
		out = append(out, hostKeyView{
			Host:        host,
			Port:        port,
			KeyType:     rec.KeyType,
			Fingerprint: rec.Fingerprint,
			AddedAt:     rec.AddedAt,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Host != out[j].Host {
			return out[i].Host < out[j].Host
		}
		return out[i].Port < out[j].Port
	})
	return out
}

func (h *hostKeyStore) flushLocked() error {
	return h.st.WriteJSON(h.st.KnownHostsPath(), knownHostsFile{Version: 1, Hosts: h.hosts}, 0o600)
}

// recordOf 把本次握手看到的主机密钥转成记录形态。
func recordOf(key ssh.PublicKey) hostKeyRecord {
	return hostKeyRecord{
		KeyType:     key.Type(),
		Fingerprint: ssh.FingerprintSHA256(key),
		PublicKey:   base64.StdEncoding.EncodeToString(key.Marshal()),
		AddedAt:     time.Now().Format(time.RFC3339),
	}
}

// ---------------------------------------------------------------------------
// 待确认的主机密钥
// ---------------------------------------------------------------------------

// pendingKeyTTL 是「刚扫到、还没被用户确认」的公钥的保留时长。
const pendingKeyTTL = 15 * time.Minute

// pendingKeys 暂存测试连接时扫到的主机公钥。
//
// 为什么需要它：/api/sessions/trust 的请求体只有 {host, port, fingerprint}（前端手上
// 只有指纹），而 known_hosts 里要存完整的 publicKey。这里按 host:port 暂存扫描结果，
// trust 时按指纹认领——指纹对不上就拒绝，避免用户「随手点信任」放行一把没确认过的密钥。
// 带 TTL：几小时前的扫描结果不该还能被信任。
type pendingKeys struct {
	mu    sync.Mutex
	ttl   time.Duration
	items map[string]pendingEntry
}

type pendingEntry struct {
	rec hostKeyRecord
	at  time.Time
}

func newPendingKeys() *pendingKeys {
	return &pendingKeys{ttl: pendingKeyTTL, items: map[string]pendingEntry{}}
}

func (p *pendingKeys) put(id string, rec hostKeyRecord) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	for k, v := range p.items {
		if now.Sub(v.at) > p.ttl {
			delete(p.items, k)
		}
	}
	p.items[id] = pendingEntry{rec: rec, at: now}
}

// take 认领一条待确认记录：要求指纹与用户确认的一致，否则返回 false。
// 认领成功后条目即被移除（信任是一次性动作，重复点击应当报 not_found 而不是再次写入）。
func (p *pendingKeys) take(id, fingerprint string) (hostKeyRecord, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	entry, ok := p.items[id]
	if !ok || time.Since(entry.at) > p.ttl {
		delete(p.items, id)
		return hostKeyRecord{}, false
	}
	if entry.rec.Fingerprint != fingerprint {
		return hostKeyRecord{}, false
	}
	delete(p.items, id)
	return entry.rec, true
}
