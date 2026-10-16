package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// The task token, rather than a caller-supplied header or the latest run,
// identifies both the live event and the durable field-change history.
func TestIssueUpdatePreservesAuthoringRun(t *testing.T) {
	for _, batch := range []bool{false, true} {
		for _, agent := range []bool{true, false} {
			name := "single"
			if batch {
				name = "batch"
			}
			if agent {
				name += "/agent"
			} else {
				name += "/member_with_forged_headers"
			}
			t.Run(name, func(t *testing.T) {
				f := workspaceFixture(t)
				runtime := f.Runtime(t, "run attribution runtime")
				agentID := f.Agent(t, "run attribution agent", runtime)
				issueID := f.Issue(t, "run attribution issue", testutil.Cols{"status": "todo"})
				t.Cleanup(func() { cleanupActivities(t, issueID) })
				cols := testutil.Cols{"runtime_id": runtime, "issue_id": issueID, "status": "running", "originator_user_id": testUserID, "accountable_user_id": testUserID}
				taskID := f.Task(t, agentID, cols)
				otherTaskID := f.Task(t, agentID, cols)
				token, wantActor, wantActorID := testToken, "member", testUserID
				if agent {
					token = mintAgentTaskToken(t, agentID, taskID, testUserID)
					wantActor, wantActorID = "agent", agentID
				}

				conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(testServer.URL, "http")+"/ws?workspace_id="+testWorkspaceID, nil)
				if err != nil {
					t.Fatal(err)
				}
				defer conn.Close()
				if err := conn.WriteJSON(map[string]any{"type": "auth", "payload": map[string]string{"token": testToken}}); err != nil {
					t.Fatal(err)
				}
				readWSMessageOfType(t, conn, "auth_ack")

				updates := map[string]any{"status": "in_progress", "assignee_type": "member", "assignee_id": testUserID, "suppress_run": true}
				method, path, body := http.MethodPut, "/api/issues/"+issueID, updates
				if batch {
					method, path = http.MethodPost, "/api/issues/batch-update"
					body = map[string]any{"issue_ids": []string{issueID}, "updates": updates}
				}
				encoded, err := json.Marshal(body)
				if err != nil {
					t.Fatal(err)
				}
				req, err := http.NewRequest(method, testServer.URL+path, bytes.NewReader(encoded))
				if err != nil {
					t.Fatal(err)
				}
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("Authorization", "Bearer "+token)
				req.Header.Set("X-Workspace-ID", testWorkspaceID)
				req.Header.Set("X-Agent-ID", agentID)
				req.Header.Set("X-Task-ID", otherTaskID)
				res, err := http.DefaultClient.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				response, err := io.ReadAll(res.Body)
				res.Body.Close()
				if err != nil || res.StatusCode != http.StatusOK {
					t.Fatalf("update: status=%d body=%s error=%v", res.StatusCode, response, err)
				}
				frame := readWSMessageOfType(t, conn, protocol.EventIssueUpdated)
				payload := frame["payload"].(map[string]any)
				wantTask := any(nil)
				if agent {
					wantTask = taskID
				}
				if payload["task_id"] != wantTask {
					t.Errorf("event task_id=%v, want %v", payload["task_id"], wantTask)
				}

				var timeline []struct {
					Action    string         `json:"action"`
					ActorType string         `json:"actor_type"`
					ActorID   string         `json:"actor_id"`
					Details   map[string]any `json:"details"`
				}
				readJSON(t, authRequest(t, http.MethodGet, "/api/issues/"+issueID+"/timeline", nil), &timeline)
				seen := map[string]bool{}
				for _, entry := range timeline {
					if entry.Action != "status_changed" && entry.Action != "assignee_changed" {
						continue
					}
					seen[entry.Action] = true
					if entry.ActorType != wantActor || entry.ActorID != wantActorID || entry.Details["task_id"] != wantTask {
						t.Errorf("%s attribution=%s/%s/%v, want %s/%s/%v", entry.Action, entry.ActorType, entry.ActorID, entry.Details["task_id"], wantActor, wantActorID, wantTask)
					}
					if entry.Action == "status_changed" && (entry.Details["from"] != "todo" || entry.Details["to"] != "in_progress") {
						t.Errorf("status history lost old/new values: %#v", entry.Details)
					}
				}
				if !seen["status_changed"] || !seen["assignee_changed"] {
					t.Fatalf("missing field history: %v", seen)
				}
			})
		}
	}
}
