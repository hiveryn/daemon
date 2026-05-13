package mcp

import "fmt"

type ErrorCode string

const (
	ErrorCodeNotFound      ErrorCode = "NOT_FOUND"
	ErrorCodeValidation    ErrorCode = "VALIDATION_ERROR"
	ErrorCodeInternal      ErrorCode = "INTERNAL_ERROR"
	ErrorCodeStateConflict ErrorCode = "STATE_CONFLICT"
)

type ToolError struct {
	Code    ErrorCode `json:"code"`
	Message string    `json:"message"`
}

func (e *ToolError) Error() string {
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func newValidationError(field, message string) *ToolError {
	return &ToolError{
		Code:    ErrorCodeValidation,
		Message: fmt.Sprintf("validation error for %s: %s", field, message),
	}
}

func newInternalError(message string) *ToolError {
	return &ToolError{Code: ErrorCodeInternal, Message: message}
}

func newStateConflictError(message string) *ToolError {
	return &ToolError{Code: ErrorCodeStateConflict, Message: message}
}
