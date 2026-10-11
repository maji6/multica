ALTER TABLE IF EXISTS vcs_pull_request
    DROP COLUMN IF EXISTS snapshot_lease_until,
    DROP COLUMN IF EXISTS snapshot_lease_id,
    DROP COLUMN IF EXISTS gongfeng_project_id;

ALTER TABLE IF EXISTS gongfeng_repository
    DROP COLUMN IF EXISTS sync_lease_until,
    DROP COLUMN IF EXISTS sync_lease_id,
    DROP COLUMN IF EXISTS enabled;
