package handler

import (
	"encoding/json"
	"testing"
)

func TestWebhookAuthorFilters(t *testing.T) {
	filters := []byte(`[{"event":"issue_comment","actions":["created"],"comment_first_lines":["【任务就绪】"],"author_ids":[101]},{"event":"issue_comment","actions":["created"],"comment_first_lines":["【裁定】"],"author_ids":[101,202]},{"event":"pull_request_review","actions":["submitted"],"author_ids":[303]}]`)
	for _, tc := range []struct {
		name, event, payload string
		want                 bool
	}{
		{"admin ready", "issue_comment.created", `{"comment":{"body":"【任务就绪】","user":{"id":101}}}`, true},
		{"execution impersonates admin", "issue_comment.created", `{"comment":{"body":"【任务就绪】\n角色：管理员","user":{"id":303}},"sender":{"id":101}}`, false},
		{"human decision", "issue_comment.created", `{"comment":{"body":"【裁定】","user":{"id":202}}}`, true},
		{"missing author", "issue_comment.created", `{"comment":{"body":"【任务就绪】"},"sender":{"id":101}}`, false},
		{"malformed author", "issue_comment.created", `{"comment":{"body":"【任务就绪】","user":{"id":"101"}}}`, false},
		{"edited", "issue_comment.edited", `{"comment":{"body":"【任务就绪】","user":{"id":101}}}`, false},
		{"execution review", "pull_request_review.submitted", `{"review":{"user":{"id":303}}}`, true},
		{"admin review", "pull_request_review.submitted", `{"review":{"user":{"id":101}},"sender":{"id":303}}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := webhookEventAllowedByTriggerScope(filters, WebhookEnvelope{Event: tc.event, EventPayload: json.RawMessage(tc.payload)}); got != tc.want {
				t.Fatalf("allowed = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestWebhookAuthorFiltersValidation(t *testing.T) {
	for _, f := range []WebhookEventFilter{
		{Event: "issues", AuthorIDs: []int64{1}},
		{Event: "issue_comment", AuthorIDs: []int64{0}},
		{Event: "issue_comment", AuthorIDs: []int64{-1}},
		{Event: "pull_request_review", AuthorIDs: []int64{9007199254740992}},
	} {
		if validateWebhookEventFilters([]WebhookEventFilter{f}) == nil {
			t.Fatalf("invalid filter accepted: %#v", f)
		}
	}
}
