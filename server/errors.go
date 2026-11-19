package server

import (
	"errors"
	"fmt"
	"net/http"
)

// Code classifies an API error. Codes are part of the API contract: the
// HTTP API returns them in the "code" field of an error body, and package
// rpc maps them to gRPC status codes. New codes may be added; existing ones
// keep their meaning.
type Code string

const (
	CodeInvalidArgument  Code = "invalid_argument"  // the request is malformed; do not retry it unchanged
	CodeUnauthenticated  Code = "unauthenticated"   // missing or invalid API key
	CodePermissionDenied Code = "permission_denied" // the key lacks a scope
	CodeNotFound         Code = "not_found"         // no such persona or job (or the key cannot see it)
	CodeUnavailable      Code = "unavailable"       // not configured here, or temporarily down; retry later
	CodeUnimplemented    Code = "unimplemented"     // this deployment cannot do it (streaming on a buffered host)
	CodeUpstream         Code = "upstream"          // the chat model failed
	CodeInternal         Code = "internal"
)

// HTTPStatus is the HTTP status the API answers with for c.
func (c Code) HTTPStatus() int {
	switch c {
	case CodeInvalidArgument:
		return http.StatusBadRequest
	case CodeUnauthenticated:
		return http.StatusUnauthorized
	case CodePermissionDenied:
		return http.StatusForbidden
	case CodeNotFound:
		return http.StatusNotFound
	case CodeUnavailable:
		return http.StatusServiceUnavailable
	case CodeUnimplemented:
		return http.StatusNotImplemented
	case CodeUpstream:
		return http.StatusBadGateway
	}
	return http.StatusInternalServerError
}

// Error is an API error: what the operations of Server return, whatever
// the transport.
type Error struct {
	Code    Code
	Message string
	Err     error // the cause, if any (not sent to clients unless it is the message)
}

func (e *Error) Error() string { return e.Message }
func (e *Error) Unwrap() error { return e.Err }

func newError(code Code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

func wrapError(code Code, err error) *Error {
	return &Error{Code: code, Message: err.Error(), Err: err}
}

// AsError returns err as an *Error; anything else is an internal error.
func AsError(err error) *Error {
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	return wrapError(CodeInternal, err)
}
