package inertia

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"sort"
	"strings"

	"github.com/paulmanoni/nexus/httpx"
)

// headerErrorBag is the Inertia protocol header that scopes validation errors
// under a named bag (so one page can host several independent forms).
const headerErrorBag = "X-Inertia-Error-Bag"

// errorsCookie is the one-shot flash cookie that carries validation errors
// across the redirect-back. Inertia's validation flow is: a failed submit
// redirects back to the form, and the errors appear in the next render's
// `errors` prop. With no server session, the errors ride in this short-lived,
// HttpOnly cookie that the next render consumes and clears.
const errorsCookie = "nexus_inertia_errors"

// validationError is the sentinel a page handler returns to signal a failed
// validation. The framework's ErrorRenderer hook (pageRenderer.RenderError)
// flashes the field messages and redirects back, the Inertia way.
type validationError struct {
	fields map[string]string
}

func (e *validationError) Error() string {
	keys := make([]string, 0, len(e.fields))
	for k := range e.fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return "inertia: validation failed: " + strings.Join(keys, ", ")
}

// Invalid returns from a page handler to report validation failures. The engine
// redirects back to the submitting page (303) and the messages surface in the
// next render's `errors` prop — exactly what the Inertia client's useForm reads
// to populate form.errors and fire onError:
//
//	func NewRegister(svc *UserService, p nexus.Params[NewUser]) (any, error) {
//	    if bad := svc.Validate(p.Args); len(bad) > 0 {
//	        return nil, inertia.Invalid(bad) // map[field]message
//	    }
//	    svc.Create(p.Args)
//	    return nil, inertia.Redirect("/users")
//	}
//
// To scope errors to a named bag (multiple forms on one page), the client sends
// X-Inertia-Error-Bag; the engine nests the messages under that bag
// automatically.
func Invalid(fields map[string]string) error {
	return &validationError{fields: fields}
}

// InvalidField is the single-field convenience for Invalid — handy when one
// check fails and building a map literal is overkill:
//
//	if !validEmail(p.Args.Email) {
//	    return nil, inertia.InvalidField("email", "Enter a valid email")
//	}
func InvalidField(field, message string) error {
	return &validationError{fields: map[string]string{field: message}}
}

// writeValidationRedirect flashes the errors into the one-shot cookie and
// redirects back to the form. Honors X-Inertia-Error-Bag by nesting the
// messages under the bag name. The redirect is 303 See Other so the follow-up
// is a GET (correct after a POST/PUT/PATCH/DELETE), which the Inertia client
// follows transparently; the next render injects the flashed errors.
func writeValidationRedirect(c *httpx.Ctx, ve *validationError) error {
	var payload any = ve.fields
	if bag := c.GetHeader(headerErrorBag); bag != "" {
		payload = map[string]any{bag: ve.fields}
	}
	blob, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	enc := base64.RawURLEncoding.EncodeToString(blob)
	// Max-Age 30s is a safety net: the cookie is normally consumed (and
	// cleared) by the very next render. HttpOnly because only the server reads
	// it — it's injected into props, never touched by client JS.
	c.SetCookie(errorsCookie, enc, 30, "/", "", false, true)

	target := c.GetHeader("Referer")
	if target == "" {
		target = c.Request.URL.RequestURI()
	}
	c.Redirect(http.StatusSeeOther, target)
	return nil
}

// consumeFlashErrors reads and clears the flash cookie, returning the decoded
// errors object (flat map, or bag-nested) for the `errors` prop. Returns an
// empty map when there's nothing flashed, so page.props.errors is always an
// object — the shape the Inertia client expects unconditionally.
func consumeFlashErrors(c *httpx.Ctx) map[string]any {
	out := map[string]any{}
	raw, err := c.Cookie(errorsCookie)
	if err != nil || raw == "" {
		return out
	}
	// Clear the cookie regardless of decode success — it's one-shot.
	c.SetCookie(errorsCookie, "", -1, "/", "", false, true)
	blob, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return out
	}
	_ = json.Unmarshal(blob, &out)
	return out
}
