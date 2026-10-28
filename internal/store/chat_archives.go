package store

import (
	"context"
	"encoding/json"
	"time"

	"github.com/hiveryn/daemon/internal/domain"
)

func (s *SessionStore) IsArchivedChat(ctx context.Context, id string) (bool, error) {
	var count int
	err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM chat_archives WHERE session_id=?", id).Scan(&count)
	return count != 0, err
}

// ArchiveChat commits discovery, queue disposition and final item revisions
// together. The same transaction boundary prevents late native item resurrection.
func (s *SessionStore) ArchiveChat(ctx context.Context, chat domain.ArchivedChat) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	data, err := json.Marshal(chat)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO chat_archives(session_id,architect_key,descriptor_json) VALUES(?,?,?) ON CONFLICT(session_id) DO NOTHING", chat.ID, chat.ArchitectKey, string(data)); err != nil {
		return err
	}
	// An attempt whose acceptance was never established stays unconfirmed;
	// anything else still open ends with the session.
	if _, err = tx.ExecContext(ctx, "UPDATE conversation_submissions SET status='uncertain',error='The session ended before acceptance of this input was established; not resent',updated_at=? WHERE session_id=? AND (status='unknown' OR (status='delivering' AND handed_at<>''))", chat.EndedAt.Format(time.RFC3339Nano), chat.ID); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "UPDATE conversation_submissions SET status='rejected',error_code=?,error='Not sent: the session ended before the hand-off',updated_at=? WHERE session_id=? AND status='delivering'", domain.DeliverySessionEnding, chat.EndedAt.Format(time.RFC3339Nano), chat.ID); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "UPDATE conversation_submissions SET status='failed',error='Session ended before submission completed',updated_at=? WHERE session_id=? AND status NOT IN ('completed','failed','uncertain','rejected')", chat.EndedAt.Format(time.RFC3339Nano), chat.ID); err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `SELECT item_json FROM conversation_items WHERE revision IN (SELECT MAX(revision) FROM conversation_items WHERE session_id=? GROUP BY item_id)`, chat.ID)
	if err != nil {
		return err
	}
	var interrupted []domain.ConversationItem
	for rows.Next() {
		var raw string
		if err = rows.Scan(&raw); err != nil {
			_ = rows.Close()
			return err
		}
		var item domain.ConversationItem
		if err = json.Unmarshal([]byte(raw), &item); err != nil {
			_ = rows.Close()
			return err
		}
		// Turn-start markers can have a separate native completion item. Only
		// tool activity and permission waits need a synthetic end disposition.
		open := item.Status == "started" || item.Status == "blocked" || item.Status == "running" || item.Status == "pending" || item.Status == "inProgress" || item.Status == "in_progress"
		if open && (item.Role == "tool" || item.Status == "blocked") {
			item.Status = "interrupted"
			interrupted = append(interrupted, item)
		}
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return err
	}
	for _, item := range interrupted {
		data, err = json.Marshal(item)
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO conversation_items(session_id,item_id,item_json) VALUES(?,?,?)", chat.ID, item.ID, string(data)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *SessionStore) ListSubmissions(ctx context.Context, sid string, q domain.SequenceQuery) (domain.SubmissionPage, error) {
	page := domain.SubmissionPage{Submissions: []domain.Submission{}, NextSequence: q.AfterSequence}
	if q.Limit <= 0 || q.Limit > 200 {
		q.Limit = 100
	}
	rows, err := s.db.QueryContext(ctx, "SELECT "+submissionColumns+" FROM conversation_submissions WHERE session_id=? AND seq>? ORDER BY seq LIMIT ?", sid, q.AfterSequence, q.Limit+1)
	if err != nil {
		return page, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		if len(page.Submissions) == q.Limit {
			page.HasMore = true
			break
		}
		sub, err := scanSubmission(rows)
		if err != nil {
			return page, err
		}
		page.Submissions = append(page.Submissions, sub)
		page.NextSequence = sub.Sequence
	}
	return page, rows.Err()
}
