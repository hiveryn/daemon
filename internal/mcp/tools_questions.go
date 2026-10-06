package mcp

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/hiveryn/daemon/internal/domain"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// registerUserTools registers notify and askQuestion. Every session type gets
// them: architects, ticket workers and Action agents.
//
// Both return plain text, not a JSON envelope, because the text is the
// contract: "Notification sent to user", the user's answer verbatim, or the
// one-hour timeout instruction.
func (s *Server) registerUserTools() {
	mcp.AddTool(s.mcpServer, &mcp.Tool{
		Name: "notify",
		Description: fmt.Sprintf("Send a short notification to the user's phone and return at once with %q. Use it for important updates the user should see while away from the laptop (a milestone reached, work finished, something blocked). It does not wait for, or collect, any reply — use askQuestion when you need an answer. An error means the user was NOT notified (for example notifications are not configured); tell them in the conversation instead. shortMessage: 1-%d characters.",
			domain.NotificationSentText, domain.MaxNotifyMessageLength),
	}, s.handleNotify)

	mcp.AddTool(s.mcpServer, &mcp.Tool{
		Name: "askQuestion",
		Description: fmt.Sprintf("Ask the user a question you need answered before continuing. Their phone is alerted and the question appears in your session in the Hiveryn desktop, with your suggested answers and the recommended one marked; the user picks one or writes their own answer. This call waits up to 1 hour and returns only the answer text — which may be free text rather than one of your suggestions. If nobody answers within the hour it returns %q: then stop and wait for the user in the conversation; do not ask again or poll. Prefer this over any built-in question tool. question: 1-%d characters. answers: %d-%d distinct suggested answers, each 1-%d characters. recommendedIndex: the 0-based index of your recommended answer in answers (required; it is shown as a recommendation and is never chosen for the user). An error means the question was NOT asked.",
			domain.QuestionTimeoutText, domain.MaxQuestionLength, domain.MinQuestionAnswers, domain.MaxQuestionAnswers, domain.MaxQuestionAnswerLength),
	}, s.handleAskQuestion)
}

type NotifyInput struct {
	ShortMessage string `json:"shortMessage" jsonschema:"The notification text (required)."`
}

// AskQuestionInput leaves answers and recommendedIndex out of the schema's
// required list on purpose: the daemon validates them with a precise message,
// instead of a client-side schema rejection (and some MCP clients drop
// required arrays).
type AskQuestionInput struct {
	Question         string   `json:"question" jsonschema:"The question (required)."`
	Answers          []string `json:"answers,omitempty" jsonschema:"Suggested answers (required), distinct."`
	RecommendedIndex *int     `json:"recommendedIndex,omitempty" jsonschema:"0-based index of the recommended answer in answers (required)."`
}

func textResult(text string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}
}

func (s *Server) handleNotify(ctx context.Context, _ *mcp.CallToolRequest, input NotifyInput) (*mcp.CallToolResult, any, error) {
	var res domain.NotifyResult
	if err := s.sessionRequest(ctx, http.MethodPost, "notify", domain.NotifyRequest{Message: input.ShortMessage}, &res); err != nil {
		return nil, nil, err
	}
	if res.Text != domain.NotificationSentText {
		return nil, nil, newInternalError(fmt.Sprintf("daemon returned unexpected notify result %q", res.Text))
	}
	return textResult(res.Text), nil, nil
}

func (s *Server) handleAskQuestion(ctx context.Context, _ *mcp.CallToolRequest, input AskQuestionInput) (*mcp.CallToolResult, any, error) {
	var res domain.AskQuestionResult
	err := s.sessionRequest(ctx, http.MethodPost, "questions", domain.AskQuestionRequest{
		Question:         input.Question,
		Answers:          input.Answers,
		RecommendedIndex: input.RecommendedIndex,
	}, &res)
	if err != nil {
		var toolErr *ToolError
		if errors.As(err, &toolErr) || ctx.Err() != nil {
			return nil, nil, err
		}
		// The connection to the daemon broke while waiting (it stopped or
		// restarted). The question did not survive it.
		return nil, nil, newInternalError(fmt.Sprintf("lost the Hiveryn daemon while waiting for the user's answer (%v); the question is no longer pending. Stop here and wait for the user to get back.", err))
	}
	if res.Text == "" {
		return nil, nil, newInternalError(fmt.Sprintf("daemon returned question %s (%s) without a reply text", res.QuestionID, res.Status))
	}
	return textResult(res.Text), nil, nil
}
