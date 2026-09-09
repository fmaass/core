package database

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"net"
	"syscall"
)

// ErrUnavailable marks a failure of the database ITSELF — the process could
// not reach it, or lost the connection mid-statement — as opposed to a
// statement the database answered with an error (a constraint violation, a
// missing row, a syntax error).
//
// The distinction is not cosmetic: it decides whether a caller should tell a
// client "your request is wrong" or "come back later". The REST v1 bearer
// middleware reported a dead Postgres as 401 INVALID_TOKEN, which invites the
// operator to rotate a token that was never the problem (INFRA-328).
var ErrUnavailable = errors.New("database unavailable")

// IsUnavailable classifies err by TYPE — never by matching its text, which is
// driver- and locale-dependent and changes between releases.
//
// It reports true for: a wrapped ErrUnavailable; a transport failure of the
// connection (net.Error, *net.OpError, and the connect/reset syscall errnos
// they carry); a pool/driver connection that is gone (driver.ErrBadConn,
// sql.ErrConnDone, sql.ErrTxDone is deliberately NOT included — that is a
// caller bug); a deadline the statement did not meet; and an unexpected EOF,
// which is how a server that closed the socket mid-statement surfaces.
func IsUnavailable(err error) bool {
	if err == nil {
		return false
	}
	// A cancellation is the caller's own decision (a client that hung up
	// mid-dial arrives here wrapped in a *net.OpError), never the database's
	// state; it must not pause a scheduler or answer 503.
	if errors.Is(err, context.Canceled) {
		return false
	}
	if errors.Is(err, ErrUnavailable) {
		return true
	}
	if errors.Is(err, driver.ErrBadConn) || errors.Is(err, sql.ErrConnDone) {
		return true
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
		return true
	}
	if errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.ECONNABORTED) || errors.Is(err, syscall.EHOSTUNREACH) ||
		errors.Is(err, syscall.ENETUNREACH) || errors.Is(err, syscall.ENETDOWN) ||
		errors.Is(err, syscall.EPIPE) {
		return true
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	return false
}

// MarkUnavailable returns err wrapped so IsUnavailable and errors.Is report it
// as an outage, and returns it untouched when it is not one. Use it at the
// point a statement fails, where the driver error is still intact: once an
// error has been reduced to a string, nothing downstream can classify it.
func MarkUnavailable(err error) error {
	if err == nil || !IsUnavailable(err) {
		return err
	}
	if errors.Is(err, ErrUnavailable) {
		return err
	}
	return &unavailableError{err: err}
}

type unavailableError struct{ err error }

func (e *unavailableError) Error() string { return ErrUnavailable.Error() + ": " + e.err.Error() }
func (e *unavailableError) Unwrap() error { return e.err }
func (e *unavailableError) Is(target error) bool {
	return target == ErrUnavailable
}
