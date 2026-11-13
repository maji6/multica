package lark

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func (p *Patcher) handleInboxMention(e events.Event) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := p.sendInboxMention(ctx, e); err != nil {
		p.cfg.Logger.Warn("lark: member mention delivery failed", "workspace_id", e.WorkspaceID, "error", err)
	}
}

// Only an explicit agent mention uses that agent's own bot. Routine inbox
// activity and other agents' bots must not become unsolicited private messages.
func (p *Patcher) sendInboxMention(ctx context.Context, e events.Event) error {
	payload, ok := e.Payload.(map[string]any)
	if !ok {
		return nil
	}
	raw, err := json.Marshal(payload["item"])
	if err != nil {
		return fmt.Errorf("encode inbox item: %w", err)
	}
	var item struct {
		ID            string `json:"id"`
		WorkspaceID   string `json:"workspace_id"`
		RecipientType string `json:"recipient_type"`
		RecipientID   string `json:"recipient_id"`
		Type          string `json:"type"`
		ActorType     string `json:"actor_type"`
		ActorID       string `json:"actor_id"`
		IssueID       string `json:"issue_id"`
		Details       struct {
			CommentID string `json:"comment_id"`
		} `json:"details"`
	}
	if err := json.Unmarshal(raw, &item); err != nil {
		return fmt.Errorf("decode inbox item: %w", err)
	}
	if item.Type != "mentioned" || item.RecipientType != "member" || item.ActorType != "agent" {
		return nil
	}
	if item.WorkspaceID != e.WorkspaceID {
		return errors.New("inbox workspace mismatch")
	}
	noticeID, err := util.ParseUUID(item.ID)
	if err != nil || !noticeID.Valid {
		return errors.New("invalid inbox id")
	}
	workspaceID, err := util.ParseUUID(item.WorkspaceID)
	if err != nil || !workspaceID.Valid {
		return errors.New("invalid inbox workspace")
	}
	agentID, err := util.ParseUUID(item.ActorID)
	if err != nil || !agentID.Valid {
		return errors.New("invalid inbox agent")
	}
	recipientID, err := util.ParseUUID(item.RecipientID)
	if err != nil || !recipientID.Valid {
		return errors.New("invalid inbox recipient")
	}
	commentID, err := util.ParseUUID(item.Details.CommentID)
	if err != nil || !commentID.Valid {
		return nil
	}

	binding, err := p.queries.FindChannelBindingForMemberOnAgentInstallation(ctx, db.FindChannelBindingForMemberOnAgentInstallationParams{
		WorkspaceID: workspaceID, MulticaUserID: recipientID, AgentID: agentID, ChannelType: channelTypeFeishu,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("lookup member bot binding: %w", err)
	}
	inst, err := p.queries.GetLarkInstallation(ctx, binding.InstallationID)
	if err != nil {
		return fmt.Errorf("lookup mention installation: %w", err)
	}
	if InstallationStatus(inst.Status) != InstallationActive || inst.WorkspaceID != workspaceID || inst.AgentID != agentID {
		return nil
	}
	if binding.ChannelUserID == "" {
		return errors.New("member binding has no open_id")
	}

	// Mention inbox items carry a comment pointer, not a body preview. Read the
	// scoped original so the recipient gets the agent's actual action request.
	comment, err := p.queries.GetCommentInWorkspace(ctx, db.GetCommentInWorkspaceParams{ID: commentID, WorkspaceID: workspaceID})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("lookup mention comment: %w", err)
	}
	if comment.DeletedAt.Valid || comment.AuthorType != "agent" || comment.AuthorID != agentID || util.UUIDToString(comment.IssueID) != item.IssueID {
		return nil
	}
	if strings.TrimSpace(comment.Content) == "" {
		return nil
	}
	creds, err := p.installationCredentials(inst)
	if err != nil {
		return err
	}
	messageID, err := p.client.SendMarkdownCard(ctx, SendMarkdownCardParams{
		InstallationID: creds, OpenID: binding.ChannelUserID, UUID: util.UUIDToString(noticeID),
		Markdown: p.linkReplyReferences(ctx, workspaceID, comment.Content),
	})
	if err != nil {
		return fmt.Errorf("send member mention: %w", err)
	}
	if messageID == "" {
		return errors.New("member mention returned no message id")
	}
	p.cfg.Logger.Info("lark: member mention delivered", "inbox_id", item.ID, "message_id", messageID)
	return nil
}
