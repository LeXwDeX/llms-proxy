// keypool.go — 多 API Key 池状态机：顺序消费、耗尽检测、冷却恢复。
package proxy

import (
	"log/slog"
	"sync"
	"time"
)

// keyEntry 表示 key 池中一个 key 的运行时状态。
type keyEntry struct {
	key         string
	exhausted   bool
	exhaustedAt time.Time
}

// keyPool 管理一个 target 的多个 API key，按顺序消费。
type keyPool struct {
	mu         sync.Mutex
	entries    []keyEntry
	cooldown   time.Duration
	logger     *slog.Logger
	targetName string
}

func newKeyPool(targetName string, keys []string, cooldownSecs int, logger *slog.Logger) *keyPool {
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
	entries := make([]keyEntry, len(keys))
	for i, k := range keys {
		entries[i] = keyEntry{key: k}
	}
	return &keyPool{
		entries:    entries,
		cooldown:   cooldown,
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
		if !p.entries[i].exhausted {
			return p.entries[i].key, i
		}
	}

	// 第二轮：全部 exhausted，找冷却期已过的最旧的
	for i := range p.entries {
		if now.Sub(p.entries[i].exhaustedAt) >= p.cooldown {
			p.entries[i].exhausted = false
			p.entries[i].exhaustedAt = time.Time{}
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

	p.entries[index].exhausted = true
	p.entries[index].exhaustedAt = time.Now()
	maskKey := maskAPIKey(p.entries[index].key)
	p.logger.Warn("[keypool] key exhausted",
		"target", p.targetName,
		"key_index", index,
		"key", maskKey,
		"error_code", errorCode,
		"cooldown_seconds", int(p.cooldown.Seconds()),
	)
}

// resetAll 重置所有 key 为 active（config reload / admin 手动重置时调用）。
func (p *keyPool) resetAll() {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := range p.entries {
		p.entries[i].exhausted = false
		p.entries[i].exhaustedAt = time.Time{}
	}
	p.logger.Info("[keypool] all keys reset",
		"target", p.targetName,
		"count", len(p.entries),
	)
}

// KeyStatus 表示单个 key 的状态快照（用于 admin API）。
type KeyStatus struct {
	Index       int       `json:"index"`
	KeyMask     string    `json:"key_mask"`
	Exhausted   bool      `json:"exhausted"`
	ExhaustedAt time.Time `json:"exhausted_at,omitempty"`
	CooldownEnd time.Time `json:"cooldown_end,omitempty"`
}

func (p *keyPool) status() []KeyStatus {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	result := make([]KeyStatus, len(p.entries))
	for i, e := range p.entries {
		ks := KeyStatus{
			Index:     i,
			KeyMask:   maskAPIKey(e.key),
			Exhausted: e.exhausted,
		}
		if e.exhausted {
			ks.ExhaustedAt = e.exhaustedAt
			ks.CooldownEnd = e.exhaustedAt.Add(p.cooldown)
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
