package handler

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/integrations/vcs"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

type GongfengRepositoryResponse struct {
	ID                int64   `json:"id"`
	Path              string  `json:"path"`
	WebURL            string  `json:"web_url"`
	CloneURL          string  `json:"clone_url"`
	Description       string  `json:"description"`
	DefaultBranch     string  `json:"default_branch"`
	Archived          bool    `json:"archived"`
	SyncedAt          *string `json:"synced_at"`
	Syncing           bool    `json:"syncing"`
	SyncError         string  `json:"sync_error"`
	WebhookConfigured bool    `json:"webhook_configured"`
}

func gongfengRepositoryResponse(p db.GongfengRepository) GongfengRepositoryResponse {
	return GongfengRepositoryResponse{ID: p.ProjectID, Path: p.Path, WebURL: p.WebUrl, CloneURL: p.CloneUrl, Description: p.Description, DefaultBranch: p.DefaultBranch, SyncedAt: timestampToPtr(p.SyncedAt), Syncing: p.ScanStartedAt.Valid || !p.SyncedAt.Valid, SyncError: p.SyncError, WebhookConfigured: p.HookID > 0 && p.HookRevision.Valid}
}
func (h *Handler) gongfengClient(conn db.VcsConnection) (*vcs.GongfengClient, error) {
	if !h.isVCSAvailable() || !h.isVCSConfigured() || conn.Provider != "gongfeng" {
		return nil, errors.New("Gongfeng integration is not configured")
	}
	token, err := h.openVCSSecret(conn.AccessTokenEncrypted)
	if err != nil {
		return nil, errors.New("Could not decrypt the Gongfeng token")
	}
	return vcs.NewGongfengClient(conn.InstanceUrl, token)
}
func (h *Handler) loadGongfengConnection(w http.ResponseWriter, r *http.Request, write bool) (db.VcsConnection, *vcs.GongfengClient, func(), bool) {
	if !h.isVCSAvailable() {
		writeError(w, http.StatusNotFound, "vcs integration is not available")
		return db.VcsConnection{}, nil, nil, false
	}
	if !h.isVCSConfigured() {
		writeFeatureDisabled(w, "vcs_not_configured", "vcs integration not configured")
		return db.VcsConnection{}, nil, nil, false
	}
	ws, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "id"), "workspace id")
	if !ok {
		return db.VcsConnection{}, nil, nil, false
	}
	id, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "connectionId"), "connection id")
	if !ok {
		return db.VcsConnection{}, nil, nil, false
	}
	unlock := h.GongfengSync.lockConnection(id, write)
	conn, err := h.Queries.GetVCSConnectionByID(r.Context(), id)
	if err != nil || conn.WorkspaceID != ws || conn.Provider != "gongfeng" {
		unlock()
		writeError(w, 404, "Gongfeng connection not found")
		return db.VcsConnection{}, nil, nil, false
	}
	client, err := h.gongfengClient(conn)
	if err != nil {
		writeError(w, 500, err.Error())
		unlock()
		return conn, nil, nil, false
	}
	return conn, client, unlock, true
}
func gongfengProjectID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(chi.URLParam(r, "projectId"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, 400, "invalid project id")
		return 0, false
	}
	return id, true
}

func splitGongfengProjectPath(path string) (string, string, bool) {
	idx := strings.LastIndex(strings.Trim(path, "/"), "/")
	if idx <= 0 || idx == len(strings.Trim(path, "/"))-1 {
		return "", "", false
	}
	path = strings.Trim(path, "/")
	return path[:idx], path[idx+1:], true
}

// reconcileGongfengIdentityTx makes the provider's immutable project ID the
// local identity. Older rows used only the mutable namespace/path, so a rename
// can leave two cards for the same merge request. Merge those rows before
// moving the survivor to the current path.
func reconcileGongfengIdentityTx(ctx context.Context, q *db.Queries, connectionID pgtype.UUID, projectID int64, oldPath, newPath, webURL string) error {
	newOwner, newName, ok := splitGongfengProjectPath(newPath)
	if !ok {
		return errors.New("Gongfeng returned an invalid repository identity")
	}
	oldOwner, oldName, ok := splitGongfengProjectPath(oldPath)
	if !ok {
		oldOwner, oldName = newOwner, newName
	}
	prs, err := q.ListGongfengProjectPullRequests(ctx, db.ListGongfengProjectPullRequestsParams{
		ConnectionID: connectionID,
		ProjectID:    projectID,
		OldOwner:     oldOwner,
		OldName:      oldName,
		NewOwner:     newOwner,
		NewName:      newName,
	})
	if err != nil {
		return err
	}
	byNumber := make(map[int32][]db.VcsPullRequest)
	for _, pr := range prs {
		byNumber[pr.PrNumber] = append(byNumber[pr.PrNumber], pr)
	}
	for _, duplicates := range byNumber {
		// The query orders newest first. Keep the freshest payload while moving
		// every link and explicit exclusion off legacy path-keyed duplicates.
		target := duplicates[0]
		for _, source := range duplicates[1:] {
			if err := q.MergeGongfengPullRequestDuplicate(ctx, db.MergeGongfengPullRequestDuplicateParams{TargetID: target.ID, SourceID: source.ID}); err != nil {
				return err
			}
		}
		if err := q.UpdateGongfengPullRequestIdentity(ctx, db.UpdateGongfengPullRequestIdentityParams{
			ID: target.ID, GongfengProjectID: projectID, RepoOwner: newOwner, RepoName: newName, WebUrl: webURL,
		}); err != nil {
			return err
		}
		if err := q.DeleteGongfengExcludedPullRequestLinks(ctx, target.ID); err != nil {
			return err
		}
		if err := q.DeleteGongfengSupersededPullRequestExclusions(ctx, target.ID); err != nil {
			return err
		}
	}
	return nil
}

func (h *Handler) selectGongfengProject(ctx context.Context, conn db.VcsConnection, project vcs.GongfengProject) (db.GongfengRepository, error) {
	tx, err := h.TxStarter.Begin(ctx)
	if err != nil {
		return db.GongfengRepository{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	q := h.Queries.WithTx(tx)
	if _, err := q.LockVCSConnectionRevision(ctx, db.LockVCSConnectionRevisionParams{
		ID: conn.ID, WorkspaceID: conn.WorkspaceID, Provider: conn.Provider, UpdatedAt: conn.UpdatedAt,
	}); err != nil {
		return db.GongfengRepository{}, err
	}
	oldPath := project.Path
	if existing, lockErr := q.LockGongfengRepository(ctx, db.LockGongfengRepositoryParams{ConnectionID: conn.ID, ProjectID: project.ID}); lockErr == nil {
		oldPath = existing.Path
	} else if !errors.Is(lockErr, pgx.ErrNoRows) {
		return db.GongfengRepository{}, lockErr
	}
	row, err := q.AddGongfengRepository(ctx, db.AddGongfengRepositoryParams{
		ConnectionID: conn.ID, ProjectID: project.ID, Path: project.Path, WebUrl: project.WebURL,
		CloneUrl: project.CloneURL(), Description: project.Description, DefaultBranch: project.DefaultBranch,
	})
	if err != nil {
		return db.GongfengRepository{}, err
	}
	if err := reconcileGongfengIdentityTx(ctx, q, conn.ID, project.ID, oldPath, project.Path, project.WebURL); err != nil {
		return db.GongfengRepository{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return db.GongfengRepository{}, err
	}
	return row, nil
}

// reconcileGongfengProject refreshes selected-repository metadata without
// selecting a manually configured webhook. It returns false for an explicitly
// stopped repository so late deliveries are acknowledged without mutation.
func (h *Handler) reconcileGongfengProject(ctx context.Context, conn db.VcsConnection, project vcs.GongfengProject, fullMetadata bool) (db.GongfengRepository, bool, error) {
	tx, err := h.TxStarter.Begin(ctx)
	if err != nil {
		return db.GongfengRepository{}, false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	q := h.Queries.WithTx(tx)
	if _, lockErr := q.LockVCSConnectionRevision(ctx, db.LockVCSConnectionRevisionParams{
		ID: conn.ID, WorkspaceID: conn.WorkspaceID, Provider: conn.Provider, UpdatedAt: conn.UpdatedAt,
	}); errors.Is(lockErr, pgx.ErrNoRows) {
		return db.GongfengRepository{}, false, nil
	} else if lockErr != nil {
		return db.GongfengRepository{}, false, lockErr
	}
	row, lockErr := q.LockGongfengRepository(ctx, db.LockGongfengRepositoryParams{ConnectionID: conn.ID, ProjectID: project.ID})
	oldPath := project.Path
	selected := lockErr == nil
	if selected {
		oldPath = row.Path
		if !row.Enabled {
			return row, false, nil
		}
	} else if !errors.Is(lockErr, pgx.ErrNoRows) {
		return db.GongfengRepository{}, false, lockErr
	}
	if err := reconcileGongfengIdentityTx(ctx, q, conn.ID, project.ID, oldPath, project.Path, project.WebURL); err != nil {
		return db.GongfengRepository{}, false, err
	}
	if selected {
		metadataChanged := row.Path != project.Path || row.WebUrl != project.WebURL || row.CloneUrl != project.CloneURL() ||
			row.Description != project.Description || row.DefaultBranch != project.DefaultBranch
		if fullMetadata && metadataChanged {
			row, err = q.UpdateGongfengRepositoryMetadata(ctx, db.UpdateGongfengRepositoryMetadataParams{
				ConnectionID: conn.ID, ProjectID: project.ID, Path: project.Path, WebUrl: project.WebURL,
				CloneUrl: project.CloneURL(), Description: project.Description, DefaultBranch: project.DefaultBranch,
			})
		} else if row.Path != project.Path || (project.WebURL != "" && row.WebUrl != project.WebURL) {
			row, err = q.UpdateGongfengRepositoryIdentity(ctx, db.UpdateGongfengRepositoryIdentityParams{
				ConnectionID: conn.ID, ProjectID: project.ID, Path: project.Path, WebUrl: project.WebURL,
			})
		}
		if err != nil {
			return db.GongfengRepository{}, false, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return db.GongfengRepository{}, false, err
	}
	return row, true, nil
}

// Repository management is admin-only at the router, just like GitHub browsing.
func (h *Handler) ListGongfengRepositories(w http.ResponseWriter, r *http.Request) {
	conn, client, unlock, ok := h.loadGongfengConnection(w, r, false)
	if !ok {
		return
	}
	defer unlock()
	page := 1
	if raw := r.URL.Query().Get("page"); raw != "" {
		var err error
		page, err = strconv.Atoi(raw)
		if err != nil || page < 1 || page > 10000 {
			writeError(w, 400, "invalid page")
			return
		}
	}
	rows, err := h.Queries.ListGongfengRepositories(r.Context(), db.ListGongfengRepositoriesParams{ConnectionID: conn.ID, WorkspaceID: conn.WorkspaceID})
	if err != nil {
		writeError(w, 500, "failed to list selected repositories")
		return
	}
	selected := make([]GongfengRepositoryResponse, 0, len(rows))
	for _, row := range rows {
		selected = append(selected, gongfengRepositoryResponse(row))
	}
	projects, next, err := client.Projects(r.Context(), r.URL.Query().Get("search"), page)
	if err != nil {
		writeJSON(w, 200, map[string]any{
			"repositories": []GongfengRepositoryResponse{}, "selected": selected, "next_page": nil,
			"browse_error": "Gongfeng repository browsing is unavailable.",
		})
		return
	}
	repositories := make([]GongfengRepositoryResponse, 0, len(projects))
	for _, p := range projects {
		repositories = append(repositories, GongfengRepositoryResponse{ID: p.ID, Path: p.Path, WebURL: p.WebURL, CloneURL: p.CloneURL(), Description: p.Description, DefaultBranch: p.DefaultBranch, Archived: p.Archived})
	}
	writeJSON(w, 200, map[string]any{"repositories": repositories, "selected": selected, "next_page": next})
}
func (h *Handler) AddGongfengRepository(w http.ResponseWriter, r *http.Request) {
	conn, client, unlock, ok := h.loadGongfengConnection(w, r, true)
	if !ok {
		return
	}
	defer unlock()
	id, ok := gongfengProjectID(w, r)
	if !ok {
		return
	}
	project, err := client.Project(r.Context(), strconv.FormatInt(id, 10))
	if err != nil {
		writeError(w, 502, err.Error())
		return
	}
	if project.ID != id || project.Archived {
		writeError(w, 400, "repository is archived or has a mismatched identity")
		return
	}
	row, err := h.selectGongfengProject(r.Context(), conn, project)
	if err != nil {
		writeError(w, 500, "failed to save repository")
		return
	}
	h.GongfengSync.enqueueRepository(row)
	writeJSON(w, 200, gongfengRepositoryResponse(row))
}
func (h *Handler) RetryGongfengRepository(w http.ResponseWriter, r *http.Request) {
	conn, _, unlock, ok := h.loadGongfengConnection(w, r, true)
	if !ok {
		return
	}
	defer unlock()
	id, ok := gongfengProjectID(w, r)
	if !ok {
		return
	}
	row, err := h.Queries.ResetGongfengRepositorySync(r.Context(), db.ResetGongfengRepositorySyncParams{ConnectionID: conn.ID, ProjectID: id})
	if err != nil {
		writeError(w, 404, "selected repository not found")
		return
	}
	h.GongfengSync.enqueueRepository(row)
	writeJSON(w, 200, gongfengRepositoryResponse(row))
}
func (h *Handler) DeleteGongfengRepository(w http.ResponseWriter, r *http.Request) {
	conn, client, unlock, ok := h.loadGongfengConnection(w, r, true)
	if !ok {
		return
	}
	defer unlock()
	id, ok := gongfengProjectID(w, r)
	if !ok {
		return
	}
	row, err := h.Queries.GetGongfengRepository(r.Context(), db.GetGongfengRepositoryParams{ConnectionID: conn.ID, ProjectID: id})
	if err != nil {
		writeError(w, 404, "selected repository not found")
		return
	}
	row, err = h.Queries.DisableGongfengRepository(r.Context(), db.DisableGongfengRepositoryParams{ConnectionID: conn.ID, WorkspaceID: conn.WorkspaceID, ProjectID: id})
	if err != nil {
		writeError(w, 500, "failed to remove repository")
		return
	}
	if err = client.RemoveHook(r.Context(), id, row.HookID, h.vcsWebhookURL(uuidToString(conn.ID))); err != nil {
		writeJSON(w, 200, map[string]string{"webhook_cleanup_error": "Repository stopped, but its Gongfeng webhook could not be removed. Remove it manually in Gongfeng."})
		return
	}
	w.WriteHeader(204)
}
func (h *Handler) resetGongfengHooks(ctx context.Context, conn db.VcsConnection) error {
	if conn.Provider != "gongfeng" {
		return nil
	}
	if err := h.Queries.ResetGongfengConnectionHooks(ctx, conn.ID); err != nil {
		return err
	}
	rows, err := h.Queries.ListGongfengRepositories(ctx, db.ListGongfengRepositoriesParams{ConnectionID: conn.ID, WorkspaceID: conn.WorkspaceID})
	if err != nil {
		return err
	}
	for _, row := range rows {
		h.GongfengSync.enqueueRepository(row)
	}
	return nil
}
func (h *Handler) removeGongfengHooks(ctx context.Context, conn db.VcsConnection) error {
	rows, err := h.Queries.ListAllGongfengRepositories(ctx, db.ListAllGongfengRepositoriesParams{ConnectionID: conn.ID, WorkspaceID: conn.WorkspaceID})
	if err != nil {
		return err
	}
	return h.removeGongfengHookRows(ctx, conn, rows)
}

func (h *Handler) removeGongfengHookRows(ctx context.Context, conn db.VcsConnection, rows []db.GongfengRepository) error {
	client, err := h.gongfengClient(conn)
	if err != nil {
		return err
	}
	for _, row := range rows {
		if err := client.RemoveHook(ctx, row.ProjectID, row.HookID, h.vcsWebhookURL(uuidToString(conn.ID))); err != nil {
			return err
		}
	}
	return nil
}

// deleteGongfengConnectionLocally fences remote hook creation, captures every
// selected project after older workers finish, and revokes local access in one
// transaction. Remote cleanup deliberately follows the commit so an expired
// provider token cannot keep the connection active in Multica.
func (h *Handler) deleteGongfengConnectionLocally(ctx context.Context, connectionID, workspaceID pgtype.UUID) (db.VcsConnection, []db.GongfengRepository, error) {
	tx, err := h.TxStarter.Begin(ctx)
	if err != nil {
		return db.VcsConnection{}, nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	q := h.Queries.WithTx(tx)
	conn, err := q.LockVCSConnection(ctx, db.LockVCSConnectionParams{ID: connectionID, WorkspaceID: workspaceID})
	if err != nil {
		return db.VcsConnection{}, nil, err
	}
	rows, err := q.ListAllGongfengRepositories(ctx, db.ListAllGongfengRepositoriesParams{ConnectionID: connectionID, WorkspaceID: workspaceID})
	if err != nil {
		return db.VcsConnection{}, nil, err
	}
	if err := q.DeleteVCSConnection(ctx, db.DeleteVCSConnectionParams{ID: connectionID, WorkspaceID: workspaceID}); err != nil {
		return db.VcsConnection{}, nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return db.VcsConnection{}, nil, err
	}
	return conn, rows, nil
}

// Keep workers and callbacks outside workspace teardown. Capture hook metadata
// before its rows disappear, and remove remote hooks only after the DB commits.
func (h *Handler) prepareGongfengWorkspaceDeletion(ctx context.Context, wsID pgtype.UUID) (func(context.Context), func(), error) {
	connections, err := h.Queries.ListVCSConnectionsByWorkspace(ctx, wsID)
	if err != nil {
		return nil, nil, err
	}
	sort.Slice(connections, func(i, j int) bool { return uuidToString(connections[i].ID) < uuidToString(connections[j].ID) })
	var unlocks []func()
	unlock := func() {
		for i := len(unlocks) - 1; i >= 0; i-- {
			unlocks[i]()
		}
	}
	type hookCleanup struct {
		connection db.VcsConnection
		repository db.GongfengRepository
	}
	var hooks []hookCleanup
	for _, conn := range connections {
		if conn.Provider != "gongfeng" {
			continue
		}
		unlocks = append(unlocks, h.GongfengSync.lockConnection(conn.ID, true))
		conn, err = h.Queries.GetVCSConnectionByID(ctx, conn.ID)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			unlock()
			return nil, nil, err
		}
		rows, err := h.Queries.ListAllGongfengRepositories(ctx, db.ListAllGongfengRepositoriesParams{ConnectionID: conn.ID, WorkspaceID: wsID})
		if err != nil {
			unlock()
			return nil, nil, err
		}
		for _, row := range rows {
			hooks = append(hooks, hookCleanup{conn, row})
		}
	}
	cleanup := func(ctx context.Context) {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		for _, hook := range hooks {
			client, err := h.gongfengClient(hook.connection)
			if err == nil {
				err = client.RemoveHook(ctx, hook.repository.ProjectID, hook.repository.HookID, h.vcsWebhookURL(uuidToString(hook.connection.ID)))
			}
			if err != nil {
				slog.Warn("gongfeng: deleted workspace webhook cleanup failed", "connection_id", uuidToString(hook.connection.ID), "project_id", hook.repository.ProjectID, "err", err)
			}
		}
	}
	return cleanup, unlock, nil
}

func (h *Handler) publishGongfengSnapshot(ctx context.Context, pr db.VcsPullRequest) {
	ids, err := h.Queries.ListIssueIDsForVCSPullRequest(ctx, pr.ID)
	if err != nil {
		return
	}
	linked := make([]string, 0, len(ids))
	for _, id := range ids {
		linked = append(linked, uuidToString(id))
	}
	h.publish(protocol.EventPullRequestUpdated, uuidToString(pr.WorkspaceID), "system", "", map[string]any{"linked_issue_ids": linked, "pull_request": vcsPullRequestToResponse(pr)})
}
