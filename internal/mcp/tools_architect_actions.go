package mcp

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/hiveryn/daemon/internal/domain"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Architect Actions tools. The architect is always the calling session's; the
// daemon enforces availableActions on discovery and on every request, and
// scopes results to the architect.
func (s *Server) registerArchitectActionTools() {
	mcp.AddTool(s.mcpServer, &mcp.Tool{
		Name:        "getAvailableActions",
		Description: "List the Actions this project may request (hiveryn.yaml availableActions), each with its description — what the Action does and what your prompt must contain — and its artifact contract. A listed Action with valid=false cannot be requested; its problems say why (for example a missing definition). running_execution_id is set while an execution of that Action is running.",
	}, s.handleGetAvailableActions)

	mcp.AddTool(s.mcpServer, &mcp.Tool{
		Name:        "executeAction",
		Description: "Request one execution of an available Action with a prompt. Returns immediately with the execution_id and status pending_approval: the user reviews the request, picks the agent variant and approves or denies it — nothing runs until then, and there is no timeout that approves it. Keep the execution_id; it is the same through approval, execution and result. Follow up with waitForActionResult or getActionResult. Errors if the Action is not available to this project, is invalid, or is already running (only one execution of an Action runs at a time). An identical request (same name and prompt) from this session while it is pending, or within ten minutes of its approval or denial, returns the original execution instead of creating another.",
	}, s.handleExecuteAction)

	mcp.AddTool(s.mcpServer, &mcp.Tool{
		Name:        "getActionResult",
		Description: "Read the current state of an execution this project requested, by execution_id: pending_approval (not started), denied (with the user's reason), running (with started time, elapsed seconds, the agent's last reported activity when known, and its attention), completed (artifact package delivered to output_dir, with the agent's summary) or failed (with the error or the agent's summary). attention.state is input_required when an explicit provider signal shows the Action agent waiting for the user at its terminal (for example a folder-trust dialog or an interrupted conversation after a restart; message says what it asks), none_detected when no such signal is present — which does not prove it is not waiting, see attention.coverage — or unavailable when it is not running. The execution stays running either way; only the user can answer, by opening the execution's terminal in the Actions window, so tell them what the agent is asking instead of waiting repeatedly. Returns immediately.",
	}, s.handleGetActionResult)

	mcp.AddTool(s.mcpServer, &mcp.Tool{
		Name:        "waitForActionResult",
		Description: fmt.Sprintf("Wait for an execution this project requested to change state, for at most timeout_seconds (1-%d, default %d). Returns as soon as the status changes (for example pending_approval → running, running → completed) or the agent's attention changes (input_required appearing, clearing or changing reason — see getActionResult), or immediately if it is already final; otherwise returns the unchanged state with timed_out=true, and you may call it again. Waiting never affects the execution.", domain.MaxActionWaitSeconds, domain.MaxActionWaitSeconds),
	}, s.handleWaitForActionResult)
}

type GetAvailableActionsInput struct{}

type GetAvailableActionsOutput struct {
	Actions []domain.ActionDefinition `json:"actions"`
}

type ExecuteActionInput struct {
	Name   string `json:"name" jsonschema:"The Action name, from getAvailableActions (required)."`
	Prompt string `json:"prompt" jsonschema:"What to do, containing everything the Action's description asks for (required)."`
}

type ActionResultInput struct {
	ExecutionID string `json:"execution_id" jsonschema:"The execution_id executeAction returned (required)."`
}

type WaitForActionResultInput struct {
	ExecutionID    string `json:"execution_id" jsonschema:"The execution_id executeAction returned (required)."`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty" jsonschema:"How long to wait for a status or attention change, 1-30 seconds; omitted means 30."`
}

type ActionResultOutput struct {
	Result   domain.ActionResult `json:"result"`
	Guidance string              `json:"guidance" jsonschema:"Plain-language instruction for what to do about this state."`
}

type WaitForActionResultOutput struct {
	Result   domain.ActionResult `json:"result"`
	Changed  bool                `json:"changed" jsonschema:"The status changed during this wait."`
	TimedOut bool                `json:"timed_out" jsonschema:"The wait ended at its timeout with the status unchanged; you may wait again."`
	Guidance string              `json:"guidance" jsonschema:"Plain-language instruction for what to do about this state."`
}

// actionGuidance spells out, per status, what the state means and what to do.
var actionGuidance = map[domain.ActionRunStatus]string{
	domain.ActionRunPendingApproval: "Waiting for the user to approve it and choose the agent variant. Nothing has started. Call waitForActionResult to wait for the decision, or carry on and check getActionResult later.",
	domain.ActionRunDenied:          "The user denied this request; it never ran. Do not request it again without asking the user — read `reason` and ask what they want instead.",
	domain.ActionRunRunning:         "Approved and running. The Action agent is working; it ends in completed or failed. Call waitForActionResult again to keep waiting.",
	domain.ActionRunCompleted:       "Completed: the artifact package is in `output_dir`. Read `summary`, then the artifacts. Findings inside the package (for example failed checks) are results, not a failed execution.",
	domain.ActionRunFailed:          "Failed: the Action could not be started or delivered. Read `error` and `summary`. If it never started (no started_at), it may be requested again once the cause is resolved.",
}

func actionResultGuidance(result domain.ActionResult) (string, error) {
	guidance, ok := actionGuidance[result.Status]
	if !ok {
		return "", newInternalError(fmt.Sprintf("daemon returned unknown action status %q", result.Status))
	}
	return guidance, nil
}

func (s *Server) handleGetAvailableActions(
	ctx context.Context,
	_ *mcp.CallToolRequest,
	_ GetAvailableActionsInput,
) (*mcp.CallToolResult, GetAvailableActionsOutput, error) {
	var out domain.AvailableActionList
	if err := s.sessionRequest(ctx, http.MethodGet, "available-actions", nil, &out); err != nil {
		return nil, GetAvailableActionsOutput{}, err
	}
	if out.Actions == nil {
		out.Actions = []domain.ActionDefinition{}
	}
	for i := range out.Actions {
		if out.Actions[i].Problems == nil {
			out.Actions[i].Problems = []domain.ActionProblem{}
		}
	}
	return nil, GetAvailableActionsOutput{Actions: out.Actions}, nil
}

func (s *Server) handleExecuteAction(
	ctx context.Context,
	_ *mcp.CallToolRequest,
	input ExecuteActionInput,
) (*mcp.CallToolResult, ActionResultOutput, error) {
	if strings.TrimSpace(input.Name) == "" {
		return nil, ActionResultOutput{}, newValidationError("name", "is required")
	}
	if strings.TrimSpace(input.Prompt) == "" {
		return nil, ActionResultOutput{}, newValidationError("prompt", "is required")
	}
	var result domain.ActionResult
	if err := s.sessionRequest(ctx, http.MethodPost, "intents/execute-action", domain.ExecuteActionRequest{
		Name:   input.Name,
		Prompt: input.Prompt,
	}, &result); err != nil {
		return nil, ActionResultOutput{}, err
	}
	guidance, err := actionResultGuidance(result)
	if err != nil {
		return nil, ActionResultOutput{}, err
	}
	return nil, ActionResultOutput{Result: result, Guidance: guidance}, nil
}

func (s *Server) handleGetActionResult(
	ctx context.Context,
	_ *mcp.CallToolRequest,
	input ActionResultInput,
) (*mcp.CallToolResult, ActionResultOutput, error) {
	id := strings.TrimSpace(input.ExecutionID)
	if id == "" {
		return nil, ActionResultOutput{}, newValidationError("execution_id", "is required")
	}
	var result domain.ActionResult
	if err := s.sessionRequest(ctx, http.MethodGet, "action-results/"+url.PathEscape(id), nil, &result); err != nil {
		return nil, ActionResultOutput{}, err
	}
	guidance, err := actionResultGuidance(result)
	if err != nil {
		return nil, ActionResultOutput{}, err
	}
	return nil, ActionResultOutput{Result: result, Guidance: guidance}, nil
}

func (s *Server) handleWaitForActionResult(
	ctx context.Context,
	_ *mcp.CallToolRequest,
	input WaitForActionResultInput,
) (*mcp.CallToolResult, WaitForActionResultOutput, error) {
	id := strings.TrimSpace(input.ExecutionID)
	if id == "" {
		return nil, WaitForActionResultOutput{}, newValidationError("execution_id", "is required")
	}
	timeout := input.TimeoutSeconds
	if timeout == 0 {
		timeout = domain.MaxActionWaitSeconds
	}
	if timeout < 1 || timeout > domain.MaxActionWaitSeconds {
		return nil, WaitForActionResultOutput{}, newValidationError("timeout_seconds", fmt.Sprintf("must be between 1 and %d (omit it for %d); to wait longer, call waitForActionResult again", domain.MaxActionWaitSeconds, domain.MaxActionWaitSeconds))
	}
	var wait domain.ActionWaitResult
	path := fmt.Sprintf("action-results/%s/wait?timeout_seconds=%d", url.PathEscape(id), timeout)
	if err := s.sessionRequest(ctx, http.MethodGet, path, nil, &wait); err != nil {
		return nil, WaitForActionResultOutput{}, err
	}
	guidance, err := actionResultGuidance(wait.Result)
	if err != nil {
		return nil, WaitForActionResultOutput{}, err
	}
	if wait.TimedOut {
		guidance = "No change within the wait. " + guidance
	}
	return nil, WaitForActionResultOutput{Result: wait.Result, Changed: wait.Changed, TimedOut: wait.TimedOut, Guidance: guidance}, nil
}
