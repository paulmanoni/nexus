package oauth2

import (
	stderrors "errors"
	"net/http"

	"github.com/go-oauth2/oauth2/v4/errors"
)

// Sentinel errors callers can return from PasswordAuthenticator to
// get the standard, user-friendly OAuth2 response without writing
// their own ErrorMapper. Domain code stays free of OAuth2 types —
// just return one of these and the mapper does the translation.
var (
	ErrInvalidCredentials = stderrors.New("oauth2: invalid credentials")
	ErrAccountDisabled    = stderrors.New("oauth2: account disabled")
	ErrAccountLocked      = stderrors.New("oauth2: account locked")
	ErrServiceUnavailable = stderrors.New("oauth2: service unavailable")
)

// DefaultErrorMessages maps each well-known sentinel to the
// description string emitted in the OAuth2 error response. Exported
// so apps can clone, edit (i18n, brand voice), and pass the result
// to NewErrorMapper.
var DefaultErrorMessages = map[error]string{
	ErrInvalidCredentials: "The username or password you entered is incorrect. Please try again.",
	ErrAccountDisabled:    "Your account has been disabled. Please contact your administrator.",
	ErrAccountLocked:      "Your account is locked due to too many failed sign-in attempts. Please contact your administrator.",
	ErrServiceUnavailable: "The sign-in service is temporarily unavailable. Please try again in a moment.",
}

// DefaultErrorMapper translates the bundled sentinel errors into
// OAuth2 responses with DefaultErrorMessages. Returns nil for any
// other error so go-oauth2's stock translation runs.
func DefaultErrorMapper(err error) *errors.Response {
	switch {
	case stderrors.Is(err, ErrInvalidCredentials):
		return &errors.Response{
			Error:       errors.ErrInvalidGrant,
			Description: DefaultErrorMessages[ErrInvalidCredentials],
			StatusCode:  http.StatusBadRequest,
		}
	case stderrors.Is(err, ErrAccountDisabled):
		return &errors.Response{
			Error:       errors.ErrInvalidGrant,
			Description: DefaultErrorMessages[ErrAccountDisabled],
			StatusCode:  http.StatusBadRequest,
		}
	case stderrors.Is(err, ErrAccountLocked):
		return &errors.Response{
			Error:       errors.ErrInvalidGrant,
			Description: DefaultErrorMessages[ErrAccountLocked],
			StatusCode:  http.StatusBadRequest,
		}
	case stderrors.Is(err, ErrServiceUnavailable):
		return &errors.Response{
			Error:       errors.ErrTemporarilyUnavailable,
			Description: DefaultErrorMessages[ErrServiceUnavailable],
			StatusCode:  http.StatusServiceUnavailable,
		}
	}
	return nil
}

// NewErrorMapper builds an ErrorMapper from a custom message table —
// use to override the bundled descriptions (i18n) without rewriting
// the switch logic.
func NewErrorMapper(messages map[error]string) ErrorMapper {
	return func(err error) *errors.Response {
		switch {
		case stderrors.Is(err, ErrInvalidCredentials):
			return &errors.Response{
				Error:       errors.ErrInvalidGrant,
				Description: pick(messages, ErrInvalidCredentials),
				StatusCode:  http.StatusBadRequest,
			}
		case stderrors.Is(err, ErrAccountDisabled):
			return &errors.Response{
				Error:       errors.ErrInvalidGrant,
				Description: pick(messages, ErrAccountDisabled),
				StatusCode:  http.StatusBadRequest,
			}
		case stderrors.Is(err, ErrAccountLocked):
			return &errors.Response{
				Error:       errors.ErrInvalidGrant,
				Description: pick(messages, ErrAccountLocked),
				StatusCode:  http.StatusBadRequest,
			}
		case stderrors.Is(err, ErrServiceUnavailable):
			return &errors.Response{
				Error:       errors.ErrTemporarilyUnavailable,
				Description: pick(messages, ErrServiceUnavailable),
				StatusCode:  http.StatusServiceUnavailable,
			}
		}
		return nil
	}
}

// SoftenStockMessages returns a ResponseErrorRewriter that replaces
// the worst of go-oauth2's stock descriptions with friendlier
// strings. Mirrors the rewriter in portal_admin/services/server.go.
// Pass directly as Config.ResponseErrorRewriter.
func SoftenStockMessages(re *errors.Response) {
	switch re.Error {
	case errors.ErrInvalidRequest:
		if re.Description == errors.Descriptions[errors.ErrInvalidRequest] {
			re.Description = "Please enter your username and password to sign in."
		}
	case errors.ErrInvalidClient:
		re.Description = "This application is not authorized to sign you in. Please contact your administrator."
	case errors.ErrInvalidGrant:
		if re.Description == errors.Descriptions[errors.ErrInvalidGrant] {
			re.Description = "Your sign-in session is invalid or has expired. Please sign in again."
		}
	case errors.ErrUnauthorizedClient:
		re.Description = "This application is not permitted to use this sign-in method."
	case errors.ErrUnsupportedGrantType:
		re.Description = "This sign-in method is not supported."
	}
}

func pick(m map[error]string, key error) string {
	if s, ok := m[key]; ok {
		return s
	}
	return DefaultErrorMessages[key]
}