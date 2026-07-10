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
	Daily  bool
	Weekly bool
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

	for i := range groups {
		group := groups[i]
		if !s.groupDue(group, now) {
			continue
		}

		checkCtx, cancel := context.WithTimeout(ctx, officialQuotaCheckTimeout)
		if err := s.checkGroup(checkCtx, group, now); err != nil {
			slog.Warn("official_quota_sync_group_failed", "group_id", group.ID, "error", err)
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
	if group.ID <= 0 || !group.IsActive() || !group.IsSubscriptionType() {
		return false
	}
	if group.Platform != PlatformOpenAI || group.QuotaSourceAccountID == nil || *group.QuotaSourceAccountID <= 0 {
		return false
	}
	if !group.QuotaFollowOfficialReset || !group.hasOfficialQuotaResetLimit() {
		return false
	}

	interval := time.Duration(group.normalizedQuotaCheckIntervalMinutes()) * time.Minute
	s.mu.Lock()
	last := s.lastChecks[group.ID]
	s.mu.Unlock()
	return last.IsZero() || !now.Before(last.Add(interval))
}

func (s *OfficialQuotaSyncService) checkGroup(ctx context.Context, group Group, scheduledAt time.Time) error {
	defer func() {
		if s.previousCheck(group.ID).Before(scheduledAt) {
			s.markChecked(group.ID, scheduledAt)
		}
	}()

	accountID := *group.QuotaSourceAccountID
	account, err := s.accountRepo.GetByID(ctx, accountID)
	if err != nil {
		return err
	}
	if account == nil || account.Platform != PlatformOpenAI || account.Type != AccountTypeOAuth {
		return nil
	}

	previousCheck := s.previousCheck(group.ID)
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
	updates["official_quota_source_group_id"] = group.ID

	reset := detectOfficialQuotaReset(account.Extra, windows, group)
	if reset.any() && !previousCheck.IsZero() {
		if err := s.reconcileGroupReset(ctx, group, previousCheck, now, reset); err != nil {
			return err
		}
		slog.Info("official_quota_reset_reconciled",
			"group_id", group.ID,
			"account_id", accountID,
			"window_start", previousCheck,
			"window_end", now,
			"daily", reset.Daily,
			"weekly", reset.Weekly,
			"lag_reconcile", group.QuotaLagReconcileEnabled,
		)
	}

	if err := s.accountRepo.UpdateExtra(ctx, accountID, updates); err != nil {
		return err
	}

	s.markChecked(group.ID, now)
	return nil
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

func (s *OfficialQuotaSyncService) reconcileGroupReset(ctx context.Context, group Group, windowStart, windowEnd time.Time, reset officialQuotaWindowReset) error {
	if windowStart.IsZero() || !windowEnd.After(windowStart) {
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
lag_usage AS (
	SELECT subscription_id, COALESCE(SUM(total_cost), 0)::float8 AS used
	FROM usage_logs
	WHERE group_id = $1
	  AND subscription_id IS NOT NULL
	  AND created_at >= $3
	  AND created_at < $4
	GROUP BY subscription_id
),
updated AS (
	UPDATE user_subscriptions us
	SET
		daily_usage_usd = CASE WHEN $5 THEN CASE WHEN $7 THEN COALESCE(lu.used, 0) ELSE 0 END ELSE daily_usage_usd END,
		daily_window_start = CASE WHEN $5 THEN $3 ELSE daily_window_start END,
		weekly_usage_usd = CASE WHEN $6 THEN CASE WHEN $7 THEN COALESCE(lu.used, 0) ELSE 0 END ELSE weekly_usage_usd END,
		weekly_window_start = CASE WHEN $6 THEN $3 ELSE weekly_window_start END,
		updated_at = NOW()
	FROM active_subscriptions active
	LEFT JOIN lag_usage lu ON lu.subscription_id = active.id
	WHERE us.id = active.id
	RETURNING us.user_id, us.group_id
)
SELECT user_id, group_id FROM updated
`, group.ID, SubscriptionStatusActive, windowStart, windowEnd, reset.Daily, reset.Weekly, lagEnabled)
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
		resetAfter := usage.ResetAfterSeconds
		if resetAfter < 0 {
			resetAfter = 0
		}
		updates[prefix+"_reset_at"] = base.Add(time.Duration(resetAfter) * time.Second).Format(time.RFC3339)
	}
}

func detectOfficialQuotaReset(extra map[string]any, windows map[officialQuotaWindow]officialQuotaWindowUsage, group Group) officialQuotaWindowReset {
	var reset officialQuotaWindowReset
	if len(extra) == 0 || len(windows) == 0 {
		return reset
	}
	if group.OfficialQuotaDailyLimitUSD != nil && *group.OfficialQuotaDailyLimitUSD > 0 {
		reset.Daily = windowUsageDropped(extra, "official_quota_day_used_percent", "", windows[officialQuotaWindowDaily])
	}
	if group.OfficialQuotaWeeklyLimitUSD != nil && *group.OfficialQuotaWeeklyLimitUSD > 0 {
		reset.Weekly = windowUsageDropped(extra, "codex_7d_used_percent", "official_quota_7d_used_percent", windows[officialQuotaWindowWeekly])
	}
	return reset
}

func windowUsageDropped(extra map[string]any, primaryKey, fallbackKey string, current officialQuotaWindowUsage) bool {
	if current.WindowMinutes <= 0 {
		return false
	}
	prev, ok := extraFloat(extra, primaryKey)
	if !ok && fallbackKey != "" {
		prev, ok = extraFloat(extra, fallbackKey)
	}
	if !ok || math.IsNaN(prev) || math.IsInf(prev, 0) {
		return false
	}
	return current.UsedPercent+officialQuotaResetDropPercent < prev
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
