// Package quota implements account-level USD quota limiting (daily/weekly/monthly).
//
// A Manager loads per-client quota config, evaluates current spend against limits,
// and provides two enforcement points:
//   - Admit: pre-flight check returning 429 when all applicable periods are exceeded.
//   - Record (async observer): post-response usage recording with re-evaluation,
//     triggering cancel callbacks for in-flight SSE streams via TCP RST.
//
// Period boundaries follow calendar day (UTC), ISO week, and calendar month.
package quota

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ycgame/llms-proxy/internal/catalog"
	"github.com/ycgame/llms-proxy/internal/config"
	"github.com/ycgame/llms-proxy/internal/costutil"
	"github.com/ycgame/llms-proxy/internal/nosql"
)

// Options configures Manager dependencies.
type Options struct {
	Catalog     *catalog.Catalog
	CostStore   *nosql.ModelCostStore
	UsageStore  *nosql.UsageStore
	ClientStore *nosql.ClientStore
	Logger      *slog.Logger
	Interval    time.Duration // 0 = default 5s
}

// Manager manages quota evaluation and stream cancellation (docs/quota-design.md §3).
type Manager struct {
	catalog     *catalog.Catalog
	costStore   *nosql.ModelCostStore
	usageStore  *nosql.UsageStore
	clientStore *nosql.ClientStore
	logger      *slog.Logger
	interval    time.Duration

	mu            sync.RWMutex
	exceeded      map[string]ExceededInfo
	activeStreams map[string]map[int64]context.CancelFunc
	nextStreamID  atomic.Int64

	// lifecycle
	ticker  *time.Ticker
	stopCh  chan struct{}
	started atomic.Bool
}

// New constructs a Manager.
func New(opts Options) (*Manager, error) {
	interval := opts.Interval
	if interval <= 0 {
		interval = 5 * time.Second
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Manager{
		catalog:       opts.Catalog,
		costStore:     opts.CostStore,
		usageStore:    opts.UsageStore,
		clientStore:   opts.ClientStore,
		logger:        logger,
		interval:      interval,
		exceeded:      make(map[string]ExceededInfo),
		activeStreams: make(map[string]map[int64]context.CancelFunc),
	}, nil
}

// Start launches preload + ticker goroutine.
func (m *Manager) Start(ctx context.Context) error {
	if m.started.Swap(true) {
		return nil
	}

	// Batch preload: Evaluate all clients that have quota > 0.
	// 10s timeout to avoid blocking startup on slow DB.
	preloadCtx, preloadCancel := context.WithTimeout(ctx, 10*time.Second)
	defer preloadCancel()

	if m.clientStore != nil {
		clients, err := m.clientStore.ListWithQuota()
		if err != nil {
			m.logger.Warn("quota.Manager: preload ListWithQuota failed", "error", err)
		} else {
			for _, c := range clients {
				select {
				case <-preloadCtx.Done():
					m.logger.Warn("quota.Manager: preload timeout, continuing")
					goto startTicker
				default:
					m.Evaluate(c.Name)
				}
			}
		}
	}
startTicker:

	m.ticker = time.NewTicker(m.interval)
	m.stopCh = make(chan struct{})
	go m.runTicker()

	return nil
}

func (m *Manager) runTicker() {
	for {
		select {
		case <-m.ticker.C:
			m.evaluateAll()
		case <-m.stopCh:
			m.ticker.Stop()
			return
		}
	}
}

func (m *Manager) evaluateAll() {
	if m.clientStore == nil {
		return
	}
	clients, err := m.clientStore.ListWithQuota()
	if err != nil {
		m.logger.Warn("quota.Manager: ListWithQuota failed", "error", err)
		return
	}
	for _, c := range clients {
		m.Evaluate(c.Name)
	}
}

// Stop halts the ticker and clears active stream map.
func (m *Manager) Stop() {
	if !m.started.Swap(false) {
		return
	}
	if m.stopCh != nil {
		select {
		case <-m.stopCh:
		default:
			close(m.stopCh)
		}
	}
	m.mu.Lock()
	m.activeStreams = make(map[string]map[int64]context.CancelFunc)
	m.mu.Unlock()
}

// Evaluate assesses a single client's quota usage across all configured dimensions.
func (m *Manager) Evaluate(clientName string) {
	if m.clientStore == nil || m.usageStore == nil {
		return
	}

	// Fetch client config.
	allClients, err := m.clientStore.List()
	if err != nil {
		m.logger.Warn("quota.Manager: List failed", "client", clientName, "error", err)
		return
	}
	var client *config.Client
	for i := range allClients {
		if allClients[i].Name == clientName {
			client = &allClients[i]
			break
		}
	}
	if client == nil {
		// Client 已被删除：清除旧的 exceeded 标记，避免残留至周期结束
		m.mu.Lock()
		delete(m.exceeded, clientName)
		m.mu.Unlock()
		return
	}

	// No quota configured at all → skip.
	if client.QuotaDailyUSD <= 0 && client.QuotaWeeklyUSD <= 0 && client.QuotaMonthlyUSD <= 0 {
		return
	}

	now := time.Now().UTC()

	// Build cost table fresh each evaluation (picks up recent changes).
	var costs []nosql.ModelCost
	if m.costStore != nil {
		costs, _ = m.costStore.List()
	}
	costTable := costutil.ToCostTable(costs, m.catalog)

	type dimResult struct {
		dim      string
		limit    float64
		used     float64
		resetsAt time.Time
	}
	var results []dimResult

	for _, dim := range []struct {
		name  string
		limit float64
	}{
		{DimensionDaily, client.QuotaDailyUSD},
		{DimensionWeekly, client.QuotaWeeklyUSD},
		{DimensionMonthly, client.QuotaMonthlyUSD},
	} {
		if dim.limit <= 0 {
			continue
		}
		from, to, err := PeriodRange(dim.name, now)
		if err != nil {
			continue
		}
		resetsAt, err := NextResetAt(dim.name, now)
		if err != nil {
			continue
		}
		totals, err := m.usageStore.SumByClientRange(clientName, from, to)
		if err != nil {
			m.logger.Warn("quota.Manager: SumByClientRange failed",
				"client", clientName, "dim", dim.name, "error", err)
			continue
		}
		used := 0.0
		for groupKey, t := range totals {
			parts := strings.SplitN(groupKey, ":", 2)
			epType, model := "", groupKey
			if len(parts) == 2 {
				epType, model = parts[0], parts[1]
			}
			rates, ok := costTable.LookupCost(epType, model)
			if !ok {
				continue // unknown model: no charge
			}
			used += float64(t.InputTokens)/1e6*rates.InputPer1MTokens +
				float64(t.OutputTokens)/1e6*rates.OutputPer1MTokens +
				float64(t.CachedTokens)/1e6*rates.CachedInputPer1MToken
		}
		results = append(results, dimResult{dim.name, dim.limit, used, resetsAt})
	}

	// Update exceeded map and collect cancel functions.
	m.mu.Lock()
	delete(m.exceeded, clientName) // clear stale entries

	var cancels []context.CancelFunc
	for _, r := range results {
		if r.used >= r.limit {
			m.exceeded[clientName] = ExceededInfo{
				Dimension: r.dim,
				Limit:     r.limit,
				Used:      r.used,
				ResetsAt:  r.resetsAt,
			}
			// Collect cancel functions to invoke outside the lock.
			if cancelSet, ok := m.activeStreams[clientName]; ok {
				for _, cancel := range cancelSet {
					cancels = append(cancels, cancel)
				}
			}
			m.logger.Info("quota.Manager: client quota exceeded",
				"client", clientName, "dimension", r.dim,
				"limit_usd", r.limit, "used_usd", r.used,
				"resets_at", r.resetsAt)
			break // record only the first-exceeded dimension
		}
	}
	m.mu.Unlock()

	// Invoke cancels in a goroutine to avoid blocking Evaluate.
	if len(cancels) > 0 {
		go func(cs []context.CancelFunc) {
			for _, c := range cs {
				c()
			}
		}(cancels)
	}
}

// Check returns whether the client's quota is exceeded.
// Automatically clears stale entries whose ResetsAt has passed.
func (m *Manager) Check(clientName string) (ExceededInfo, bool) {
	m.mu.RLock()
	info, exceeded := m.exceeded[clientName]
	m.mu.RUnlock()
	if !exceeded {
		return ExceededInfo{}, false
	}
	// Auto-unseal: if ResetsAt has passed, clear the entry.
	if !info.ResetsAt.After(time.Now().UTC()) {
		m.mu.Lock()
		delete(m.exceeded, clientName)
		m.mu.Unlock()
		return ExceededInfo{}, false
	}
	return info, true
}

// RegisterActiveStream registers a stream's cancel function under a client.
// Returns an unregister closure.
func (m *Manager) RegisterActiveStream(clientName string, cancel context.CancelFunc) func() {
	id := m.nextStreamID.Add(1)
	m.mu.Lock()
	if m.activeStreams[clientName] == nil {
		m.activeStreams[clientName] = make(map[int64]context.CancelFunc)
	}
	m.activeStreams[clientName][id] = cancel
	m.mu.Unlock()
	return func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		if set, ok := m.activeStreams[clientName]; ok {
			delete(set, id)
			if len(set) == 0 {
				delete(m.activeStreams, clientName)
			}
		}
	}
}

// Status returns the quota usage report for a client.
func (m *Manager) Status(clientName string) QuotaStatus {
	status := QuotaStatus{
		Client: clientName,
		Quotas: make(map[string]QuotaUsage),
	}

	if m.clientStore == nil {
		return status
	}
	allClients, err := m.clientStore.List()
	if err != nil {
		return status
	}
	var client *config.Client
	for i := range allClients {
		if allClients[i].Name == clientName {
			client = &allClients[i]
			break
		}
	}
	if client == nil {
		return status
	}

	// Read exceeded status.
	if info, exceeded := m.Check(clientName); exceeded {
		status.Exceeded = &info
	}

	now := time.Now().UTC()

	// Build cost table once for all dimensions.
	var costs []nosql.ModelCost
	if m.costStore != nil {
		costs, _ = m.costStore.List()
	}
	costTable := costutil.ToCostTable(costs, m.catalog)

	for _, dim := range []struct {
		name  string
		limit float64
	}{
		{DimensionDaily, client.QuotaDailyUSD},
		{DimensionWeekly, client.QuotaWeeklyUSD},
		{DimensionMonthly, client.QuotaMonthlyUSD},
	} {
		resetsAt, _ := NextResetAt(dim.name, now)
		from, to, _ := PeriodRange(dim.name, now)
		used := 0.0
		if m.usageStore != nil {
			totals, err := m.usageStore.SumByClientRange(clientName, from, to)
			if err == nil {
				for groupKey, t := range totals {
					parts := strings.SplitN(groupKey, ":", 2)
					epType, model := "", groupKey
					if len(parts) == 2 {
						epType, model = parts[0], parts[1]
					}
					rates, ok := costTable.LookupCost(epType, model)
					if !ok {
						continue
					}
					used += float64(t.InputTokens)/1e6*rates.InputPer1MTokens +
						float64(t.OutputTokens)/1e6*rates.OutputPer1MTokens +
						float64(t.CachedTokens)/1e6*rates.CachedInputPer1MToken
				}
			}
		}
		status.Quotas[dim.name] = QuotaUsage{
			Limit:    dim.limit,
			Used:     used,
			ResetsAt: resetsAt,
		}
	}

	return status
}
