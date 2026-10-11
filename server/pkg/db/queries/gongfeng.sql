-- name: ListGongfengRepositories :many
SELECT gr.* FROM gongfeng_repository gr
JOIN vcs_connection vc ON vc.id = gr.connection_id
WHERE gr.connection_id = $1 AND vc.workspace_id = $2 AND gr.enabled
ORDER BY gr.path;

-- name: ListAllGongfengRepositories :many
SELECT gr.* FROM gongfeng_repository gr
JOIN vcs_connection vc ON vc.id = gr.connection_id
WHERE gr.connection_id = $1 AND vc.workspace_id = $2
ORDER BY gr.path;

-- name: GetGongfengRepository :one
SELECT * FROM gongfeng_repository WHERE connection_id = $1 AND project_id = $2;

-- name: LockGongfengRepository :one
SELECT * FROM gongfeng_repository
WHERE connection_id = $1 AND project_id = $2
FOR UPDATE;

-- name: GetGongfengRepositoryByPath :one
SELECT * FROM gongfeng_repository
WHERE connection_id = $1 AND path = $2
ORDER BY enabled DESC, project_id
LIMIT 1;

-- name: AddGongfengRepository :one
INSERT INTO gongfeng_repository (connection_id, project_id, path, web_url, clone_url, description, default_branch)
VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (connection_id, project_id) DO UPDATE SET
    path = EXCLUDED.path, web_url = EXCLUDED.web_url, clone_url = EXCLUDED.clone_url,
    description = EXCLUDED.description, default_branch = EXCLUDED.default_branch,
    enabled = TRUE,
    next_page = CASE WHEN gongfeng_repository.enabled THEN gongfeng_repository.next_page ELSE 1 END,
    scan_started_at = CASE WHEN gongfeng_repository.enabled THEN gongfeng_repository.scan_started_at ELSE NULL END,
    synced_at = CASE WHEN gongfeng_repository.enabled THEN gongfeng_repository.synced_at ELSE NULL END,
    attempted_at = CASE WHEN gongfeng_repository.enabled THEN gongfeng_repository.attempted_at ELSE NULL END,
    hook_revision = CASE WHEN gongfeng_repository.enabled THEN gongfeng_repository.hook_revision ELSE NULL END,
    sync_error = CASE WHEN gongfeng_repository.enabled THEN gongfeng_repository.sync_error ELSE '' END,
    sync_lease_id = CASE WHEN gongfeng_repository.enabled THEN gongfeng_repository.sync_lease_id ELSE NULL END,
    sync_lease_until = CASE WHEN gongfeng_repository.enabled THEN gongfeng_repository.sync_lease_until ELSE NULL END,
    updated_at = now()
RETURNING *;

-- name: DisableGongfengRepository :one
WITH bumped AS (
    UPDATE vcs_connection SET updated_at = now()
    WHERE id = sqlc.arg('connection_id')::uuid AND workspace_id = sqlc.arg('workspace_id')::uuid
    RETURNING id
)
UPDATE gongfeng_repository SET enabled = FALSE, attempted_at = now(), scan_started_at = NULL,
    sync_lease_id = NULL, sync_lease_until = NULL, hook_revision = NULL, updated_at = now()
WHERE connection_id IN (SELECT id FROM bumped) AND project_id = sqlc.arg('project_id')::bigint AND enabled
RETURNING gongfeng_repository.*;

-- name: ResetGongfengRepositorySync :one
UPDATE gongfeng_repository SET next_page = 1, scan_started_at = NULL, synced_at = NULL, attempted_at = NULL,
    hook_revision = NULL, sync_error = '', sync_lease_id = NULL, sync_lease_until = NULL, updated_at = now()
WHERE connection_id = $1 AND project_id = $2 AND enabled RETURNING *;

-- name: ResetGongfengConnectionHooks :exec
UPDATE gongfeng_repository SET hook_revision = NULL, attempted_at = NULL,
    sync_lease_id = NULL, sync_lease_until = NULL, updated_at = now()
WHERE connection_id = $1 AND enabled;

-- name: ListDueGongfengRepositories :many
SELECT gr.* FROM gongfeng_repository gr JOIN vcs_connection vc ON vc.id = gr.connection_id
WHERE vc.provider = 'gongfeng' AND gr.enabled
    AND (gr.sync_lease_until IS NULL OR gr.sync_lease_until < now())
    AND (gr.attempted_at IS NULL OR gr.attempted_at < now() - CASE WHEN gr.sync_error <> '' THEN interval '5 minutes' ELSE interval '1 minute' END)
ORDER BY (ROW(gr.connection_id, gr.project_id) > ROW(sqlc.arg('after_connection')::uuid, sqlc.arg('after_project')::bigint)) DESC,
    gr.connection_id, gr.project_id LIMIT 20;

-- name: ClaimGongfengRepositorySync :one
UPDATE gongfeng_repository SET attempted_at = now(), scan_started_at = COALESCE(scan_started_at, now()),
    sync_lease_id = gen_random_uuid(), sync_lease_until = now() + interval '2 minutes'
WHERE connection_id = $1 AND project_id = $2 AND updated_at = $3 AND enabled
    AND (sync_lease_until IS NULL OR sync_lease_until < now())
RETURNING *;

-- name: SaveGongfengRepositorySync :execrows
WITH live_connection AS MATERIALIZED (
    SELECT id FROM vcs_connection
    WHERE id = sqlc.arg('connection_id')::uuid
      AND updated_at = sqlc.arg('connection_updated_at')::timestamptz
    FOR UPDATE
)
UPDATE gongfeng_repository SET hook_id = sqlc.arg('hook_id')::bigint, next_page = sqlc.arg('next_page')::integer,
    sync_error = sqlc.arg('sync_error')::text, hook_revision = sqlc.narg('hook_revision'),
    synced_at = CASE WHEN sqlc.arg('completed')::boolean THEN scan_started_at ELSE synced_at END,
    scan_started_at = CASE WHEN sqlc.arg('completed')::boolean THEN NULL ELSE scan_started_at END,
    sync_lease_id = NULL, sync_lease_until = NULL
WHERE connection_id IN (SELECT id FROM live_connection) AND project_id = sqlc.arg('project_id')::bigint
    AND updated_at = sqlc.arg('updated_at')::timestamptz AND sync_lease_id = sqlc.arg('lease_id')::uuid AND enabled;

-- name: SaveGongfengRepositoryHook :one
UPDATE gongfeng_repository SET hook_id = sqlc.arg('hook_id')::bigint,
    hook_revision = sqlc.arg('hook_revision')::timestamptz
WHERE connection_id = sqlc.arg('connection_id')::uuid AND project_id = sqlc.arg('project_id')::bigint
    AND updated_at = sqlc.arg('updated_at')::timestamptz
    AND sync_lease_id = sqlc.arg('lease_id')::uuid AND enabled
RETURNING *;

-- name: UpdateGongfengRepositoryMetadata :one
UPDATE gongfeng_repository SET path = $3, web_url = $4, clone_url = $5,
    description = $6, default_branch = $7
WHERE connection_id = $1 AND project_id = $2 AND enabled
RETURNING *;

-- name: UpdateGongfengRepositoryIdentity :one
UPDATE gongfeng_repository SET path = $3, web_url = $4
WHERE connection_id = $1 AND project_id = $2 AND enabled
RETURNING *;

-- name: ListGongfengProjectPullRequests :many
SELECT * FROM vcs_pull_request
WHERE connection_id = sqlc.arg('connection_id')::uuid AND provider = 'gongfeng'
  AND (gongfeng_project_id = sqlc.arg('project_id')::bigint
    OR (gongfeng_project_id = 0 AND repo_owner = sqlc.arg('old_owner')::text AND repo_name = sqlc.arg('old_name')::text)
    OR (gongfeng_project_id = 0 AND repo_owner = sqlc.arg('new_owner')::text AND repo_name = sqlc.arg('new_name')::text))
ORDER BY pr_number, pr_updated_at DESC
FOR UPDATE;

-- name: MergeGongfengPullRequestDuplicate :exec
WITH moved_links AS (
    INSERT INTO issue_vcs_pull_request (issue_id, pull_request_id, close_intent, linked_by_type, linked_by_id, linked_at)
    SELECT issue_id, sqlc.arg('target_id')::uuid, close_intent, linked_by_type, linked_by_id, linked_at
    FROM issue_vcs_pull_request WHERE pull_request_id = sqlc.arg('source_id')::uuid
    ON CONFLICT (issue_id, pull_request_id) DO UPDATE SET
        close_intent = issue_vcs_pull_request.close_intent OR EXCLUDED.close_intent,
        linked_by_type = CASE
            WHEN issue_vcs_pull_request.linked_by_type = 'member'
                AND (EXCLUDED.linked_by_type IS DISTINCT FROM 'member' OR issue_vcs_pull_request.linked_at >= EXCLUDED.linked_at)
                THEN issue_vcs_pull_request.linked_by_type
            ELSE EXCLUDED.linked_by_type END,
        linked_by_id = CASE
            WHEN issue_vcs_pull_request.linked_by_type = 'member'
                AND (EXCLUDED.linked_by_type IS DISTINCT FROM 'member' OR issue_vcs_pull_request.linked_at >= EXCLUDED.linked_at)
                THEN issue_vcs_pull_request.linked_by_id
            ELSE EXCLUDED.linked_by_id END,
        linked_at = CASE
            WHEN issue_vcs_pull_request.linked_by_type = 'member' AND EXCLUDED.linked_by_type = 'member'
                THEN GREATEST(issue_vcs_pull_request.linked_at, EXCLUDED.linked_at)
            WHEN issue_vcs_pull_request.linked_by_type = 'member' THEN issue_vcs_pull_request.linked_at
            WHEN EXCLUDED.linked_by_type = 'member' THEN EXCLUDED.linked_at
            ELSE LEAST(issue_vcs_pull_request.linked_at, EXCLUDED.linked_at) END
), moved_exclusions AS (
    INSERT INTO issue_pull_request_exclusion (issue_id, pull_request_id, workspace_id, excluded_by_type, excluded_by_id, created_at)
    SELECT issue_id, sqlc.arg('target_id')::uuid, workspace_id, excluded_by_type, excluded_by_id, created_at
    FROM issue_pull_request_exclusion WHERE pull_request_id = sqlc.arg('source_id')::uuid
    ON CONFLICT (issue_id, pull_request_id) DO UPDATE SET
        workspace_id = CASE WHEN EXCLUDED.created_at > issue_pull_request_exclusion.created_at THEN EXCLUDED.workspace_id ELSE issue_pull_request_exclusion.workspace_id END,
        excluded_by_type = CASE WHEN EXCLUDED.created_at > issue_pull_request_exclusion.created_at THEN EXCLUDED.excluded_by_type ELSE issue_pull_request_exclusion.excluded_by_type END,
        excluded_by_id = CASE WHEN EXCLUDED.created_at > issue_pull_request_exclusion.created_at THEN EXCLUDED.excluded_by_id ELSE issue_pull_request_exclusion.excluded_by_id END,
        created_at = GREATEST(issue_pull_request_exclusion.created_at, EXCLUDED.created_at)
), deleted_links AS (
    DELETE FROM issue_vcs_pull_request WHERE pull_request_id = sqlc.arg('source_id')::uuid
), deleted_exclusions AS (
    DELETE FROM issue_pull_request_exclusion WHERE pull_request_id = sqlc.arg('source_id')::uuid
)
DELETE FROM vcs_pull_request WHERE id = sqlc.arg('source_id')::uuid;

-- name: DeleteGongfengExcludedPullRequestLinks :exec
DELETE FROM issue_vcs_pull_request l
USING issue_pull_request_exclusion e
WHERE l.pull_request_id = sqlc.arg('pull_request_id')::uuid
  AND e.issue_id = l.issue_id AND e.pull_request_id = l.pull_request_id
  AND (l.linked_by_type IS DISTINCT FROM 'member' OR l.linked_at < e.created_at);

-- name: DeleteGongfengSupersededPullRequestExclusions :exec
DELETE FROM issue_pull_request_exclusion e
USING issue_vcs_pull_request l
WHERE e.pull_request_id = sqlc.arg('pull_request_id')::uuid
  AND l.issue_id = e.issue_id AND l.pull_request_id = e.pull_request_id
  AND l.linked_by_type = 'member' AND l.linked_at >= e.created_at;

-- name: UpdateGongfengPullRequestIdentity :exec
UPDATE vcs_pull_request SET gongfeng_project_id = $2, repo_owner = $3, repo_name = $4,
    html_url = CASE WHEN sqlc.arg('web_url')::text = '' THEN html_url
        ELSE rtrim(sqlc.arg('web_url')::text, '/') || '/merge_requests/' || pr_number::text END,
    updated_at = now()
WHERE id = $1;

-- name: GetGongfengPullRequestByProjectKey :one
SELECT * FROM vcs_pull_request
WHERE connection_id = $1 AND provider = 'gongfeng' AND gongfeng_project_id = $2 AND pr_number = $3;

-- name: ListDueGongfengPullRequests :many
SELECT pr.* FROM vcs_pull_request pr JOIN vcs_connection vc ON vc.id = pr.connection_id
WHERE pr.provider = 'gongfeng' AND pr.state IN ('open', 'draft')
    AND EXISTS (SELECT 1 FROM issue_vcs_pull_request ipr WHERE ipr.pull_request_id = pr.id)
    AND (pr.snapshot_lease_until IS NULL OR pr.snapshot_lease_until < now())
    AND NOT EXISTS (SELECT 1 FROM gongfeng_repository gr
        WHERE gr.connection_id = pr.connection_id AND NOT gr.enabled
          AND ((pr.gongfeng_project_id > 0 AND gr.project_id = pr.gongfeng_project_id)
            OR (pr.gongfeng_project_id = 0 AND gr.path = pr.repo_owner || '/' || pr.repo_name)))
    AND (pr.snapshot_attempted_at IS NULL OR (pr.snapshot_head_sha <> pr.head_sha AND pr.snapshot_error = '') OR pr.snapshot_attempted_at < now() - CASE WHEN pr.snapshot_error <> '' THEN interval '5 minutes' ELSE interval '2 minutes' END)
ORDER BY (pr.id > sqlc.arg('after_id')::uuid) DESC, pr.id LIMIT 40;

-- name: ClaimGongfengSnapshot :one
UPDATE vcs_pull_request pr SET snapshot_lease_id = gen_random_uuid(),
    snapshot_lease_until = now() + interval '2 minutes', snapshot_attempted_at = now()
WHERE pr.id = sqlc.arg('id')::uuid AND pr.workspace_id = sqlc.arg('workspace_id')::uuid
  AND pr.provider = 'gongfeng' AND (pr.snapshot_lease_until IS NULL OR pr.snapshot_lease_until < now())
  AND EXISTS (SELECT 1 FROM vcs_connection vc WHERE vc.id = pr.connection_id
      AND vc.workspace_id = pr.workspace_id AND vc.provider = 'gongfeng'
      AND vc.updated_at = sqlc.arg('connection_updated_at')::timestamptz FOR UPDATE)
  AND NOT EXISTS (SELECT 1 FROM gongfeng_repository gr WHERE gr.connection_id = pr.connection_id AND NOT gr.enabled
      AND ((pr.gongfeng_project_id > 0 AND gr.project_id = pr.gongfeng_project_id)
        OR (pr.gongfeng_project_id = 0 AND gr.path = pr.repo_owner || '/' || pr.repo_name)))
RETURNING pr.*;

-- name: SaveGongfengSnapshot :execrows
WITH live_connection AS MATERIALIZED (
    SELECT vc.id FROM vcs_connection vc
    JOIN vcs_pull_request pr ON pr.connection_id = vc.id
    WHERE pr.id = sqlc.arg('id')::uuid
      AND vc.updated_at = sqlc.arg('updated_at')::timestamptz
    FOR UPDATE OF vc
)
UPDATE vcs_pull_request pr SET snapshot_head_sha = sqlc.arg('snapshot_head_sha')::text, snapshot_fetched_at = now(), snapshot_attempted_at = now(),
    snapshot = sqlc.arg('snapshot')::jsonb, snapshot_error = '', snapshot_lease_id = NULL, snapshot_lease_until = NULL
WHERE pr.id = sqlc.arg('id')::uuid AND pr.head_sha = sqlc.arg('snapshot_head_sha')::text
    AND pr.pr_updated_at = sqlc.arg('pr_updated_at')::timestamptz
    AND pr.snapshot_lease_id = sqlc.arg('snapshot_lease_id')::uuid
    AND pr.connection_id IN (SELECT id FROM live_connection);

-- name: FailGongfengSnapshot :execrows
WITH live_connection AS MATERIALIZED (
    SELECT vc.id FROM vcs_connection vc
    JOIN vcs_pull_request pr ON pr.connection_id = vc.id
    WHERE pr.id = sqlc.arg('id')::uuid
      AND vc.updated_at = sqlc.arg('updated_at')::timestamptz
    FOR UPDATE OF vc
)
UPDATE vcs_pull_request pr SET snapshot_error = sqlc.arg('snapshot_error')::text, snapshot_attempted_at = now(),
    snapshot_lease_id = NULL, snapshot_lease_until = NULL
WHERE pr.id = sqlc.arg('id')::uuid AND pr.head_sha = sqlc.arg('head_sha')::text
    AND pr.pr_updated_at = sqlc.arg('pr_updated_at')::timestamptz
    AND pr.snapshot_lease_id = sqlc.arg('snapshot_lease_id')::uuid
    AND pr.connection_id IN (SELECT id FROM live_connection);
