package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
)

// workspaceRequest performs an architect-scoped workspace GET and decodes the
// envelope data into out. subPath is the portion after
// /api/architects/{key}, e.g. "/workspace/check".
func (s *Server) workspaceRequest(ctx context.Context, subPath string, out any) error {
	u := fmt.Sprintf("%s/api/architects/%s%s", s.daemonURL, url.PathEscape(s.architectKey), subPath)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return fmt.Errorf("build GET %s request: %w", subPath, err)
	}

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("request GET %s: %w", subPath, err)
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
	if len(env.Data) == 0 {
		return newInternalError("daemon response missing data")
	}
	if err := json.Unmarshal(env.Data, out); err != nil {
		return newInternalError(fmt.Sprintf("decode payload: %v", err))
	}
	return nil
}

func (s *Server) checkWorkspace(ctx context.Context) (CheckWorkspaceOutput, error) {
	var output CheckWorkspaceOutput
	if err := s.workspaceRequest(ctx, "/workspace/check", &output); err != nil {
		return CheckWorkspaceOutput{}, err
	}
	return output, nil
}

func (s *Server) describeArtifact(ctx context.Context, kind string) (DescribeArtifactOutput, error) {
	var output DescribeArtifactOutput
	if err := s.workspaceRequest(ctx, "/workspace/artifacts/"+url.PathEscape(kind), &output); err != nil {
		return DescribeArtifactOutput{}, err
	}
	return output, nil
}
