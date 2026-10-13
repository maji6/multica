package handler

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/multica-ai/multica/server/internal/auth"
	"github.com/multica-ai/multica/server/internal/middleware"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

const executionExitProofHeader = "X-Multica-Execution-Exit-Proof"

// The proof is delivered only to the claiming daemon, never to the agent or
// public task API. Binding it to the dispatch prevents a previous claimant's
// delayed callback from acknowledging a new execution of the same task.
func executionExitProof(task db.AgentTaskQueue, runtime db.AgentRuntime) string {
	if !task.RuntimeID.Valid || task.RuntimeID != runtime.ID || !task.DispatchedAt.Valid || !runtime.DaemonID.Valid || runtime.DaemonID.String == "" {
		return ""
	}
	payload, _ := json.Marshal([]string{uuidToString(task.ID), uuidToString(runtime.WorkspaceID), uuidToString(task.RuntimeID), runtime.DaemonID.String, task.DispatchedAt.Time.UTC().Format(time.RFC3339Nano)})
	mac := hmac.New(sha256.New, auth.JWTSecret())
	mac.Write([]byte("multica-task-exit-v1\x00"))
	mac.Write(payload)
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// Cancellation is business state. Only a terminal callback from the execution
// owner can acknowledge process exit and transcript drain. Revocation is atomic
// with this receipt; an old dispatch must not revoke the new dispatch's token.
func (h *Handler) recordExecutionExit(w http.ResponseWriter, r *http.Request, task *db.AgentTaskQueue) bool {
	proof := r.Header.Get(executionExitProofHeader)
	daemonID := middleware.DaemonIDFromContext(r.Context())
	if proof == "" && (daemonID == "" || !task.RuntimeID.Valid) {
		return true
	}
	runtime, err := h.Queries.GetAgentRuntime(r.Context(), task.RuntimeID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to verify execution owner")
		return false
	}
	if proof != "" {
		expected := executionExitProof(*task, runtime)
		if expected == "" || !hmac.Equal([]byte(proof), []byte(expected)) {
			writeError(w, http.StatusForbidden, "invalid execution exit proof")
			return false
		}
	} else if !runtime.DaemonID.Valid || runtime.DaemonID.String != daemonID || uuidToString(runtime.WorkspaceID) != middleware.DaemonWorkspaceIDFromContext(r.Context()) {
		return true
	}
	recorded, err := h.Queries.RecordAgentTaskExecutionExit(r.Context(), db.RecordAgentTaskExecutionExitParams{
		ID: task.ID, RuntimeID: task.RuntimeID, DispatchedAt: task.DispatchedAt,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusConflict, "task dispatch changed before execution exit")
		return false
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to record execution exit")
		return false
	}
	*task = db.AgentTaskQueue(recorded)
	return true
}
