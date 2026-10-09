package mcp

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/hiveryn/daemon/internal/domain"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// registerSpawnTicketWorkerTool is architect-only: workers and Action agents
// never launch workers.
func (s *Server) registerSpawnTicketWorkerTool() {
	mcp.AddTool(s.mcpServer, &mcp.Tool{
		Name:        "spawnTicketWorker",
		Description: "Request a worker session for one of this project's backlog tickets, run by an explicit agent variant with an explicit workflow selection. variant is required and has no default: if the user has not said which variant to use, ask them first — the variant must be configured for the machine the ticket's repositories are on (local or a named machine), and an omitted, unknown or other-machine variant is rejected naming that machine and its eligible variants. workflows names files in this workspace's workflows/ directory as NAME or NAME.md (for example [\"AUTONOMOUS_COMMIT\", \"ISOLATED_VERIFICATION\"]); the list is the whole selection — nothing is added from repository suggestions, an empty or omitted list selects none, and paths are refused. Unknown names are rejected with the available workflows. The ticket, variant, workflows and the worker context are validated before the user is asked. The call waits for the user's approval like createWorkTicket and auto-approves if they do not answer in time. Check outcome: approved or auto_approved means the worker was launched and is running (worker gives its session_id and the recorded workflows); denied_by_user means nothing was launched; error means it could not launch (reason says why). The call never waits for the worker to finish; the worker concludes its own ticket. A retry of the same request returns the original outcome instead of launching another worker, and a ticket with a running worker cannot get a second one.",
	}, s.handleSpawnTicketWorker)
}

// Variant and Workflows are omitempty on purpose: a missing variant must reach
// the daemon, which answers with the configured variants, and an omitted
// workflow list is a valid empty selection.
type SpawnTicketWorkerInput struct {
	TicketID  string   `json:"ticketId" jsonschema:"The backlog ticket to launch a worker for (required)."`
	Variant   string   `json:"variant,omitempty" jsonschema:"The configured agent variant that runs the worker (required, no default). It must be configured for the ticket's execution machine. Ask the user which variant to use if they have not said."`
	Workflows []string `json:"workflows,omitempty" jsonschema:"Workflow names from this workspace's workflows/ directory, as NAME or NAME.md. The whole selection; empty selects none."`
}

type SpawnTicketWorkerOutput struct {
	IntentEnvelopeFields
	Worker *domain.SpawnedTicketWorker `json:"worker,omitempty" jsonschema:"The launched worker session: present only when outcome is approved or auto_approved."`
}

func (s *Server) handleSpawnTicketWorker(
	ctx context.Context,
	_ *mcp.CallToolRequest,
	input SpawnTicketWorkerInput,
) (*mcp.CallToolResult, SpawnTicketWorkerOutput, error) {
	if strings.TrimSpace(input.TicketID) == "" {
		return nil, SpawnTicketWorkerOutput{}, newValidationError("ticketId", "is required")
	}
	var res domain.SpawnTicketWorkerResponse
	if err := s.sessionRequest(ctx, http.MethodPost, "intents/spawn-ticket-worker", domain.SpawnTicketWorkerRequest{
		TicketID:  input.TicketID,
		Variant:   input.Variant,
		Workflows: input.Workflows,
	}, &res); err != nil {
		return nil, SpawnTicketWorkerOutput{}, err
	}
	guidance, ok := spawnTicketWorkerGuidance[res.Outcome]
	if !ok {
		return nil, SpawnTicketWorkerOutput{}, newInternalError(fmt.Sprintf("daemon returned unknown intent outcome %q", res.Outcome))
	}
	if res.Outcome.Approved() && res.Worker == nil {
		return nil, SpawnTicketWorkerOutput{}, newInternalError("daemon reported the worker launched without its session")
	}
	return nil, SpawnTicketWorkerOutput{
		IntentEnvelopeFields: IntentEnvelopeFields{
			Outcome:  string(res.Outcome),
			Guidance: guidance,
			Reason:   res.Reason,
			IntentID: res.IntentID,
		},
		Worker: res.Worker,
	}, nil
}

// spawnTicketWorkerGuidance is the per-outcome instruction. Approval means
// launched and running, never finished.
var spawnTicketWorkerGuidance = map[domain.IntentOutcome]string{
	domain.IntentOutcomeApproved:     "The user approved the request and the worker was launched; it is running and will conclude its own ticket. worker.session_id identifies it. Do not wait for it here.",
	domain.IntentOutcomeAutoApproved: "The user did not answer within the approval window, so the request was auto-approved and the worker was launched; it is running and will conclude its own ticket. worker.session_id identifies it. Do not wait for it here.",
	domain.IntentOutcomeDeniedByUser: "The user denied this request; no worker was launched. Do not request it again without asking the user — read `reason` and ask what they want instead.",
	domain.IntentOutcomeAutoDenied:   "The request was denied without a user response; no worker was launched. Do not request it again without asking the user.",
	domain.IntentOutcomeError:        "No worker is running for this request: the launch failed after approval, or the request ended unresolved. Read `reason`. It may be requested again once the cause is resolved.",
}
