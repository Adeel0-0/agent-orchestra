// Package app is the local Orchestra app server: a SQLite session journal, a session
// manager that owns agent turns independently of HTTP requests, and the loopback-only
// HTTP/WebSocket API used by the React UI. It is a port of orchestra/app/*.py and
// shares the Python server's data directory, database schema and API.
package app

import (
	"errors"
	"fmt"
)

// RequestError is an explicit client mistake (HTTP 400).
type RequestError struct{ Msg string }

func (e *RequestError) Error() string { return e.Msg }

// NotFoundError is an unknown project or session (HTTP 404).
type NotFoundError struct{ Msg string }

func (e *NotFoundError) Error() string { return e.Msg }

// ConflictError is a conflicting turn or a recovery step that is still required (HTTP 409).
type ConflictError struct{ Msg string }

func (e *ConflictError) Error() string { return e.Msg }

// BodyTooLargeError is an oversized request body (HTTP 413).
type BodyTooLargeError struct{ Limit int }

func (e *BodyTooLargeError) Error() string {
	return fmt.Sprintf("Request body exceeds %d bytes", e.Limit)
}

// ErrSlowSubscriber ends a subscription that fell behind; the client replays from its last seq.
var ErrSlowSubscriber = errors.New("Subscriber fell behind; reconnect with last received seq")

// ErrServerOwned means another Orchestra server (Go or Python) holds the data directory.
var ErrServerOwned = errors.New("Another Orchestra server owns this data directory.")

func requestErr(msg string) error  { return &RequestError{Msg: msg} }
func notFound(msg string) error    { return &NotFoundError{Msg: msg} }
func conflictErr(msg string) error { return &ConflictError{Msg: msg} }

// statusFor maps explicit domain errors to HTTP status codes. Anything else is an
// implementation defect and stays a 500, as in the Python server.
func statusFor(err error) (int, bool) {
	var tooLarge *BodyTooLargeError
	var nf *NotFoundError
	var conflict *ConflictError
	var req *RequestError
	switch {
	case errors.As(err, &tooLarge):
		return 413, true
	case errors.As(err, &nf):
		return 404, true
	case errors.As(err, &conflict):
		return 409, true
	case errors.As(err, &req):
		return 400, true
	}
	return 500, false
}
