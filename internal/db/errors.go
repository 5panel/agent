// Errors with a protocol code. An engine returns one of these when the
// request is well-formed but cannot run as asked; anything else it returns
// is reported to the gateway as code "db" with the database's own text.
package db

import (
	"errors"
	"fmt"

	"github.com/5panel/agent/internal/protocol"
)

// Error carries a protocol error code.
type Error struct {
	Code    string
	Message string
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

// Invalid builds an "invalid" error.
func Invalid(format string, args ...any) error {
	return &Error{Code: protocol.ErrInvalid, Message: fmt.Sprintf(format, args...)}
}

// Unsupported builds an "unsupported" error.
func Unsupported(format string, args ...any) error {
	return &Error{Code: protocol.ErrUnsupported, Message: fmt.Sprintf(format, args...)}
}

// Refused builds a "refused" error.
func Refused(format string, args ...any) error {
	return &Error{Code: protocol.ErrRefused, Message: fmt.Sprintf(format, args...)}
}

// CodeOf returns the protocol code of err, or "" when it is not an *Error.
func CodeOf(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}
