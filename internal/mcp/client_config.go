package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

// configRequest performs an architect-config HTTP call and decodes the envelope
// data into out (pass nil when no payload is expected). subPath is the portion
// after /api/architects/{key}/config, e.g. "" or "/default-prompt".
func (s *Server) configRequest(ctx context.Context, method, subPath string, body, out any) error {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return newInternalError(fmt.Sprintf("marshal request body: %v", err))
		}
		reader = bytes.NewReader(encoded)
	}

	u := fmt.Sprintf("%s/api/architects/%s/config%s", s.daemonURL, url.PathEscape(s.architectKey), subPath)
	req, err := http.NewRequestWithContext(ctx, method, u, reader)
	if err != nil {
		return fmt.Errorf("build %s %s request: %w", method, subPath, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("request %s %s: %w", method, subPath, err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	var env daemonEnvelope
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return newInternalError(fmt.Sprintf("decode daemon response: %v", err))
	}
	if env.Error != nil {
		return mapDaemonError(env.Error)
	}
	if out == nil {
		return nil
	}
	if len(env.Data) == 0 {
		return newInternalError("daemon response missing data")
	}
	if err := json.Unmarshal(env.Data, out); err != nil {
		return newInternalError(fmt.Sprintf("decode payload: %v", err))
	}
	return nil
}

func (s *Server) readArchitectConfig(ctx context.Context) (ReadArchitectConfigOutput, error) {
	var output ReadArchitectConfigOutput
	if err := s.configRequest(ctx, http.MethodGet, "", nil, &output); err != nil {
		return ReadArchitectConfigOutput{}, err
	}
	return output, nil
}

func (s *Server) updateArchitectConfig(ctx context.Context, input UpdateArchitectConfigInput) (UpdateArchitectConfigOutput, error) {
	var output UpdateArchitectConfigOutput
	if err := s.configRequest(ctx, http.MethodPut, "", input, &output); err != nil {
		return UpdateArchitectConfigOutput{}, err
	}
	return output, nil
}

func (s *Server) readDefaultPrompt(ctx context.Context, kind string) (ReadDefaultPromptOutput, error) {
	var output ReadDefaultPromptOutput
	if err := s.configRequest(ctx, http.MethodGet, "/default-prompt?kind="+url.QueryEscape(kind), nil, &output); err != nil {
		return ReadDefaultPromptOutput{}, err
	}
	return output, nil
}
