package service

import "testing"

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

	reset := detectOfficialQuotaReset(extra, windows, group)
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
		"codex_7d_used_percent":          70.0,
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

	reset := detectOfficialQuotaReset(extra, windows, group)
	if !reset.Daily || !reset.Weekly || !reset.any() {
		t.Fatalf("daily and weekly official usage drops should still trigger reset: %+v", reset)
	}
	if !group.hasOfficialQuotaResetLimit() {
		t.Fatalf("daily/weekly official quotas should make a group eligible for official reset sync")
	}
}
