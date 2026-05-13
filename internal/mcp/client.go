package mcp

import (
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
