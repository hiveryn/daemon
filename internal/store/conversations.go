package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/hiveryn/daemon/internal/domain"
)

const submissionColumns = "seq,id,session_id,request_id,origin,text,status,native_turn_id,error_code,error,accepted_at,acceptance,created_at,updated_at"

// submissionFinal are statuses no later evidence changes.
const submissionFinal = "('completed','failed','rejected')"

func scanSubmission(row interface{ Scan(...any) error }, extra ...any) (domain.Submission, error) {
	var s domain.Submission
	var created, updated, accepted string
	err := row.Scan(append([]any{&s.Sequence, &s.ID, &s.SessionID, &s.RequestID, &s.Origin, &s.Text, &s.Status, &s.NativeTurnID, &s.ErrorCode, &s.Error, &accepted, &s.Acceptance, &created, &updated}, extra...)...)
	if err != nil {
		return s, err
	}
	if accepted != "" {
		at, err := time.Parse(time.RFC3339Nano, accepted)
		if err != nil {
			return s, err
		}
		s.AcceptedAt = &at
	}
	s.CreatedAt, err = time.Parse(time.RFC3339Nano, created)
	if err != nil {
		return s, err
	}
	s.UpdatedAt, err = time.Parse(time.RFC3339Nano, updated)
	return s, err
}

func formatOptionalTime(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

// InsertSubmission records a request identity, or returns the existing record
// of the same request (created=false). Reuse with different text or origin is
// a conflict. status is the initial state: delivering (a user send about to be
// attempted), rejected (refused before any attempt, with code and reason) or
// queued (an internal chat entry).
func (s *SessionStore) InsertSubmission(ctx context.Context, sid string, req domain.SubmissionRequest, origin, status, code, reason string) (domain.Submission, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.Submission{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	prior, err := scanSubmission(tx.QueryRowContext(ctx, "SELECT "+submissionColumns+" FROM conversation_submissions WHERE session_id=? AND request_id=?", sid, req.RequestID))
	if err == nil {
		if prior.Text != req.Text || prior.Origin != origin {
			return domain.Submission{}, false, &domain.ConflictError{Resource: "submission", Field: "request_id", Message: "request_id was already used with different text or origin"}
		}
		return prior, false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return domain.Submission{}, false, err
	}
	var archived int
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM chat_archives WHERE session_id=?", sid).Scan(&archived); err != nil {
		return domain.Submission{}, false, err
	}
	if archived != 0 {
		return domain.Submission{}, false, &domain.DeliveryError{Code: domain.DeliverySessionArchived, Message: "chat is archived"}
	}
	now := time.Now().UTC()
	id := domain.SubmissionIdentity(sid, req.RequestID)
	result, err := tx.ExecContext(ctx, `INSERT INTO conversation_submissions(id,session_id,request_id,origin,text,status,error_code,error,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?)`,
		id, sid, req.RequestID, origin, req.Text, status, code, reason, now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano))
	if err != nil {
		return domain.Submission{}, false, fmt.Errorf("record conversation submission: %w", err)
	}
	seq, err := result.LastInsertId()
	if err != nil {
		return domain.Submission{}, false, err
	}
	if err = tx.Commit(); err != nil {
		return domain.Submission{}, false, err
	}
	return domain.Submission{ID: id, SessionID: sid, RequestID: req.RequestID, Sequence: seq, Origin: origin, Text: req.Text, Status: status, ErrorCode: code, Error: reason, CreatedAt: now, UpdatedAt: now}, true, nil
}
func (s *SessionStore) GetSubmission(ctx context.Context, sid, id string) (domain.Submission, error) {
	result, err := scanSubmission(s.db.QueryRowContext(ctx, "SELECT "+submissionColumns+" FROM conversation_submissions WHERE session_id=? AND id=?", sid, id))
	if errors.Is(err, sql.ErrNoRows) {
		return result, &domain.NotFoundError{Resource: "submission", ID: id}
	}
	return result, err
}

// ReserveSubmission hands the chat owner its next entry: the oldest open
// entry, when it is an internal queued entry or a user send awaiting the
// owner. The reservation (delivering + handed_at) is durable before the turn.
func (s *SessionStore) ReserveSubmission(ctx context.Context, sid string) (*domain.Submission, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	// An open blocked/unknown/uncertain turn holds the chat. Never skip it or blindly resend.
	var handed string
	sub, err := scanSubmission(tx.QueryRowContext(ctx, "SELECT "+submissionColumns+",handed_at FROM conversation_submissions WHERE session_id=? AND status NOT IN "+submissionFinal+" ORDER BY seq LIMIT 1", sid), &handed)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if sub.Status != domain.SubmissionQueued && (sub.Status != domain.SubmissionDelivering || handed != "") {
		return nil, nil
	}
	now := time.Now().UTC()
	sub.Status = domain.SubmissionDelivering
	sub.UpdatedAt = now
	if _, err = tx.ExecContext(ctx, "UPDATE conversation_submissions SET status='delivering',handed_at=?,updated_at=? WHERE id=?", now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano), sub.ID); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return &sub, nil
}

// UpdateSubmission writes the evidence-derived state. Final states are never
// changed again, and acceptance once recorded is kept.
func (s *SessionStore) UpdateSubmission(ctx context.Context, sub domain.Submission) error {
	_, err := s.db.ExecContext(ctx, `UPDATE conversation_submissions SET status=?,native_turn_id=?,error_code=?,error=?,
		accepted_at=CASE WHEN accepted_at='' THEN ? ELSE accepted_at END,
		acceptance=CASE WHEN acceptance='' THEN ? ELSE acceptance END,updated_at=?
		WHERE id=? AND session_id=? AND status NOT IN `+submissionFinal,
		sub.Status, sub.NativeTurnID, sub.ErrorCode, sub.Error, formatOptionalTime(sub.AcceptedAt), sub.Acceptance, time.Now().UTC().Format(time.RFC3339Nano), sub.ID, sub.SessionID)
	return err
}

// SettleDelivery concludes a delivery attempt that is still delivering: to
// rejected only while no hand-off was reserved (nothing can have reached the
// provider), otherwise to the given status. It reports whether it changed the
// record; false means other evidence already moved it on.
func (s *SessionStore) SettleDelivery(ctx context.Context, sid, id, status, code, reason string) (bool, error) {
	guard := ""
	if status == domain.SubmissionRejected {
		guard = " AND handed_at=''"
	}
	result, err := s.db.ExecContext(ctx, "UPDATE conversation_submissions SET status=?,error_code=?,error=?,updated_at=? WHERE session_id=? AND id=? AND status='delivering'"+guard,
		status, code, reason, time.Now().UTC().Format(time.RFC3339Nano), sid, id)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n == 1, err
}

// ReconcileDeliveries settles attempts a previous daemon left delivering:
// never handed over is rejected, a reserved hand-off is unknown.
func (s *SessionStore) ReconcileDeliveries(ctx context.Context) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := s.db.ExecContext(ctx, "UPDATE conversation_submissions SET status='rejected',error_code=?,error=?,updated_at=? WHERE status='delivering' AND handed_at=''",
		domain.DeliveryInterrupted, "Not sent: the daemon stopped before the hand-off", now); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, "UPDATE conversation_submissions SET status='unknown',error_code=?,error=?,updated_at=? WHERE status='delivering'",
		domain.DeliveryUnknown, "The daemon stopped during the hand-off; acceptance is reconciled from the native record and the input is never resent", now)
	return err
}

// conversationGap separates appended display positions so later-discovered
// items can be placed between existing ones without renumbering.
const conversationGap = 1024

// PutConversationItems records items given in native record order. Changed
// items get a new revision. Display order (sequence) follows native order: the
// longest run of already stored items whose positions agree with this order
// stays put, and every other item — newly seen, or stored out of native order
// (an earlier input persisted only after its replies) — is placed directly
// after its native predecessor, or before its first native successor, or else
// appended. Stored items absent from the batch keep their positions. A moved
// item is a new revision, so incremental readers and fresh reads converge; the
// whole batch is one transaction, so every snapshot is consistently ordered.
func (s *SessionStore) PutConversationItems(ctx context.Context, sid string, items []domain.ConversationItem) error {
	if len(items) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var archived int
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM chat_archives WHERE session_id=?", sid).Scan(&archived); err != nil {
		return err
	}
	if archived != 0 {
		return nil
	} // Session end wins over late native output.
	stored, err := latestConversationItems(ctx, tx, sid)
	if err != nil {
		return err
	}
	batch := make([]domain.ConversationItem, 0, len(items))
	seen := map[string]int{}
	for _, item := range items {
		if i, ok := seen[item.ID]; ok {
			batch[i] = item // the latest state; the first position
			continue
		}
		seen[item.ID] = len(batch)
		batch = append(batch, item)
	}
	changed := map[string]bool{}
	for i := range batch {
		item := &batch[i]
		old, ok := stored[item.ID]
		if !ok {
			item.Sequence = 0
			changed[item.ID] = true
			continue
		}
		if item.Tool == "" {
			item.Tool = old.Tool
		}
		if !old.At.IsZero() {
			item.At = old.At
		}
		item.Sequence = old.Sequence
		if old.Text != item.Text || old.Status != item.Status || old.Tool != item.Tool || old.NativeTurnID != item.NativeTurnID ||
			old.Origin != item.Origin || old.Via != item.Via || old.NativeOrigin != item.NativeOrigin || old.SubmissionID != item.SubmissionID {
			changed[item.ID] = true
		}
	}
	others := make([]domain.ConversationItem, 0, len(stored))
	for id, item := range stored {
		if _, ok := seen[id]; !ok {
			others = append(others, item)
		}
	}
	for _, item := range orderConversation(others, batch) {
		if item.Sequence != stored[item.ID].Sequence {
			changed[item.ID] = true
		}
		if !changed[item.ID] {
			continue
		}
		item.Revision = 0
		data, err := json.Marshal(item)
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO conversation_items(session_id,item_id,item_json) VALUES(?,?,?)", sid, item.ID, string(data)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// latestConversationItems is the current state of every item of a session.
func latestConversationItems(ctx context.Context, tx *sql.Tx, sid string) (map[string]domain.ConversationItem, error) {
	rows, err := tx.QueryContext(ctx, `SELECT item_json FROM conversation_items WHERE revision IN
		(SELECT MAX(revision) FROM conversation_items WHERE session_id=? GROUP BY item_id)`, sid)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string]domain.ConversationItem{}
	for rows.Next() {
		var data string
		if err := rows.Scan(&data); err != nil {
			return nil, err
		}
		var item domain.ConversationItem
		if err := json.Unmarshal([]byte(data), &item); err != nil {
			return nil, err
		}
		out[item.ID] = item
	}
	return out, rows.Err()
}

// orderConversation returns every item (others: stored items outside the
// batch; batch: in native order, stored ones carrying their sequence, new ones
// zero) in display order with sequences assigned: unchanged where order
// allows, otherwise between their display neighbours, renumbering the whole
// session only when no gap is left.
func orderConversation(others, batch []domain.ConversationItem) []domain.ConversationItem {
	fixed := nativeOrderRun(batch)
	display := make([]domain.ConversationItem, 0, len(others)+len(batch))
	display = append(display, others...)
	for i, item := range batch {
		if fixed[i] {
			display = append(display, item)
		}
	}
	sort.Slice(display, func(i, j int) bool {
		if display[i].Sequence != display[j].Sequence {
			return display[i].Sequence < display[j].Sequence
		}
		return display[i].ID < display[j].ID
	})
	position := func(id string) int {
		for i := range display {
			if display[i].ID == id {
				return i
			}
		}
		return -1
	}
	firstFixed := -1
	for i := range batch {
		if fixed[i] {
			firstFixed = i
			break
		}
	}
	for i, item := range batch {
		if fixed[i] {
			continue
		}
		at := len(display)
		switch {
		case i > 0:
			at = position(batch[i-1].ID) + 1
		case firstFixed >= 0:
			at = position(batch[firstFixed].ID)
		}
		item.Sequence = 0
		display = append(display, domain.ConversationItem{})
		copy(display[at+1:], display[at:])
		display[at] = item
	}
	assignSequences(display)
	return display
}

// nativeOrderRun marks the longest subsequence of already stored batch items
// whose sequences increase in native order (ties broken by earliest index):
// those keep their positions, all others are placed anew.
func nativeOrderRun(batch []domain.ConversationItem) []bool {
	var idx []int // batch indexes of stored items
	for i, item := range batch {
		if item.Sequence > 0 {
			idx = append(idx, i)
		}
	}
	fixed := make([]bool, len(batch))
	if len(idx) == 0 {
		return fixed
	}
	// Patience sorting: tails[k] is the batch index ending the best run of length k+1.
	tails := []int{}
	prev := make([]int, len(idx))
	for n, i := range idx {
		k := sort.Search(len(tails), func(k int) bool { return batch[idx[tails[k]]].Sequence >= batch[i].Sequence })
		if k > 0 {
			prev[n] = tails[k-1]
		} else {
			prev[n] = -1
		}
		if k == len(tails) {
			tails = append(tails, n)
		} else {
			tails[k] = n
		}
	}
	for n := tails[len(tails)-1]; n >= 0; n = prev[n] {
		fixed[idx[n]] = true
	}
	return fixed
}

// assignSequences gives every unplaced item (zero sequence) a position between
// its neighbours, or renumbers the whole list when a run does not fit.
func assignSequences(display []domain.ConversationItem) {
	for start := 0; start < len(display); start++ {
		if display[start].Sequence != 0 {
			continue
		}
		end := start
		for end < len(display) && display[end].Sequence == 0 {
			end++
		}
		var low int64
		if start > 0 {
			low = display[start-1].Sequence
		}
		count := int64(end - start)
		step := int64(conversationGap)
		if end < len(display) {
			step = (display[end].Sequence - low) / (count + 1)
			if step < 1 {
				for i := range display {
					display[i].Sequence = int64(i+1) * conversationGap
				}
				return
			}
		}
		for i := start; i < end; i++ {
			low += step
			display[i].Sequence = low
		}
		start = end
	}
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

// SettleSubmissions ends a chat's open entries: status failed settles
// everything open; uncertain settles accepted/running/blocked turns only.
func (s *SessionStore) SettleSubmissions(ctx context.Context, sid, status, reason string) error {
	predicate := "status NOT IN " + submissionFinal
	if status == domain.SubmissionUncertain {
		predicate = "status IN ('accepted','running','blocked')"
	}
	_, err := s.db.ExecContext(ctx, "UPDATE conversation_submissions SET status=?,error=?,updated_at=? WHERE session_id=? AND "+predicate, status, reason, time.Now().UTC().Format(time.RFC3339Nano), sid)
	return err
}

func (s *SessionStore) PendingSubmission(ctx context.Context, sid string) (*domain.Submission, error) {
	sub, err := scanSubmission(s.db.QueryRowContext(ctx, "SELECT "+submissionColumns+" FROM conversation_submissions WHERE session_id=? AND status NOT IN "+submissionFinal+" ORDER BY seq LIMIT 1", sid))
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

// OpenTerminalSubmissions lists submissions whose delivery or turn is still
// settled from native evidence, in admission order.
func (s *SessionStore) OpenTerminalSubmissions(ctx context.Context, sid string) ([]domain.TerminalSubmission, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+submissionColumns+",provider_ref,handed_at,handed_terminal FROM conversation_submissions WHERE session_id=? AND status IN ('delivering','unknown','accepted','running') ORDER BY seq", sid)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []domain.TerminalSubmission
	for rows.Next() {
		var t domain.TerminalSubmission
		var handed string
		if t.Submission, err = scanSubmission(rows, &t.ProviderRef, &handed, &t.HandedTerminal); err != nil {
			return nil, err
		}
		if handed != "" {
			if t.HandedAt, err = time.Parse(time.RFC3339Nano, handed); err != nil {
				return nil, err
			}
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ReserveTerminalSubmission records the provider reference, hand-off time and
// receiving terminal of a delivering submission before the hand-off; false if
// it is no longer an unreserved delivery.
func (s *SessionStore) ReserveTerminalSubmission(ctx context.Context, sid, id, providerRef, terminalID string, handedAt time.Time) (bool, error) {
	result, err := s.db.ExecContext(ctx, "UPDATE conversation_submissions SET provider_ref=?,handed_at=?,handed_terminal=?,updated_at=? WHERE session_id=? AND id=? AND status='delivering' AND handed_at=''",
		providerRef, handedAt.UTC().Format(time.RFC3339Nano), terminalID, time.Now().UTC().Format(time.RFC3339Nano), sid, id)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n == 1, err
}

func (s *SessionStore) TerminalProviderRefs(ctx context.Context, sid string) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT provider_ref,id FROM conversation_submissions WHERE session_id=? AND provider_ref<>''", sid)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	refs := map[string]string{}
	for rows.Next() {
		var ref, id string
		if err := rows.Scan(&ref, &id); err != nil {
			return nil, err
		}
		refs[ref] = id
	}
	return refs, rows.Err()
}
