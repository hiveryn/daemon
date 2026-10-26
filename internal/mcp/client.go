package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"

	"github.com/hiveryn/daemon/internal/domain"
)

type daemonEnvelope struct {
	Data  json.RawMessage   `json:"data"`
	Error *domain.ErrorBody `json:"error"`
	Meta  domain.Meta       `json:"meta"`
}

func (s *Server) readTicket(ctx context.Context, id string) (TicketOutput, error) {
	var output TicketOutput

	path := fmt.Sprintf("%s/api/architects/%s/tickets/%s", s.daemonURL, url.PathEscape(s.architectKey), url.PathEscape(id))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, path, nil)
	if err != nil {
		return TicketOutput{}, fmt.Errorf("build readTicket request: %w", err)
	}

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return TicketOutput{}, fmt.Errorf("request readTicket: %w", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	var env daemonEnvelope
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return TicketOutput{}, newInternalError(fmt.Sprintf("decode daemon response: %v", err))
	}

	if env.Error != nil {
		return TicketOutput{}, mapDaemonError(env.Error)
	}
	if len(env.Data) == 0 {
		return TicketOutput{}, newInternalError("daemon response missing data")
	}
	if err := json.Unmarshal(env.Data, &output); err != nil {
		return TicketOutput{}, newInternalError(fmt.Sprintf("decode ticket payload: %v", err))
	}

	return output, nil
}

func (s *Server) requestIntent(ctx context.Context, subPath string, body any) (intentResolutionResponse, error) {
	if s.sessionID == "" {
		return intentResolutionResponse{}, newInternalError("HIVERYN_SESSION_ID not set")
	}

	bodyBytes, err := json.Marshal(body)
	if err != nil {
		return intentResolutionResponse{}, newInternalError(fmt.Sprintf("marshal %s body: %v", subPath, err))
	}

	u := fmt.Sprintf("%s/api/sessions/%s/intents/%s", s.daemonURL, url.PathEscape(s.sessionID), subPath)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(bodyBytes))
	if err != nil {
		return intentResolutionResponse{}, fmt.Errorf("build %s request: %w", subPath, err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return intentResolutionResponse{}, fmt.Errorf("request %s: %w", subPath, err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	var env daemonEnvelope
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return intentResolutionResponse{}, newInternalError(fmt.Sprintf("decode daemon response: %v", err))
	}
	if env.Error != nil {
		return intentResolutionResponse{}, mapDaemonError(env.Error)
	}
	if len(env.Data) == 0 {
		return intentResolutionResponse{}, newInternalError("daemon response missing data")
	}

	var out intentResolutionResponse
	if err := json.Unmarshal(env.Data, &out); err != nil {
		return intentResolutionResponse{}, newInternalError(fmt.Sprintf("decode intent resolution: %v", err))
	}
	return out, nil
}

// concludeSession waits for the lifecycle approval result.
func (s *Server) concludeSession(ctx context.Context, payload concludeRequest) (ConcludeSessionOutput, error) {
	res, err := s.requestIntent(ctx, "conclude-session", payload)
	if err != nil {
		return ConcludeSessionOutput{}, err
	}

	envelope, err := intentEnvelope(res)
	if err != nil {
		return ConcludeSessionOutput{}, err
	}

	output := ConcludeSessionOutput{IntentEnvelopeFields: envelope}
	if !intentApproved(res.Outcome) {
		return output, nil
	}
	if len(res.Result) == 0 {
		return ConcludeSessionOutput{}, newInternalError("approved concludeSession returned no result")
	}
	var result ConcludeSessionResult
	if err := json.Unmarshal(res.Result, &result); err != nil {
		return ConcludeSessionOutput{}, newInternalError(fmt.Sprintf("decode conclude payload: %v", err))
	}
	output.Session = &result
	return output, nil
}

func mapDaemonError(err *domain.ErrorBody) error {
	if err == nil {
		return nil
	}

	switch domain.ErrorCode(err.Code) {
	case domain.ErrCodeValidation:
		return &ToolError{Code: ErrorCodeValidation, Message: err.Message}
	case domain.ErrCodeNotFound:
		return &ToolError{Code: ErrorCodeNotFound, Message: err.Message}
	case domain.ErrCodeConflict:
		return newStateConflictError(err.Message)
	default:
		return newInternalError(err.Message)
	}
}

type intentResolutionResponse struct {
	IntentID string          `json:"intent_id"`
	Outcome  string          `json:"outcome"`
	Reason   string          `json:"reason,omitempty"`
	Result   json.RawMessage `json:"result,omitempty"`
}

type concludeRequest struct {
	Commits         []domain.CommitRef `json:"commits,omitempty"`
	Outcome         string             `json:"outcome,omitempty"`
	RejectionReason string             `json:"rejection_reason,omitempty"`
	Summary         string             `json:"summary,omitempty"`
	Narrative       string             `json:"narrative,omitempty"`
	Implementation  string             `json:"implementation,omitempty"`
	Verification    string             `json:"verification,omitempty"`
	TicketsTouched  string             `json:"tickets_touched,omitempty"`
	Decisions       string             `json:"decisions,omitempty"`
	ConfigChanges   string             `json:"config_changes,omitempty"`
	UserPriorities  string             `json:"user_priorities,omitempty"`
	Deviations      string             `json:"deviations,omitempty"`
	FollowUps       string             `json:"follow_ups,omitempty"`
	OpenQuestions   string             `json:"open_questions,omitempty"`
	NextSteps       string             `json:"next_steps,omitempty"`
}

// sessionRequest calls /api/sessions/{session}/{subPath} — subPath may carry a
// query — and decodes the envelope data into out.
func (s *Server) sessionRequest(ctx context.Context, method, subPath string, body any, out any) error {
	var reader *bytes.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return newInternalError(fmt.Sprintf("marshal %s body: %v", subPath, err))
		}
		reader = bytes.NewReader(raw)
	}
	u := fmt.Sprintf("%s/api/sessions/%s/%s", s.daemonURL, url.PathEscape(s.sessionID), subPath)
	var req *http.Request
	var err error
	if reader != nil {
		req, err = http.NewRequestWithContext(ctx, method, u, reader)
	} else {
		req, err = http.NewRequestWithContext(ctx, method, u, nil)
	}
	if err != nil {
		return fmt.Errorf("build %s %s request: %w", method, subPath, err)
	}
	if reader != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("request %s %s: %w", method, subPath, err)
	}
	defer func() { _ = resp.Body.Close() }()

	var env daemonEnvelope
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return newInternalError(fmt.Sprintf("decode daemon response: %v", err))
	}
	if env.Error != nil {
		return mapDaemonError(env.Error)
	}
	if len(env.Data) == 0 {
		return newInternalError("daemon response missing data")
	}
	if err := json.Unmarshal(env.Data, out); err != nil {
		return newInternalError(fmt.Sprintf("decode %s payload: %v", subPath, err))
	}
	return nil
}
