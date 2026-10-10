package handler

import (
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"net/http"
	"testing"
	"time"
)

func TestExecutionExitReceiptRequiresDaemonAndRevokesTokens(t *testing.T) {
	f := newSupplementFixture(t, "codex", "running", true)
	dbfx.Exec(t, "UPDATE agent_runtime SET daemon_id='legit-daemon' WHERE id=$1", f.runtimeID)
	req := withChatTestWorkspaceCtx(t, withURLParam(newRequest(http.MethodPost, "/api/tasks/"+f.taskID+"/cancel", nil), "taskId", f.taskID))
	var cancelled AgentTaskResponse
	testutil.Call(t, testHandler.CancelTaskByUser, req).Want(http.StatusOK).JSON(&cancelled)
	if cancelled.ExecutionExitedAt != nil {
		t.Fatal("cancel response claimed process exit")
	}
	_, err := testHandler.Queries.CreateTaskToken(t.Context(), db.CreateTaskTokenParams{
		TokenHash: "exit-test-" + f.taskID, TaskID: parseUUID(f.taskID), AgentID: parseUUID(f.agentID), WorkspaceID: parseUUID(testWorkspaceID), UserID: parseUUID(testUserID), ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	dbfx.Cleanup(t, "DELETE FROM task_token WHERE task_id=$1", f.taskID)
	foreign := withURLParam(newDaemonTokenRequest(http.MethodPost, "/cancel-ack", nil, "00000000-0000-0000-0000-000000000000", "foreign"), "taskId", f.taskID)
	testutil.Call(t, testHandler.AckTaskCancelled, foreign).Want(http.StatusNotFound)
	task, err := testHandler.Queries.GetAgentTask(t.Context(), parseUUID(f.taskID))
	if err != nil {
		t.Fatal(err)
	}
	if taskToResponse(task, testWorkspaceID).ExecutionExitedAt != nil {
		t.Fatal("foreign runtime recorded an exit")
	}
	other := withURLParam(newDaemonTokenRequest(http.MethodPost, "/cancel-ack", nil, testWorkspaceID, "other-runtime"), "taskId", f.taskID)
	testutil.Call(t, testHandler.AckTaskCancelled, other).Want(http.StatusOK)
	task, err = testHandler.Queries.GetAgentTask(t.Context(), parseUUID(f.taskID))
	if err != nil {
		t.Fatal(err)
	}
	if taskToResponse(task, testWorkspaceID).ExecutionExitedAt != nil {
		t.Fatal("another runtime forged an exit receipt")
	}
	var first string
	for range 2 {
		ack := withURLParam(newDaemonTokenRequest(http.MethodPost, "/cancel-ack", map[string]any{}, testWorkspaceID, "legit-daemon"), "taskId", f.taskID)
		testutil.Call(t, testHandler.AckTaskCancelled, ack).Want(http.StatusOK)
		task, err = testHandler.Queries.GetAgentTask(t.Context(), parseUUID(f.taskID))
		if err != nil {
			t.Fatal(err)
		}
		receipt := taskToResponse(task, testWorkspaceID).ExecutionExitedAt
		if receipt == nil {
			t.Fatal("daemon acknowledgement did not record exit")
		}
		if first != "" && first != *receipt {
			t.Fatal("retry changed exit timestamp")
		}
		first = *receipt
	}
	if n := dbfx.Count(t, "SELECT count(*) FROM task_token WHERE task_id=$1", f.taskID); n != 0 {
		t.Fatal("exit receipt left task credentials active")
	}
	// Retry context is inherited, but the old task's proof must not transfer.
	task.ID = parseUUID("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")
	if taskToResponse(task, testWorkspaceID).ExecutionExitedAt != nil {
		t.Fatal("parent exit proof transferred to retry")
	}
}

func TestExecutionExitRecordedOnDaemonCompletionAndFailure(t *testing.T) {
	for _, kind := range []string{"complete", "fail"} {
		t.Run(kind, func(t *testing.T) {
			f := newSupplementFixture(t, "codex", "running", true)
			dbfx.Exec(t, "UPDATE agent_runtime SET daemon_id='legit-daemon' WHERE id=$1", f.runtimeID)
			body := map[string]any{"output": "done"}
			fn := testHandler.CompleteTask
			if kind == "fail" {
				body = map[string]any{"error": "controlled failure", "failure_reason": "agent_error"}
				fn = testHandler.FailTask
			}
			req := withURLParam(newDaemonTokenRequest(http.MethodPost, "/"+kind, body, testWorkspaceID, "legit-daemon"), "taskId", f.taskID)
			var response AgentTaskResponse
			testutil.Call(t, fn, req).Want(http.StatusOK).JSON(&response)
			if response.ExecutionExitedAt == nil {
				t.Fatal("terminal daemon callback did not record exit")
			}
		})
	}
}
