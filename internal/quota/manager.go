// Package quota implements account-level USD quota limiting (daily/weekly/monthly).
//
// Architecture: memory-first, event-driven, hourly DB calibration.
//
//   - Increment: on request completion, add cost to in-memory counters (zero DB hit).
//   - Check: read in-memory exceeded map for admission control (zero DB hit).
//   - Evaluate: authoritative DB aggregation for a single client — used at startup
//     preload, admin config changes, and hourly calibration.
//   - Hourly calibration: aligns to natural hours (00:00, 01:00, ...).
//     At each hour boundary, Evaluate all clients with quota > 0 from DB.
//     This naturally handles period resets (daily/weekly/monthly boundaries
//     all fall on hour boundaries) and corrects any accumulated drift.
//   - Lazy period flip: increment/Check detect ResetsAt crossing and clear
//     the counter, eliminating the sub-second gap between boundary and
//     calibration tick.
//
// No 5-second ticker. No full-recompute polling.
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
}

// dimCounter holds an in-memory incremental counter for one quota dimension.
type dimCounter struct {
	limit    float64   // cached from Client config
	used     float64   // incremental USD spend
	resetsAt time.Time // next period boundary (UTC)
}

// clientCounters holds per-dimension counters for one client.
type clientCounters struct {
	dims map[string]*dimCounter // keyed by DimensionDaily/Weekly/Monthly
}

// Manager manages quota evaluation.
// Decision source: in-memory counters + exceeded map.
type Manager struct {
	catalog     *catalog.Catalog
	costStore   *nosql.ModelCostStore
	usageStore  *nosql.UsageStore
	clientStore *nosql.ClientStore
	logger      *slog.Logger

	mu       sync.RWMutex
	exceeded map[string]ExceededInfo
	counters map[string]*clientCounters // clientName → per-dim counters

	// hourly calibration timer
	calTimer *time.Timer
	stopCh   chan struct{}
	started  atomic.Bool
}

// New constructs a Manager.
func New(opts Options) (*Manager, error) {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Manager{
		catalog:     opts.Catalog,
		costStore:   opts.CostStore,
		usageStore:  opts.UsageStore,
		clientStore: opts.ClientStore,
		logger:      logger,
		exceeded:    make(map[string]ExceededInfo),
		counters:    make(map[string]*clientCounters),
	}, nil
}

// Start runs one-time preload + launches hourly calibration timer.
func (m *Manager) Start(ctx context.Context) error {
	if m.started.Swap(true) {
		return nil
	}

	// Preload: Evaluate all clients with quota > 0 from DB.
	preloadCtx, preloadCancel := context.WithTimeout(ctx, 10*time.Second)
	defer preloadCancel()

	if m.clientStore != nil {
		clients, err := m.clientStore.ListWithQuota()
		if err != nil {
			m.logger.Warn("quota.Manager: preload failed", "error", err)
		} else {
			for _, c := range clients {
				select {
				case <-preloadCtx.Done():
					m.logger.Warn("quota.Manager: preload timeout")
					goto startCalibration
				default:
					m.Evaluate(c.Name)
				}
			}
		}
	}

startCalibration:
	// Launch hourly calibration aligned to natural hours.
	m.stopCh = make(chan struct{})
	m.scheduleCalibration()

	return nil
}

// scheduleCalibration schedules the next calibration at the next natural hour boundary.
func (m *Manager) scheduleCalibration() {
	now := time.Now().UTC()
	nextHour := time.Date(now.Year(), now.Month(), now.Day(), now.Hour()+1, 0, 0, 0, time.UTC)
	delay := nextHour.Sub(now)
	if delay <= 0 {
		delay = time.Second // edge case: exactly on hour boundary
	}
	m.calTimer = time.AfterFunc(delay, func() {
		m.calibrateAll()
		m.scheduleCalibration() // reschedule for next hour
	})
}

// calibrateAll runs authoritative DB aggregation for all clients with quota > 0.
// Called at each natural hour boundary.
func (m *Manager) calibrateAll() {
	if m.clientStore == nil {
		return
	}
	clients, err := m.clientStore.ListWithQuota()
	if err != nil {
		m.logger.Warn("quota.Manager: calibration ListWithQuota failed", "error", err)
		return
	}
	m.logger.Debug("quota.Manager: hourly calibration", "clients", len(clients))
	for _, c := range clients {
		m.Evaluate(c.Name)
	}
}

// Stop halts calibration timer.
func (m *Manager) Stop() {
	if !m.started.Swap(false) {
		return
	}
	if m.calTimer != nil {
		m.calTimer.Stop()
	}
	if m.stopCh != nil {
		close(m.stopCh)
	}
}

// Increment adds cost to a client's in-memory counters and updates exceeded state.
// Called after request completion. Accepts raw token counts; computes USD cost internally.
// If a period boundary (ResetsAt) has been crossed, the counter resets first (lazy flip).
func (m *Manager) Increment(clientName string, epType string, model string, inputTokens, outputTokens, cachedTokens int64) {
	// Compute USD cost from token counts using cost table.
	costUSD := m.computeCost(epType, model, inputTokens, outputTokens, cachedTokens)
	if costUSD <= 0 {
		return
	}
	now := time.Now().UTC()

	m.mu.Lock()
	cc := m.counters[clientName]
	if cc == nil {
		// No counters yet — release lock and evaluate from DB to get accurate usage.
		// If Evaluate fails (DB unavailable etc.), fallback to loadCountersFromConfigLocked (used=0, best effort).
		m.mu.Unlock()
		m.Evaluate(clientName)
		m.mu.Lock()
		cc = m.counters[clientName]
		if cc == nil {
			cc = m.loadCountersFromConfigLocked(clientName)
			if cc == nil {
				m.mu.Unlock()
				return // client not found or no quota configured
			}
		}
	}

	// Increment each dimension, applying lazy period flip.
	prevExceeded := m.exceeded[clientName].Dimension != ""
	allUnderLimit := true
	for _, dimOrder := range []string{DimensionDaily, DimensionWeekly, DimensionMonthly} {
		dc := cc.dims[dimOrder]
		if dc == nil {
			continue
		}
		// Lazy period flip: if ResetsAt has passed, reset counter.
		if !dc.resetsAt.After(now) {
			newResetsAt, err := NextResetAt(dimOrder, now)
			if err != nil {
				continue
			}
			dc.used = 0
			dc.resetsAt = newResetsAt
			// Also refresh limit from DB config (may have changed).
			m.refreshLimitLocked(clientName, dimOrder, dc)
		}
		dc.used += costUSD
		if dc.used >= dc.limit && dc.limit > 0 {
			allUnderLimit = false
		}
	}

	// Update exceeded map.
	if allUnderLimit {
		delete(m.exceeded, clientName)
	} else {
		// Find the first exceeded dimension (deterministic order: daily→weekly→monthly).
		for _, dimOrder := range []string{DimensionDaily, DimensionWeekly, DimensionMonthly} {
			dc := cc.dims[dimOrder]
			if dc == nil {
				continue
			}
			if dc.limit > 0 && dc.used >= dc.limit {
				newInfo := ExceededInfo{
					Dimension: dimOrder,
					Limit:     dc.limit,
					Used:      dc.used,
					ResetsAt:  dc.resetsAt,
				}
				m.exceeded[clientName] = newInfo

				// Log only on state transition (first exceed).
				if !prevExceeded {
					m.logger.Info("quota.Manager: client quota exceeded",
						"client", clientName, "dimension", dimOrder,
						"limit_usd", dc.limit, "used_usd", dc.used,
						"resets_at", dc.resetsAt)
				}

				m.mu.Unlock()
				return
			}
		}
	}

	m.mu.Unlock()
}

// loadCountersFromConfigLocked creates clientCounters from DB config.
// Must be called with m.mu held.
func (m *Manager) loadCountersFromConfigLocked(clientName string) *clientCounters {
	if m.clientStore == nil {
		return nil
	}
	clients, err := m.clientStore.List()
	if err != nil {
		return nil
	}
	var client *config.Client
	for i := range clients {
		if clients[i].Name == clientName {
			client = &clients[i]
			break
		}
	}
	if client == nil {
		return nil
	}
	if client.QuotaDailyUSD <= 0 && client.QuotaWeeklyUSD <= 0 && client.QuotaMonthlyUSD <= 0 {
		return nil
	}
	now := time.Now().UTC()
	cc := &clientCounters{dims: make(map[string]*dimCounter)}
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
		resetsAt, err := NextResetAt(dim.name, now)
		if err != nil {
			continue
		}
		cc.dims[dim.name] = &dimCounter{
			limit:    dim.limit,
			used:     0,
			resetsAt: resetsAt,
		}
	}
	if len(cc.dims) == 0 {
		return nil
	}
	m.counters[clientName] = cc
	return cc
}

// refreshLimitLocked updates a single dimension's limit from DB config.
// Must be called with m.mu held.
func (m *Manager) refreshLimitLocked(clientName, dimName string, dc *dimCounter) {
	if m.clientStore == nil {
		return
	}
	clients, err := m.clientStore.List()
	if err != nil {
		return
	}
	for i := range clients {
		if clients[i].Name == clientName {
			switch dimName {
			case DimensionDaily:
				dc.limit = clients[i].QuotaDailyUSD
			case DimensionWeekly:
				dc.limit = clients[i].QuotaWeeklyUSD
			case DimensionMonthly:
				dc.limit = clients[i].QuotaMonthlyUSD
			}
			return
		}
	}
}

// Evaluate performs authoritative DB aggregation for a single client.
// Used at: startup preload, hourly calibration, admin config changes.
// Replaces in-memory counters with DB-derived values (authoritative).
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
		// Client deleted: clear all state.
		m.mu.Lock()
		delete(m.exceeded, clientName)
		delete(m.counters, clientName)
		m.mu.Unlock()
		return
	}

	// No quota configured → clear state.
	if client.QuotaDailyUSD <= 0 && client.QuotaWeeklyUSD <= 0 && client.QuotaMonthlyUSD <= 0 {
		m.mu.Lock()
		delete(m.exceeded, clientName)
		delete(m.counters, clientName)
		m.mu.Unlock()
		return
	}

	now := time.Now().UTC()

	// Build cost table.
	var costs []nosql.ModelCost
	if m.costStore != nil {
		costs, _ = m.costStore.List()
	}
	costTable := costutil.ToCostTable(costs, m.catalog)

	// Evaluate each dimension from DB.
	cc := &clientCounters{dims: make(map[string]*dimCounter)}
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
				continue
			}
			used += float64(t.InputTokens)/1e6*rates.InputPer1MTokens +
				float64(t.OutputTokens)/1e6*rates.OutputPer1MTokens +
				float64(t.CachedTokens)/1e6*rates.CachedInputPer1MToken
		}
		cc.dims[dim.name] = &dimCounter{
			limit:    dim.limit,
			used:     used,
			resetsAt: resetsAt,
		}
	}

	// Update exceeded map and counters.
	m.mu.Lock()
	prevExceeded := m.exceeded[clientName].Dimension != ""

	var firstExceeded *ExceededInfo
	for _, dimOrder := range []string{DimensionDaily, DimensionWeekly, DimensionMonthly} {
		dc := cc.dims[dimOrder]
		if dc == nil {
			continue
		}
		if dc.limit > 0 && dc.used >= dc.limit {
			info := ExceededInfo{
				Dimension: dimOrder,
				Limit:     dc.limit,
				Used:      dc.used,
				ResetsAt:  dc.resetsAt,
			}
			firstExceeded = &info
			break // only record first exceeded dimension
		}
	}

	if firstExceeded != nil {
		m.exceeded[clientName] = *firstExceeded
		if !prevExceeded {
			m.logger.Info("quota.Manager: client quota exceeded",
				"client", clientName, "dimension", firstExceeded.Dimension,
				"limit_usd", firstExceeded.Limit, "used_usd", firstExceeded.Used,
				"resets_at", firstExceeded.ResetsAt)
		}
	} else {
		delete(m.exceeded, clientName)
	}

	// Replace in-memory counters with DB-derived values.
	m.counters[clientName] = cc

	m.mu.Unlock()
}

// Check returns whether the client's quota is exceeded.
// Automatically clears stale entries whose ResetsAt has passed (lazy period flip).
func (m *Manager) Check(clientName string) (ExceededInfo, bool) {
	m.mu.RLock()
	info, exceeded := m.exceeded[clientName]
	m.mu.RUnlock()
	if !exceeded {
		return ExceededInfo{}, false
	}
	// Lazy period flip: if ResetsAt has passed, clear and return not exceeded.
	if !info.ResetsAt.After(time.Now().UTC()) {
		m.mu.Lock()
		delete(m.exceeded, clientName)
		// Also reset in-memory counters if they exist.
		if cc, ok := m.counters[clientName]; ok {
			for _, dimOrder := range []string{DimensionDaily, DimensionWeekly, DimensionMonthly} {
				dc := cc.dims[dimOrder]
				if dc == nil {
					continue
				}
				if dc.resetsAt.Equal(info.ResetsAt) || !dc.resetsAt.After(time.Now().UTC()) {
					dc.used = 0
					newResetsAt, err := NextResetAt(dimOrder, time.Now().UTC())
					if err == nil {
						dc.resetsAt = newResetsAt
					}
					m.refreshLimitLocked(clientName, dimOrder, dc)
				}
			}
		}
		m.mu.Unlock()
		return ExceededInfo{}, false
	}
	return info, true
}

// Status returns the quota usage report for a client.
// Uses in-memory counters for fast response; if no counters exist,
// falls back to DB aggregation (first request after restart before preload).
func (m *Manager) Status(clientName string) QuotaStatus {
	status := QuotaStatus{
		Client: clientName,
		Quotas: make(map[string]QuotaUsage),
	}

	m.mu.RLock()
	cc := m.counters[clientName]
	m.mu.RUnlock()

	if cc != nil {
		// Fast path: read from in-memory counters.
		for dimName, dc := range cc.dims {
			status.Quotas[dimName] = QuotaUsage{
				Limit:    dc.limit,
				Used:     dc.used,
				ResetsAt: dc.resetsAt,
			}
		}
	} else {
		// Slow path: DB aggregation (client not yet loaded, or no quota).
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

		now := time.Now().UTC()
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
			if dim.limit <= 0 {
				continue
			}
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
	}

	// Read exceeded status.
	if info, exceeded := m.Check(clientName); exceeded {
		status.Exceeded = &info
	}

	return status
}

// computeCost calculates USD cost from token counts using the cost table.
func (m *Manager) computeCost(epType, model string, inputTokens, outputTokens, cachedTokens int64) float64 {
	model = strings.ToLower(strings.TrimSpace(model))
	if model == "" {
		return 0
	}
	var costs []nosql.ModelCost
	if m.costStore != nil {
		costs, _ = m.costStore.List()
	}
	costTable := costutil.ToCostTable(costs, m.catalog)
	rates, ok := costTable.LookupCost(epType, model)
	if !ok {
		return 0 // unknown model: no charge
	}
	return float64(inputTokens)/1e6*rates.InputPer1MTokens +
		float64(outputTokens)/1e6*rates.OutputPer1MTokens +
		float64(cachedTokens)/1e6*rates.CachedInputPer1MToken
}
