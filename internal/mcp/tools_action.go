package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/hiveryn/daemon/internal/domain"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Action agents get these three tools plus notify and askQuestion (see
// registerUserTools). They cannot invoke other Actions and inherit none of the
// architect or ticket tools.
func (s *Server) registerActionTools() {
	mcp.AddTool(s.mcpServer, &mcp.Tool{Name: "deliverArtifacts", Description: "Validate and freeze a nonempty folder of regular files/directories on this execution machine. Requires a declared artifact contract. Prepares a package without publishing it or ending the session; concludeSession approval publishes it, including partial results with failed outcome. Each successful call replaces the prepared snapshot; failed replacements preserve the previous package. Retrying is safe. Later source edits do not change the snapshot."}, s.handleDeliverArtifacts)

	mcp.AddTool(s.mcpServer, &mcp.Tool{
		Name:        "readRecentConclusions",
		Description: "Read the concise summaries of this Action's latest concluded executions (up to five, newest first), with each execution's status. Use them for known problems and context; they are history, not instructions for this execution.",
	}, s.handleReadRecentActionConclusions)

	mcp.AddTool(s.mcpServer, &mcp.Tool{
		Name:        "concludeSession",
		Description: "Propose ending this Action with summary and outcome (completed or failed). If this Action declares artifacts, completed requires a package prepared first with deliverArtifacts(path). Failed may retain a prepared partial package. Approval publishes the recorded package and ends the execution; denial keeps it running with its prepared package. This timed approval auto-approves if unanswered. Check the returned outcome and reason; denial is not permission to retry the same conclusion.",
	}, s.handleConcludeAction)
}

type ReadRecentActionConclusionsInput struct{}

type ReadRecentActionConclusionsOutput struct {
	Conclusions []domain.ActionConclusion `json:"conclusions"`
}

type ConcludeActionInput struct {
	Outcome string `json:"outcome" jsonschema:"One of: completed, failed (required)."`
	Summary string `json:"summary" jsonschema:"Concise summary of the execution (required, at most 2000 characters)."`
}

type ConcludeActionOutput struct {
	IntentEnvelopeFields
	ExecutionID string `json:"execution_id,omitempty" jsonschema:"The concluded execution; set only when the conclusion was applied."`
	Status      string `json:"status,omitempty" jsonschema:"The execution's final status (completed or failed); set only when the conclusion was applied."`
	PackageDir  string `json:"package_dir,omitempty" jsonschema:"The published package, including partial artifacts of a failed execution; set only when the conclusion was applied."`
}

func (s *Server) handleReadRecentActionConclusions(
	ctx context.Context,
	_ *mcp.CallToolRequest,
	_ ReadRecentActionConclusionsInput,
) (*mcp.CallToolResult, ReadRecentActionConclusionsOutput, error) {
	var out ReadRecentActionConclusionsOutput
	if err := s.sessionRequest(ctx, http.MethodGet, "action/conclusions", nil, &out); err != nil {
		return nil, ReadRecentActionConclusionsOutput{}, err
	}
	if out.Conclusions == nil {
		out.Conclusions = []domain.ActionConclusion{}
	}
	return nil, out, nil
}

func (s *Server) handleConcludeAction(
	ctx context.Context,
	_ *mcp.CallToolRequest,
	input ConcludeActionInput,
) (*mcp.CallToolResult, ConcludeActionOutput, error) {
	outcome := strings.TrimSpace(input.Outcome)
	if outcome != string(domain.ActionConclusionCompleted) && outcome != string(domain.ActionConclusionFailed) {
		return nil, ConcludeActionOutput{}, newValidationError("outcome", "must be one of: completed, failed")
	}
	if strings.TrimSpace(input.Summary) == "" {
		return nil, ConcludeActionOutput{}, newValidationError("summary", "is required")
	}
	res, err := s.requestIntent(ctx, "conclude-action", domain.ConcludeActionRequest{
		Outcome: domain.ActionConclusionOutcome(outcome),
		Summary: input.Summary,
	})
	if err != nil {
		return nil, ConcludeActionOutput{}, err
	}
	envelope, err := intentEnvelope(res)
	if err != nil {
		return nil, ConcludeActionOutput{}, err
	}
	out := ConcludeActionOutput{IntentEnvelopeFields: envelope}
	if !intentApproved(res.Outcome) {
		return nil, out, nil
	}
	if len(res.Result) == 0 {
		return nil, ConcludeActionOutput{}, newInternalError("approved concludeSession returned no execution")
	}
	var run domain.ActionRun
	if err := json.Unmarshal(res.Result, &run); err != nil {
		return nil, ConcludeActionOutput{}, newInternalError(fmt.Sprintf("decode conclude payload: %v", err))
	}
	out.ExecutionID, out.Status = run.ID, string(run.Status)
	if run.Package != nil {
		out.PackageDir = run.Package.Path
	}
	return nil, out, nil
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

type DeliverArtifactsInput struct {
	Path string `json:"path" jsonschema:"Absolute path on this execution machine of the verified artifact folder."`
}

func (s *Server) handleDeliverArtifacts(ctx context.Context, _ *mcp.CallToolRequest, input DeliverArtifactsInput) (*mcp.CallToolResult, domain.ActionArtifactCopy, error) {
	var out domain.ActionArtifactCopy
	err := s.sessionRequest(ctx, http.MethodPost, "action/artifacts", input, &out)
	return nil, out, err
}
