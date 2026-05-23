// keypool.go — 多 API Key 池状态机：客户端亲和、耗尽检测、冷却恢复。
package proxy

import (
	"log/slog"
	"sync"
	"time"
)

// keyEntry 表示 key 池中一个 key 的运行时状态。
type keyEntry struct {
	key           string
	exhausted     bool
	exhaustedAt   time.Time
	exhaustReason string    // 耗尽原因
	cooldownEnd   time.Time // 冷却结束时间
	blocked       bool      // 手动屏蔽（永久，直到手动解除）
}

// keyPool 管理一个 target 的多个 API key。
// 支持客户端亲和（hash 绑定）、耗尽检测、冷却恢复。
// 适用于所有 provider 类型。
type keyPool struct {
	mu         sync.Mutex
	entries    []keyEntry
	cooldown   time.Duration // 默认冷却（非 quota/rate_limited）
	resetTime  time.Time     // 额度重置时间点（零值表示未配置）
	logger     *slog.Logger
	targetName string
}

func newKeyPool(targetName string, keys []string, cooldownSecs int, resetTimeStr string, logger *slog.Logger) *keyPool {
	if len(keys) == 0 {
		return nil
	}
	if logger == nil {
		logger = slog.Default()
	}
	cooldown := time.Duration(cooldownSecs) * time.Second
	if cooldown <= 0 {
		cooldown = 1800 * time.Second
	}
	
	// 解析 resetTimeStr
	var resetTime time.Time
	cst := time.FixedZone("CST", 8*3600)
	if resetTimeStr != "" {
		if t, err := time.ParseInLocation("2006-01-02", resetTimeStr, cst); err == nil {
			resetTime = t
		} else if t, err := time.ParseInLocation("2006-01-02 15:04", resetTimeStr, cst); err == nil {
			resetTime = t
		} else {
			logger.Warn("[keypool] invalid reset_time format, ignoring",
				"target", targetName,
				"reset_time", resetTimeStr,
				"error", err,
			)
		}
	}
	
	entries := make([]keyEntry, len(keys))
	for i, k := range keys {
		entries[i] = keyEntry{key: k}
	}
	return &keyPool{
		entries:    entries,
		cooldown:   cooldown,
		resetTime:  resetTime,
		logger:     logger,
		targetName: targetName,
	}
}

// selectKey 按顺序返回第一个可用的 key。
// 如果全部 exhausted，检查冷却期是否已过，过了则恢复。
// 返回 (key, index)。如果无可用 key 返回 ("", -1)。
func (p *keyPool) selectKey() (string, int) {
	if p == nil || len(p.entries) == 0 {
		return "", -1
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	now := time.Now()

	// 第一轮：找第一个 active 的
	for i := range p.entries {
		if !p.entries[i].exhausted && !p.entries[i].blocked {
			return p.entries[i].key, i
		}
	}

	// 第二轮：全部 exhausted/blocked，找冷却期已过的最旧的（跳过 blocked）
	for i := range p.entries {
		if p.entries[i].blocked {
			continue
		}
		if now.After(p.entries[i].cooldownEnd) || now.Equal(p.entries[i].cooldownEnd) {
			p.entries[i].exhausted = false
			p.entries[i].exhaustedAt = time.Time{}
			p.entries[i].exhaustReason = ""
			p.entries[i].cooldownEnd = time.Time{}
			maskKey := maskAPIKey(p.entries[i].key)
			p.logger.Info("[keypool] key recovered",
				"target", p.targetName,
				"key_index", i,
				"key", maskKey,
				"reason", "cooldown_expired",
			)
			return p.entries[i].key, i
		}
	}

	// 全部在冷却中
	return "", -1
}

// selectKeyForClient 按客户端名称做 hash 亲和，保证同一客户端倾向走同一个 key。
// 如果绑定的 key 已 exhausted，溢出到下一个 active key。
// 全部 exhausted 时走冷却恢复逻辑（与 selectKey 一致）。
// clientName 为空时退化为 selectKey（顺序消费）。
func (p *keyPool) selectKeyForClient(clientName string) (string, int) {
	if p == nil || len(p.entries) == 0 {
		return "", -1
	}
	if clientName == "" {
		return p.selectKey()
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	now := time.Now()
	n := len(p.entries)

	// 1. 计算亲和 key index
	preferred := int(hashClientKey(clientName, p.targetName) % uint32(n))

	// 2. 优先用亲和 key（如果 active 且未 blocked）
	if !p.entries[preferred].exhausted && !p.entries[preferred].blocked {
		return p.entries[preferred].key, preferred
	}

	// 3. 亲和 key exhausted/blocked，溢出：从 preferred+1 开始找第一个 active
	for offset := 1; offset < n; offset++ {
		idx := (preferred + offset) % n
		if !p.entries[idx].exhausted && !p.entries[idx].blocked {
			maskKey := maskAPIKey(p.entries[idx].key)
			p.logger.Info("[keypool] affinity overflow",
				"target", p.targetName,
				"client", clientName,
				"preferred_key_index", preferred,
				"actual_key_index", idx,
				"actual_key", maskKey,
			)
			return p.entries[idx].key, idx
		}
	}

	// 4. 全部 exhausted/blocked，找冷却期已过的最旧的（跳过 blocked）
	for offset := 0; offset < n; offset++ {
		idx := (preferred + offset) % n
		if p.entries[idx].blocked {
			continue
		}
		if now.After(p.entries[idx].cooldownEnd) || now.Equal(p.entries[idx].cooldownEnd) {
			p.entries[idx].exhausted = false
			p.entries[idx].exhaustedAt = time.Time{}
			p.entries[idx].exhaustReason = ""
			p.entries[idx].cooldownEnd = time.Time{}
			maskKey := maskAPIKey(p.entries[idx].key)
			p.logger.Info("[keypool] key recovered",
				"target", p.targetName,
				"key_index", idx,
				"key", maskKey,
				"reason", "cooldown_expired",
				"client", clientName,
			)
			return p.entries[idx].key, idx
		}
	}

	// 5. 全部在冷却中
	return "", -1
}

// hashClientKey 对 clientName + targetName 做 FNV-1a hash，保证同一客户端在同一 target 下映射到固定 key。
// 内联实现以避免 fnv.New32a() 的堆分配（hash.Hash 接口逃逸）。
func hashClientKey(clientName, targetName string) uint32 {
	const (
		offset32 = uint32(2166136261)
		prime32  = uint32(16777619)
	)
	h := offset32
	for i := 0; i < len(clientName); i++ {
		h ^= uint32(clientName[i])
		h *= prime32
	}
	h *= prime32 // separator byte 0x00
	for i := 0; i < len(targetName); i++ {
		h ^= uint32(targetName[i])
		h *= prime32
	}
	return h
}

// markExhausted 标记指定 index 的 key 为耗尽。
func (p *keyPool) markExhausted(index int, errorCode string) {
	if p == nil || index < 0 || index >= len(p.entries) {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.entries[index].exhausted {
		return // 已标记，不重复
	}

	now := time.Now()
	p.entries[index].exhausted = true
	p.entries[index].exhaustedAt = now
	p.entries[index].exhaustReason = errorCode
	
	// 按 errorCode 计算冷却结束时间
	switch errorCode {
	case "rate_limited":
		p.entries[index].cooldownEnd = now.Add(60 * time.Second)
	case "quota_exceeded":
		if !p.resetTime.IsZero() && p.resetTime.After(now) {
			p.entries[index].cooldownEnd = p.resetTime
		} else {
			p.entries[index].cooldownEnd = now.Add(p.cooldown)
		}
	default:
		p.entries[index].cooldownEnd = now.Add(p.cooldown)
	}
	
	maskKey := maskAPIKey(p.entries[index].key)
	p.logger.Warn("[keypool] key exhausted",
		"target", p.targetName,
		"key_index", index,
		"key", maskKey,
		"error_code", errorCode,
		"cooldown_seconds", int(p.cooldown.Seconds()),
		"cooldown_end", p.entries[index].cooldownEnd,
	)
}

// blockKey 手动屏蔽指定 key（永久，直到手动解除）。
func (p *keyPool) blockKey(index int) {
	if p == nil || index < 0 || index >= len(p.entries) {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.entries[index].blocked = true
	maskKey := maskAPIKey(p.entries[index].key)
	p.logger.Info("[keypool] key blocked manually",
		"target", p.targetName,
		"key_index", index,
		"key", maskKey,
	)
}

// unblockKey 解除手动屏蔽。
func (p *keyPool) unblockKey(index int) {
	if p == nil || index < 0 || index >= len(p.entries) {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.entries[index].blocked = false
	p.entries[index].exhausted = false
	p.entries[index].exhaustedAt = time.Time{}
	p.entries[index].exhaustReason = ""
	p.entries[index].cooldownEnd = time.Time{}
	maskKey := maskAPIKey(p.entries[index].key)
	p.logger.Info("[keypool] key unblocked manually",
		"target", p.targetName,
		"key_index", index,
		"key", maskKey,
	)
}

// KeyStatus 表示单个 key 的状态快照（用于 admin API）。
type KeyStatus struct {
	Index         int       `json:"index"`
	KeyMask       string    `json:"key_mask"`
	Exhausted     bool      `json:"exhausted"`
	ExhaustReason string    `json:"exhaust_reason,omitempty"`
	ExhaustedAt   time.Time `json:"exhausted_at,omitempty"`
	CooldownEnd   time.Time `json:"cooldown_end,omitempty"`
	Blocked       bool      `json:"blocked"`
	Status        string    `json:"status"` // 已屏蔽 / 额度超限 / 限流中 / 使用中 / 等待中 / 使用过
}

func (p *keyPool) status() []KeyStatus {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	now := time.Now()
	result := make([]KeyStatus, len(p.entries))

	// 找第一个 active key 作为"使用中"
	currentIdx := -1
	for i, e := range p.entries {
		if !e.exhausted && !e.blocked {
			currentIdx = i
			break
		}
	}

	for i, e := range p.entries {
		ks := KeyStatus{
			Index:     i,
			KeyMask:   maskAPIKey(e.key),
			Exhausted: e.exhausted,
			Blocked:   e.blocked,
		}
		if e.exhausted {
			ks.ExhaustReason = e.exhaustReason
			ks.ExhaustedAt = e.exhaustedAt
			ks.CooldownEnd = e.cooldownEnd
		}

		// 计算显示状态
		switch {
		case e.blocked:
			ks.Status = "已屏蔽"
		case e.exhausted && e.exhaustReason == "quota_exceeded":
			ks.Status = "额度超限"
		case e.exhausted && e.exhaustReason == "rate_limited" && now.Before(e.cooldownEnd):
			ks.Status = "限流中"
		case e.exhausted:
			ks.Status = "使用过"
		case i == currentIdx:
			ks.Status = "使用中"
		default:
			ks.Status = "等待中"
		}

		result[i] = ks
	}
	return result
}

// maskAPIKey 遮蔽 key 中间部分，只保留前 6 后 4。
func maskAPIKey(key string) string {
	if len(key) <= 10 {
		return "***"
	}
	return key[:6] + "***" + key[len(key)-4:]
}
