// Package httpx provides consistent JSON errors and request helpers.
package httpx

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/labstack/echo/v4"
)

// Error is the single error shape returned by the API:
//
//	{"error": {"code": "validation_failed", "message": "...", "details": {...}}}
type Error struct {
	Status  int    `json:"-"`
	Code    string `json:"code"`
	Message string `json:"message"`
	Details any    `json:"details,omitempty"`
}

func (e *Error) Error() string { return fmt.Sprintf("%s: %s", e.Code, e.Message) }

func NewError(status int, code, msg string) *Error {
	return &Error{Status: status, Code: code, Message: msg}
}

func (e *Error) With(details any) *Error {
	c := *e
	c.Details = details
	return &c
}

var (
	ErrUnauthorized = NewError(http.StatusUnauthorized, "unauthorized", "Sign in to continue.")
	ErrForbidden    = NewError(http.StatusForbidden, "forbidden", "You do not have permission to do this.")
	ErrNotFound     = NewError(http.StatusNotFound, "not_found", "The record was not found.")
	ErrStaleVersion = NewError(http.StatusConflict, "stale_version",
		"This record was changed by someone else. Reload it and try again.")
)

// FieldErrors collects validation messages keyed by field name.
type FieldErrors map[string]string

func (f FieldErrors) Add(field, msg string) {
	if _, exists := f[field]; !exists {
		f[field] = msg
	}
}

// Err returns a validation error, or nil when there are no field errors.
func (f FieldErrors) Err() error {
	if len(f) == 0 {
		return nil
	}
	return NewError(http.StatusUnprocessableEntity, "validation_failed",
		"Some fields need attention.").With(f)
}

// ErrorHandler converts any returned error into the standard JSON shape.
func ErrorHandler(log *slog.Logger) echo.HTTPErrorHandler {
	return func(err error, c echo.Context) {
		if c.Response().Committed {
			return
		}
		var appErr *Error
		var echoErr *echo.HTTPError
		switch {
		case errors.As(err, &appErr):
		case errors.As(err, &echoErr):
			appErr = NewError(echoErr.Code, http.StatusText(echoErr.Code), fmt.Sprint(echoErr.Message))
		default:
			log.Error("unhandled error", "err", err, "path", c.Path(), "request_id", RequestID(c))
			appErr = NewError(http.StatusInternalServerError, "internal_error",
				"Something went wrong. The error has been logged.")
		}
		_ = c.JSON(appErr.Status, map[string]any{"error": appErr})
	}
}

func RequestID(c echo.Context) string {
	return c.Response().Header().Get(echo.HeaderXRequestID)
}

// Page holds limit/offset pagination parsed from the query string.
type Page struct {
	Limit  int
	Offset int
}

func ParsePage(c echo.Context) Page {
	p := Page{Limit: 50}
	if v, err := strconv.Atoi(c.QueryParam("limit")); err == nil && v > 0 {
		p.Limit = min(v, 200)
	}
	if v, err := strconv.Atoi(c.QueryParam("offset")); err == nil && v > 0 {
		p.Offset = v
	}
	return p
}

// List is the standard envelope for collection responses.
type List[T any] struct {
	Items  []T `json:"items"`
	Total  int `json:"total"`
	Limit  int `json:"limit"`
	Offset int `json:"offset"`
}

// Bind decodes the JSON body and returns a 400 on malformed input.
func Bind(c echo.Context, dst any) error {
	if err := (&echo.DefaultBinder{}).BindBody(c, dst); err != nil {
		return NewError(http.StatusBadRequest, "bad_request", "The request body is not valid JSON.")
	}
	return nil
}
