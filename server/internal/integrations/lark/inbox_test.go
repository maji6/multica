package lark

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/multica-ai/multica/server/internal/events"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

func (f *fakePatcherQueries) FindChannelBindingForMemberOnAgentInstallation(context.Context, db.FindChannelBindingForMemberOnAgentInstallationParams) (db.ChannelUserBinding, error) {
	return db.ChannelUserBinding{}, pgx.ErrNoRows
}
func (f *fakePatcherQueries) GetCommentInWorkspace(context.Context, db.GetCommentInWorkspaceParams) (db.Comment, error) {
	return db.Comment{}, pgx.ErrNoRows
}

type mentionQueries struct {
	*fakePatcherQueries
	member        db.ChannelUserBinding
	comment       db.Comment
	bindingErr    error
	lookup        db.FindChannelBindingForMemberOnAgentInstallationParams
	commentLookup db.GetCommentInWorkspaceParams
}

func (q *mentionQueries) FindChannelBindingForMemberOnAgentInstallation(_ context.Context, arg db.FindChannelBindingForMemberOnAgentInstallationParams) (db.ChannelUserBinding, error) {
	q.lookup = arg
	return q.member, q.bindingErr
}
func (q *mentionQueries) GetCommentInWorkspace(_ context.Context, arg db.GetCommentInWorkspaceParams) (db.Comment, error) {
	q.commentLookup = arg
	return q.comment, nil
}

func TestInboxMentionDelivery(t *testing.T) {
	const ws = "11111111-1111-4111-8111-111111111111"
	const actor = "22222222-2222-4222-8222-222222222222"
	const recipient = "33333333-3333-4333-8333-333333333333"
	const issue = "44444444-4444-4444-8444-444444444444"
	const notice = "55555555-5555-4555-8555-555555555555"
	const comment = "66666666-6666-4666-8666-666666666666"
	cases := []struct {
		name    string
		change  func(map[string]any, *mentionQueries, *fakeAPIClient)
		sends   int
		wantErr bool
	}{
		{name: "agent request", sends: 1},
		{name: "binding lookup failed", change: func(i map[string]any, q *mentionQueries, c *fakeAPIClient) {
			q.bindingErr = errors.New("database unavailable")
		}, wantErr: true},
		{name: "different installation owner", change: func(i map[string]any, q *mentionQueries, c *fakeAPIClient) {
			q.installation.AgentID = q.member.MulticaUserID
		}},
		{name: "different installation workspace", change: func(i map[string]any, q *mentionQueries, c *fakeAPIClient) {
			q.installation.WorkspaceID = q.member.MulticaUserID
		}},
		{name: "ordinary activity", change: func(i map[string]any, q *mentionQueries, c *fakeAPIClient) { i["type"] = "new_comment" }},
		{name: "human author", change: func(i map[string]any, q *mentionQueries, c *fakeAPIClient) { i["actor_type"] = "member" }},
		{name: "agent recipient", change: func(i map[string]any, q *mentionQueries, c *fakeAPIClient) { i["recipient_type"] = "agent" }},
		{name: "no binding to this bot", change: func(i map[string]any, q *mentionQueries, c *fakeAPIClient) { q.bindingErr = pgx.ErrNoRows }},
		{name: "revoked", change: func(i map[string]any, q *mentionQueries, c *fakeAPIClient) {
			q.installation.Status = string(InstallationRevoked)
		}},
		{name: "wrong workspace", change: func(i map[string]any, q *mentionQueries, c *fakeAPIClient) { i["workspace_id"] = recipient }, wantErr: true},
		{name: "different comment author", change: func(i map[string]any, q *mentionQueries, c *fakeAPIClient) {
			q.comment.AuthorID = q.member.MulticaUserID
		}},
		{name: "deleted comment", change: func(i map[string]any, q *mentionQueries, c *fakeAPIClient) { q.comment.DeletedAt.Valid = true }},
		{name: "send failure is not retried", change: func(i map[string]any, q *mentionQueries, c *fakeAPIClient) {
			c.mdCardErr = errors.New("network timeout")
		}, sends: 1, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, base, c := newTestPatcher(t)
			q := &mentionQueries{fakePatcherQueries: base}
			q.installation.WorkspaceID = uuidFromString(t, ws)
			q.installation.AgentID = uuidFromString(t, actor)
			q.installation.Status = string(InstallationActive)
			q.member = db.ChannelUserBinding{InstallationID: q.installation.ID, MulticaUserID: uuidFromString(t, recipient), ChannelUserID: "ou_owner"}
			q.comment = db.Comment{ID: uuidFromString(t, comment), IssueID: uuidFromString(t, issue), WorkspaceID: uuidFromString(t, ws), AuthorType: "agent", AuthorID: uuidFromString(t, actor), Content: "Please provide the existing staging entry. No deployment approval is requested."}
			p.queries = q
			c.mdCardReturn = "om_notice"
			item := map[string]any{"id": notice, "workspace_id": ws, "recipient_type": "member", "recipient_id": recipient, "actor_type": "agent", "actor_id": actor, "issue_id": issue, "type": "mentioned", "details": json.RawMessage(`{"comment_id":"` + comment + `"}`)}
			if tc.change != nil {
				tc.change(item, q, c)
			}
			e := events.Event{Type: protocol.EventInboxNew, WorkspaceID: ws, Payload: map[string]any{"item": item}}
			err := p.sendInboxMention(context.Background(), e)
			if (err != nil) != tc.wantErr {
				t.Fatalf("error=%v", err)
			}
			if len(c.mdCardSent) != tc.sends {
				t.Fatalf("sends=%d, want %d", len(c.mdCardSent), tc.sends)
			}
			if tc.name == "agent request" {
				bus := events.New()
				p.Register(bus)
				bus.Publish(e)
				if len(c.mdCardSent) != 2 || c.mdCardSent[0].UUID != c.mdCardSent[1].UUID {
					t.Fatal("event subscription or stable retry key missing")
				}
			}
			if tc.sends > 0 {
				s := c.mdCardSent[0]
				if s.OpenID != "ou_owner" || s.UUID != notice || s.ChatID != "" || !strings.Contains(s.Markdown, "staging entry") {
					t.Fatalf("wrong notification: %+v", s)
				}
				if q.lookup.AgentID != q.installation.AgentID || q.lookup.MulticaUserID != q.member.MulticaUserID || q.lookup.WorkspaceID != q.installation.WorkspaceID || q.commentLookup.WorkspaceID != q.installation.WorkspaceID {
					t.Fatal("lookup was not scoped to actor, recipient and workspace")
				}
			}
		})
	}
}
