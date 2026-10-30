package mcp

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/hiveryn/daemon/internal/domain"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Companion tools (shared domain/companion.go). Identity and ownership are
// the daemon's: every call is addressed by this server's session, which the
// daemon resolves to the companion or architect it is. Messages return once
// durably queued ("accepted"); they never wait for a reply.

const (
	// Error codes of companion tool failures that are not daemon domain
	// errors.
	ErrorCodeActionDeliveryFailed ErrorCode = "ACTION_REQUEST_DELIVERY_FAILED"
	ErrorCodeMessageNotQueued     ErrorCode = "MESSAGE_NOT_QUEUED"
)

// registerCompanionTools registers the companion's four tools.
func (s *Server) registerCompanionTools() {
	mcp.AddTool(s.mcpServer, &mcp.Tool{
		Name:        "sendMessageToArchitect",
		Description: "Send a message to your owning architect: findings, completed results or a material blocker. The architect is derived from your session; there is no recipient parameter. Returns message_id and status accepted once Hiveryn has durably queued the message — it does not wait for a reply; replies arrive later as new messages carrying their own message_id. A tool error means the message was NOT queued. message: required, non-empty text.",
	}, s.handleSendMessageToArchitect)
	mcp.AddTool(s.mcpServer, &mcp.Tool{
		Name:        "replyToArchitectMessage",
		Description: "Reply to a specific message from your architect. message_id: the id of an architect message you received (each one names it). message: required, non-empty text. Returns message_id (the queued reply), in_reply_to and status accepted once durably queued; further messages arrive asynchronously. An unknown message_id or one that was not sent to you, and any queueing failure, is a tool error and nothing was queued.",
	}, s.handleReplyToArchitectMessage)
	mcp.AddTool(s.mcpServer, &mcp.Tool{
		Name:        "requestAction",
		Description: "Ask the user to authorize one concrete external action (changes outside your repository: other repositories, pushes, emails, service updates). title: concise label for the phone notification and dialog. description: Markdown proposal — what will change, where, and the exact content or bounded scope to approve; Hiveryn shows it and executes nothing. The user's phone is notified and the call waits up to one hour for the dialog decision: {\"outcome\":\"accepted\"} authorizes only the submitted proposal (executing and verifying it remains yours); {\"outcome\":\"denied\"} and {\"outcome\":\"replied\",\"message\":...} authorize nothing — a revised proposal needs a new request. It goes directly to the user, never to the architect, and is never accepted automatically. Failures are tool errors and authorize nothing: " + domain.ActionRequestTimeoutCode + " when nobody decided within one hour (call requestAction again to submit another request), " + domain.ActionRequestCancelledCode + " when the request was withdrawn (for example because your shutdown was requested), and delivery or notification failures.",
	}, s.handleRequestAction)
	mcp.AddTool(s.mcpServer, &mcp.Tool{
		Name:        "completeCompanionShutdown",
		Description: "Complete the shutdown your architect requested. request_id: the UUID from the architect's shutdown message — no other id can end your session. handoff: required Markdown covering completed work, preserved artifacts, unresolved matters and any operation still in progress or with an uncertain outcome. Returns request_id and status accepted once the handoff is recorded; Hiveryn then stops your session and delivers the handoff and the actual shutdown outcome to the architect. Repeating the call with the same request_id and handoff returns the recorded acceptance; a different handoff for a completed request is an error. A missing or foreign request_id is a tool error.",
	}, s.handleCompleteCompanionShutdown)
}

// registerArchitectCompanionTools registers the architect's companion tools.
func (s *Server) registerArchitectCompanionTools() {
	mcp.AddTool(s.mcpServer, &mcp.Tool{
		Name:        "sendMessageToCompanion",
		Description: "Send an assignment or message to a named companion belonging to this architect session. companion: the configured companion name. message: required, non-empty text. Returns message_id and status accepted once Hiveryn has durably queued it for the companion; it does not wait for a reply — replies arrive later as new messages. A companion that failed to start, is shutting down or has stopped accepts no new assignments; that, an unknown name and any queueing failure are tool errors and nothing was queued.",
	}, s.handleSendMessageToCompanion)
	mcp.AddTool(s.mcpServer, &mcp.Tool{
		Name:        "replyToCompanionMessage",
		Description: "Respond to a specific message from one of this session's companions. message_id: the id named in the companion's message; the reply goes to the companion that sent it. message: required, non-empty text. Returns message_id (the queued reply), in_reply_to and status accepted once durably queued. An unknown message_id or one not sent to this session, and any queueing failure, is a tool error and nothing was queued.",
	}, s.handleReplyToCompanionMessage)
	mcp.AddTool(s.mcpServer, &mcp.Tool{
		Name:        "requestCompanionShutdown",
		Description: "Request shutdown of a named companion of this architect session. companion: its configured name. request_id: a UUID you generate for this request (distinct per companion). Returns request_id and status accepted once Hiveryn has recorded the request and queued the shutdown instruction, which carries the request_id; the companion is then shutting down and accepts no new assignments, and its pending action requests are withdrawn. It does not wait: Hiveryn later sends the companion's handoff and the actual shutdown outcome, and acceptance is not completion. Repeating the same request_id for the same companion returns the recorded acceptance; reusing it for another companion is an error; a different request_id for a companion already shutting down is a conflict.",
	}, s.handleRequestCompanionShutdown)
}

type CompanionMessageInput struct {
	Message string `json:"message" jsonschema:"The message text (required)."`
}

type CompanionReplyInput struct {
	MessageID string `json:"message_id" jsonschema:"ID of the message being answered (required)."`
	Message   string `json:"message" jsonschema:"Reply text (required)."`
}

type SendMessageToCompanionInput struct {
	Companion string `json:"companion" jsonschema:"The companion's configured name (required)."`
	Message   string `json:"message" jsonschema:"The assignment or message (required)."`
}

type RequestActionInput struct {
	Title       string `json:"title" jsonschema:"Concise label for the notification and dialog (required)."`
	Description string `json:"description" jsonschema:"Markdown proposal: what changes, where, and the exact content or bounded scope to approve (required)."`
}

type RequestCompanionShutdownInput struct {
	Companion string `json:"companion" jsonschema:"The companion's configured name (required)."`
	RequestID string `json:"request_id" jsonschema:"Architect-generated UUID for this shutdown request (required)."`
}

type CompleteCompanionShutdownInput struct {
	RequestID string `json:"request_id" jsonschema:"ID supplied in the architect's shutdown message (required)."`
	Handoff   string `json:"handoff" jsonschema:"Markdown handoff: completed work, preserved artifacts, unresolved matters, operations in progress or with an uncertain outcome (required)."`
}

type CompanionMessageOutput struct {
	MessageID string `json:"message_id"`
	InReplyTo string `json:"in_reply_to,omitempty"`
	Status    string `json:"status"`
}

type RequestActionOutput struct {
	Outcome string `json:"outcome"`
	Message string `json:"message,omitempty"`
}

type CompanionShutdownOutput struct {
	RequestID string `json:"request_id"`
	Status    string `json:"status"`
}

func (s *Server) handleSendMessageToArchitect(ctx context.Context, _ *mcp.CallToolRequest, input CompanionMessageInput) (*mcp.CallToolResult, CompanionMessageOutput, error) {
	if strings.TrimSpace(input.Message) == "" {
		return nil, CompanionMessageOutput{}, newValidationError("message", "is required")
	}
	return s.queueMessage(ctx, "companion/messages", domain.CompanionMessageRequest{Message: input.Message})
}

func (s *Server) handleReplyToArchitectMessage(ctx context.Context, _ *mcp.CallToolRequest, input CompanionReplyInput) (*mcp.CallToolResult, CompanionMessageOutput, error) {
	if strings.TrimSpace(input.MessageID) == "" {
		return nil, CompanionMessageOutput{}, newValidationError("message_id", "is required")
	}
	if strings.TrimSpace(input.Message) == "" {
		return nil, CompanionMessageOutput{}, newValidationError("message", "is required")
	}
	return s.queueMessage(ctx, "companion/replies", domain.CompanionReplyRequest{MessageID: input.MessageID, Message: input.Message})
}

func (s *Server) handleSendMessageToCompanion(ctx context.Context, _ *mcp.CallToolRequest, input SendMessageToCompanionInput) (*mcp.CallToolResult, CompanionMessageOutput, error) {
	if strings.TrimSpace(input.Companion) == "" {
		return nil, CompanionMessageOutput{}, newValidationError("companion", "is required")
	}
	if strings.TrimSpace(input.Message) == "" {
		return nil, CompanionMessageOutput{}, newValidationError("message", "is required")
	}
	return s.queueMessage(ctx, "companions/"+url.PathEscape(strings.TrimSpace(input.Companion))+"/messages", domain.CompanionMessageRequest{Message: input.Message})
}

func (s *Server) handleReplyToCompanionMessage(ctx context.Context, _ *mcp.CallToolRequest, input CompanionReplyInput) (*mcp.CallToolResult, CompanionMessageOutput, error) {
	if strings.TrimSpace(input.MessageID) == "" {
		return nil, CompanionMessageOutput{}, newValidationError("message_id", "is required")
	}
	if strings.TrimSpace(input.Message) == "" {
		return nil, CompanionMessageOutput{}, newValidationError("message", "is required")
	}
	return s.queueMessage(ctx, "companions/replies", domain.CompanionReplyRequest{MessageID: input.MessageID, Message: input.Message})
}

// queueMessage posts one message and reports its acceptance. Any failure —
// including a broken connection, whose outcome the daemon may have recorded —
// is a tool error saying the message is not confirmed queued.
func (s *Server) queueMessage(ctx context.Context, subPath string, body any) (*mcp.CallToolResult, CompanionMessageOutput, error) {
	var res domain.CompanionMessageResult
	if err := s.sessionRequest(ctx, http.MethodPost, subPath, body, &res); err != nil {
		var toolErr *ToolError
		if errors.As(err, &toolErr) {
			return nil, CompanionMessageOutput{}, err
		}
		return nil, CompanionMessageOutput{}, &ToolError{Code: ErrorCodeMessageNotQueued, Message: fmt.Sprintf("the message could not be confirmed as queued: Hiveryn was unreachable or the connection broke (%v). Treat it as not sent", err)}
	}
	if res.Status != domain.CompanionAccepted || res.MessageID == "" {
		return nil, CompanionMessageOutput{}, newInternalError(fmt.Sprintf("daemon returned unexpected message result %+v", res))
	}
	return nil, CompanionMessageOutput{MessageID: res.MessageID, InReplyTo: res.InReplyTo, Status: res.Status}, nil
}

func (s *Server) handleRequestAction(ctx context.Context, _ *mcp.CallToolRequest, input RequestActionInput) (*mcp.CallToolResult, RequestActionOutput, error) {
	if strings.TrimSpace(input.Title) == "" {
		return nil, RequestActionOutput{}, newValidationError("title", "is required")
	}
	if strings.TrimSpace(input.Description) == "" {
		return nil, RequestActionOutput{}, newValidationError("description", "is required")
	}
	var res domain.RequestActionResult
	err := s.sessionRequest(ctx, http.MethodPost, "actions", domain.RequestActionRequest{Title: input.Title, Description: input.Description}, &res)
	if err != nil {
		var toolErr *ToolError
		if errors.As(err, &toolErr) {
			return nil, RequestActionOutput{}, err
		}
		return nil, RequestActionOutput{}, &ToolError{Code: ErrorCodeActionDeliveryFailed, Message: fmt.Sprintf("the request did not reach Hiveryn or the connection broke while waiting for the decision (%v). No action is authorized; the request is no longer pending. Submit a new request with requestAction once Hiveryn is reachable", err)}
	}
	if res.ErrorCode != "" {
		return nil, RequestActionOutput{}, &ToolError{Code: ErrorCode(res.ErrorCode), Message: res.Error}
	}
	switch domain.ActionRequestStatus(res.Outcome) {
	case domain.ActionAccepted, domain.ActionDenied:
		return nil, RequestActionOutput{Outcome: res.Outcome}, nil
	case domain.ActionReplied:
		return nil, RequestActionOutput{Outcome: res.Outcome, Message: res.Message}, nil
	default:
		return nil, RequestActionOutput{}, newInternalError(fmt.Sprintf("daemon returned unknown requestAction outcome %q; no action is authorized", res.Outcome))
	}
}

func (s *Server) handleRequestCompanionShutdown(ctx context.Context, _ *mcp.CallToolRequest, input RequestCompanionShutdownInput) (*mcp.CallToolResult, CompanionShutdownOutput, error) {
	if strings.TrimSpace(input.Companion) == "" {
		return nil, CompanionShutdownOutput{}, newValidationError("companion", "is required")
	}
	return s.shutdownCall(ctx, "companions/shutdown", domain.CompanionShutdownRequest{Companion: input.Companion, RequestID: input.RequestID})
}

func (s *Server) handleCompleteCompanionShutdown(ctx context.Context, _ *mcp.CallToolRequest, input CompleteCompanionShutdownInput) (*mcp.CallToolResult, CompanionShutdownOutput, error) {
	if strings.TrimSpace(input.Handoff) == "" {
		return nil, CompanionShutdownOutput{}, newValidationError("handoff", "is required")
	}
	result, out, err := s.shutdownCall(ctx, "companion/shutdown-completion", domain.CompleteCompanionShutdownRequest{RequestID: input.RequestID, Handoff: input.Handoff})
	if err == nil {
		if rec, ok := ctx.Value(shutdownCompletionKey{}).(*ShutdownCompletion); ok {
			rec.accept(out.RequestID)
		}
	}
	return result, out, err
}

// ShutdownCompletion records, for one HTTP request to the MCP handler, the
// request_id of a completeCompanionShutdown the daemon accepted while serving
// it. The remote gateway releases a companion's stop only once that request's
// response — carrying exactly this acceptance — has been written.
type ShutdownCompletion struct {
	mu        sync.Mutex
	requestID string
}

type shutdownCompletionKey struct{}

// WithShutdownCompletion returns a context whose completeCompanionShutdown
// acceptance is recorded in the returned ShutdownCompletion. The streamable
// HTTP handler passes the request's context to tool handlers.
func WithShutdownCompletion(ctx context.Context) (context.Context, *ShutdownCompletion) {
	rec := &ShutdownCompletion{}
	return context.WithValue(ctx, shutdownCompletionKey{}, rec), rec
}

func (c *ShutdownCompletion) accept(requestID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.requestID = requestID
}

// Accepted is the request_id of the accepted completion, "" if none.
func (c *ShutdownCompletion) Accepted() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.requestID
}

func (s *Server) shutdownCall(ctx context.Context, subPath string, body any) (*mcp.CallToolResult, CompanionShutdownOutput, error) {
	var res domain.CompanionShutdownResult
	if err := s.sessionRequest(ctx, http.MethodPost, subPath, body, &res); err != nil {
		return nil, CompanionShutdownOutput{}, err
	}
	if res.Status != domain.CompanionAccepted || res.RequestID == "" {
		return nil, CompanionShutdownOutput{}, newInternalError(fmt.Sprintf("daemon returned unexpected shutdown result %+v", res))
	}
	return nil, CompanionShutdownOutput{RequestID: res.RequestID, Status: res.Status}, nil
}
