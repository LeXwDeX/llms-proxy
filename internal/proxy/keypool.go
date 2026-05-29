// keypool.go — 多 API Key 池状态机：客户端亲和、耗尽检测、冷却恢复。
package proxy

import (
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// tokenSample 记录一次请求的 token 用量和时间戳，用于滑动窗口统计。
type tokenSample struct {
	at     time.Time
	tokens int64 // input + output
}

// keyEntry 表示 key 池中一个 key 的运行时状态。
type keyEntry struct {
	key           string
	exhausted     bool
	exhaustedAt   time.Time
	exhaustReason string        // 耗尽原因
	cooldownEnd   time.Time     // 冷却结束时间
	blocked       bool          // 手动屏蔽（永久，直到手动解除）
	tokenSamples  []tokenSample // 滑动窗口内的 token 用量样本
	lastError     string        // 最近一次触发耗尽的上游错误响应体（截断），用于诊断
	lastErrorAt   time.Time     // 最近一次错误时间
}

// keyPool 管理一个 target 的多个 API key。
// 支持轮询分配 + 客户端亲和（首次分配后记忆绑定）、耗尽检测、冷却恢复。
// 支持 token 感知调度：新客户端优先分配到滑动窗口内累计 token 最少的 key。
// 支持唤醒模型：所有 key 不可用时，单例化探测被屏蔽的 key 是否恢复。
// 适用于所有 provider 类型。
type keyPool struct {
	mu             sync.Mutex
	entries        []keyEntry
	resetTime      time.Time // 额度重置时间点（零值表示未配置）
	logger         *slog.Logger
	targetName     string
	rrCounter      uint32         // 轮询计数器，新客户端按此值分配 key
	clientAffinity map[string]int // 客户端 → 已分配的 key index（记忆绑定）
	tokenWindow    time.Duration  // token 统计滑动窗口时长（默认 3 分钟）

	// #3 并发削峰：每个 key 当前在途请求数（least-connections 调度信号，实时性优于滞后的 token 窗口）
	inFlight []int64

	// #2 绑定再平衡 / #5 绑定淘汰
	clientAffinityAt   map[string]time.Time // 客户端 → 绑定时间戳
	rebalanceInterval  time.Duration        // 已绑定客户端的再平衡间隔（默认 30s）
	affinityTTL        time.Duration        // 绑定空闲淘汰 TTL（默认 10min）
	maxAffinityEntries int                  // clientAffinity 容量上限（默认 10000）

	// 唤醒模型：所有 key 不可用时，单例化探测被屏蔽的 key 是否恢复
	wakeUpCooldown   time.Time   // 唤醒冷却时间（1分钟内不重复触发）
	wakeUpInProgress atomic.Bool // 单例标记（防止并发惊群）
}

func newKeyPool(targetName string, keys []string, resetTimeStr string, logger *slog.Logger) *keyPool {
	if len(keys) == 0 {
		return nil
	}
	if logger == nil {
		logger = slog.Default()
	}

	// 解析 resetTimeStr
	var resetTime time.Time
	cst := time.FixedZone("CST", 8*3600)
	if resetTimeStr != "" {
		// 支持周期性格式：monthly:23（每月23号）
		if strings.HasPrefix(resetTimeStr, "monthly:") {
			dayStr := strings.TrimPrefix(resetTimeStr, "monthly:")
			if day, err := strconv.Atoi(dayStr); err == nil && day >= 1 && day <= 31 {
				// 计算下一个重置日期
				now := time.Now().In(cst)
				next := time.Date(now.Year(), now.Month(), day, 0, 0, 0, 0, cst)
				if !next.After(now) {
					// 如果本月已过，推到下月
					next = next.AddDate(0, 1, 0)
				}
				resetTime = next
			} else {
				logger.Warn("[keypool] invalid monthly reset_time format, ignoring",
					"target", targetName,
					"reset_time", resetTimeStr,
				)
			}
		} else if t, err := time.ParseInLocation("2006-01-02", resetTimeStr, cst); err == nil {
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
		entries:        entries,
		resetTime:      resetTime,
		logger:         logger,
		targetName:     targetName,
		clientAffinity: make(map[string]int),
		tokenWindow:    3 * time.Minute, // 默认 3 分钟滑动窗口

		inFlight:           make([]int64, len(keys)),
		clientAffinityAt:   make(map[string]time.Time),
		rebalanceInterval:  30 * time.Second,
		affinityTTL:        10 * time.Minute,
		maxAffinityEntries: 10000,
	}
}

// selectKey 按顺序返回第一个可用的 key。
// 如果全部 exhausted，检查冷却期是否已过，过了则恢复。
// 绝境探测：如果所有 key 都在冷却中，选冷却剩余最短的 key 做探测（返回该 key，但不自动恢复）。
// 返回 (key, index)。如果无可用 key 返回 ("", -1)。
func (p *keyPool) selectKey() (string, int) {
	if p == nil || len(p.entries) == 0 {
		return "", -1
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	now := time.Now()
	n := len(p.entries)

	// 第一轮：从轮询位置开始找第一个 active 的（#4 均衡：避免重试/探测总是回到 key-0）
	start := int(p.rrCounter) % n
	for i := 0; i < n; i++ {
		idx := (start + i) % n
		if !p.entries[idx].exhausted && !p.entries[idx].blocked {
			p.rrCounter++
			return p.entries[idx].key, idx
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

	// 第三轮：绝境探测 — 所有 key 都在冷却中，选冷却剩余最短的做探测
	// 探测成功由调用方负责恢复 key（markSuccess），失败则延长冷却
	bestIdx := -1
	shortestRemaining := time.Duration(0)
	for i := range p.entries {
		if p.entries[i].blocked {
			continue
		}
		// 跳过 rate_limited（同账号共享配额，探测也会被限）
		if p.entries[i].exhaustReason == "rate_limited" {
			continue
		}
		remaining := p.entries[i].cooldownEnd.Sub(now)
		if bestIdx == -1 || remaining < shortestRemaining {
			bestIdx = i
			shortestRemaining = remaining
		}
	}
	if bestIdx >= 0 {
		maskKey := maskAPIKey(p.entries[bestIdx].key)
		p.logger.Warn("[keypool] desperation probe: all keys exhausted, probing key with shortest cooldown",
			"target", p.targetName,
			"key_index", bestIdx,
			"key", maskKey,
			"cooldown_remaining_ms", shortestRemaining.Milliseconds(),
		)
		return p.entries[bestIdx].key, bestIdx
	}

	// 全部在冷却中（包括 rate_limited）
	return "", -1
}

// selectKeyForClient 按轮询分配 + 客户端亲和选择 key。
//
// 分配策略：
//  1. 已有亲和绑定 → 复用绑定的 key（如果 active）
//  2. 新客户端 → 优先选滑动窗口内累计 token 最少的 active key（token 感知调度）；
//     如果无 token 数据（刚启动），退化为轮询分配
//  3. 绑定的 key exhausted/blocked → 溢出到下一个 active key，更新亲和绑定
//  4. 全部 exhausted → 冷却恢复（与 selectKey 一致）
//
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

	// 1. 检查已有亲和绑定
	if preferred, ok := p.clientAffinity[clientName]; ok && preferred >= 0 && preferred < n {
		if !p.entries[preferred].exhausted && !p.entries[preferred].blocked {
			// #2 再平衡：绑定存活，但若超过 rebalanceInterval 未重新评估，
			// 且存在负载更低的 active key，则迁移绑定（least-connections + least-token）。
			// 仅在确有更优 key 时迁移，避免无意义抖动。
			boundAt := p.clientAffinityAt[clientName]
			if now.Sub(boundAt) >= p.rebalanceInterval {
				best := p.bestActiveKeyLocked(now)
				if best >= 0 && best != preferred && p.scoreLessLocked(best, preferred, now) {
					p.logger.Info("[keypool] affinity rebalanced",
						"target", p.targetName,
						"client", clientName,
						"prev_key_index", preferred,
						"new_key_index", best,
					)
					preferred = best
				}
				p.bindLocked(clientName, preferred, now)
			}
			return p.entries[preferred].key, preferred
		}
		// 绑定的 key 不可用，走溢出
	}

	// 2. 新客户端：优先 least-connections + least-token 调度，无任何负载信号时退化为轮询
	if _, ok := p.clientAffinity[clientName]; !ok {
		bestIdx := -1
		hasSignal := false // 有在途请求 或 有 token 数据
		for i := 0; i < n; i++ {
			if p.entries[i].exhausted || p.entries[i].blocked {
				continue
			}
			if p.inFlight[i] > 0 || p.tokenSumInWindow(i, now) > 0 {
				hasSignal = true
			}
			if bestIdx == -1 || p.scoreLessLocked(i, bestIdx, now) {
				bestIdx = i
			}
		}
		if bestIdx >= 0 {
			if hasSignal {
				// 负载感知调度：选在途最少、其次累计 token 最少的
				p.bindLocked(clientName, bestIdx, now)
				return p.entries[bestIdx].key, bestIdx
			}
			// 无负载信号（刚启动 / 全空闲），退化为轮询
			for i := 0; i < n; i++ {
				idx := int(p.rrCounter) % n
				p.rrCounter++
				if !p.entries[idx].exhausted && !p.entries[idx].blocked {
					p.bindLocked(clientName, idx, now)
					return p.entries[idx].key, idx
				}
			}
		}
		// 所有 key 都 exhausted/blocked，跳过轮询分配，走冷却恢复
	}

	// 3. 溢出：从当前位置开始找下一个 active key
	start := int(p.rrCounter) % n
	for i := 0; i < n; i++ {
		idx := (start + i) % n
		if !p.entries[idx].exhausted && !p.entries[idx].blocked {
			p.bindLocked(clientName, idx, now)
			maskKey := maskAPIKey(p.entries[idx].key)
			p.logger.Info("[keypool] affinity overflow",
				"target", p.targetName,
				"client", clientName,
				"actual_key_index", idx,
				"actual_key", maskKey,
			)
			return p.entries[idx].key, idx
		}
	}

	// 4. 全部 exhausted/blocked，找冷却期已过的（跳过 blocked）
	for i := 0; i < n; i++ {
		if p.entries[i].blocked {
			continue
		}
		if now.After(p.entries[i].cooldownEnd) || now.Equal(p.entries[i].cooldownEnd) {
			p.entries[i].exhausted = false
			p.entries[i].exhaustedAt = time.Time{}
			p.entries[i].exhaustReason = ""
			p.entries[i].cooldownEnd = time.Time{}
			p.bindLocked(clientName, i, now)
			maskKey := maskAPIKey(p.entries[i].key)
			p.logger.Info("[keypool] key recovered",
				"target", p.targetName,
				"key_index", i,
				"key", maskKey,
				"reason", "cooldown_expired",
				"client", clientName,
			)
			return p.entries[i].key, i
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
	p.markExhaustedWithError(index, errorCode, "")
}

// markExhaustedWithError 标记 key 耗尽，并记录触发该耗尽的上游错误响应体（截断），用于诊断。
func (p *keyPool) markExhaustedWithError(index int, errorCode, errBody string) {
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
	if errBody != "" {
		p.entries[index].lastError = truncateErrBody(errBody)
		p.entries[index].lastErrorAt = now
	}

	// 按 errorCode 计算冷却结束时间
	// quota_exceeded → 根据 resetTime 拆分为两种子类型：
	//   - 有 resetTime → quota_exceeded_subscription（自动恢复）
	//   - 无 resetTime → quota_exceeded_api（人工恢复，永久屏蔽）
	// rate_limited → 5 秒冷却（限流是临时的，短冷却后重试）
	// invalid_token / billing_error / account_disabled → 5 分钟冷却（防止误判导致永久屏蔽，支持唤醒恢复）
	// 其他错误 → 永久屏蔽（需手动解除）
	permanentEnd := time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)
	switch errorCode {
	case "quota_exceeded":
		if !p.resetTime.IsZero() && p.resetTime.After(now) {
			p.entries[index].exhaustReason = "quota_exceeded_subscription"
			p.entries[index].cooldownEnd = p.resetTime
		} else {
			p.entries[index].exhaustReason = "quota_exceeded_api"
			p.entries[index].cooldownEnd = permanentEnd
		}
	case "rate_limited":
		p.entries[index].exhaustReason = errorCode
		p.entries[index].cooldownEnd = now.Add(5 * time.Second)
	case "invalid_token", "billing_error", "account_disabled":
		p.entries[index].exhaustReason = errorCode
		p.entries[index].cooldownEnd = now.Add(5 * time.Minute)
	default:
		p.entries[index].exhaustReason = errorCode
		p.entries[index].cooldownEnd = permanentEnd
	}

	maskKey := maskAPIKey(p.entries[index].key)
	p.logger.Warn("[keypool] key exhausted",
		"target", p.targetName,
		"key_index", index,
		"key", maskKey,
		"error_code", errorCode,
		"cooldown_end", p.entries[index].cooldownEnd,
		"upstream_error", p.entries[index].lastError,
	)
}

// truncateErrBody 截断上游错误响应体，避免日志/状态膨胀。
func truncateErrBody(s string) string {
	const maxLen = 512
	s = strings.TrimSpace(s)
	if len(s) > maxLen {
		return s[:maxLen] + "...(truncated)"
	}
	return s
}

// markRateLimited 标记指定 key 为限流状态（5 秒冷却）。
// 与 markExhausted 不同，限流是临时的，冷却后自动恢复。
func (p *keyPool) markRateLimited(index int) {
	p.markExhausted(index, "rate_limited")
}

// selectNextActiveKey 从指定 key 之后找下一个 active key。
// 返回 (key, index)。如果无可用 key 返回 ("", -1)。
func (p *keyPool) selectNextActiveKey(afterIndex int) (string, int) {
	if p == nil || len(p.entries) == 0 {
		return "", -1
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	now := time.Now()
	n := len(p.entries)

	// 从 afterIndex+1 开始找
	for i := 1; i <= n; i++ {
		idx := (afterIndex + i) % n
		if !p.entries[idx].exhausted && !p.entries[idx].blocked {
			return p.entries[idx].key, idx
		}
	}

	// 全部 exhausted/blocked，找冷却期已过的（跳过 blocked）
	for i := 0; i < n; i++ {
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

	return "", -1
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

// markRecovered 标记指定 key 为已恢复（绝境探测成功后调用）。
// 返回 true 表示状态发生了变化（key 由 exhausted 转为 active），
// 返回 false 表示该 key 本就不是 exhausted（含纯手动 blocked），未做任何改动。
func (p *keyPool) markRecovered(index int) bool {
	if p == nil || index < 0 || index >= len(p.entries) {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.entries[index].exhausted {
		return false // 已经是 active 状态（或仅手动 blocked），无需恢复
	}
	p.entries[index].exhausted = false
	p.entries[index].exhaustedAt = time.Time{}
	p.entries[index].exhaustReason = ""
	p.entries[index].cooldownEnd = time.Time{}
	maskKey := maskAPIKey(p.entries[index].key)
	p.logger.Info("[keypool] key recovered after desperation probe",
		"target", p.targetName,
		"key_index", index,
		"key", maskKey,
	)
	return true
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
	LastError     string    `json:"last_error,omitempty"`    // 最近一次触发耗尽的上游错误响应体（截断）
	LastErrorAt   time.Time `json:"last_error_at,omitempty"` // 最近一次错误时间
	Status        string    `json:"status"`                  // 已屏蔽 / 额度超限 / 限流中 / 使用中 / 等待中 / 使用过
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
		if e.lastError != "" {
			ks.LastError = e.lastError
			ks.LastErrorAt = e.lastErrorAt
		}

		// 计算显示状态
		switch {
		case e.blocked:
			ks.Status = "已屏蔽"
		case e.exhausted && e.exhaustReason == "quota_exceeded_subscription":
			ks.Status = "额度超限(自动恢复)"
		case e.exhausted && e.exhaustReason == "quota_exceeded_api":
			ks.Status = "额度超限(人工恢复)"
		case e.exhausted && e.exhaustReason == "quota_exceeded":
			// 向后兼容：旧的 quota_exceeded 状态
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

// recordTokens 记录指定 key 的一次请求 token 用量，用于滑动窗口负载均衡。
func (p *keyPool) recordTokens(keyIndex int, tokens int64) {
	if p == nil || keyIndex < 0 || keyIndex >= len(p.entries) || tokens <= 0 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	now := time.Now()
	p.entries[keyIndex].tokenSamples = append(p.entries[keyIndex].tokenSamples, tokenSample{at: now, tokens: tokens})
	p.pruneExpiredSamplesLocked(keyIndex, now)
}

// pruneExpiredSamplesLocked 清理指定 key 的过期样本（超出滑动窗口）。
// 调用方必须持有 p.mu。
func (p *keyPool) pruneExpiredSamplesLocked(keyIndex int, now time.Time) {
	samples := p.entries[keyIndex].tokenSamples
	cutoff := now.Add(-p.tokenWindow)
	// 找到第一个未过期的索引
	i := 0
	for i < len(samples) && samples[i].at.Before(cutoff) {
		i++
	}
	if i > 0 {
		p.entries[keyIndex].tokenSamples = samples[i:]
	}
}

// tokenSumInWindow 返回指定 key 在滑动窗口内的累计 token 数。
// 调用方必须持有 p.mu。
func (p *keyPool) tokenSumInWindow(keyIndex int, now time.Time) int64 {
	p.pruneExpiredSamplesLocked(keyIndex, now)
	var sum int64
	for _, s := range p.entries[keyIndex].tokenSamples {
		sum += s.tokens
	}
	return sum
}

// acquireInFlight 在为一次真实转发选中 key 后调用，增加该 key 的在途计数。
func (p *keyPool) acquireInFlight(index int) {
	if p == nil || index < 0 {
		return
	}
	p.mu.Lock()
	if index < len(p.inFlight) {
		p.inFlight[index]++
	}
	p.mu.Unlock()
}

// releaseInFlight 在一次转发结束（响应体关闭或转发出错）时调用，减少在途计数。
// 带下限保护，避免重复释放导致计数变负而永久扭曲调度。
func (p *keyPool) releaseInFlight(index int) {
	if p == nil || index < 0 {
		return
	}
	p.mu.Lock()
	if index < len(p.inFlight) && p.inFlight[index] > 0 {
		p.inFlight[index]--
	}
	p.mu.Unlock()
}

// activeKeyCount 返回当前处于可用状态（未冷却、未耗尽、未阻塞）的 key 数量，
// 供目标层（target.go #1）按真实存活容量加权分配请求。
func (p *keyPool) activeKeyCount() int {
	if p == nil {
		return 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	count := 0
	for i := range p.entries {
		if !p.entries[i].exhausted && !p.entries[i].blocked {
			count++
		}
	}
	return count
}

// scoreLessLocked 比较两个 key 的负载得分：先比在途请求数（least-connections），
// 再比滑动窗口累计 token（least-token）。a 更空闲返回 true。调用方必须持有 p.mu。
func (p *keyPool) scoreLessLocked(a, b int, now time.Time) bool {
	if a < 0 || a >= len(p.entries) {
		return false
	}
	if b < 0 || b >= len(p.entries) {
		return true
	}
	infA, infB := p.inFlight[a], p.inFlight[b]
	if infA != infB {
		return infA < infB
	}
	return p.tokenSumInWindow(a, now) < p.tokenSumInWindow(b, now)
}

// bestActiveKeyLocked 返回当前负载最低（least-connections + least-token）的 active key。
// 无 active key 返回 -1。调用方必须持有 p.mu。
func (p *keyPool) bestActiveKeyLocked(now time.Time) int {
	best := -1
	for i := range p.entries {
		if p.entries[i].exhausted || p.entries[i].blocked {
			continue
		}
		if best == -1 || p.scoreLessLocked(i, best, now) {
			best = i
		}
	}
	return best
}

// bindLocked 建立/更新客户端到 key 的亲和绑定，并记录时间戳。
// 新增绑定前按需淘汰陈旧条目，防止 map 无限增长。调用方必须持有 p.mu。
func (p *keyPool) bindLocked(clientName string, idx int, now time.Time) {
	if _, exists := p.clientAffinity[clientName]; !exists {
		p.evictStaleAffinityLocked(now)
	}
	p.clientAffinity[clientName] = idx
	p.clientAffinityAt[clientName] = now
}

// evictStaleAffinityLocked 在绑定表达到容量上限时淘汰条目：
// 优先淘汰空闲超过 affinityTTL 的绑定；若仍超限，淘汰最旧的一条。
// 调用方必须持有 p.mu。
func (p *keyPool) evictStaleAffinityLocked(now time.Time) {
	if len(p.clientAffinity) < p.maxAffinityEntries {
		return
	}
	// 第一轮：淘汰所有空闲超过 TTL 的绑定
	for name, ts := range p.clientAffinityAt {
		if now.Sub(ts) >= p.affinityTTL {
			delete(p.clientAffinity, name)
			delete(p.clientAffinityAt, name)
		}
	}
	// 第二轮：若仍达上限，淘汰最旧的一条，保证能腾出空间
	for len(p.clientAffinity) >= p.maxAffinityEntries {
		oldestName := ""
		var oldestTS time.Time
		for name, ts := range p.clientAffinityAt {
			if oldestName == "" || ts.Before(oldestTS) {
				oldestName = name
				oldestTS = ts
			}
		}
		if oldestName == "" {
			break
		}
		delete(p.clientAffinity, oldestName)
		delete(p.clientAffinityAt, oldestName)
	}
}

// leastTokenActiveKeyIndex 返回滑动窗口内累计 token 最少的 active key 的索引。
// 如果没有 active key，返回 -1。
// 如果所有 active key 都没有 token 数据（刚启动），返回 -2 表示应退化为轮询。
func (p *keyPool) leastTokenActiveKeyIndex() int {
	if p == nil || len(p.entries) == 0 {
		return -1
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	now := time.Now()
	bestIdx := -1
	bestTokens := int64(-1)
	hasAnyData := false

	for i := range p.entries {
		if p.entries[i].exhausted || p.entries[i].blocked {
			continue
		}
		sum := p.tokenSumInWindow(i, now)
		if sum > 0 {
			hasAnyData = true
		}
		if bestIdx == -1 || sum < bestTokens {
			bestIdx = i
			bestTokens = sum
		}
	}

	if bestIdx == -1 {
		return -1 // 无 active key
	}
	if !hasAnyData {
		return -2 // 有 active key 但无数据，退化为轮询
	}
	return bestIdx
}

// tryWakeUp 尝试触发唤醒模型。
// 返回 true 表示成功获取唤醒锁（调用方应执行探测逻辑），返回 false 表示唤醒冷却中或已有其他协程在执行。
// 调用方必须在探测完成后调用 wakeUpComplete()。
func (p *keyPool) tryWakeUp(now time.Time) bool {
	if p == nil {
		return false
	}

	// 检查冷却
	p.mu.Lock()
	if now.Before(p.wakeUpCooldown) {
		p.mu.Unlock()
		return false
	}
	p.mu.Unlock()

	// 单例检查（原子操作）
	if !p.wakeUpInProgress.CompareAndSwap(false, true) {
		return false // 已有其他协程在执行唤醒
	}

	p.logger.Info("[keypool] wake-up model triggered",
		"target", p.targetName,
	)
	return true
}

// wakeUpComplete 唤醒探测完成后调用，设置冷却并重置单例标记。
func (p *keyPool) wakeUpComplete() {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.wakeUpCooldown = time.Now().Add(1 * time.Minute)
	p.mu.Unlock()
	p.wakeUpInProgress.Store(false)
}

// getExhaustedKeys 返回所有被屏蔽或超额的 key 索引（用于唤醒探测）。
// 按冷却剩余时间从短到长排序，优先探测即将恢复的 key。
func (p *keyPool) getExhaustedKeys() []int {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	now := time.Now()
	type keyWithRemaining struct {
		index     int
		remaining time.Duration
	}
	var exhausted []keyWithRemaining

	for i := range p.entries {
		if p.entries[i].exhausted || p.entries[i].blocked {
			remaining := p.entries[i].cooldownEnd.Sub(now)
			if remaining < 0 {
				remaining = 0
			}
			exhausted = append(exhausted, keyWithRemaining{index: i, remaining: remaining})
		}
	}

	// 按冷却剩余时间排序（短的优先）
	sort.Slice(exhausted, func(i, j int) bool {
		return exhausted[i].remaining < exhausted[j].remaining
	})

	result := make([]int, len(exhausted))
	for i, e := range exhausted {
		result[i] = e.index
	}
	return result
}

// isAccountLevelExhaustion 判定耗尽原因是否为"账户级"失败——即同一账户下所有 key 大概率
// 共享的失效（无效凭证、欠费、封禁、总额度永久耗尽）。这类失败换 key 重试无望，应停止
// 逐 key 重试并把真实上游响应透传给客户端。
//
// 注意：与 isHardFailureReason 不同，这里**不包含** rate_limited（瞬时限流，换 key 或短
// 冷却后重试通常可恢复）与 quota_exceeded_subscription（到 resetTime 自动恢复，按订阅周期
// 计而非账户失效）。
func isAccountLevelExhaustion(reason string) bool {
	switch reason {
	case "invalid_token",
		"account_disabled",
		"billing_error",
		"quota_exceeded_api",
		"Arrearage",
		"free_tier_exhausted",
		"AccessDenied.Unpurchased":
		return true
	default:
		return false
	}
}

// isHardFailureReason 判定某个耗尽原因是否为"硬失败"——即 key/账户本身的问题
// （无效、欠费、封禁、总额度耗尽），只能通过真实请求成功或冷却定时器到期来确认恢复，
// 廉价的 GET /models 探测无法可靠验证（部分上游对任意 key 都放行 /models，会造成假阳性）。
func isHardFailureReason(reason string) bool {
	switch reason {
	case "invalid_token",
		"account_disabled",
		"billing_error",
		"rate_limited",
		"quota_exceeded_api",
		"quota_exceeded_subscription",
		"Arrearage",
		"free_tier_exhausted",
		"AccessDenied.Unpurchased":
		return true
	default:
		return false
	}
}

// exhaustReasonAt 返回指定 key 当前的耗尽原因（未耗尽返回 ""）。供重试收敛逻辑读取。
func (p *keyPool) exhaustReasonAt(index int) string {
	if p == nil || index < 0 {
		return ""
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if index >= len(p.entries) || !p.entries[index].exhausted {
		return ""
	}
	return p.entries[index].exhaustReason
}

// getProbeableExhaustedKeys 返回适合廉价 GET 探测的耗尽 key（剔除硬失败）。
// 硬失败 key 不参与廉价探测，只靠冷却定时器被动恢复，避免假阳性死循环。
func (p *keyPool) getProbeableExhaustedKeys() []int {
	all := p.getExhaustedKeys()
	if len(all) == 0 {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []int
	for _, idx := range all {
		if idx >= 0 && idx < len(p.entries) && !isHardFailureReason(p.entries[idx].exhaustReason) {
			out = append(out, idx)
		}
	}
	return out
}
