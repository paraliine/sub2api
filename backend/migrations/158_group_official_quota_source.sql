ALTER TABLE groups
    ADD COLUMN IF NOT EXISTS quota_source_account_id BIGINT,
    ADD COLUMN IF NOT EXISTS official_quota_five_hour_limit_usd DECIMAL(20,8),
    ADD COLUMN IF NOT EXISTS official_quota_daily_limit_usd DECIMAL(20,8),
    ADD COLUMN IF NOT EXISTS official_quota_weekly_limit_usd DECIMAL(20,8),
    ADD COLUMN IF NOT EXISTS quota_allocation_strategy VARCHAR(50) NOT NULL DEFAULT 'manual',
    ADD COLUMN IF NOT EXISTS quota_follow_official_reset BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN IF NOT EXISTS quota_lag_reconcile_enabled BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN IF NOT EXISTS quota_check_interval_minutes INTEGER NOT NULL DEFAULT 10;

CREATE INDEX IF NOT EXISTS idx_groups_quota_source_account_id
    ON groups (quota_source_account_id)
    WHERE deleted_at IS NULL AND quota_source_account_id IS NOT NULL;
