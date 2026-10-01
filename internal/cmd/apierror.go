package cmd

// apierror.go is the one place an HTTP failure becomes an exit code. Every
// verb that calls chat-api routes its failures here: errors from Client.Do
// through failFromDo, non-2xx responses through failFromResponse. The mapping
// reads the HTTP status and the body's `code`, never the message text.
//
//	401                         -> 4  authentication failed
//	402 (any)                   -> 4  legacy paywall: server error, suggestion, account_url
//	403 PRO_REQUIRED            -> 4  with account_url (fallback client.AccountURL)
//	403 NO_INFERENCE_ON_CLI     -> 1  with suggestion
//	403 other                   -> 4
//	400, 422                    -> 2
//	404                         -> 3
//	429 (CLI_QUOTA or burst)    -> 7
//	5xx                         -> 5
//	anything else               -> 1
//
// In JSON mode the same failure is also written to stdout as
// {"error": <code>, "message", "exit_code", "suggestion"?, "account_url"?, "reset_date"?}.

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/mosaicss/archivist/internal/client"
	"github.com/spf13/cobra"
)

// failure is one mapped failure, ready to print.
type failure struct {
	exitCode   int
	code       string
	message    string
	suggestion string
	accountURL string
	resetDate  string
	// reported is true when the message already reached stderr.
	reported bool
}

// failFromDo maps an error from Client.Do, or from a helper that wraps one.
// An *client.APIError maps by status and code; an *client.ExitCodeError keeps
// its exit code; anything else is a network failure (exit 5).
func failFromDo(cmd *cobra.Command, err error, format string) error {
	var apiErr *client.APIError
	if errors.As(err, &apiErr) {
		return reportFailure(cmd, mapAPIError(apiErr), format)
	}
	var exitErr *client.ExitCodeError
	if errors.As(err, &exitErr) {
		code := exitErr.APICode
		if code == "" {
			code = defaultCodeForExit(exitErr.Code)
		}
		return reportFailure(cmd, failure{
			exitCode:   exitErr.Code,
			code:       code,
			message:    exitErr.Message,
			suggestion: exitErr.Suggestion,
			reported:   exitErr.Reported,
		}, format)
	}
	return reportFailure(cmd, failure{
		exitCode: ExitServerError,
		code:     "NETWORK_ERROR",
		message:  err.Error(),
	}, format)
}

// failFromResponse maps a non-2xx response. It reads (at most 64 KiB of) the
// body; the caller still closes it.
func failFromResponse(cmd *cobra.Command, resp *http.Response, format string) error {
	return reportFailure(cmd, mapAPIError(client.ParseAPIError(resp)), format)
}

// mapAPIError applies the status-then-code table above.
func mapAPIError(e *client.APIError) failure {
	f := failure{
		code:       e.Code,
		message:    e.Message,
		suggestion: e.Suggestion,
		accountURL: e.AccountURL,
		resetDate:  e.ResetDate,
	}
	switch {
	case e.Status == http.StatusUnauthorized:
		f.exitCode = ExitAuthError
		f.message = "authentication failed. Run 'archivist auth status'"
	case e.Status == http.StatusPaymentRequired:
		f.exitCode = ExitAuthError
		if f.accountURL == "" {
			f.accountURL = client.AccountURL
		}
	case e.Status == http.StatusForbidden:
		switch e.Code {
		case "NO_INFERENCE_ON_CLI":
			f.exitCode = ExitGenericError
		case "PRO_REQUIRED":
			f.exitCode = ExitAuthError
			if f.accountURL == "" {
				f.accountURL = client.AccountURL
			}
		default:
			f.exitCode = ExitAuthError
		}
	case e.Status == http.StatusBadRequest, e.Status == http.StatusUnprocessableEntity:
		f.exitCode = ExitUsageError
	case e.Status == http.StatusNotFound:
		f.exitCode = ExitNotFound
	case e.Status == http.StatusTooManyRequests:
		f.exitCode = ExitRateLimit
	case e.Status >= 500:
		f.exitCode = ExitServerError
	default:
		f.exitCode = ExitGenericError
	}
	if f.code == "" {
		f.code = defaultCodeForStatus(e.Status)
	}
	return f
}

// reportFailure prints f to stderr (and to stdout as JSON in JSON mode) and
// returns the typed exit.
func reportFailure(cmd *cobra.Command, f failure, format string) error {
	stderr := cmd.ErrOrStderr()
	if !f.reported {
		if f.code != "" {
			_, _ = fmt.Fprintf(stderr, "%s: %s [%s]\n", cmd.CommandPath(), f.message, f.code)
		} else {
			_, _ = fmt.Fprintf(stderr, "%s: %s\n", cmd.CommandPath(), f.message)
		}
		if f.resetDate != "" {
			_, _ = fmt.Fprintf(stderr, "Resets on %s.\n", f.resetDate)
		}
		if f.suggestion != "" {
			_, _ = fmt.Fprintln(stderr, f.suggestion)
		}
		if f.accountURL != "" {
			_, _ = fmt.Fprintf(stderr, "Account: %s\n", f.accountURL)
		}
	}
	if format == "json" {
		writeJSONError(cmd.OutOrStdout(), jsonError{
			Error:      f.code,
			Message:    f.message,
			ExitCode:   f.exitCode,
			Suggestion: f.suggestion,
			AccountURL: f.accountURL,
			ResetDate:  f.resetDate,
		})
	}
	return &ExitError{Code: f.exitCode}
}

func defaultCodeForStatus(status int) string {
	switch {
	case status == http.StatusBadRequest:
		return "BAD_REQUEST"
	case status == http.StatusUnauthorized:
		return "UNAUTHORIZED"
	case status == http.StatusPaymentRequired:
		return "PAYMENT_REQUIRED"
	case status == http.StatusForbidden:
		return "FORBIDDEN"
	case status == http.StatusNotFound:
		return "NOT_FOUND"
	case status == http.StatusUnprocessableEntity:
		return "UNPROCESSABLE"
	case status == http.StatusTooManyRequests:
		return "RATE_LIMITED"
	case status >= 500:
		return "SERVER_ERROR"
	default:
		return "HTTP_ERROR"
	}
}

func defaultCodeForExit(exit int) string {
	switch exit {
	case ExitRateLimit:
		return "RATE_LIMITED"
	case ExitServerError:
		return "SERVER_ERROR"
	default:
		return "ERROR"
	}
}
