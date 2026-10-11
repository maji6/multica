package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/integrations/vcs"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func TestGongfengStoppedRepositoryFencesLateWorkWhenCleanupFails(t *testing.T) {
	ctx := context.Background()
	withVCSBox(t)
	var providerCalls int
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		providerCalls++
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer provider.Close()
	conn, pr := seedGongfengSyncPR(t, provider.URL)
	t.Cleanup(func() {
		_ = testHandler.Queries.DeleteVCSConnection(ctx, db.DeleteVCSConnectionParams{ID: conn.ID, WorkspaceID: conn.WorkspaceID})
	})
	row, err := testHandler.Queries.AddGongfengRepository(ctx, db.AddGongfengRepositoryParams{
		ConnectionID: conn.ID, ProjectID: 55, Path: "acme/widget", WebUrl: "https://git.code.tencent.com/acme/widget",
	})
	if err != nil {
		t.Fatal(err)
	}
	dbfx.Exec(t, "UPDATE gongfeng_repository SET hook_id=9 WHERE connection_id=$1 AND project_id=55", conn.ID)
	req := testutil.WithURLParams(newRequest("DELETE", "/repositories/55", nil),
		"id", uuidToString(conn.WorkspaceID), "connectionId", uuidToString(conn.ID), "projectId", "55")
	var warning struct {
		Error string `json:"webhook_cleanup_error"`
	}
	testutil.Call(t, testHandler.DeleteGongfengRepository, req).Want(http.StatusOK).JSON(&warning)
	if warning.Error == "" {
		t.Fatal("cleanup failure did not return a safe warning")
	}
	stopped, err := testHandler.Queries.GetGongfengRepository(ctx, db.GetGongfengRepositoryParams{ConnectionID: conn.ID, ProjectID: 55})
	if err != nil || stopped.Enabled {
		t.Fatalf("repository was not durably stopped: enabled=%v err=%v", stopped.Enabled, err)
	}
	callsAfterStop := providerCalls
	testHandler.GongfengSync.syncRepository(ctx, row)
	testHandler.GongfengSync.refreshPR(ctx, pr)
	if providerCalls != callsAfterStop {
		t.Fatalf("stopped repository made %d late provider requests", providerCalls-callsAfterStop)
	}
	event := vcs.PullRequestEvent{
		ProjectID: 55, RepoOwner: "acme", RepoName: "widget", Number: 7,
		Title: "Late merge", State: "merged", HTMLURL: pr.HtmlUrl, Branch: "feature",
		CreatedAt: "2026-10-09T07:00:00Z", UpdatedAt: "2026-10-09T09:00:00Z",
	}
	if err := testHandler.mirrorVCSPullRequest(ctx, conn, event); err != nil {
		t.Fatal(err)
	}
	got, err := testHandler.Queries.GetVCSPullRequestInWorkspace(ctx, db.GetVCSPullRequestInWorkspaceParams{ID: pr.ID, WorkspaceID: pr.WorkspaceID})
	if err != nil || got.State != "open" {
		t.Fatalf("late event mutated stopped MR: state=%q err=%v", got.State, err)
	}
	reenabled, err := testHandler.Queries.AddGongfengRepository(ctx, db.AddGongfengRepositoryParams{
		ConnectionID: conn.ID, ProjectID: 55, Path: "acme/widget", WebUrl: "https://git.code.tencent.com/acme/widget",
	})
	if err != nil || !reenabled.Enabled || reenabled.NextPage != 1 || reenabled.SyncLeaseID.Valid {
		t.Fatalf("re-enable did not reset durable stop state: %+v err=%v", reenabled, err)
	}
}

func TestGongfengRepositoryListKeepsSelectionsWhenBrowseFails(t *testing.T) {
	ctx := context.Background()
	withVCSBox(t)
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusUnauthorized) }))
	defer provider.Close()
	conn, _ := seedGongfengSyncPR(t, provider.URL)
	t.Cleanup(func() {
		_ = testHandler.Queries.DeleteVCSConnection(ctx, db.DeleteVCSConnectionParams{ID: conn.ID, WorkspaceID: conn.WorkspaceID})
	})
	if _, err := testHandler.Queries.AddGongfengRepository(ctx, db.AddGongfengRepositoryParams{
		ConnectionID: conn.ID, ProjectID: 55, Path: "acme/widget", WebUrl: "https://git.code.tencent.com/acme/widget",
	}); err != nil {
		t.Fatal(err)
	}
	req := testutil.WithURLParams(newRequest("GET", "/repositories", nil), "id", uuidToString(conn.WorkspaceID), "connectionId", uuidToString(conn.ID))
	var response struct {
		Repositories []GongfengRepositoryResponse `json:"repositories"`
		Selected     []GongfengRepositoryResponse `json:"selected"`
		NextPage     *int                         `json:"next_page"`
		BrowseError  string                       `json:"browse_error"`
	}
	testutil.Call(t, testHandler.ListGongfengRepositories, req).Want(http.StatusOK).JSON(&response)
	if len(response.Repositories) != 0 || len(response.Selected) != 1 || response.NextPage != nil || response.BrowseError == "" {
		t.Fatalf("degraded repository list lost local state: %+v", response)
	}
}

func TestGongfengConnectionRevisionFenceBlocksStop(t *testing.T) {
	ctx := context.Background()
	withVCSBox(t)
	conn, pr := seedGongfengSyncPR(t, "https://gongfeng-fence.test")
	t.Cleanup(func() {
		_ = testHandler.Queries.DeleteVCSConnection(ctx, db.DeleteVCSConnectionParams{ID: conn.ID, WorkspaceID: conn.WorkspaceID})
	})
	if _, err := testHandler.Queries.AddGongfengRepository(ctx, db.AddGongfengRepositoryParams{
		ConnectionID: conn.ID, ProjectID: 55, Path: "acme/widget", WebUrl: "https://git.code.tencent.com/acme/widget",
	}); err != nil {
		t.Fatal(err)
	}
	tx, err := testPool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	qtx := testHandler.Queries.WithTx(tx)
	if _, err := qtx.UpsertVCSPullRequest(ctx, db.UpsertVCSPullRequestParams{
		WorkspaceID: conn.WorkspaceID, ConnectionID: conn.ID, Provider: "gongfeng",
		RepoOwner: "acme", RepoName: "widget", PrNumber: 7, Title: "Held mirror", State: "open", HtmlUrl: pr.HtmlUrl,
		PrCreatedAt: pr.PrCreatedAt, PrUpdatedAt: pr.PrUpdatedAt, HeadSha: pr.HeadSha,
		GongfengProjectID: 55, ConnectionUpdatedAt: conn.UpdatedAt,
	}); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := testHandler.Queries.DisableGongfengRepository(ctx, db.DisableGongfengRepositoryParams{
			ConnectionID: conn.ID, WorkspaceID: conn.WorkspaceID, ProjectID: 55,
		})
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("stop bypassed an older mirror transaction: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("stop did not resume after mirror commit")
	}
	_, err = testHandler.Queries.UpsertVCSPullRequest(ctx, db.UpsertVCSPullRequestParams{
		WorkspaceID: conn.WorkspaceID, ConnectionID: conn.ID, Provider: "gongfeng",
		RepoOwner: "acme", RepoName: "widget", PrNumber: 7, Title: "Stale mirror", State: "merged", HtmlUrl: pr.HtmlUrl,
		PrCreatedAt: pr.PrCreatedAt, PrUpdatedAt: pgtype.Timestamptz{Time: pr.PrUpdatedAt.Time.Add(time.Minute), Valid: true},
		GongfengProjectID: 55, ConnectionUpdatedAt: conn.UpdatedAt,
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("old revision write after stop returned %v, want no rows", err)
	}
}

func TestGongfengDisconnectWaitsForOlderMirrorTransaction(t *testing.T) {
	ctx := context.Background()
	withVCSBox(t)
	conn, pr := seedGongfengSyncPR(t, "https://gongfeng-disconnect-fence.test")
	if _, err := testHandler.Queries.AddGongfengRepository(ctx, db.AddGongfengRepositoryParams{
		ConnectionID: conn.ID, ProjectID: 55, Path: "acme/widget", WebUrl: "https://git.code.tencent.com/acme/widget",
	}); err != nil {
		t.Fatal(err)
	}
	tx, err := testPool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := testHandler.Queries.WithTx(tx).UpsertVCSPullRequest(ctx, db.UpsertVCSPullRequestParams{
		WorkspaceID: conn.WorkspaceID, ConnectionID: conn.ID, Provider: "gongfeng",
		RepoOwner: "acme", RepoName: "widget", PrNumber: 7, Title: "Held mirror", State: "open", HtmlUrl: pr.HtmlUrl,
		PrCreatedAt: pr.PrCreatedAt, PrUpdatedAt: pr.PrUpdatedAt, GongfengProjectID: 55, ConnectionUpdatedAt: conn.UpdatedAt,
	}); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		done <- testHandler.Queries.DeleteVCSConnection(ctx, db.DeleteVCSConnectionParams{ID: conn.ID, WorkspaceID: conn.WorkspaceID})
	}()
	select {
	case err := <-done:
		t.Fatalf("disconnect bypassed an older mirror transaction: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("disconnect did not resume after mirror commit")
	}
	for _, table := range []string{"vcs_connection", "gongfeng_repository", "vcs_pull_request"} {
		var count int
		if err := testPool.QueryRow(ctx, "SELECT count(*) FROM "+table+" WHERE "+map[string]string{"vcs_connection": "id", "gongfeng_repository": "connection_id", "vcs_pull_request": "connection_id"}[table]+"=$1", conn.ID).Scan(&count); err != nil || count != 0 {
			t.Fatalf("disconnect left %s count=%d err=%v", table, count, err)
		}
	}
}

func TestGongfengDisconnectWaitsForRemoteHookCreationAndRemovesExactCallback(t *testing.T) {
	ctx := context.Background()
	withVCSBox(t)
	started := make(chan struct{})
	release := make(chan struct{})
	var mu sync.Mutex
	hookExists := false
	endpoint := ""
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		exists := hookExists
		mu.Unlock()
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v3/projects/55":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 55, "path_with_namespace": "acme/widget", "web_url": "https://git.code.tencent.com/acme/widget"})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v3/projects/55/hooks":
			if exists {
				_ = json.NewEncoder(w).Encode([]map[string]any{{"id": 9, "url": endpoint}})
			} else {
				_, _ = w.Write([]byte(`[]`))
			}
		case r.Method == http.MethodPost && r.URL.Path == "/api/v3/projects/55/hooks":
			close(started)
			<-release
			mu.Lock()
			hookExists = true
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 9})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v3/projects/55/hooks/9":
			if !exists {
				http.NotFound(w, r)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 9, "url": endpoint})
		case r.Method == http.MethodDelete && r.URL.Path == "/api/v3/projects/55/hooks/9":
			mu.Lock()
			hookExists = false
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v3/projects/55/merge_requests":
			_, _ = w.Write([]byte(`[]`))
		default:
			t.Errorf("unexpected provider request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer provider.Close()
	conn, _ := seedGongfengSyncPR(t, provider.URL)
	oldPublic := testHandler.cfg.PublicURL
	testHandler.cfg.PublicURL = "https://multica.example"
	t.Cleanup(func() { testHandler.cfg.PublicURL = oldPublic })
	endpoint = testHandler.vcsWebhookURL(uuidToString(conn.ID))
	row, err := testHandler.Queries.AddGongfengRepository(ctx, db.AddGongfengRepositoryParams{
		ConnectionID: conn.ID, ProjectID: 55, Path: "acme/widget", WebUrl: "https://git.code.tencent.com/acme/widget",
	})
	if err != nil {
		t.Fatal(err)
	}
	worker := make(chan struct{})
	go func() {
		testHandler.GongfengSync.syncRepository(ctx, row)
		close(worker)
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("remote hook creation did not start")
	}

	// A distinct manager models another server instance: there is no shared
	// in-process connection mutex, so only the database fence can serialize it.
	other := *testHandler
	other.GongfengSync = newGongfengSync(&other)
	deleted := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		w := httptest.NewRecorder()
		other.DeleteVCSConnection(w, vcsHandlerRequest("DELETE", "/connection", nil, uuidToString(conn.ID)))
		deleted <- w
	}()
	select {
	case w := <-deleted:
		close(release)
		t.Fatalf("disconnect returned before remote hook identity was fenced: %d %s", w.Code, w.Body.String())
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	select {
	case <-worker:
	case <-time.After(3 * time.Second):
		t.Fatal("repository worker did not finish")
	}
	select {
	case w := <-deleted:
		if w.Code != http.StatusNoContent {
			t.Fatalf("disconnect status %d: %s", w.Code, w.Body.String())
		}
	case <-time.After(3 * time.Second):
		t.Fatal("disconnect did not resume after hook creation")
	}
	mu.Lock()
	defer mu.Unlock()
	if hookExists {
		t.Fatal("disconnect returned with the exact callback hook still present")
	}
}

func TestGongfengSnapshotMetadataRefreshDoesNotInvalidateRepositoryLease(t *testing.T) {
	ctx := context.Background()
	withVCSBox(t)
	conn, _ := seedGongfengSyncPR(t, "https://gongfeng-metadata-fence.test")
	t.Cleanup(func() {
		_ = testHandler.Queries.DeleteVCSConnection(ctx, db.DeleteVCSConnectionParams{ID: conn.ID, WorkspaceID: conn.WorkspaceID})
	})
	row, err := testHandler.Queries.AddGongfengRepository(ctx, db.AddGongfengRepositoryParams{
		ConnectionID: conn.ID, ProjectID: 55, Path: "acme/widget", WebUrl: "https://git.code.tencent.com/acme/widget",
		CloneUrl: "git@git.code.tencent.com:acme/widget.git", Description: "Widget", DefaultBranch: "main",
	})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := testHandler.Queries.ClaimGongfengRepositorySync(ctx, db.ClaimGongfengRepositorySyncParams{
		ConnectionID: conn.ID, ProjectID: 55, UpdatedAt: row.UpdatedAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	refreshed, allowed, err := testHandler.reconcileGongfengProject(ctx, conn, vcs.GongfengProject{
		ID: 55, Path: "acme/widget", WebURL: "https://git.code.tencent.com/acme/widget",
		SSHURL: "git@git.code.tencent.com:acme/widget.git", Description: "Widget", DefaultBranch: "main",
	}, true)
	if err != nil || !allowed || refreshed.UpdatedAt != claimed.UpdatedAt {
		t.Fatalf("unchanged metadata invalidated scanner revision: allowed=%v before=%v after=%v err=%v", allowed, claimed.UpdatedAt, refreshed.UpdatedAt, err)
	}
	count, err := testHandler.Queries.SaveGongfengRepositorySync(ctx, db.SaveGongfengRepositorySyncParams{
		ConnectionID: conn.ID, ProjectID: 55, UpdatedAt: claimed.UpdatedAt, LeaseID: claimed.SyncLeaseID,
		ConnectionUpdatedAt: conn.UpdatedAt, HookID: 9, HookRevision: conn.UpdatedAt, NextPage: 1, Completed: true,
	})
	if err != nil || count != 1 {
		t.Fatalf("scanner could not save hook/progress after snapshot refresh: count=%d err=%v", count, err)
	}
	saved, err := testHandler.Queries.GetGongfengRepository(ctx, db.GetGongfengRepositoryParams{ConnectionID: conn.ID, ProjectID: 55})
	if err != nil || saved.HookID != 9 || !saved.SyncedAt.Valid || saved.SyncLeaseID.Valid {
		t.Fatalf("scanner progress not persisted after refresh: %+v err=%v", saved, err)
	}
}

func TestGongfengSnapshotSaveAndFailureHoldConnectionFence(t *testing.T) {
	for _, failed := range []bool{false, true} {
		name := "save"
		if failed {
			name = "failure"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			withVCSBox(t)
			conn, pr := seedGongfengSyncPR(t, "https://gongfeng-snapshot-fence.test/"+name)
			t.Cleanup(func() {
				_ = testHandler.Queries.DeleteVCSConnection(ctx, db.DeleteVCSConnectionParams{ID: conn.ID, WorkspaceID: conn.WorkspaceID})
			})
			if _, err := testHandler.Queries.AddGongfengRepository(ctx, db.AddGongfengRepositoryParams{
				ConnectionID: conn.ID, ProjectID: 55, Path: "acme/widget", WebUrl: "https://git.code.tencent.com/acme/widget",
			}); err != nil {
				t.Fatal(err)
			}
			claimed, err := testHandler.Queries.ClaimGongfengSnapshot(ctx, db.ClaimGongfengSnapshotParams{
				ID: pr.ID, WorkspaceID: pr.WorkspaceID, ConnectionUpdatedAt: conn.UpdatedAt,
			})
			if err != nil {
				t.Fatal(err)
			}
			tx, err := testPool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx) //nolint:errcheck
			qtx := testHandler.Queries.WithTx(tx)
			if failed {
				_, err = qtx.FailGongfengSnapshot(ctx, db.FailGongfengSnapshotParams{
					ID: claimed.ID, SnapshotError: "provider failed", HeadSha: claimed.HeadSha,
					PrUpdatedAt: claimed.PrUpdatedAt, SnapshotLeaseID: claimed.SnapshotLeaseID, UpdatedAt: conn.UpdatedAt,
				})
			} else {
				_, err = qtx.SaveGongfengSnapshot(ctx, db.SaveGongfengSnapshotParams{
					ID: claimed.ID, SnapshotHeadSha: claimed.HeadSha, Snapshot: []byte(`{"total":0}`),
					PrUpdatedAt: claimed.PrUpdatedAt, SnapshotLeaseID: claimed.SnapshotLeaseID, UpdatedAt: conn.UpdatedAt,
				})
			}
			if err != nil {
				t.Fatal(err)
			}
			stopped := make(chan error, 1)
			go func() {
				_, stopErr := testHandler.Queries.DisableGongfengRepository(ctx, db.DisableGongfengRepositoryParams{
					ConnectionID: conn.ID, WorkspaceID: conn.WorkspaceID, ProjectID: 55,
				})
				stopped <- stopErr
			}()
			select {
			case err := <-stopped:
				t.Fatalf("stop returned while an older snapshot %s was uncommitted: %v", name, err)
			case <-time.After(50 * time.Millisecond):
			}
			if err := tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-stopped:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("stop did not resume after snapshot transaction")
			}
		})
	}
}

func TestGongfengMergeAutoCompletesUsingMirroredStateInOneTransaction(t *testing.T) {
	ctx := context.Background()
	withVCSBox(t)
	setWorkspacePRMergeStatusForTest(t, "")
	issue := prAutoCompleteTestIssue(t, "Gongfeng transaction completion", 0)
	conn, pr := seedGongfengSyncPR(t, "https://gongfeng-auto-complete.test")
	t.Cleanup(func() {
		_ = testHandler.Queries.DeleteVCSConnection(ctx, db.DeleteVCSConnectionParams{ID: conn.ID, WorkspaceID: conn.WorkspaceID})
	})
	if _, err := testHandler.Queries.AddGongfengRepository(ctx, db.AddGongfengRepositoryParams{
		ConnectionID: conn.ID, ProjectID: 55, Path: "acme/widget", WebUrl: "https://git.code.tencent.com/acme/widget",
	}); err != nil {
		t.Fatal(err)
	}
	event := vcs.PullRequestEvent{
		ProjectID: 55, RepoOwner: "acme", RepoName: "widget", Number: 7,
		Title: issue.Identifier + ": merged in Gongfeng", State: "merged", HTMLURL: pr.HtmlUrl,
		Branch: "feature/transaction", HeadSHA: "B", CreatedAt: "2026-10-09T07:00:00Z", UpdatedAt: "2026-10-09T09:00:00Z",
	}
	if err := testHandler.mirrorVCSPullRequest(ctx, conn, event); err != nil {
		t.Fatal(err)
	}
	if got := issueStatusForTest(t, issue.ID); got != "done" {
		t.Fatalf("merged Gongfeng MR left issue status %q, want done", got)
	}
	var links int
	dbfx.QueryRow(t, "SELECT count(*) FROM issue_vcs_pull_request WHERE issue_id=$1", issue.ID).Scan(&links)
	if links != 1 {
		t.Fatalf("merged Gongfeng MR linked %d pull requests, want 1", links)
	}
}

func TestGongfengRenameMergesDuplicatesAndPreservesLatestManualIntent(t *testing.T) {
	ctx := context.Background()
	withVCSBox(t)
	conn, oldPR := seedGongfengSyncPR(t, "https://gongfeng-rename.test")
	t.Cleanup(func() {
		_ = testHandler.Queries.DeleteVCSConnection(ctx, db.DeleteVCSConnectionParams{ID: conn.ID, WorkspaceID: conn.WorkspaceID})
	})
	if _, err := testHandler.Queries.AddGongfengRepository(ctx, db.AddGongfengRepositoryParams{
		ConnectionID: conn.ID, ProjectID: 55, Path: "acme/widget", WebUrl: "https://git.code.tencent.com/acme/widget",
	}); err != nil {
		t.Fatal(err)
	}
	newPRID := dbfx.Insert(t, "vcs_pull_request", testutil.Cols{
		"workspace_id": conn.WorkspaceID, "connection_id": conn.ID, "provider": "gongfeng",
		"repo_owner": "acme", "repo_name": "renamed", "pr_number": 7,
		"title": "Renamed", "state": "open", "html_url": "https://git.code.tencent.com/acme/renamed/merge_requests/7",
		"pr_created_at": "2026-10-09T07:00:00Z", "pr_updated_at": "2026-10-09T09:00:00Z",
	})
	newPR := parseUUID(newPRID)
	legacyPRID := dbfx.Insert(t, "vcs_pull_request", testutil.Cols{
		"workspace_id": conn.WorkspaceID, "connection_id": conn.ID, "provider": "gongfeng",
		"repo_owner": "legacy", "repo_name": "widget", "pr_number": 7, "gongfeng_project_id": int64(55),
		"title": "Legacy identity", "state": "open", "html_url": "https://git.code.tencent.com/legacy/widget/merge_requests/7",
		"pr_created_at": "2026-10-09T07:00:00Z", "pr_updated_at": "2026-10-09T07:30:00Z",
	})
	legacyPR := parseUUID(legacyPRID)
	earliest, middle, latest := time.Now().Add(-3*time.Hour), time.Now().Add(-2*time.Hour), time.Now().Add(-time.Hour)
	autoExcluded := dbfx.Issue(t, "Rename exclusion beats auto link")
	newerUnlink := dbfx.Issue(t, "Rename newer unlink wins")
	newerRelink := dbfx.Issue(t, "Rename newer relink wins")
	dbfx.Exec(t, "INSERT INTO issue_vcs_pull_request (issue_id,pull_request_id,linked_by_type,linked_at) VALUES ($1,$2,'system',$3)", autoExcluded, newPR, earliest)
	dbfx.Exec(t, "INSERT INTO issue_vcs_pull_request (issue_id,pull_request_id,linked_by_type,linked_at) VALUES ($1,$2,'member',$3)", newerUnlink, oldPR.ID, middle)
	dbfx.Exec(t, "INSERT INTO issue_vcs_pull_request (issue_id,pull_request_id,linked_by_type,linked_at) VALUES ($1,$2,'system',$3)", newerRelink, newPR, earliest)
	dbfx.Exec(t, "INSERT INTO issue_pull_request_exclusion (issue_id,pull_request_id,workspace_id,created_at) VALUES ($1,$2,$3,$4)", autoExcluded, oldPR.ID, conn.WorkspaceID, latest)
	dbfx.Exec(t, "INSERT INTO issue_pull_request_exclusion (issue_id,pull_request_id,workspace_id,created_at) VALUES ($1,$2,$3,$4)", newerUnlink, newPR, conn.WorkspaceID, earliest)
	dbfx.Exec(t, "INSERT INTO issue_pull_request_exclusion (issue_id,pull_request_id,workspace_id,created_at) VALUES ($1,$2,$3,$4)", newerUnlink, legacyPR, conn.WorkspaceID, latest)
	dbfx.Exec(t, "INSERT INTO issue_pull_request_exclusion (issue_id,pull_request_id,workspace_id,created_at) VALUES ($1,$2,$3,$4)", newerRelink, oldPR.ID, conn.WorkspaceID, earliest)
	if count, err := testHandler.Queries.LinkIssueToVCSPullRequestManually(ctx, db.LinkIssueToVCSPullRequestManuallyParams{
		IssueID: parseUUID(newerRelink), PullRequestID: newPR,
	}); err != nil || count != 1 {
		t.Fatalf("promote manual link: count=%d err=%v", count, err)
	}
	if _, allowed, err := testHandler.reconcileGongfengProject(ctx, conn, vcs.GongfengProject{
		ID: 55, Path: "acme/renamed", WebURL: "https://git.code.tencent.com/acme/renamed",
	}, false); err != nil || !allowed {
		t.Fatalf("reconcile rename: allowed=%v err=%v", allowed, err)
	}
	var prCount int
	if err := testPool.QueryRow(ctx, "SELECT count(*) FROM vcs_pull_request WHERE connection_id=$1 AND pr_number=7", conn.ID).Scan(&prCount); err != nil || prCount != 1 {
		t.Fatalf("rename left %d PR rows: %v", prCount, err)
	}
	canonical, err := testHandler.Queries.GetGongfengPullRequestByProjectKey(ctx, db.GetGongfengPullRequestByProjectKeyParams{
		ConnectionID: conn.ID, GongfengProjectID: 55, PrNumber: 7,
	})
	if err != nil || canonical.RepoName != "renamed" {
		t.Fatalf("canonical identity: %+v err=%v", canonical, err)
	}
	assertIntent := func(issueID string, wantLink, wantExclusion int) {
		t.Helper()
		var links, exclusions int
		dbfx.QueryRow(t, "SELECT count(*) FROM issue_vcs_pull_request WHERE issue_id=$1 AND pull_request_id=$2", issueID, canonical.ID).Scan(&links)
		dbfx.QueryRow(t, "SELECT count(*) FROM issue_pull_request_exclusion WHERE issue_id=$1 AND pull_request_id=$2", issueID, canonical.ID).Scan(&exclusions)
		if links != wantLink || exclusions != wantExclusion {
			t.Fatalf("issue %s intent link/exclusion=%d/%d, want %d/%d", issueID, links, exclusions, wantLink, wantExclusion)
		}
	}
	assertIntent(autoExcluded, 0, 1)
	assertIntent(newerUnlink, 0, 1)
	assertIntent(newerRelink, 1, 0)

	// A different stable project may later reuse the old namespace. Its card
	// must never be folded into project 55 merely because the path matches.
	otherID := dbfx.Insert(t, "vcs_pull_request", testutil.Cols{
		"workspace_id": conn.WorkspaceID, "connection_id": conn.ID, "provider": "gongfeng",
		"repo_owner": "acme", "repo_name": "widget", "pr_number": 7, "gongfeng_project_id": int64(99),
		"title": "Reused path", "state": "open", "html_url": "https://git.code.tencent.com/acme/widget/merge_requests/7",
		"pr_created_at": "2026-10-09T10:00:00Z", "pr_updated_at": "2026-10-09T10:00:00Z",
	})
	if _, allowed, err := testHandler.reconcileGongfengProject(ctx, conn, vcs.GongfengProject{
		ID: 55, Path: "acme/renamed", WebURL: "https://git.code.tencent.com/acme/renamed",
	}, false); err != nil || !allowed {
		t.Fatalf("second reconcile: allowed=%v err=%v", allowed, err)
	}
	var otherCount int
	if err := testPool.QueryRow(ctx, "SELECT count(*) FROM vcs_pull_request WHERE id=$1 AND gongfeng_project_id=99", otherID).Scan(&otherCount); err != nil || otherCount != 1 {
		t.Fatalf("reused-path project was merged into the wrong identity: count=%d err=%v", otherCount, err)
	}
}

func TestGongfengPendingChecksCountAsRunning(t *testing.T) {
	now := time.Now()
	snapshot, err := json.Marshal(vcs.GongfengChecks{Total: 2, Passed: 1, Pending: 1})
	if err != nil {
		t.Fatal(err)
	}
	resp := vcsPullRequestRowToResponse(db.ListVCSPullRequestsByIssueRow{
		Provider: "gongfeng", HeadSha: "head", SnapshotHeadSha: "head", Snapshot: snapshot,
		SnapshotFetchedAt: pgtype.Timestamptz{Time: now, Valid: true},
	})
	if resp.ChecksPending != 1 || resp.ChecksRunning != 1 {
		t.Fatalf("pending/running=%d/%d, want 1/1", resp.ChecksPending, resp.ChecksRunning)
	}
}
