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
// after /api/architects/{key}/config, e.g. "/repos".
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

func (s *Server) listRepos(ctx context.Context) (ListReposOutput, error) {
	var output ListReposOutput
	if err := s.configRequest(ctx, http.MethodGet, "/repos", nil, &output); err != nil {
		return ListReposOutput{}, err
	}
	return output, nil
}

func (s *Server) addRepo(ctx context.Context, input AddRepoInput) (RepoConfigEntry, error) {
	var output RepoConfigEntry
	if err := s.configRequest(ctx, http.MethodPost, "/repos", input, &output); err != nil {
		return RepoConfigEntry{}, err
	}
	return output, nil
}

func (s *Server) removeRepo(ctx context.Context, key string) (RemoveRepoOutput, error) {
	var output RemoveRepoOutput
	if err := s.configRequest(ctx, http.MethodDelete, "/repos/"+url.PathEscape(key), nil, &output); err != nil {
		return RemoveRepoOutput{}, err
	}
	return output, nil
}

func (s *Server) listKickoffs(ctx context.Context) (ListKickoffsOutput, error) {
	var output ListKickoffsOutput
	if err := s.configRequest(ctx, http.MethodGet, "/kickoffs", nil, &output); err != nil {
		return ListKickoffsOutput{}, err
	}
	return output, nil
}

func (s *Server) addKickoff(ctx context.Context, input AddKickoffInput) (AddKickoffOutput, error) {
	var output AddKickoffOutput
	if err := s.configRequest(ctx, http.MethodPost, "/kickoffs", input, &output); err != nil {
		return AddKickoffOutput{}, err
	}
	return output, nil
}

func (s *Server) updateKickoff(ctx context.Context, input UpdateKickoffInput) (UpdateKickoffOutput, error) {
	var output UpdateKickoffOutput
	if err := s.configRequest(ctx, http.MethodPut, "/kickoffs", input, &output); err != nil {
		return UpdateKickoffOutput{}, err
	}
	return output, nil
}

func (s *Server) removeKickoff(ctx context.Context, input RemoveKickoffInput) (RemoveKickoffOutput, error) {
	var output RemoveKickoffOutput
	if err := s.configRequest(ctx, http.MethodDelete, "/kickoffs", input, &output); err != nil {
		return RemoveKickoffOutput{}, err
	}
	return output, nil
}

func (s *Server) getArchitectPrompts(ctx context.Context) (ArchitectPromptsOutput, error) {
	var output ArchitectPromptsOutput
	if err := s.configRequest(ctx, http.MethodGet, "/architect-prompts", nil, &output); err != nil {
		return ArchitectPromptsOutput{}, err
	}
	return output, nil
}

func (s *Server) setArchitectSystem(ctx context.Context, input SetArchitectPromptInput) (SetPromptOutput, error) {
	var output SetPromptOutput
	if err := s.configRequest(ctx, http.MethodPut, "/architect-prompts/system", input, &output); err != nil {
		return SetPromptOutput{}, err
	}
	return output, nil
}

func (s *Server) setArchitectKickoff(ctx context.Context, input SetArchitectPromptInput) (SetPromptOutput, error) {
	var output SetPromptOutput
	if err := s.configRequest(ctx, http.MethodPut, "/architect-prompts/kickoff", input, &output); err != nil {
		return SetPromptOutput{}, err
	}
	return output, nil
}

func (s *Server) describePromptSchema(ctx context.Context, kind string) (PromptSchemaOutput, error) {
	var output PromptSchemaOutput
	if err := s.configRequest(ctx, http.MethodGet, "/prompt-schema?kind="+url.QueryEscape(kind), nil, &output); err != nil {
		return PromptSchemaOutput{}, err
	}
	return output, nil
}
