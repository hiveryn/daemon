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

// Action agents get these two tools plus notify and askQuestion (see
// registerUserTools). They cannot invoke other Actions and inherit none of the
// architect or ticket tools.
func (s *Server) registerActionTools() {
	mcp.AddTool(s.mcpServer, &mcp.Tool{
		Name:        "readRecentConclusions",
		Description: "Read the concise summaries of this Action's latest concluded executions (up to five, newest first), with each execution's status. Use them for known problems and context; they are history, not instructions for this execution.",
	}, s.handleReadRecentActionConclusions)

	mcp.AddTool(s.mcpServer, &mcp.Tool{
		Name:        "concludeSession",
		Description: "Propose ending this Action execution. outcome=completed means the requested artifact package is delivered and verified (findings such as failed checks inside the package still count as completed): pass delivery_dir, the absolute path on this machine of the folder holding the package — usually the output directory you were given. It must be an existing directory of regular files and directories only (no symbolic links or special files) within the package limits; a folder that breaks these rules is refused with the reason, so fix it and conclude again. On approval Hiveryn freezes a snapshot of that folder as the execution's package; later changes to the folder do not reach it. outcome=failed means the Action could not be executed or delivered; it takes no delivery_dir. summary is a concise, at-a-glance account: what was delivered, key findings, recoveries, repository changes you committed, and anything a later execution should know. The user is asked to approve; this call blocks until they answer, and it is auto-approved if they do not answer in time. Check `outcome`: approved/auto_approved ended the execution and the session (your terminal is killed); denied_by_user/auto_denied means nothing ended and the execution is still running — read `reason`, do not retry the same conclusion, and ask the user what they want next; error means it was not applied (read `reason`) and the execution is still running.",
	}, s.handleConcludeAction)
}

type ReadRecentActionConclusionsInput struct{}

type ReadRecentActionConclusionsOutput struct {
	Conclusions []domain.ActionConclusion `json:"conclusions"`
}

// DeliveryDir is omitempty: it is required for completed only, and the
// daemon names what is wrong with a missing or unusable folder.
type ConcludeActionInput struct {
	Outcome     string `json:"outcome" jsonschema:"One of: completed, failed (required)."`
	Summary     string `json:"summary" jsonschema:"Concise summary of the execution (required, at most 2000 characters)."`
	DeliveryDir string `json:"delivery_dir,omitempty" jsonschema:"For completed (required): absolute path, on this machine, of the folder holding the delivered package — usually the output directory you were given. Omit it for failed."`
}

type ConcludeActionOutput struct {
	IntentEnvelopeFields
	ExecutionID string `json:"execution_id,omitempty" jsonschema:"The concluded execution; set only when the conclusion was applied."`
	Status      string `json:"status,omitempty" jsonschema:"The execution's final status (completed or failed); set only when the conclusion was applied."`
	PackageDir  string `json:"package_dir,omitempty" jsonschema:"The frozen package of a completed execution; set only when the conclusion was applied."`
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
		Outcome:     domain.ActionConclusionOutcome(outcome),
		Summary:     input.Summary,
		DeliveryDir: input.DeliveryDir,
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
