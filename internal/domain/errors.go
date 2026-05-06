package domain

import (
	"errors"
	"fmt"
)

type ErrorCode string

const (
	ErrCodeValidation ErrorCode = "VALIDATION"
	ErrCodeConflict   ErrorCode = "CONFLICT"
	ErrCodeNotFound   ErrorCode = "NOT_FOUND"
	ErrCodeInternal   ErrorCode = "INTERNAL"
)

var ErrNotFound = errors.New("resource not found")

type ValidationError struct {
	Field   string
	Message string
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("%s %s", e.Field, e.Message)
}

type ConflictError struct {
	Resource string
	Field    string
	Message  string
}

func (e *ConflictError) Error() string {
	return fmt.Sprintf("%s %s: %s", e.Resource, e.Field, e.Message)
}

type NotFoundError struct {
	Resource string
	ID       string
}

func (e *NotFoundError) Error() string {
	return fmt.Sprintf("%s %s not found", e.Resource, e.ID)
}

func (e *NotFoundError) Unwrap() error {
	return ErrNotFound
}
