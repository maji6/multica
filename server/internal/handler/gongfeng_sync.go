package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/integrations/vcs"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

type gongfengJob struct {
	key        string
	repository *db.GongfengRepository
	pr         *db.VcsPullRequest
}

const gongfengFailureBackoff = 5 * time.Minute

type gongfengRateLimit struct {
	until    time.Time
	revision pgtype.Timestamptz
}

// A bounded queue serves webhooks, page visits and persisted sweeps. Dropped
// open-MR/repository jobs recover on sweeps; terminal MRs need a later event/view.
type gongfengSync struct {
	h               *Handler
	queue           chan gongfengJob
	mu              sync.Mutex
	pending         map[string]bool
	inFlight        map[string]bool
	trailing        map[string]gongfengJob
	rateLimits      map[pgtype.UUID]gongfengRateLimit
	now             func() time.Time
	sweepPR         pgtype.UUID
	sweepRepository db.ListDueGongfengRepositoriesParams
	once            sync.Once
	connectionLocks map[string]*gongfengConnectionLock
}

func newGongfengSync(h *Handler) *gongfengSync {
	return &gongfengSync{
		h: h, queue: make(chan gongfengJob, 64), pending: map[string]bool{},
		inFlight: map[string]bool{}, trailing: map[string]gongfengJob{},
		rateLimits: map[pgtype.UUID]gongfengRateLimit{}, now: time.Now,
		connectionLocks: map[string]*gongfengConnectionLock{},
	}
}

// Sync jobs may read concurrently, but deleting a connection or repository and
// rotating credentials wait for those jobs before changing their persisted scope.
// Reference counting keeps locks out of memory after a connection is removed.
type gongfengConnectionLock struct {
	mu         sync.RWMutex
	references int
}

func (s *gongfengSync) lockConnection(id pgtype.UUID, write bool) func() {
	key := uuidToString(id)
	s.mu.Lock()
	lock := s.connectionLocks[key]
	if lock == nil {
		lock = &gongfengConnectionLock{}
		s.connectionLocks[key] = lock
	}
	lock.references++
	s.mu.Unlock()
	if write {
		lock.mu.Lock()
	} else {
		lock.mu.RLock()
	}
	return func() {
		if write {
			lock.mu.Unlock()
		} else {
			lock.mu.RUnlock()
		}
		s.mu.Lock()
		lock.references--
		if lock.references == 0 {
			delete(s.connectionLocks, key)
		}
		s.mu.Unlock()
	}
}

func (s *gongfengSync) enqueue(job gongfengJob, replayInFlight bool) {
	if s == nil || !s.h.isVCSAvailable() || !s.h.isVCSConfigured() {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pending[job.key] {
		if replayInFlight && s.inFlight[job.key] {
			s.trailing[job.key] = job
		}
		return
	}
	select {
	case s.queue <- job:
		s.pending[job.key] = true
	default:
	}
}
func (s *gongfengSync) enqueueRepository(repo db.GongfengRepository) {
	s.enqueue(gongfengJob{key: fmt.Sprintf("repo:%s:%d", uuidToString(repo.ConnectionID), repo.ProjectID), repository: &repo}, true)
}
func (s *gongfengSync) enqueuePR(pr db.VcsPullRequest) {
	if pr.Provider == "gongfeng" {
		s.enqueue(gongfengJob{key: "pr:" + uuidToString(pr.ID), pr: &pr}, true)
	}
}
func (s *gongfengSync) onView(pr db.ListVCSPullRequestsByIssueRow) {
	if pr.Provider != "gongfeng" {
		return
	}
	// Failed attempts need a cooldown even before the first successful snapshot.
	// A new head clears this marker in UpsertVCSPullRequest.
	if pr.SnapshotError != "" && pr.SnapshotAttemptedAt.Valid && s.now().Sub(pr.SnapshotAttemptedAt.Time) < gongfengFailureBackoff {
		return
	}
	if pr.SnapshotHeadSha == pr.HeadSha && pr.HeadSha != "" && pr.SnapshotFetchedAt.Valid && s.now().Sub(pr.SnapshotFetchedAt.Time) < time.Minute {
		return
	}
	s.enqueue(gongfengJob{key: "pr:" + uuidToString(pr.ID), pr: &db.VcsPullRequest{ID: pr.ID, WorkspaceID: pr.WorkspaceID, ConnectionID: pr.ConnectionID}}, false)
}
func (h *Handler) StartGongfengSync(ctx context.Context) {
	if !h.isVCSAvailable() || !h.isVCSConfigured() {
		return
	}
	s := h.GongfengSync
	s.once.Do(func() {
		for i := 0; i < 2; i++ {
			go s.worker(ctx)
		}
		go func() {
			s.sweep(ctx)
			ticker := time.NewTicker(30 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					s.sweep(ctx)
				}
			}
		}()
	})
}

// Views join the current request; only change/sweep triggers retain a trailing
// refresh. Otherwise our own snapshot broadcasts could keep a worker busy forever.
func (s *gongfengSync) worker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case job := <-s.queue:
			s.mu.Lock()
			s.inFlight[job.key] = true
			s.mu.Unlock()
			jobCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
			if job.repository != nil {
				s.syncRepository(jobCtx, *job.repository)
			} else if job.pr != nil {
				s.refreshPR(jobCtx, *job.pr)
			}
			cancel()
			s.mu.Lock()
			delete(s.inFlight, job.key)
			delete(s.pending, job.key)
			if next, ok := s.trailing[job.key]; ok {
				delete(s.trailing, job.key)
				select {
				case s.queue <- next:
					s.pending[job.key] = true
				default:
					slog.Warn("gongfeng: refresh queue full; later triggers will retry")
				}
			}
			s.mu.Unlock()
		}
	}
}

func (s *gongfengSync) rateLimited(conn db.VcsConnection) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	for id, limit := range s.rateLimits {
		if !now.Before(limit.until) || (id == conn.ID && limit.revision != conn.UpdatedAt) {
			delete(s.rateLimits, id)
		}
	}
	_, limited := s.rateLimits[conn.ID]
	return limited
}

func (s *gongfengSync) recordRateLimit(conn db.VcsConnection, err error) {
	var apiErr *vcs.GongfengAPIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusTooManyRequests {
		return
	}
	until := s.now().Add(max(apiErr.RetryAfter, gongfengFailureBackoff))
	s.mu.Lock()
	defer s.mu.Unlock()
	previous := s.rateLimits[conn.ID]
	if previous.revision != conn.UpdatedAt || until.After(previous.until) {
		s.rateLimits[conn.ID] = gongfengRateLimit{until: until, revision: conn.UpdatedAt}
	}
}

func (s *gongfengSync) sweep(ctx context.Context) {
	repos, err := s.h.Queries.ListDueGongfengRepositories(ctx, s.sweepRepository)
	if err != nil {
		slog.Warn("gongfeng: repository sweep failed", "err", err)
		return
	}
	for _, repo := range repos {
		s.sweepRepository = db.ListDueGongfengRepositoriesParams{AfterConnection: repo.ConnectionID, AfterProject: repo.ProjectID}
		s.enqueueRepository(repo)
	}
	prs, err := s.h.Queries.ListDueGongfengPullRequests(ctx, s.sweepPR)
	if err != nil {
		slog.Warn("gongfeng: MR sweep failed", "err", err)
		return
	}
	for _, pr := range prs {
		s.sweepPR = pr.ID
		s.enqueuePR(pr)
	}
}
func (s *gongfengSync) syncRepository(ctx context.Context, queued db.GongfengRepository) {
	unlock := s.lockConnection(queued.ConnectionID, false)
	defer unlock()
	h := s.h
	// Reload the selection: removing it or rotating credentials invalidates queued work.
	row, err := h.Queries.GetGongfengRepository(ctx, db.GetGongfengRepositoryParams{ConnectionID: queued.ConnectionID, ProjectID: queued.ProjectID})
	if err != nil {
		return
	}
	conn, err := h.Queries.GetVCSConnectionByID(ctx, row.ConnectionID)
	if err != nil {
		return
	}
	if s.rateLimited(conn) {
		return
	}
	client, err := h.gongfengClient(conn)
	if err != nil {
		return
	}
	row, err = h.Queries.ClaimGongfengRepositorySync(ctx, db.ClaimGongfengRepositorySyncParams{ConnectionID: row.ConnectionID, ProjectID: row.ProjectID, UpdatedAt: row.UpdatedAt})
	if err != nil {
		return
	}
	next := int(row.NextPage)
	complete := false
	createdHook := false
	defer func() {
		message := ""
		if err != nil {
			s.recordRateLimit(conn, err)
			message = err.Error()
		}
		// Use the parent server lifetime for persistence even when this API batch timed out.
		saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		count, saveErr := h.Queries.SaveGongfengRepositorySync(saveCtx, db.SaveGongfengRepositorySyncParams{
			ConnectionID: row.ConnectionID, ProjectID: row.ProjectID, UpdatedAt: row.UpdatedAt,
			LeaseID: row.SyncLeaseID, ConnectionUpdatedAt: conn.UpdatedAt, HookID: row.HookID,
			HookRevision: row.HookRevision, NextPage: int32(next), SyncError: message, Completed: complete,
		})
		if saveErr != nil {
			slog.Warn("gongfeng: save sync progress failed", "err", saveErr)
		} else if count == 0 && createdHook {
			// A stop, disconnect, or credential rotation fenced this worker
			// after it touched the remote hook. Stopped/deleted scopes remove it;
			// a rotated live connection gets a trailing repair with the new secret.
			current, repoErr := h.Queries.GetGongfengRepository(saveCtx, db.GetGongfengRepositoryParams{ConnectionID: row.ConnectionID, ProjectID: row.ProjectID})
			_, connErr := h.Queries.GetVCSConnectionByID(saveCtx, row.ConnectionID)
			if repoErr == nil && current.Enabled && connErr == nil {
				s.enqueueRepository(current)
			} else if cleanupErr := client.RemoveHook(saveCtx, row.ProjectID, row.HookID, h.vcsWebhookURL(uuidToString(conn.ID))); cleanupErr != nil {
				slog.Warn("gongfeng: stale worker webhook cleanup failed", "connection_id", uuidToString(conn.ID), "project_id", row.ProjectID, "err", cleanupErr)
			}
		}
	}()
	var project vcs.GongfengProject
	project, err = client.Project(ctx, strconv.FormatInt(row.ProjectID, 10))
	if err != nil {
		return
	}
	if project.ID != row.ProjectID {
		err = errors.New("Gongfeng repository identity changed")
		return
	}
	var allowed bool
	row, allowed, err = h.reconcileGongfengProject(ctx, conn, project, true)
	if err != nil || !allowed {
		return
	}
	if row.HookID == 0 || !row.HookRevision.Valid || !row.HookRevision.Time.Equal(conn.UpdatedAt.Time) {
		row, createdHook, err = s.ensureRepositoryHook(ctx, conn, row, client)
		if err != nil {
			return
		}
	}
	var mrs []vcs.GongfengMR
	var nextPage int
	mrs, nextPage, err = client.MergeRequests(ctx, strconv.FormatInt(row.ProjectID, 10), int(row.NextPage))
	if err != nil {
		return
	}
	for _, mr := range mrs {
		var event vcs.PullRequestEvent
		event, err = mr.Event(project)
		if err != nil {
			return
		}
		updated := parseGHTimeRequired(event.UpdatedAt)
		// Descending update order lets later sweeps stop at the last completed scan.
		if row.SyncedAt.Valid && updated.Time.Before(row.SyncedAt.Time) {
			complete = true
			break
		}
		err = h.mirrorVCSPullRequest(ctx, conn, event)
		if err != nil {
			return
		}
		pr, getErr := h.Queries.GetGongfengPullRequestByProjectKey(ctx, db.GetGongfengPullRequestByProjectKeyParams{ConnectionID: conn.ID, GongfengProjectID: event.ProjectID, PrNumber: event.Number})
		if getErr != nil {
			err = getErr
			return
		}
		linked, linkErr := h.Queries.ListIssueIDsForVCSPullRequest(ctx, pr.ID)
		if linkErr != nil {
			err = linkErr
			return
		}
		if len(linked) > 0 {
			s.enqueuePR(pr)
		}
	}
	if nextPage == 0 {
		complete = true
	}
	if complete {
		next = 1
	} else {
		next = nextPage
	}
}

// ensureRepositoryHook holds the connection revision lock across the remote
// side effect and its local ID write. Disconnect/stop can then wait, capture
// the final hook identity, revoke it, and return without a late unknown hook.
func (s *gongfengSync) ensureRepositoryHook(ctx context.Context, conn db.VcsConnection, row db.GongfengRepository, client *vcs.GongfengClient) (db.GongfengRepository, bool, error) {
	h := s.h
	tx, err := h.TxStarter.Begin(ctx)
	if err != nil {
		return row, false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	q := h.Queries.WithTx(tx)
	if _, err := q.LockVCSConnectionRevision(ctx, db.LockVCSConnectionRevisionParams{
		ID: conn.ID, WorkspaceID: conn.WorkspaceID, Provider: conn.Provider, UpdatedAt: conn.UpdatedAt,
	}); err != nil {
		return row, false, err
	}
	current, err := q.LockGongfengRepository(ctx, db.LockGongfengRepositoryParams{ConnectionID: row.ConnectionID, ProjectID: row.ProjectID})
	if err != nil {
		return row, false, err
	}
	if !current.Enabled || current.UpdatedAt != row.UpdatedAt || current.SyncLeaseID != row.SyncLeaseID {
		return row, false, pgx.ErrNoRows
	}
	secret, err := h.openVCSSecret(conn.WebhookSecretEncrypted)
	if err != nil {
		return row, false, err
	}
	endpoint := h.vcsWebhookURL(uuidToString(conn.ID))
	hookID, err := client.EnsureHook(ctx, row.ProjectID, endpoint, secret)
	if err != nil {
		return row, false, err
	}
	remove := func() {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if cleanupErr := client.RemoveHook(cleanupCtx, row.ProjectID, hookID, endpoint); cleanupErr != nil {
			slog.Warn("gongfeng: uncommitted webhook cleanup failed", "connection_id", uuidToString(conn.ID), "project_id", row.ProjectID, "err", cleanupErr)
		}
	}
	saved, err := q.SaveGongfengRepositoryHook(ctx, db.SaveGongfengRepositoryHookParams{
		HookID: hookID, HookRevision: conn.UpdatedAt, ConnectionID: row.ConnectionID,
		ProjectID: row.ProjectID, UpdatedAt: row.UpdatedAt, LeaseID: row.SyncLeaseID,
	})
	if err != nil {
		remove()
		return row, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		remove()
		return row, false, err
	}
	return saved, true, nil
}
func (s *gongfengSync) refreshPR(ctx context.Context, queued db.VcsPullRequest) {
	unlock := s.lockConnection(queued.ConnectionID, false)
	defer unlock()
	h := s.h
	pr, err := h.Queries.GetVCSPullRequestInWorkspace(ctx, db.GetVCSPullRequestInWorkspaceParams{ID: queued.ID, WorkspaceID: queued.WorkspaceID})
	if err != nil {
		return
	}
	conn, err := h.Queries.GetVCSConnectionByID(ctx, pr.ConnectionID)
	if err != nil {
		return
	}
	if s.rateLimited(conn) {
		return
	}
	client, err := h.gongfengClient(conn)
	if err != nil {
		return
	}
	pr, err = h.Queries.ClaimGongfengSnapshot(ctx, db.ClaimGongfengSnapshotParams{ID: pr.ID, WorkspaceID: pr.WorkspaceID, ConnectionUpdatedAt: conn.UpdatedAt})
	if err != nil {
		return
	}
	defer func() {
		if err != nil {
			s.recordRateLimit(conn, err)
			saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			count, saveErr := h.Queries.FailGongfengSnapshot(saveCtx, db.FailGongfengSnapshotParams{
				ID: pr.ID, SnapshotError: err.Error(), HeadSha: pr.HeadSha, PrUpdatedAt: pr.PrUpdatedAt,
				SnapshotLeaseID: pr.SnapshotLeaseID, UpdatedAt: conn.UpdatedAt,
			})
			if saveErr == nil && count > 0 {
				h.publishGongfengSnapshot(saveCtx, pr)
			}
		}
	}()
	var project vcs.GongfengProject
	projectRef := pr.RepoOwner + "/" + pr.RepoName
	if pr.GongfengProjectID > 0 {
		projectRef = strconv.FormatInt(pr.GongfengProjectID, 10)
	}
	project, err = client.Project(ctx, projectRef)
	if err != nil {
		return
	}
	if pr.GongfengProjectID > 0 && project.ID != pr.GongfengProjectID {
		err = fmt.Errorf("Gongfeng repository identity changed")
		return
	}
	var allowed bool
	_, allowed, err = h.reconcileGongfengProject(ctx, conn, project, true)
	if err != nil || !allowed {
		return
	}
	canonical, getErr := h.Queries.GetGongfengPullRequestByProjectKey(ctx, db.GetGongfengPullRequestByProjectKeyParams{
		ConnectionID: conn.ID, GongfengProjectID: project.ID, PrNumber: pr.PrNumber,
	})
	if getErr == nil && canonical.ID != pr.ID {
		s.enqueuePR(canonical)
		return
	}
	var mr vcs.GongfengMR
	mr, err = client.MergeRequest(ctx, strconv.FormatInt(project.ID, 10), pr.PrNumber)
	if err != nil {
		return
	}
	var event vcs.PullRequestEvent
	event, err = mr.Event(project)
	if err != nil {
		return
	}
	if event.State == "open" || event.State == "draft" {
		event.HeadSHA, err = client.HeadSHA(ctx, mr)
		if err != nil {
			return
		}
	}
	// Closed source branches may be deleted; preserve a known terminal head.
	if event.HeadSHA == "" {
		event.HeadSHA = pr.HeadSha
	}
	previousHead := pr.HeadSha
	err = h.mirrorVCSPullRequest(ctx, conn, event)
	if err != nil {
		return
	}
	// Upsert deliberately clears an older snapshot lease when it learns a new
	// head. This worker discovered that head, so claim the refreshed row before
	// saving its checks instead of waiting for the next background sweep.
	if event.HeadSHA != previousHead {
		pr, err = h.Queries.ClaimGongfengSnapshot(ctx, db.ClaimGongfengSnapshotParams{
			ID: pr.ID, WorkspaceID: pr.WorkspaceID, ConnectionUpdatedAt: conn.UpdatedAt,
		})
		if err != nil {
			return
		}
	}
	pr.HeadSha = event.HeadSHA
	pr.PrUpdatedAt = parseGHTimeRequired(event.UpdatedAt)
	var checks vcs.GongfengChecks
	if event.HeadSHA == "" {
		return
	}
	checksProject := project.ID
	if mr.SourceProjectID > 0 {
		checksProject = mr.SourceProjectID
	}
	checks, err = client.Checks(ctx, checksProject, event.HeadSHA)
	if err != nil {
		return
	}
	switch strings.ToLower(mr.MergeStatus) {
	case "can_be_merged":
		value := "mergeable"
		checks.Mergeable = &value
	case "cannot_be_merged":
		value := "conflicting"
		checks.Mergeable = &value
	}
	var encoded []byte
	encoded, err = json.Marshal(checks)
	if err != nil {
		return
	}
	var count int64
	count, err = h.Queries.SaveGongfengSnapshot(ctx, db.SaveGongfengSnapshotParams{
		ID: pr.ID, SnapshotHeadSha: event.HeadSHA, Snapshot: encoded,
		PrUpdatedAt: parseGHTimeRequired(event.UpdatedAt), SnapshotLeaseID: pr.SnapshotLeaseID, UpdatedAt: conn.UpdatedAt,
	})
	if err == nil && count > 0 {
		h.publishGongfengSnapshot(ctx, pr)
	}
}
