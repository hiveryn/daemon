package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/hiveryn/daemon/internal/domain"
)

const submissionColumns = "seq,id,session_id,request_id,origin,text,status,native_turn_id,error,created_at,updated_at"

func scanSubmission(row interface{ Scan(...any) error }) (domain.Submission, error) {
	var s domain.Submission
	var created, updated string
	err := row.Scan(&s.Sequence, &s.ID, &s.SessionID, &s.RequestID, &s.Origin, &s.Text, &s.Status, &s.NativeTurnID, &s.Error, &created, &updated)
	if err != nil {
		return s, err
	}
	s.CreatedAt, err = time.Parse(time.RFC3339Nano, created)
	if err != nil {
		return s, err
	}
	s.UpdatedAt, err = time.Parse(time.RFC3339Nano, updated)
	return s, err
}
func (s *SessionStore) EnqueueSubmission(ctx context.Context, sid string, req domain.SubmissionRequest, origin string) (domain.Submission, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.Submission{}, err
	}
	defer func() { _ = tx.Rollback() }()
	prior, err := scanSubmission(tx.QueryRowContext(ctx, "SELECT "+submissionColumns+" FROM conversation_submissions WHERE session_id=? AND request_id=?", sid, req.RequestID))
	if err == nil {
		if prior.Text != req.Text || prior.Origin != origin {
			return domain.Submission{}, &domain.ConflictError{Resource: "submission", Field: "request_id", Message: "request_id was already used with different text or origin"}
		}
		return prior, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return domain.Submission{}, err
	}
	now := time.Now().UTC()
	id := domain.SubmissionIdentity(sid, req.RequestID)
	result, err := tx.ExecContext(ctx, `INSERT INTO conversation_submissions(id,session_id,request_id,origin,text,status,created_at,updated_at) VALUES(?,?,?,?,?,'queued',?,?)`, id, sid, req.RequestID, origin, req.Text, now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano))
	if err != nil {
		return domain.Submission{}, fmt.Errorf("enqueue conversation: %w", err)
	}
	seq, err := result.LastInsertId()
	if err != nil {
		return domain.Submission{}, err
	}
	if err = tx.Commit(); err != nil {
		return domain.Submission{}, err
	}
	return domain.Submission{ID: id, SessionID: sid, RequestID: req.RequestID, Sequence: seq, Origin: origin, Text: req.Text, Status: "queued", CreatedAt: now, UpdatedAt: now}, nil
}
func (s *SessionStore) GetSubmission(ctx context.Context, sid, id string) (domain.Submission, error) {
	result, err := scanSubmission(s.db.QueryRowContext(ctx, "SELECT "+submissionColumns+" FROM conversation_submissions WHERE session_id=? AND id=?", sid, id))
	if errors.Is(err, sql.ErrNoRows) {
		return result, &domain.NotFoundError{Resource: "submission", ID: id}
	}
	return result, err
}
func (s *SessionStore) ReserveSubmission(ctx context.Context, sid string) (*domain.Submission, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	// An uncertain/blocked turn holds the queue. Never skip it or blindly resend.
	sub, err := scanSubmission(tx.QueryRowContext(ctx, "SELECT "+submissionColumns+" FROM conversation_submissions WHERE session_id=? AND status NOT IN ('completed','failed') ORDER BY seq LIMIT 1", sid))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if sub.Status != "queued" {
		return nil, nil
	}
	sub.Status = "started"
	sub.UpdatedAt = time.Now().UTC()
	if _, err = tx.ExecContext(ctx, "UPDATE conversation_submissions SET status='started',updated_at=? WHERE id=?", sub.UpdatedAt.Format(time.RFC3339Nano), sub.ID); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return &sub, nil
}
func (s *SessionStore) UpdateSubmission(ctx context.Context, sub domain.Submission) error {
	_, err := s.db.ExecContext(ctx, `UPDATE conversation_submissions SET status=?,native_turn_id=?,error=?,updated_at=? WHERE id=? AND session_id=? AND status NOT IN ('completed','failed')`, sub.Status, sub.NativeTurnID, sub.Error, time.Now().UTC().Format(time.RFC3339Nano), sub.ID, sub.SessionID)
	return err
}
func (s *SessionStore) PutConversationItem(ctx context.Context, sid string, item domain.ConversationItem) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var previous string
	err = tx.QueryRowContext(ctx, "SELECT item_json FROM conversation_items WHERE session_id=? AND item_id=? ORDER BY revision DESC LIMIT 1", sid, item.ID).Scan(&previous)
	if err == nil {
		var old domain.ConversationItem
		if err = json.Unmarshal([]byte(previous), &old); err != nil {
			return err
		}
		item.Sequence = old.Sequence
		if !old.At.IsZero() {
			item.At = old.At
		}
		if old.Text == item.Text && old.Status == item.Status && old.Tool == item.Tool && old.NativeTurnID == item.NativeTurnID {
			return nil
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if item.Sequence == 0 {
		if err = tx.QueryRowContext(ctx, "SELECT COALESCE(MAX(revision),0)+1 FROM conversation_items").Scan(&item.Sequence); err != nil {
			return err
		}
	}
	data, err := json.Marshal(item)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO conversation_items(session_id,item_id,item_json) VALUES(?,?,?)", sid, item.ID, string(data)); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *SessionStore) ReadConversation(ctx context.Context, sid string, q domain.ConversationQuery) (domain.ConversationPage, error) {
	page := domain.ConversationPage{Items: []domain.ConversationItem{}, SnapshotRevision: q.SnapshotRevision, NextRevision: q.AfterRevision}
	if q.Limit <= 0 || q.Limit > 200 {
		q.Limit = 100
	}
	var latest int64
	if err := s.db.QueryRowContext(ctx, "SELECT COALESCE(MAX(revision),0) FROM conversation_items WHERE session_id=?", sid).Scan(&latest); err != nil {
		return page, err
	}
	if q.SnapshotRevision == 0 {
		page.SnapshotRevision = latest
	}
	if page.SnapshotRevision > latest || q.AfterRevision > page.SnapshotRevision {
		return page, &domain.ValidationError{Field: "pagination", Message: "revision cursor is outside this conversation snapshot"}
	}
	rows, err := s.db.QueryContext(ctx, "SELECT revision,item_json FROM conversation_items WHERE session_id=? AND revision>? AND revision<=? ORDER BY revision LIMIT ?", sid, q.AfterRevision, page.SnapshotRevision, q.Limit+1)
	if err != nil {
		return page, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var revision int64
		var data string
		if err = rows.Scan(&revision, &data); err != nil {
			return page, err
		}
		if len(page.Items) == q.Limit {
			page.HasMore = true
			break
		}
		var item domain.ConversationItem
		if err = json.Unmarshal([]byte(data), &item); err != nil {
			return page, err
		}
		item.Revision = revision
		page.Items = append(page.Items, item)
		page.NextRevision = revision
	}
	return page, rows.Err()
}
func (s *SessionStore) SettleSubmissions(ctx context.Context, sid, status, reason string) error {
	predicate := "status NOT IN ('completed','failed')"
	if status == "uncertain" {
		predicate = "status IN ('started','blocked')"
	}
	_, err := s.db.ExecContext(ctx, "UPDATE conversation_submissions SET status=?,error=?,updated_at=? WHERE session_id=? AND "+predicate, status, reason, time.Now().UTC().Format(time.RFC3339Nano), sid)
	return err
}

func (s *SessionStore) PendingSubmission(ctx context.Context, sid string) (*domain.Submission, error) {
	sub, err := scanSubmission(s.db.QueryRowContext(ctx, "SELECT "+submissionColumns+" FROM conversation_submissions WHERE session_id=? AND status NOT IN ('completed','failed') ORDER BY seq LIMIT 1", sid))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &sub, nil
}

func (s *SessionStore) FailQueuedSubmissions(ctx context.Context, sid, reason string) error {
	_, err := s.db.ExecContext(ctx, "UPDATE conversation_submissions SET status='failed',error=?,updated_at=? WHERE session_id=? AND status='queued'", reason, time.Now().UTC().Format(time.RFC3339Nano), sid)
	return err
}
