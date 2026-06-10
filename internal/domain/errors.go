package domain

import sd "github.com/hiveryn/shared/domain"

type ErrorCode = sd.ErrorCode

const (
	ErrCodeValidation = sd.ErrCodeValidation
	ErrCodeConflict   = sd.ErrCodeConflict
	ErrCodeNotFound   = sd.ErrCodeNotFound
	ErrCodeInternal   = sd.ErrCodeInternal
)

var ErrNotFound = sd.ErrNotFound

type ValidationError = sd.ValidationError

type ConflictError = sd.ConflictError

type NotFoundError = sd.NotFoundError

type InternalError = sd.InternalError
