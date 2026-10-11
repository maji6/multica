CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS gongfeng_pull_request_project_key
    ON vcs_pull_request (connection_id, gongfeng_project_id, pr_number)
    WHERE provider = 'gongfeng' AND gongfeng_project_id > 0;
