package service

import (
	"context"
	"database/sql"
	"log/slog"
	"math"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
)

const (
	officialQuotaSyncTickInterval = time.Minute
	officialQuotaCheckTimeout     = 45 * time.Second
	officialQuotaResetDropPercent = 0.5
	officialQuotaInitialDelay     = 5 * time.Second
	officialQuotaResetAdvance     = time.Minute
)

type officialQuotaWindow string

const (
	officialQuotaWindowFiveHour officialQuotaWindow = "5h"
	officialQuotaWindowDaily    officialQuotaWindow = "day"
	officialQuotaWindowWeekly   officialQuotaWindow = "7d"
)

type officialQuotaWindowUsage struct {
	UsedPercent       float64
	ResetAfterSeconds int
	WindowMinutes     int
}

type officialQuotaWindowReset struct {
	Daily                          bool
	DailyWindowStart               time.Time
	DailyPreviousWindowStart       time.Time
	DailyReason                    string
	DailySignalMismatch            bool
	DailyCurrentUsedPercent        float64
	DailyPreviousUsedPercent       float64
	DailyPreviousUsedPercentKnown  bool
	Weekly                         bool
	WeeklyWindowStart              time.Time
	WeeklyPreviousWindowStart      time.Time
	WeeklyReason                   string
	WeeklySignalMismatch           bool
	WeeklyCurrentUsedPercent       float64
	WeeklyPreviousUsedPercent      float64
	WeeklyPreviousUsedPercentKnown bool
}

type officialQuotaSourceBatch struct {
	accountID int64
	groups    []Group
	due       bool
}

// OfficialQuotaSyncService periodically polls official OpenAI quota usage for
// subscription groups configured to follow the source account reset cycle.
type OfficialQuotaSyncService struct {
	groupRepo           GroupRepository
	accountRepo         AccountRepository
	openAIQuotaService  *OpenAIQuotaService
	billingCacheService *BillingCacheService
	db                  *sql.DB

	stopCh chan struct{}
	doneCh chan struct{}

	mu         sync.Mutex
	lastChecks map[int64]time.Time
	running    bool
}

func NewOfficialQuotaSyncService(groupRepo GroupRepository, accountRepo AccountRepository, openAIQuotaService *OpenAIQuotaService, billingCacheService *BillingCacheService, db *sql.DB) *OfficialQuotaSyncService {
	return &OfficialQuotaSyncService{
		groupRepo:           groupRepo,
		accountRepo:         accountRepo,
		openAIQuotaService:  openAIQuotaService,
		billingCacheService: billingCacheService,
		db:                  db,
		stopCh:              make(chan struct{}),
		doneCh:              make(chan struct{}),
		lastChecks:          make(map[int64]time.Time),
	}
}

func ProvideOfficialQuotaSyncService(groupRepo GroupRepository, accountRepo AccountRepository, openAIQuotaService *OpenAIQuotaService, billingCacheService *BillingCacheService, db *sql.DB) *OfficialQuotaSyncService {
	svc := NewOfficialQuotaSyncService(groupRepo, accountRepo, openAIQuotaService, billingCacheService, db)
	svc.Start()
	return svc
}

func (s *OfficialQuotaSyncService) Start() {
	if s == nil || s.groupRepo == nil || s.accountRepo == nil || s.openAIQuotaService == nil || s.db == nil {
		return
	}

	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return
	}
	s.running = true
	s.mu.Unlock()

	go s.loop()
}

func (s *OfficialQuotaSyncService) Stop() {
	if s == nil {
		return
	}

	s.mu.Lock()
	if !s.running {
		s.mu.Unlock()
		return
	}
	s.running = false
	close(s.stopCh)
	s.mu.Unlock()

	<-s.doneCh
}

func (s *OfficialQuotaSyncService) loop() {
	defer close(s.doneCh)

	timer := time.NewTimer(officialQuotaInitialDelay)
	defer timer.Stop()

	select {
	case <-timer.C:
		s.pollOnce(context.Background())
	case <-s.stopCh:
		return
	}

	ticker := time.NewTicker(officialQuotaSyncTickInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			s.pollOnce(context.Background())
		case <-s.stopCh:
			return
		}
	}
}

func (s *OfficialQuotaSyncService) pollOnce(ctx context.Context) {
	now := time.Now()
	groups, err := s.listOfficialQuotaGroups(ctx)
	if err != nil {
		slog.Warn("official_quota_sync_list_groups_failed", "error", err)
		return
	}

	for _, batch := range s.sourceBatches(groups, now) {
		if !batch.due {
			continue
		}

		checkCtx, cancel := context.WithTimeout(ctx, officialQuotaCheckTimeout)
		if err := s.checkSourceGroups(checkCtx, batch.accountID, batch.groups, now); err != nil {
			slog.Warn("official_quota_sync_source_failed", "account_id", batch.accountID, "group_ids", officialQuotaGroupIDs(batch.groups), "error", err)
		}
		cancel()
	}
}

func (s *OfficialQuotaSyncService) listOfficialQuotaGroups(ctx context.Context) ([]Group, error) {
	out := make([]Group, 0)
	for page := 1; ; page++ {
		groups, result, err := s.groupRepo.List(ctx, pagination.PaginationParams{
			Page:      page,
			PageSize:  1000,
			SortBy:    "id",
			SortOrder: pagination.SortOrderAsc,
		})
		if err != nil {
			return nil, err
		}
		out = append(out, groups...)
		if result == nil || page >= result.Pages || len(groups) == 0 {
			return out, nil
		}
	}
}

func (s *OfficialQuotaSyncService) groupDue(group Group, now time.Time) bool {
	if !isOfficialQuotaResetGroup(group) {
		return false
	}

	interval := time.Duration(group.normalizedQuotaCheckIntervalMinutes()) * time.Minute
	s.mu.Lock()
	last := s.lastChecks[group.ID]
	s.mu.Unlock()
	return last.IsZero() || !now.Before(last.Add(interval))
}

// sourceBatches keeps every eligible group for a source account together. The
// official quota snapshot is account-scoped, so a reset detected for one group
// must be reconciled for all groups that share it.
func (s *OfficialQuotaSyncService) sourceBatches(groups []Group, now time.Time) []officialQuotaSourceBatch {
	batches := make([]officialQuotaSourceBatch, 0)
	indexes := make(map[int64]int)
	for i := range groups {
		group := groups[i]
		if !isOfficialQuotaResetGroup(group) {
			continue
		}

		accountID := *group.QuotaSourceAccountID
		index, exists := indexes[accountID]
		if !exists {
			index = len(batches)
			indexes[accountID] = index
			batches = append(batches, officialQuotaSourceBatch{accountID: accountID})
		}
		batches[index].groups = append(batches[index].groups, group)
		if s.groupDue(group, now) {
			batches[index].due = true
		}
	}
	return batches
}

func isOfficialQuotaResetGroup(group Group) bool {
	if group.ID <= 0 || !group.IsActive() || !group.IsSubscriptionType() {
		return false
	}
	if group.Platform != PlatformOpenAI || group.QuotaSourceAccountID == nil || *group.QuotaSourceAccountID <= 0 {
		return false
	}
	return group.QuotaFollowOfficialReset && group.hasOfficialQuotaResetLimit()
}

func officialQuotaGroupIDs(groups []Group) []int64 {
	ids := make([]int64, 0, len(groups))
	for _, group := range groups {
		ids = append(ids, group.ID)
	}
	return ids
}

func (s *OfficialQuotaSyncService) checkSourceGroups(ctx context.Context, accountID int64, groups []Group, scheduledAt time.Time) error {
	if len(groups) == 0 {
		return nil
	}
	defer func() {
		for _, group := range groups {
			if s.previousCheck(group.ID).Before(scheduledAt) {
				s.markChecked(group.ID, scheduledAt)
			}
		}
	}()

	account, err := s.accountRepo.GetByID(ctx, accountID)
	if err != nil {
		return err
	}
	if account == nil || account.Platform != PlatformOpenAI || account.Type != AccountTypeOAuth {
		return nil
	}

	previousCheck := s.previousSourceCheck(groups)
	if previousCheck.IsZero() && account.Extra != nil {
		previousCheck = parseExtraTime(account.Extra["official_quota_last_checked_at"])
	}
	usage, err := s.openAIQuotaService.QueryUsage(ctx, accountID)
	if err != nil {
		return err
	}

	now := time.Now()
	snapshot, windows := openAIQuotaUsageToCodexSnapshot(usage, now)
	updates := buildCodexUsageExtraUpdates(snapshot, now)
	if updates == nil {
		updates = make(map[string]any)
	}
	addOfficialQuotaWindowUpdates(updates, windows, now)
	updates["official_quota_last_checked_at"] = now.Format(time.RFC3339)

	for _, group := range groups {
		reset := detectOfficialQuotaReset(account.Extra, windows, group, now)
		if !reset.any() || previousCheck.IsZero() {
			continue
		}
		if reset.signalMismatch() {
			slog.Warn("official_quota_reset_signal_mismatch",
				"group_id", group.ID,
				"account_id", accountID,
				"daily", reset.DailySignalMismatch,
				"daily_previous_window_start", reset.DailyPreviousWindowStart,
				"daily_window_start", reset.DailyWindowStart,
				"daily_previous_used_percent", reset.DailyPreviousUsedPercent,
				"daily_current_used_percent", reset.DailyCurrentUsedPercent,
				"daily_previous_used_percent_known", reset.DailyPreviousUsedPercentKnown,
				"weekly", reset.WeeklySignalMismatch,
				"weekly_previous_window_start", reset.WeeklyPreviousWindowStart,
				"weekly_window_start", reset.WeeklyWindowStart,
				"weekly_previous_used_percent", reset.WeeklyPreviousUsedPercent,
				"weekly_current_used_percent", reset.WeeklyCurrentUsedPercent,
				"weekly_previous_used_percent_known", reset.WeeklyPreviousUsedPercentKnown,
			)
		}
		if err := s.reconcileGroupReset(ctx, group, now, reset); err != nil {
			return err
		}
		slog.Info("official_quota_reset_reconciled",
			"group_id", group.ID,
			"account_id", accountID,
			"detected_since", previousCheck,
			"daily_reason", reset.DailyReason,
			"daily_previous_window_start", reset.DailyPreviousWindowStart,
			"daily_window_start", reset.DailyWindowStart,
			"weekly_reason", reset.WeeklyReason,
			"weekly_previous_window_start", reset.WeeklyPreviousWindowStart,
			"weekly_window_start", reset.WeeklyWindowStart,
			"window_end", now,
			"daily", reset.Daily,
			"weekly", reset.Weekly,
			"lag_reconcile", group.QuotaLagReconcileEnabled,
		)
	}

	if err := s.accountRepo.UpdateExtra(ctx, accountID, updates); err != nil {
		return err
	}

	for _, group := range groups {
		s.markChecked(group.ID, now)
	}
	return nil
}

func (s *OfficialQuotaSyncService) previousSourceCheck(groups []Group) time.Time {
	var previous time.Time
	for _, group := range groups {
		if checked := s.previousCheck(group.ID); checked.After(previous) {
			previous = checked
		}
	}
	return previous
}

func (s *OfficialQuotaSyncService) previousCheck(groupID int64) time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastChecks[groupID]
}

func (s *OfficialQuotaSyncService) markChecked(groupID int64, at time.Time) {
	s.mu.Lock()
	s.lastChecks[groupID] = at
	s.mu.Unlock()
}

func (s *OfficialQuotaSyncService) reconcileGroupReset(ctx context.Context, group Group, windowEnd time.Time, reset officialQuotaWindowReset) error {
	dailyReset, weeklyReset := reset.validWindows(windowEnd)
	if windowEnd.IsZero() || (!dailyReset && !weeklyReset) {
		return nil
	}

	lagEnabled := group.QuotaLagReconcileEnabled
	rows, err := s.db.QueryContext(ctx, `
WITH active_subscriptions AS (
	SELECT id, user_id, group_id
	FROM user_subscriptions
	WHERE group_id = $1
	  AND status = $2
	  AND deleted_at IS NULL
	  AND expires_at > NOW()
),
daily_lag_usage AS (
	SELECT subscription_id, COALESCE(SUM(total_cost), 0)::float8 AS used
	FROM usage_logs
	WHERE group_id = $1
	  AND subscription_id IS NOT NULL
	  AND $5
	  AND created_at >= $3
	  AND created_at < $4
	GROUP BY subscription_id
),
weekly_lag_usage AS (
	SELECT subscription_id, COALESCE(SUM(total_cost), 0)::float8 AS used
	FROM usage_logs
	WHERE group_id = $1
	  AND subscription_id IS NOT NULL
	  AND $6
	  AND created_at >= $8
	  AND created_at < $4
	GROUP BY subscription_id
),
weekly_first_usage AS (
	SELECT subscription_id, MIN(created_at) AS first_used_at
	FROM usage_logs
	WHERE group_id = $1
	  AND subscription_id IS NOT NULL
	  AND $6
	  AND $7
	  AND created_at >= $8
	  AND created_at < $4
	GROUP BY subscription_id
),
five_hour_lag_usage AS (
	SELECT ul.subscription_id, COALESCE(SUM(ul.total_cost), 0)::float8 AS used
	FROM usage_logs ul
	JOIN weekly_first_usage wfu ON wfu.subscription_id = ul.subscription_id
	WHERE ul.group_id = $1
	  AND ul.subscription_id IS NOT NULL
	  AND ul.created_at >= wfu.first_used_at
	  AND ul.created_at < $4
	GROUP BY ul.subscription_id
),
updated AS (
	UPDATE user_subscriptions us
	SET
		five_hour_usage_usd = CASE WHEN $6 THEN CASE WHEN $7 THEN COALESCE(fhlu.used, 0) ELSE 0 END ELSE five_hour_usage_usd END,
		five_hour_window_start = CASE WHEN $6 THEN wfu.first_used_at ELSE five_hour_window_start END,
		daily_usage_usd = CASE WHEN $5 THEN CASE WHEN $7 THEN COALESCE(dlu.used, 0) ELSE 0 END ELSE daily_usage_usd END,
		daily_window_start = CASE WHEN $5 THEN $3 ELSE daily_window_start END,
		weekly_usage_usd = CASE WHEN $6 THEN CASE WHEN $7 THEN COALESCE(wlu.used, 0) ELSE 0 END ELSE weekly_usage_usd END,
		weekly_window_start = CASE WHEN $6 THEN $8 ELSE weekly_window_start END,
		updated_at = NOW()
	FROM active_subscriptions active
	LEFT JOIN daily_lag_usage dlu ON dlu.subscription_id = active.id
	LEFT JOIN weekly_lag_usage wlu ON wlu.subscription_id = active.id
	LEFT JOIN weekly_first_usage wfu ON wfu.subscription_id = active.id
	LEFT JOIN five_hour_lag_usage fhlu ON fhlu.subscription_id = active.id
	WHERE us.id = active.id
	RETURNING us.user_id, us.group_id
)
SELECT user_id, group_id FROM updated
		`, group.ID, SubscriptionStatusActive, nullableResetWindowStart(dailyReset, reset.DailyWindowStart), windowEnd, dailyReset, weeklyReset, lagEnabled, nullableResetWindowStart(weeklyReset, reset.WeeklyWindowStart))
	if err != nil {
		return err
	}
	defer rows.Close()

	type cacheKey struct {
		userID  int64
		groupID int64
	}
	affected := make([]cacheKey, 0)
	for rows.Next() {
		var key cacheKey
		if err := rows.Scan(&key.userID, &key.groupID); err != nil {
			return err
		}
		affected = append(affected, key)
	}
	if err := rows.Err(); err != nil {
		return err
	}

	if s.billingCacheService != nil {
		for _, key := range affected {
			if err := s.billingCacheService.InvalidateSubscription(ctx, key.userID, key.groupID); err != nil {
				slog.Warn("official_quota_sync_subscription_cache_invalidate_failed", "user_id", key.userID, "group_id", key.groupID, "error", err)
			}
		}
	}
	return nil
}

func openAIQuotaUsageToCodexSnapshot(usage *OpenAIQuotaUsage, fallbackNow time.Time) (*OpenAICodexUsageSnapshot, map[officialQuotaWindow]officialQuotaWindowUsage) {
	windows := make(map[officialQuotaWindow]officialQuotaWindowUsage)
	snapshot := &OpenAICodexUsageSnapshot{UpdatedAt: fallbackNow.Format(time.RFC3339)}
	if usage == nil || usage.RateLimit == nil {
		return snapshot, windows
	}

	if usage.FetchedAt > 0 {
		snapshot.UpdatedAt = time.Unix(usage.FetchedAt, 0).UTC().Format(time.RFC3339)
	}

	if usage.RateLimit.PrimaryWindow != nil {
		fillSnapshotWindow(snapshot, true, usage.RateLimit.PrimaryWindow)
		addClassifiedWindow(windows, usage.RateLimit.PrimaryWindow)
	}
	if usage.RateLimit.SecondaryWindow != nil {
		fillSnapshotWindow(snapshot, false, usage.RateLimit.SecondaryWindow)
		addClassifiedWindow(windows, usage.RateLimit.SecondaryWindow)
	}
	for _, item := range usage.AdditionalRateLimits {
		if item.RateLimit == nil {
			continue
		}
		addClassifiedWindow(windows, item.RateLimit.PrimaryWindow)
		addClassifiedWindow(windows, item.RateLimit.SecondaryWindow)
	}

	return snapshot, windows
}

func fillSnapshotWindow(snapshot *OpenAICodexUsageSnapshot, primary bool, window *OpenAIRateLimitWindow) {
	if snapshot == nil || window == nil {
		return
	}
	used := window.UsedPercent
	resetAfter := int(window.ResetAfterSeconds)
	windowMinutes := int(window.LimitWindowSeconds / 60)
	if windowMinutes <= 0 && window.LimitWindowSeconds > 0 {
		windowMinutes = 1
	}

	if primary {
		snapshot.PrimaryUsedPercent = &used
		snapshot.PrimaryResetAfterSeconds = &resetAfter
		snapshot.PrimaryWindowMinutes = &windowMinutes
		return
	}
	snapshot.SecondaryUsedPercent = &used
	snapshot.SecondaryResetAfterSeconds = &resetAfter
	snapshot.SecondaryWindowMinutes = &windowMinutes
}

func addClassifiedWindow(windows map[officialQuotaWindow]officialQuotaWindowUsage, window *OpenAIRateLimitWindow) {
	if window == nil || windows == nil || window.LimitWindowSeconds <= 0 {
		return
	}
	key, ok := classifyOfficialQuotaWindow(window.LimitWindowSeconds)
	if !ok {
		return
	}
	if _, exists := windows[key]; exists {
		return
	}
	resetAfter := int(window.ResetAfterSeconds)
	windowMinutes := int(window.LimitWindowSeconds / 60)
	if windowMinutes <= 0 {
		windowMinutes = 1
	}
	windows[key] = officialQuotaWindowUsage{
		UsedPercent:       window.UsedPercent,
		ResetAfterSeconds: resetAfter,
		WindowMinutes:     windowMinutes,
	}
}

func classifyOfficialQuotaWindow(seconds int64) (officialQuotaWindow, bool) {
	const hour = int64(time.Hour / time.Second)
	if seconds >= 4*hour && seconds <= 6*hour {
		return officialQuotaWindowFiveHour, true
	}
	if seconds >= 20*hour && seconds <= 30*hour {
		return officialQuotaWindowDaily, true
	}
	if seconds >= 6*24*hour && seconds <= 8*24*hour {
		return officialQuotaWindowWeekly, true
	}
	return "", false
}

func addOfficialQuotaWindowUpdates(updates map[string]any, windows map[officialQuotaWindow]officialQuotaWindowUsage, base time.Time) {
	if updates == nil {
		return
	}
	for key, usage := range windows {
		prefix := "official_quota_" + string(key)
		updates[prefix+"_used_percent"] = usage.UsedPercent
		updates[prefix+"_reset_after_seconds"] = usage.ResetAfterSeconds
		updates[prefix+"_window_minutes"] = usage.WindowMinutes
		if windowStart := officialQuotaWindowStart(usage, base); !windowStart.IsZero() {
			updates[prefix+"_window_start"] = windowStart.Format(time.RFC3339)
		}
		resetAfter := usage.ResetAfterSeconds
		if resetAfter < 0 {
			resetAfter = 0
		}
		updates[prefix+"_reset_at"] = base.Add(time.Duration(resetAfter) * time.Second).Format(time.RFC3339)
	}
}

func detectOfficialQuotaReset(extra map[string]any, windows map[officialQuotaWindow]officialQuotaWindowUsage, group Group, base time.Time) officialQuotaWindowReset {
	var reset officialQuotaWindowReset
	if len(extra) == 0 || len(windows) == 0 {
		return reset
	}
	if group.OfficialQuotaDailyLimitUSD != nil && *group.OfficialQuotaDailyLimitUSD > 0 {
		window := windows[officialQuotaWindowDaily]
		windowStart := officialQuotaWindowStart(window, base)
		percentDropped, prevUsedPercent, prevUsedPercentKnown := windowUsageDropDetails(extra, "official_quota_day_used_percent", "", window)
		if advanced, prevStart, known := officialQuotaWindowAdvanced(extra, "official_quota_day", windowStart); known {
			reset.Daily = advanced
			reset.DailyPreviousWindowStart = prevStart
			reset.DailySignalMismatch = advanced && !percentDropped
			if advanced {
				reset.DailyReason = "window_start_advanced"
			}
		} else {
			reset.Daily = percentDropped
			if reset.Daily {
				reset.DailyReason = "usage_percent_dropped"
			}
		}
		reset.DailyCurrentUsedPercent = window.UsedPercent
		reset.DailyPreviousUsedPercent = prevUsedPercent
		reset.DailyPreviousUsedPercentKnown = prevUsedPercentKnown
		if reset.Daily {
			reset.DailyWindowStart = windowStart
		}
	}
	if group.OfficialQuotaWeeklyLimitUSD != nil && *group.OfficialQuotaWeeklyLimitUSD > 0 {
		window := windows[officialQuotaWindowWeekly]
		windowStart := officialQuotaWindowStart(window, base)
		percentDropped, prevUsedPercent, prevUsedPercentKnown := windowUsageDropDetails(extra, "official_quota_7d_used_percent", "codex_7d_used_percent", window)
		if advanced, prevStart, known := officialQuotaWindowAdvanced(extra, "official_quota_7d", windowStart); known {
			reset.Weekly = advanced
			reset.WeeklyPreviousWindowStart = prevStart
			reset.WeeklySignalMismatch = advanced && !percentDropped
			if advanced {
				reset.WeeklyReason = "window_start_advanced"
			}
		} else {
			reset.Weekly = percentDropped
			if reset.Weekly {
				reset.WeeklyReason = "usage_percent_dropped"
			}
		}
		reset.WeeklyCurrentUsedPercent = window.UsedPercent
		reset.WeeklyPreviousUsedPercent = prevUsedPercent
		reset.WeeklyPreviousUsedPercentKnown = prevUsedPercentKnown
		if reset.Weekly {
			reset.WeeklyWindowStart = windowStart
		}
	}
	return reset
}

func officialQuotaWindowStart(current officialQuotaWindowUsage, base time.Time) time.Time {
	if base.IsZero() || current.WindowMinutes <= 0 {
		return time.Time{}
	}
	resetAfter := current.ResetAfterSeconds
	if resetAfter < 0 {
		resetAfter = 0
	}
	resetAt := base.Add(time.Duration(resetAfter) * time.Second)
	return resetAt.Add(-time.Duration(current.WindowMinutes) * time.Minute)
}

func officialQuotaWindowAdvanced(extra map[string]any, prefix string, currentStart time.Time) (bool, time.Time, bool) {
	if currentStart.IsZero() {
		return false, time.Time{}, false
	}
	prevStart := previousOfficialQuotaWindowStart(extra, prefix)
	if prevStart.IsZero() {
		return false, time.Time{}, false
	}
	return currentStart.After(prevStart.Add(officialQuotaResetAdvance)), prevStart, true
}

func previousOfficialQuotaWindowStart(extra map[string]any, prefix string) time.Time {
	if extra == nil || prefix == "" {
		return time.Time{}
	}
	if start := parseExtraTime(extra[prefix+"_window_start"]); !start.IsZero() {
		return start
	}
	resetAt := parseExtraTime(extra[prefix+"_reset_at"])
	windowMinutes := int(parseExtraFloat64(extra[prefix+"_window_minutes"]))
	if resetAt.IsZero() || windowMinutes <= 0 {
		return time.Time{}
	}
	return resetAt.Add(-time.Duration(windowMinutes) * time.Minute)
}

func windowUsageDropped(extra map[string]any, primaryKey, fallbackKey string, current officialQuotaWindowUsage) bool {
	dropped, _, _ := windowUsageDropDetails(extra, primaryKey, fallbackKey, current)
	return dropped
}

func windowUsageDropDetails(extra map[string]any, primaryKey, fallbackKey string, current officialQuotaWindowUsage) (bool, float64, bool) {
	if current.WindowMinutes <= 0 {
		return false, 0, false
	}
	prev, ok := extraFloat(extra, primaryKey)
	if !ok && fallbackKey != "" {
		prev, ok = extraFloat(extra, fallbackKey)
	}
	if !ok || math.IsNaN(prev) || math.IsInf(prev, 0) {
		return false, 0, false
	}
	return current.UsedPercent+officialQuotaResetDropPercent < prev, prev, true
}

func extraFloat(extra map[string]any, key string) (float64, bool) {
	if extra == nil || key == "" {
		return 0, false
	}
	value, ok := extra[key]
	if !ok {
		return 0, false
	}
	switch v := value.(type) {
	case float64:
		return v, true
	case float32:
		return float64(v), true
	case int:
		return float64(v), true
	case int64:
		return float64(v), true
	case jsonNumber:
		f, err := v.Float64()
		return f, err == nil
	default:
		return parseExtraFloat64(value), true
	}
}

type jsonNumber interface {
	Float64() (float64, error)
}

func (r officialQuotaWindowReset) any() bool {
	return r.Daily || r.Weekly
}

func (r officialQuotaWindowReset) signalMismatch() bool {
	return r.DailySignalMismatch || r.WeeklySignalMismatch
}

func (r officialQuotaWindowReset) validWindows(windowEnd time.Time) (bool, bool) {
	dailyValid := r.Daily && !r.DailyWindowStart.IsZero() && windowEnd.After(r.DailyWindowStart)
	weeklyValid := r.Weekly && !r.WeeklyWindowStart.IsZero() && windowEnd.After(r.WeeklyWindowStart)
	return dailyValid, weeklyValid
}

func nullableResetWindowStart(enabled bool, windowStart time.Time) any {
	if !enabled || windowStart.IsZero() {
		return nil
	}
	return windowStart
}

func (g Group) normalizedQuotaCheckIntervalMinutes() int {
	if g.QuotaCheckIntervalMinutes < 1 {
		return 10
	}
	return g.QuotaCheckIntervalMinutes
}

func (g Group) hasOfficialQuotaResetLimit() bool {
	return (g.OfficialQuotaDailyLimitUSD != nil && *g.OfficialQuotaDailyLimitUSD > 0) ||
		(g.OfficialQuotaWeeklyLimitUSD != nil && *g.OfficialQuotaWeeklyLimitUSD > 0)
}
