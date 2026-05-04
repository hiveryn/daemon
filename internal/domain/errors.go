package domain

import (
	"errors"
	"fmt"
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
