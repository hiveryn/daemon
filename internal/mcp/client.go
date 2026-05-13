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

func (s *Server) createWorkTicket(ctx context.Context, input CreateWorkTicketInput) (TicketOutput, error) {
	var output TicketOutput

	body := map[string]any{"title": input.Title}
	if input.Repo != "" {
		body["repo"] = input.Repo
	}
	if input.Body != "" {
		body["body"] = input.Body
	}
	if len(input.References) > 0 {
		body["references"] = input.References
	}

	bodyBytes, err := json.Marshal(body)
	if err != nil {
		return TicketOutput{}, newInternalError(fmt.Sprintf("marshal create ticket body: %v", err))
	}

	u := fmt.Sprintf("%s/api/architects/%s/tickets", s.daemonURL, url.PathEscape(s.architectKey))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(bodyBytes))
	if err != nil {
		return TicketOutput{}, fmt.Errorf("build createWorkTicket request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return TicketOutput{}, fmt.Errorf("request createWorkTicket: %w", err)
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

func (s *Server) concludeSession(ctx context.Context, input ConcludeSessionInput) (ConcludeSessionOutput, error) {
	var output ConcludeSessionOutput

	body := map[string]any{"body": input.Body}
	if len(input.Commits) > 0 {
		body["commits"] = input.Commits
	}
	if input.Rejected {
		body["rejected"] = true
	}
	if input.RejectionReason != "" {
		body["rejection_reason"] = input.RejectionReason
	}

	bodyBytes, err := json.Marshal(body)
	if err != nil {
		return ConcludeSessionOutput{}, newInternalError(fmt.Sprintf("marshal conclude body: %v", err))
	}

	u := fmt.Sprintf("%s/api/sessions/%s/conclude", s.daemonURL, url.PathEscape(s.sessionID))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(bodyBytes))
	if err != nil {
		return ConcludeSessionOutput{}, fmt.Errorf("build concludeSession request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return ConcludeSessionOutput{}, fmt.Errorf("request concludeSession: %w", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	var env daemonEnvelope
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return ConcludeSessionOutput{}, newInternalError(fmt.Sprintf("decode daemon response: %v", err))
	}

	if env.Error != nil {
		return ConcludeSessionOutput{}, mapDaemonError(env.Error)
	}
	if len(env.Data) == 0 {
		return ConcludeSessionOutput{}, newInternalError("daemon response missing data")
	}
	if err := json.Unmarshal(env.Data, &output); err != nil {
		return ConcludeSessionOutput{}, newInternalError(fmt.Sprintf("decode conclude payload: %v", err))
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
