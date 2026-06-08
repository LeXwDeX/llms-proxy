package quota

import "time"

// ExceededInfo 内存超限标记结构。详见 docs/quota-design.md §5.2。
type ExceededInfo struct {
	Dimension string    `json:"dimension"`  // "daily" | "weekly" | "monthly"
	Limit     float64   `json:"limit"`      // 触发的限额值 (USD)
	Used      float64   `json:"used"`       // 评估时的累计用量 (USD)
	ResetsAt  time.Time `json:"resets_at"`  // 下个自然周期起始 (UTC)
}

// QuotaStatus admin 查询返回结构。详见 docs/quota-design.md §11.2。
type QuotaStatus struct {
	Client   string                `json:"client"`
	Quotas   map[string]QuotaUsage `json:"quotas"`   // daily/weekly/monthly
	Exceeded *ExceededInfo         `json:"exceeded"` // nil if not exceeded
}

// QuotaUsage 单维度用量进度。
type QuotaUsage struct {
	Limit    float64   `json:"limit"`
	Used     float64   `json:"used"`
	ResetsAt time.Time `json:"resets_at"`
}
