package protocol

import (
	"errors"
	"fmt"
)

var (
	ErrInvalidRequest    = errors.New("invalid request")
	ErrUnauthorized      = errors.New("unauthorized")
	ErrNotFound          = errors.New("resource not found")
	ErrConflict          = errors.New("resource conflict")
	ErrInternalError     = errors.New("internal server error")
	ErrWorkerUnavailable = errors.New("no worker available")
	ErrJobFailed         = errors.New("job failed")
	ErrUploadFailed      = errors.New("upload failed")
	ErrDownloadFailed    = errors.New("download failed")
	ErrInvalidJobStatus  = errors.New("invalid job status")
	ErrInvalidFileID     = errors.New("invalid file id")
	ErrWorkerNotFound    = errors.New("worker not found")
	ErrJobNotFound       = errors.New("job not found")
	ErrJobTerminal       = errors.New("job already in terminal state")
	ErrRateLimitExceeded = errors.New("rate limit exceeded")
)

type ProtocolError struct {
	Code    ErrorCode
	Message string
	Detail  string
	Err     error
}

func (e *ProtocolError) Error() string {
	if e.Detail != "" {
		return fmt.Sprintf("%s: %s (%s)", e.Code, e.Message, e.Detail)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func (e *ProtocolError) Unwrap() error {
	return e.Err
}

func NewProtocolError(code ErrorCode, message string, err error) *ProtocolError {
	return &ProtocolError{
		Code:    code,
		Message: message,
		Err:     err,
	}
}

func NewProtocolErrorWithDetail(code ErrorCode, message, detail string, err error) *ProtocolError {
	return &ProtocolError{
		Code:    code,
		Message: message,
		Detail:  detail,
		Err:     err,
	}
}

func (e *ProtocolError) ToResponse() ErrorResponse {
	return ErrorResponse{
		Code:    e.Code,
		Message: e.Message,
		Detail:  e.Detail,
	}
}
