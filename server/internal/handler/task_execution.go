package handler

import (
	"net/http"

	"github.com/multica-ai/multica/server/internal/middleware"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// A cancellation response is business state; this receipt is written only
// after the owning daemon reports that execution and transcript flush ended.
// Revocation is committed with the receipt, so callers can rely on both.
func (h *Handler) recordExecutionExit(w http.ResponseWriter, r *http.Request, task *db.AgentTaskQueue) bool {
	// Older PAT-authenticated daemons retain their terminal callback contract,
	// but only the registered daemon identity can prove a process has exited.
	daemonID := middleware.DaemonIDFromContext(r.Context())
	if daemonID == "" || !task.RuntimeID.Valid {
		return true
	}
	runtime, err := h.Queries.GetAgentRuntime(r.Context(), task.RuntimeID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to verify execution owner")
		return false
	}
	if !runtime.DaemonID.Valid || runtime.DaemonID.String != daemonID || uuidToString(runtime.WorkspaceID) != middleware.DaemonWorkspaceIDFromContext(r.Context()) {
		return true
	}
	recorded, err := h.Queries.RecordAgentTaskExecutionExit(r.Context(), task.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to record execution exit")
		return false
	}
	*task = recorded
	return true
}
