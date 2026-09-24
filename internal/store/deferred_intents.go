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

// DeferredIntentStore implements domain.DeferredIntentRepository.
type DeferredIntentStore struct {
	db *sql.DB
}

func NewDeferredIntentStore(db *sql.DB) *DeferredIntentStore {
	return &DeferredIntentStore{db: db}
}

func (s *DeferredIntentStore) CreateDeferredIntent(ctx context.Context, in domain.DeferredIntent) error {
	payload, err := marshalNullableJSON(in.Payload, len(in.Payload) == 0)
	if err != nil {
		return fmt.Errorf("marshal deferred intent payload: %w", err)
	}
	origin, err := json.Marshal(in.Origin)
	if err != nil {
		return fmt.Errorf("marshal deferred intent origin: %w", err)
	}
	inputs, err := marshalNullableJSON(in.Inputs, len(in.Inputs) == 0)
	if err != nil {
		return fmt.Errorf("marshal deferred intent inputs: %w", err)
	}
	result, err := marshalNullableJSON(in.Result, in.Result == nil)
	if err != nil {
		return fmt.Errorf("marshal deferred intent result: %w", err)
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO deferred_intents (id, session_id, intent_type, summary, payload, origin, status, inputs, result, reason, error, created_at, approved_at, ended_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, in.ID, in.Origin.SessionID, string(in.Type), in.Summary, payload, string(origin), string(in.Status), inputs, result,
		nullIfEmpty(in.Reason), nullIfEmpty(in.Error), formatPreciseTime(in.CreatedAt), nullableTime(in.ApprovedAt), nullableTime(in.EndedAt))
	if err != nil {
		return fmt.Errorf("insert deferred intent %s: %w", in.ID, err)
	}
	return nil
}

func (s *DeferredIntentStore) GetDeferredIntent(ctx context.Context, id string) (domain.DeferredIntent, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, intent_type, summary, COALESCE(payload, ''), origin, status, COALESCE(inputs, ''), COALESCE(result, ''),
		       COALESCE(reason, ''), COALESCE(error, ''), created_at, COALESCE(approved_at, ''), COALESCE(ended_at, '')
		FROM deferred_intents
		WHERE id = ?
	`, id)

	var (
		out                                          domain.DeferredIntent
		typ, status, payload, origin, inputs, result string
		createdAt, approvedAt, endedAt               string
	)
	if err := row.Scan(&out.ID, &typ, &out.Summary, &payload, &origin, &status, &inputs, &result,
		&out.Reason, &out.Error, &createdAt, &approvedAt, &endedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.DeferredIntent{}, &domain.NotFoundError{Resource: "intent", ID: id}
		}
		return domain.DeferredIntent{}, fmt.Errorf("get deferred intent %s: %w", id, err)
	}
	out.Type = domain.IntentType(typ)
	out.Status = domain.DeferredIntentStatus(status)
	if err := json.Unmarshal([]byte(origin), &out.Origin); err != nil {
		return domain.DeferredIntent{}, fmt.Errorf("decode deferred intent %s origin: %w", id, err)
	}
	if err := unmarshalOptionalJSON(payload, &out.Payload); err != nil {
		return domain.DeferredIntent{}, fmt.Errorf("decode deferred intent %s payload: %w", id, err)
	}
	if err := unmarshalOptionalJSON(inputs, &out.Inputs); err != nil {
		return domain.DeferredIntent{}, fmt.Errorf("decode deferred intent %s inputs: %w", id, err)
	}
	if err := unmarshalOptionalJSON(result, &out.Result); err != nil {
		return domain.DeferredIntent{}, fmt.Errorf("decode deferred intent %s result: %w", id, err)
	}
	var err error
	if out.CreatedAt, err = parseSQLiteTime(createdAt); err != nil {
		return domain.DeferredIntent{}, fmt.Errorf("parse deferred intent %s created_at: %w", id, err)
	}
	if out.ApprovedAt, err = parseSQLiteMaybeTime(approvedAt); err != nil {
		return domain.DeferredIntent{}, fmt.Errorf("parse deferred intent %s approved_at: %w", id, err)
	}
	if out.EndedAt, err = parseSQLiteMaybeTime(endedAt); err != nil {
		return domain.DeferredIntent{}, fmt.Errorf("parse deferred intent %s ended_at: %w", id, err)
	}
	return out, nil
}

func (s *DeferredIntentStore) TransitionDeferredIntent(ctx context.Context, from domain.DeferredIntentStatus, next domain.DeferredIntent) error {
	inputs, err := marshalNullableJSON(next.Inputs, len(next.Inputs) == 0)
	if err != nil {
		return fmt.Errorf("marshal deferred intent inputs: %w", err)
	}
	result, err := marshalNullableJSON(next.Result, next.Result == nil)
	if err != nil {
		return fmt.Errorf("marshal deferred intent result: %w", err)
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE deferred_intents
		SET status = ?, inputs = ?, result = ?, reason = ?, error = ?, approved_at = ?, ended_at = ?
		WHERE id = ? AND status = ?
	`, string(next.Status), inputs, result, nullIfEmpty(next.Reason), nullIfEmpty(next.Error),
		nullableTime(next.ApprovedAt), nullableTime(next.EndedAt), next.ID, string(from))
	if err != nil {
		return fmt.Errorf("transition deferred intent %s %s -> %s: %w", next.ID, from, next.Status, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("transition deferred intent %s: rows affected: %w", next.ID, err)
	}
	if n == 1 {
		return nil
	}
	current, err := s.GetDeferredIntent(ctx, next.ID)
	if err != nil {
		return err
	}
	return &domain.ConflictError{
		Resource: "intent",
		Field:    "status",
		Message:  fmt.Sprintf("intent %s is %s, not %s", next.ID, current.Status, from),
	}
}

func (s *DeferredIntentStore) FailOpenDeferredIntents(ctx context.Context, pendingReason, runningReason string, at time.Time) (int, error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE deferred_intents
		SET error = CASE status WHEN ? THEN ? ELSE ? END,
		    status = ?,
		    ended_at = ?
		WHERE status IN (?, ?)
	`, string(domain.DeferredIntentPendingApproval), pendingReason, runningReason,
		string(domain.DeferredIntentFailed), formatPreciseTime(at),
		string(domain.DeferredIntentPendingApproval), string(domain.DeferredIntentRunning))
	if err != nil {
		return 0, fmt.Errorf("fail open deferred intents: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("fail open deferred intents: rows affected: %w", err)
	}
	return int(n), nil
}

func (s *DeferredIntentStore) PruneDeferredIntents(ctx context.Context, cutoff time.Time) (int, error) {
	res, err := s.db.ExecContext(ctx, `
		DELETE FROM deferred_intents
		WHERE ended_at IS NOT NULL AND ended_at < ? AND status IN (?, ?, ?)
	`, formatPreciseTime(cutoff),
		string(domain.DeferredIntentDenied), string(domain.DeferredIntentCompleted), string(domain.DeferredIntentFailed))
	if err != nil {
		return 0, fmt.Errorf("prune deferred intents: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("prune deferred intents: rows affected: %w", err)
	}
	return int(n), nil
}

func marshalNullableJSON(v any, empty bool) (any, error) {
	if empty {
		return nil, nil
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return string(raw), nil
}

func unmarshalOptionalJSON(raw string, dst any) error {
	if raw == "" {
		return nil
	}
	return json.Unmarshal([]byte(raw), dst)
}

func nullableTime(t *time.Time) any {
	if t == nil {
		return nil
	}
	return formatPreciseTime(*t)
}
