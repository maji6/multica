package handler

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func TestExecutionExitUserAuthenticatedClaimRoundTrip(t *testing.T) {
	for _, mode := range []string{"single", "batch"} {
		t.Run(mode, func(t *testing.T) {
			runtimeID := createClaimReclaimRuntime(t, t.Context(), "Exit proof runtime")
			dbfx.Exec(t, "UPDATE agent_runtime SET daemon_id=$1 WHERE id=$2", batchClaimTestDaemonID, runtimeID)
			agentID, issueID := createClaimReclaimAgentAndIssue(t, t.Context(), runtimeID, "Exit proof agent")
			taskID := seedQueuedIssueTask(t, t.Context(), agentID, runtimeID, issueID)
			var claimed struct {
				Task  map[string]json.RawMessage   `json:"task"`
				Tasks []map[string]json.RawMessage `json:"tasks"`
			}
			if mode == "single" {
				req := withURLParam(newRequest(http.MethodPost, "/claim", map[string]any{}), "runtimeId", runtimeID)
				testutil.Call(t, testHandler.ClaimTaskByRuntime, req).Want(http.StatusOK).JSON(&claimed)
			} else {
				req := newRequest(http.MethodPost, "/claim", map[string]any{"daemon_id": batchClaimTestDaemonID, "runtime_ids": []string{runtimeID}, "max_tasks": 1})
				testutil.Call(t, testHandler.ClaimTasksByRuntime, req).Want(http.StatusOK).JSON(&claimed)
				if len(claimed.Tasks) != 1 {
					t.Fatalf("claimed %d tasks", len(claimed.Tasks))
				}
				claimed.Task = claimed.Tasks[0]
			}
			var proof string
			_ = json.Unmarshal(claimed.Task["execution_exit_proof"], &proof)
			if proof == "" {
				t.Fatal("user-authenticated claim did not provide an execution exit proof")
			}
			retiredSessionID := "ses_cancelled_" + mode
			dbfx.Exec(t, "UPDATE agent_task_queue SET status='running', started_at=now(), session_id=$2 WHERE id=$1", taskID, retiredSessionID)
			cancel := withChatTestWorkspaceCtx(t, withURLParam(newRequest(http.MethodPost, "/cancel", nil), "taskId", taskID))
			var response AgentTaskResponse
			testutil.Call(t, testHandler.CancelTaskByUser, cancel).Want(http.StatusOK).JSON(&response)
			if response.ExecutionExitedAt != nil {
				t.Fatal("cancellation alone asserted execution exit")
			}
			ack := func(value string, body map[string]any, status int) {
				req := withURLParam(newRequest(http.MethodPost, "/cancel-ack", body), "taskId", taskID)
				req.Header.Set("X-Multica-Execution-Exit-Proof", value)
				testutil.Call(t, testHandler.AckTaskCancelled, req).Want(status)
			}
			ack(proof+"tampered", map[string]any{}, http.StatusForbidden)
			ack(proof, map[string]any{"retired_session_id": retiredSessionID}, http.StatusOK)
			task, err := testHandler.Queries.GetAgentTask(t.Context(), parseUUID(taskID))
			if err != nil {
				t.Fatal(err)
			}
			first := taskToResponse(task, testWorkspaceID).ExecutionExitedAt
			if first == nil {
				t.Fatal("valid claim proof did not record exit")
			}
			if !task.RetiredSessionID.Valid || task.RetiredSessionID.String != retiredSessionID {
				t.Fatalf("cancel acknowledgement retired session = %q, want %q", task.RetiredSessionID.String, retiredSessionID)
			}
			ack(proof, map[string]any{}, http.StatusOK)
			task, err = testHandler.Queries.GetAgentTask(t.Context(), parseUUID(taskID))
			if err != nil {
				t.Fatal(err)
			}
			if receipt := taskToResponse(task, testWorkspaceID).ExecutionExitedAt; receipt == nil || *receipt != *first {
				t.Fatal("replayed exit proof changed receipt")
			}
			if dbfx.Count(t, "SELECT count(*) FROM task_token WHERE task_id=$1", taskID) != 0 {
				t.Fatal("execution exit left agent credentials active")
			}
			body, _ := json.Marshal(taskToResponse(task, testWorkspaceID))
			var public map[string]json.RawMessage
			_ = json.Unmarshal(body, &public)
			if _, ok := public["execution_exit_proof"]; ok {
				t.Fatal("claim proof leaked through public task response")
			}
		})
	}
}

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
	testutil.Call(t, testHandler.AckTaskCancelled, other).Want(http.StatusNotFound)
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

func TestExecutionExitProofBindsDispatch(t *testing.T) {
	f := newSupplementFixture(t, "codex", "running", true)
	dbfx.Exec(t, "UPDATE agent_runtime SET daemon_id='exit-owner' WHERE id=$1", f.runtimeID)
	dbfx.Exec(t, "UPDATE agent_task_queue SET dispatched_at=now(), status='cancelled' WHERE id=$1", f.taskID)
	task, err := testHandler.Queries.GetAgentTask(t.Context(), parseUUID(f.taskID))
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := testHandler.Queries.GetAgentRuntime(t.Context(), task.RuntimeID)
	if err != nil {
		t.Fatal(err)
	}
	proof := executionExitProof(task, runtime)
	if proof == "" {
		t.Fatal("missing proof")
	}
	for _, mutate := range []func(*db.AgentTaskQueue, *db.AgentRuntime){
		func(task *db.AgentTaskQueue, _ *db.AgentRuntime) {
			task.ID = parseUUID("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")
		},
		func(task *db.AgentTaskQueue, _ *db.AgentRuntime) {
			task.DispatchedAt.Time = task.DispatchedAt.Time.Add(time.Second)
		},
		func(_ *db.AgentTaskQueue, rt *db.AgentRuntime) { rt.DaemonID.String = "another-owner" },
		func(_ *db.AgentTaskQueue, rt *db.AgentRuntime) {
			rt.WorkspaceID = parseUUID("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")
		},
		func(task *db.AgentTaskQueue, _ *db.AgentRuntime) {
			task.RuntimeID = parseUUID("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")
		},
	} {
		changed, rt := task, runtime
		mutate(&changed, &rt)
		if executionExitProof(changed, rt) == proof {
			t.Fatal("proof is reusable outside its dispatch")
		}
	}
	_, err = testHandler.Queries.CreateTaskToken(t.Context(), db.CreateTaskTokenParams{TokenHash: "exit-fence-" + f.taskID, TaskID: task.ID, AgentID: parseUUID(f.agentID), WorkspaceID: parseUUID(testWorkspaceID), UserID: parseUUID(testUserID), ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true}})
	if err != nil {
		t.Fatal(err)
	}
	dbfx.Cleanup(t, "DELETE FROM task_token WHERE task_id=$1", f.taskID)
	dbfx.Exec(t, "UPDATE agent_task_queue SET dispatched_at=dispatched_at+interval '1 second' WHERE id=$1", f.taskID)
	req := withURLParam(newRequest(http.MethodPost, "/cancel-ack", nil), "taskId", f.taskID)
	req.Header.Set(executionExitProofHeader, proof)
	testutil.Call(t, testHandler.AckTaskCancelled, req).Want(http.StatusForbidden)
	_, err = testHandler.Queries.RecordAgentTaskExecutionExit(t.Context(), db.RecordAgentTaskExecutionExitParams{ID: task.ID, RuntimeID: task.RuntimeID, DispatchedAt: task.DispatchedAt})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("stale dispatch update: %v", err)
	}
	if dbfx.Count(t, "SELECT count(*) FROM task_token WHERE task_id=$1", f.taskID) != 1 {
		t.Fatal("stale callback revoked the current execution credential")
	}
	current, err := testHandler.Queries.GetAgentTask(t.Context(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if taskToResponse(current, testWorkspaceID).ExecutionExitedAt != nil {
		t.Fatal("stale dispatch recorded exit")
	}
}
