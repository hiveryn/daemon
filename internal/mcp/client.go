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

func (s *Server) listTickets(ctx context.Context, status string, limit int) (ListTicketsOutput, error) {
	var output ListTicketsOutput

	u := fmt.Sprintf("%s/api/architects/%s/tickets?status=%s&limit=%d",
		s.daemonURL, url.PathEscape(s.architectKey), url.PathEscape(status), limit)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return ListTicketsOutput{}, fmt.Errorf("build listTickets request: %w", err)
	}

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return ListTicketsOutput{}, fmt.Errorf("request listTickets: %w", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	var env daemonEnvelope
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return ListTicketsOutput{}, newInternalError(fmt.Sprintf("decode daemon response: %v", err))
	}

	if env.Error != nil {
		return ListTicketsOutput{}, mapDaemonError(env.Error)
	}
	if err := json.Unmarshal(env.Data, &output.Tickets); err != nil {
		return ListTicketsOutput{}, newInternalError(fmt.Sprintf("decode ticket list payload: %v", err))
	}

	return output, nil
}

// intentResolutionResponse is the envelope every blocking intent endpoint
// returns. Result is the tool's own payload, present only when approved.
type intentResolutionResponse struct {
	IntentID string          `json:"intent_id"`
	Outcome  string          `json:"outcome"`
	Reason   string          `json:"reason,omitempty"`
	Result   json.RawMessage `json:"result,omitempty"`
}

// requestIntent POSTs to a blocking intent endpoint and decodes the resolution.
// The request is held open by the untimed http.DefaultClient for the whole wait
// window — that is what lets the agent's tool call block instead of polling.
// Do not give this client a timeout.
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

// createWorkTicket asks the user to approve the ticket, blocking until they
// answer or the wait window expires. The ticket is written daemon-side and only
// on approval — there is no unapproved path from here to the filesystem.
func (s *Server) createWorkTicket(ctx context.Context, input CreateWorkTicketInput) (CreateWorkTicketOutput, error) {
	body := map[string]any{"title": input.Title}
	if input.Repo != "" {
		body["repo"] = input.Repo
	}
	if len(input.AdditionalRepos) > 0 {
		body["additional_repos"] = input.AdditionalRepos
	}
	if input.Body != "" {
		body["body"] = input.Body
	}
	if len(input.References) > 0 {
		body["references"] = input.References
	}

	res, err := s.requestIntent(ctx, "create-work-ticket", body)
	if err != nil {
		return CreateWorkTicketOutput{}, err
	}

	envelope, err := intentEnvelope(res)
	if err != nil {
		return CreateWorkTicketOutput{}, err
	}

	output := CreateWorkTicketOutput{IntentEnvelopeFields: envelope}
	if !intentApproved(res.Outcome) {
		return output, nil
	}
	if len(res.Result) == 0 {
		return CreateWorkTicketOutput{}, newInternalError("approved createWorkTicket returned no ticket")
	}
	var ticket TicketOutput
	if err := json.Unmarshal(res.Result, &ticket); err != nil {
		return CreateWorkTicketOutput{}, newInternalError(fmt.Sprintf("decode ticket payload: %v", err))
	}
	output.Ticket = &ticket
	return output, nil
}

func (s *Server) moveTicketToDone(ctx context.Context, input MoveTicketToDoneInput) (MoveTicketToDoneOutput, error) {
	var output MoveTicketToDoneOutput

	body := map[string]any{"body": input.Body}
	if len(input.Commits) > 0 {
		body["commits"] = input.Commits
	}
	if input.Outcome != "" {
		body["outcome"] = input.Outcome
	}
	if input.RejectionReason != "" {
		body["rejection_reason"] = input.RejectionReason
	}

	bodyBytes, err := json.Marshal(body)
	if err != nil {
		return MoveTicketToDoneOutput{}, newInternalError(fmt.Sprintf("marshal move ticket to done body: %v", err))
	}

	u := fmt.Sprintf("%s/api/architects/%s/tickets/%s/move-to-done", s.daemonURL, url.PathEscape(s.architectKey), url.PathEscape(input.ID))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(bodyBytes))
	if err != nil {
		return MoveTicketToDoneOutput{}, fmt.Errorf("build moveTicketToDone request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return MoveTicketToDoneOutput{}, fmt.Errorf("request moveTicketToDone: %w", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	var env daemonEnvelope
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return MoveTicketToDoneOutput{}, newInternalError(fmt.Sprintf("decode daemon response: %v", err))
	}

	if env.Error != nil {
		return MoveTicketToDoneOutput{}, mapDaemonError(env.Error)
	}
	if len(env.Data) == 0 {
		return MoveTicketToDoneOutput{}, newInternalError("daemon response missing data")
	}
	if err := json.Unmarshal(env.Data, &output); err != nil {
		return MoveTicketToDoneOutput{}, newInternalError(fmt.Sprintf("decode move ticket to done payload: %v", err))
	}

	return output, nil
}

func (s *Server) deleteTicket(ctx context.Context, id string) (DeleteTicketOutput, error) {
	var output DeleteTicketOutput

	u := fmt.Sprintf("%s/api/architects/%s/tickets/%s", s.daemonURL, url.PathEscape(s.architectKey), url.PathEscape(id))
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, u, nil)
	if err != nil {
		return DeleteTicketOutput{}, fmt.Errorf("build deleteTicket request: %w", err)
	}

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return DeleteTicketOutput{}, fmt.Errorf("request deleteTicket: %w", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	var env daemonEnvelope
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return DeleteTicketOutput{}, newInternalError(fmt.Sprintf("decode daemon response: %v", err))
	}

	if env.Error != nil {
		return DeleteTicketOutput{}, mapDaemonError(env.Error)
	}
	if len(env.Data) == 0 {
		return DeleteTicketOutput{}, newInternalError("daemon response missing data")
	}
	if err := json.Unmarshal(env.Data, &output); err != nil {
		return DeleteTicketOutput{}, newInternalError(fmt.Sprintf("decode delete ticket payload: %v", err))
	}

	return output, nil
}

func (s *Server) editTicketBody(ctx context.Context, id, oldString, newString string, replaceAll bool) (TicketOutput, error) {
	var output TicketOutput

	body := map[string]any{
		"oldString": oldString,
		"newString": newString,
	}
	if replaceAll {
		body["replaceAll"] = true
	}

	bodyBytes, err := json.Marshal(body)
	if err != nil {
		return TicketOutput{}, newInternalError(fmt.Sprintf("marshal edit ticket body: %v", err))
	}

	u := fmt.Sprintf("%s/api/architects/%s/tickets/%s", s.daemonURL, url.PathEscape(s.architectKey), url.PathEscape(id))
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, u, bytes.NewReader(bodyBytes))
	if err != nil {
		return TicketOutput{}, fmt.Errorf("build editTicketBody request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return TicketOutput{}, fmt.Errorf("request editTicketBody: %w", err)
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

func (s *Server) updateTicket(ctx context.Context, input UpdateTicketInput) (TicketOutput, error) {
	var output TicketOutput

	body := map[string]any{}
	if input.Title != "" {
		body["title"] = input.Title
	}
	if input.Repo != "" {
		body["repo"] = input.Repo
	}
	if input.AdditionalRepos != nil {
		body["additional_repos"] = *input.AdditionalRepos
	}
	if len(input.References) > 0 {
		body["references"] = input.References
	}

	bodyBytes, err := json.Marshal(body)
	if err != nil {
		return TicketOutput{}, newInternalError(fmt.Sprintf("marshal update ticket body: %v", err))
	}

	u := fmt.Sprintf("%s/api/architects/%s/tickets/%s/metadata", s.daemonURL, url.PathEscape(s.architectKey), url.PathEscape(input.ID))
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, u, bytes.NewReader(bodyBytes))
	if err != nil {
		return TicketOutput{}, fmt.Errorf("build updateTicket request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return TicketOutput{}, fmt.Errorf("request updateTicket: %w", err)
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

// concludeRequest is the JSON body POSTed to /request-conclusion. It carries
// the structured conclusion fields for both session types; the daemon
// renders them into the canonical conclusion.md body. Each handler populates
// only the subset relevant to its session type (omitempty drops the rest).
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

// concludeSession asks the user to approve the conclusion, blocking until they
// answer or the wait window expires. A denial is returned as an outcome, not an
// error: the session simply stays running.
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

func (s *Server) readConclusion(ctx context.Context, id string) (ReadConclusionOutput, error) {
	var output ReadConclusionOutput

	u := fmt.Sprintf("%s/api/architects/%s/conclusions/%s", s.daemonURL, url.PathEscape(s.architectKey), url.PathEscape(id))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return ReadConclusionOutput{}, fmt.Errorf("build readConclusion request: %w", err)
	}

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return ReadConclusionOutput{}, fmt.Errorf("request readConclusion: %w", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	var env daemonEnvelope
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return ReadConclusionOutput{}, newInternalError(fmt.Sprintf("decode daemon response: %v", err))
	}

	if env.Error != nil {
		return ReadConclusionOutput{}, mapDaemonError(env.Error)
	}
	if len(env.Data) == 0 {
		return ReadConclusionOutput{}, newInternalError("daemon response missing data")
	}
	if err := json.Unmarshal(env.Data, &output); err != nil {
		return ReadConclusionOutput{}, newInternalError(fmt.Sprintf("decode readConclusion payload: %v", err))
	}

	return output, nil
}

func (s *Server) readRecentConclusion(ctx context.Context) (ReadConclusionOutput, error) {
	var output ReadConclusionOutput

	u := fmt.Sprintf("%s/api/architects/%s/conclusions/recent", s.daemonURL, url.PathEscape(s.architectKey))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return ReadConclusionOutput{}, fmt.Errorf("build readRecentConclusion request: %w", err)
	}

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return ReadConclusionOutput{}, fmt.Errorf("request readRecentConclusion: %w", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	var env daemonEnvelope
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return ReadConclusionOutput{}, newInternalError(fmt.Sprintf("decode daemon response: %v", err))
	}

	if env.Error != nil {
		return ReadConclusionOutput{}, mapDaemonError(env.Error)
	}
	if len(env.Data) == 0 {
		return ReadConclusionOutput{}, newInternalError("daemon response missing data")
	}
	if err := json.Unmarshal(env.Data, &output); err != nil {
		return ReadConclusionOutput{}, newInternalError(fmt.Sprintf("decode readRecentConclusion payload: %v", err))
	}

	return output, nil
}

func (s *Server) listConclusions(ctx context.Context, limit int) (ListConclusionsOutput, error) {
	var output ListConclusionsOutput

	u := fmt.Sprintf("%s/api/architects/%s/conclusions", s.daemonURL, url.PathEscape(s.architectKey))
	if limit > 0 {
		u += fmt.Sprintf("?limit=%d", limit)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return ListConclusionsOutput{}, fmt.Errorf("build listConclusions request: %w", err)
	}

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return ListConclusionsOutput{}, fmt.Errorf("request listConclusions: %w", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	var env daemonEnvelope
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return ListConclusionsOutput{}, newInternalError(fmt.Sprintf("decode daemon response: %v", err))
	}

	if env.Error != nil {
		return ListConclusionsOutput{}, mapDaemonError(env.Error)
	}
	if len(env.Data) == 0 {
		return ListConclusionsOutput{}, nil
	}
	if err := json.Unmarshal(env.Data, &output); err != nil {
		return ListConclusionsOutput{}, newInternalError(fmt.Sprintf("decode listConclusions payload: %v", err))
	}

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
