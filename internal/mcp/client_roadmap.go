package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"

	"github.com/hiveryn/daemon/internal/domain"
)

// roadmapRequest performs an architect-roadmap HTTP call and decodes the
// envelope data into out. subPath is the portion after
// /api/architects/{key}/roadmap, e.g. "" or "?view=archive".
func (s *Server) roadmapRequest(ctx context.Context, method, subPath string, body, out any) error {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return newInternalError(fmt.Sprintf("marshal request body: %v", err))
		}
		reader = bytes.NewReader(encoded)
	}

	u := fmt.Sprintf("%s/api/architects/%s/roadmap%s", s.daemonURL, url.PathEscape(s.architectKey), subPath)
	req, err := http.NewRequestWithContext(ctx, method, u, reader)
	if err != nil {
		return fmt.Errorf("build %s roadmap request: %w", method, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("request %s roadmap: %w", method, err)
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

func (s *Server) readRoadmap(ctx context.Context, input ReadRoadmapInput) (ReadRoadmapOutput, error) {
	query := url.Values{}
	if input.View != "" {
		query.Set("view", input.View)
	}
	if input.ID != "" {
		query.Set("id", input.ID)
	}
	if input.Depth != nil {
		query.Set("depth", strconv.Itoa(*input.Depth))
	}
	subPath := ""
	if encoded := query.Encode(); encoded != "" {
		subPath = "?" + encoded
	}
	var output ReadRoadmapOutput
	if err := s.roadmapRequest(ctx, http.MethodGet, subPath, nil, &output); err != nil {
		return ReadRoadmapOutput{}, err
	}
	return output, nil
}

func (s *Server) putRoadmap(ctx context.Context, params domain.UpdateRoadmapParams) (UpdateRoadmapOutput, error) {
	var output UpdateRoadmapOutput
	if err := s.roadmapRequest(ctx, http.MethodPut, "", params, &output); err != nil {
		return UpdateRoadmapOutput{}, err
	}
	return output, nil
}
