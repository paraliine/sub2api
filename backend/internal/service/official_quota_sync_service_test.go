package service

import (
	"testing"
	"time"
)

func TestDetectOfficialQuotaResetIgnoresFiveHourWindow(t *testing.T) {
	limit := 100.0
	group := Group{
		OfficialQuotaFiveHourLimitUSD: &limit,
	}
	extra := map[string]any{
		"codex_5h_used_percent": 90.0,
	}
	windows := map[officialQuotaWindow]officialQuotaWindowUsage{
		officialQuotaWindowFiveHour: {
			UsedPercent:       10,
			ResetAfterSeconds: 5 * 60 * 60,
			WindowMinutes:     5 * 60,
		},
	}

	reset := detectOfficialQuotaReset(extra, windows, group, time.Now())
	if reset.any() {
		t.Fatalf("five-hour official usage drop must not trigger subscription reset: %+v", reset)
	}
	if group.hasOfficialQuotaResetLimit() {
		t.Fatalf("five-hour-only official quota must not make a group eligible for official reset sync")
	}
}

func TestDetectOfficialQuotaResetStillTracksDailyAndWeekly(t *testing.T) {
	dailyLimit := 100.0
	weeklyLimit := 500.0
	group := Group{
		OfficialQuotaDailyLimitUSD:  &dailyLimit,
		OfficialQuotaWeeklyLimitUSD: &weeklyLimit,
	}
	extra := map[string]any{
		"official_quota_day_used_percent": 80.0,
		"codex_7d_used_percent":           70.0,
	}
	windows := map[officialQuotaWindow]officialQuotaWindowUsage{
		officialQuotaWindowDaily: {
			UsedPercent:       5,
			ResetAfterSeconds: 24 * 60 * 60,
			WindowMinutes:     24 * 60,
		},
		officialQuotaWindowWeekly: {
			UsedPercent:       4,
			ResetAfterSeconds: 7 * 24 * 60 * 60,
			WindowMinutes:     7 * 24 * 60,
		},
	}

	reset := detectOfficialQuotaReset(extra, windows, group, time.Now())
	if !reset.Daily || !reset.Weekly || !reset.any() {
		t.Fatalf("daily and weekly official usage drops should still trigger reset: %+v", reset)
	}
	if !group.hasOfficialQuotaResetLimit() {
		t.Fatalf("daily/weekly official quotas should make a group eligible for official reset sync")
	}
}

func TestDetectOfficialQuotaResetUsesDedicatedWeeklyBaseline(t *testing.T) {
	weeklyLimit := 500.0
	group := Group{
		OfficialQuotaWeeklyLimitUSD: &weeklyLimit,
	}
	extra := map[string]any{
		// Gateway traffic may refresh the shared Codex snapshot before the
		// periodic official-quota poll observes the reset.
		"codex_7d_used_percent":          1.0,
		"official_quota_7d_used_percent": 70.0,
	}
	windows := map[officialQuotaWindow]officialQuotaWindowUsage{
		officialQuotaWindowWeekly: {
			UsedPercent:       1,
			ResetAfterSeconds: 7 * 24 * 60 * 60,
			WindowMinutes:     7 * 24 * 60,
		},
	}

	reset := detectOfficialQuotaReset(extra, windows, group, time.Now())
	if !reset.Weekly {
		t.Fatal("dedicated weekly baseline must detect reset after shared Codex snapshot was refreshed")
	}
}

func TestDetectOfficialQuotaResetUsesOfficialWeeklyWindowStart(t *testing.T) {
	weeklyLimit := 500.0
	group := Group{
		OfficialQuotaWeeklyLimitUSD: &weeklyLimit,
	}
	extra := map[string]any{
		"official_quota_7d_used_percent": 70.0,
	}
	base := time.Date(2026, 7, 11, 14, 5, 39, 0, time.UTC)
	windows := map[officialQuotaWindow]officialQuotaWindowUsage{
		officialQuotaWindowWeekly: {
			UsedPercent:       1,
			ResetAfterSeconds: int((6*24*time.Hour + 23*time.Hour + 56*time.Minute + 37*time.Second) / time.Second),
			WindowMinutes:     7 * 24 * 60,
		},
	}

	reset := detectOfficialQuotaReset(extra, windows, group, base)
	if !reset.Weekly {
		t.Fatal("weekly official usage drop should trigger reset")
	}

	want := time.Date(2026, 7, 11, 14, 2, 16, 0, time.UTC)
	if !reset.WeeklyWindowStart.Equal(want) {
		t.Fatalf("weekly reset should use official window start: got %s want %s", reset.WeeklyWindowStart, want)
	}
}

func TestDetectOfficialQuotaResetUsesWindowStartAdvanceBeforePercentDrop(t *testing.T) {
	weeklyLimit := 500.0
	group := Group{
		OfficialQuotaWeeklyLimitUSD: &weeklyLimit,
	}
	base := time.Date(2026, 7, 11, 14, 5, 39, 0, time.UTC)
	extra := map[string]any{
		"official_quota_7d_window_start": time.Date(2026, 7, 4, 14, 2, 16, 0, time.UTC).Format(time.RFC3339),
		"official_quota_7d_used_percent": 1.0,
	}
	windows := map[officialQuotaWindow]officialQuotaWindowUsage{
		officialQuotaWindowWeekly: {
			UsedPercent:       1,
			ResetAfterSeconds: int((6*24*time.Hour + 23*time.Hour + 56*time.Minute + 37*time.Second) / time.Second),
			WindowMinutes:     7 * 24 * 60,
		},
	}

	reset := detectOfficialQuotaReset(extra, windows, group, base)
	if !reset.Weekly {
		t.Fatal("weekly window start advancing by a cycle should trigger reset even without a percent drop")
	}
	if reset.WeeklyReason != "window_start_advanced" {
		t.Fatalf("weekly reset should record window-start reason, got %q", reset.WeeklyReason)
	}
	if !reset.WeeklySignalMismatch {
		t.Fatal("weekly reset should flag signal mismatch when window advanced but percent did not drop")
	}
}

func TestDetectOfficialQuotaResetIgnoresWindowStartJitterEvenWithPercentDrop(t *testing.T) {
	weeklyLimit := 500.0
	group := Group{
		OfficialQuotaWeeklyLimitUSD: &weeklyLimit,
	}
	base := time.Date(2026, 7, 11, 14, 5, 39, 0, time.UTC)
	extra := map[string]any{
		"official_quota_7d_window_start": time.Date(2026, 7, 11, 14, 2, 16, 0, time.UTC).Format(time.RFC3339),
		"official_quota_7d_used_percent": 70.0,
	}
	windows := map[officialQuotaWindow]officialQuotaWindowUsage{
		officialQuotaWindowWeekly: {
			UsedPercent:       1,
			ResetAfterSeconds: int((6*24*time.Hour + 23*time.Hour + 57*time.Minute + 7*time.Second) / time.Second),
			WindowMinutes:     7 * 24 * 60,
		},
	}

	reset := detectOfficialQuotaReset(extra, windows, group, base)
	if reset.Weekly {
		t.Fatal("stored official window start should suppress percent-drop reset when the new start only jitters within one minute")
	}
}

func TestOfficialQuotaSourceBatchesKeepsSharedAccountGroupsTogether(t *testing.T) {
	accountID := int64(207)
	weeklyLimit := 2400.0
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	service := NewOfficialQuotaSyncService(nil, nil, nil, nil, nil)
	service.markChecked(10, now.Add(-5*time.Minute))

	groups := []Group{
		{
			ID:                          6,
			Status:                      StatusActive,
			Platform:                    PlatformOpenAI,
			SubscriptionType:            SubscriptionTypeSubscription,
			QuotaSourceAccountID:        &accountID,
			QuotaFollowOfficialReset:    true,
			OfficialQuotaWeeklyLimitUSD: &weeklyLimit,
		},
		{
			ID:                          10,
			Status:                      StatusActive,
			Platform:                    PlatformOpenAI,
			SubscriptionType:            SubscriptionTypeSubscription,
			QuotaSourceAccountID:        &accountID,
			QuotaFollowOfficialReset:    true,
			OfficialQuotaWeeklyLimitUSD: &weeklyLimit,
		},
	}

	batches := service.sourceBatches(groups, now)
	if len(batches) != 1 {
		t.Fatalf("source batches = %d, want 1", len(batches))
	}
	if !batches[0].due {
		t.Fatal("shared source batch should be due when either group is due")
	}
	if got := officialQuotaGroupIDs(batches[0].groups); len(got) != 2 || got[0] != 6 || got[1] != 10 {
		t.Fatalf("shared source groups = %v, want [6 10]", got)
	}
}
