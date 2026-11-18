package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func TestWebhookCommentFilters(t *testing.T) {
	filters := []byte(`[{"event":"issue_comment","actions":["created"],"comment_first_lines":["【待确认】","【批准开发】"]}]`)
	for _, tc := range []struct {
		name, action, body string
		want               bool
	}{
		{"question", "created", "【待确认】\n问题", true},
		{"approval", "created", "【批准开发】", true},
		{"crlf", "created", " 【待确认】 \r\n问题", true},
		{"receipt", "created", "【执行回执】\n已开工", false},
		{"quoted protocol", "created", "相关问题：\n【待确认】", false},
		{"prefix only", "created", "【待确认】并非独立首行", false},
		{"empty", "created", "", false},
		{"edited", "edited", "【待确认】", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload, _ := json.Marshal(map[string]any{"action": tc.action, "comment": map[string]any{"body": tc.body}})
			env := WebhookEnvelope{Event: "issue_comment." + tc.action, EventPayload: payload}
			if got := evaluateWebhookTriggerScope(filters, env).Allowed; got != tc.want {
				t.Fatalf("allowed = %v, want %v", got, tc.want)
			}
		})
	}
	for _, payload := range []string{`{}`, `{"comment":{"body":12}}`, `[]`} {
		if evaluateWebhookTriggerScope(filters, WebhookEnvelope{Event: "issue_comment.created", EventPayload: json.RawMessage(payload)}).Allowed {
			t.Fatalf("missing or malformed comment was accepted: %s", payload)
		}
	}
	env := WebhookEnvelope{Event: "issue_comment.created", EventPayload: json.RawMessage(`{"comment":{"body":"ordinary comment"}}`)}
	if !evaluateWebhookTriggerScope([]byte(`[{"event":"issue_comment","actions":["created"]}]`), env).Allowed {
		t.Fatal("existing filters without comment_first_lines must retain their behavior")
	}
	if !evaluateWebhookTriggerScope([]byte(`[{"event":"issue_comment","comment_first_lines":["【待确认】"]},{"event":"issue_comment"}]`), env).Allowed {
		t.Fatal("a non-matching comment filter must allow another row to match")
	}
}

func TestWebhookCommentAndPayloadFiltersMustBothMatch(t *testing.T) {
	filters := []byte(`[{"event":"issue_comment","comment_first_lines":["ready"],"conditions":[{"path":"repository.name","op":"equals","value":"widget"}]}]`)
	for _, tc := range []struct {
		body, repository string
		allowed          bool
	}{
		{"ready", "widget", true},
		{"ready", "other", false},
		{"ordinary comment", "widget", false},
	} {
		payload, err := json.Marshal(map[string]any{
			"comment":    map[string]string{"body": tc.body},
			"repository": map[string]string{"name": tc.repository},
		})
		if err != nil {
			t.Fatal(err)
		}
		env := WebhookEnvelope{Event: "issue_comment.created", EventPayload: payload}
		if got := evaluateWebhookTriggerScope(filters, env).Allowed; got != tc.allowed {
			t.Errorf("comment %q on %q: allowed = %v, want %v", tc.body, tc.repository, got, tc.allowed)
		}
	}
}

func TestWebhookCommentFiltersValidation(t *testing.T) {
	for _, f := range []WebhookEventFilter{
		{Event: "issues", CommentFirstLines: []string{"ready"}},
		{Event: "issue_comment", CommentFirstLines: []string{" "}},
		{Event: "issue_comment", CommentFirstLines: []string{"one\ntwo"}},
	} {
		if validateWebhookEventFilters([]WebhookEventFilter{f}) == nil {
			t.Fatalf("invalid filter accepted: %#v", f)
		}
	}
}

func TestWebhookHandler_CommentFilterBeforeAdmission(t *testing.T) {
	agentID := createWebhookTestAgent(t, "CommentFilter Agent")
	apID := createWebhookTestAutopilot(t, agentID, "active", "run_only")
	trig := createWebhookTriggerWithFilters(t, apID, []WebhookEventFilter{
		{Event: "issue_comment", Actions: []string{"created"}, CommentFirstLines: []string{"【待确认】"}},
	})
	row, err := testHandler.Queries.GetAutopilotTrigger(context.Background(), parseUUID(trig.ID))
	if err != nil {
		t.Fatal(err)
	}
	var persisted []WebhookEventFilter
	if err := json.Unmarshal(row.EventFilters, &persisted); err != nil || len(persisted) != 1 || len(persisted[0].CommentFirstLines) != 1 || persisted[0].CommentFirstLines[0] != "【待确认】" {
		t.Fatalf("comment filter did not persist: %v", err)
	}
	if len(trig.EventFilters) != 1 || len(trig.EventFilters[0].CommentFirstLines) != 1 {
		t.Fatal("API response dropped the comment filter")
	}
	w := postWebhook(t, *trig.WebhookToken, map[string]any{
		"action": "created", "comment": map[string]any{"body": "【执行回执】\n已开工"},
	}, map[string]string{"X-GitHub-Event": "issue_comment"})
	var response map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if w.Code != http.StatusOK || response["status"] != "ignored" || response["reason"] != "event_filtered" || response["run_id"] != nil {
		t.Fatalf("receipt was not filtered before admission: %s", w.Body.String())
	}
	runs, err := testHandler.Queries.ListAutopilotRuns(context.Background(), db.ListAutopilotRunsParams{AutopilotID: parseUUID(apID), Limit: 10})
	if err != nil || len(runs) != 0 {
		t.Fatalf("ignored receipt created a run: count=%d err=%v", len(runs), err)
	}
	w = postWebhook(t, *trig.WebhookToken, map[string]any{
		"action": "created", "comment": map[string]any{"body": "【待确认】\n问题"},
	}, map[string]string{"X-GitHub-Event": "issue_comment"})
	requireAcceptedWebhookResponse(t, w)
}
