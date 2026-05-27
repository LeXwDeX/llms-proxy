package proxy

import (
	"sync"
	"time"
)

const affinityTTL = 5 * time.Minute

type affinityEntry struct {
	targetName string
	lastUsed   time.Time
}

// affinityMap 存储客户端+模型到目标的粘连映射。
type affinityMap struct {
	mu      sync.RWMutex
	entries map[string]affinityEntry
}

func newAffinityMap() *affinityMap {
	return &affinityMap{entries: make(map[string]affinityEntry)}
}

// affinityKey 生成粘连键。
// 使用 IP + 客户端名称 + 模型，确保同一客户端从不同 IP 访问时独立分配。
func affinityKey(clientIP, clientName, model string) string {
	return clientIP + "|" + clientName + ":" + model
}

// Get 查询粘连目标，过期则惰性删除并返回空。
func (a *affinityMap) Get(key string, now time.Time) (string, bool) {
	a.mu.RLock()
	entry, ok := a.entries[key]
	a.mu.RUnlock()
	if !ok {
		return "", false
	}
	if now.Sub(entry.lastUsed) > affinityTTL {
		// 惰性删除过期条目
		a.mu.Lock()
		if e, exists := a.entries[key]; exists && now.Sub(e.lastUsed) > affinityTTL {
			delete(a.entries, key)
		}
		a.mu.Unlock()
		return "", false
	}
	return entry.targetName, true
}

// Set 设置/更新粘连记录。
func (a *affinityMap) Set(key, targetName string, now time.Time) {
	a.mu.Lock()
	a.entries[key] = affinityEntry{targetName: targetName, lastUsed: now}
	a.mu.Unlock()
}
