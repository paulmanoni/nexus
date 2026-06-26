package inertia

import "github.com/paulmanoni/nexus/httpx"

// Inertia history-state encryption (v2). The client can encrypt the history
// entry it stores for a page so that browser back/forward doesn't leak prior
// page data from memory; clearHistory drops any previously-encrypted entry.
//
// Both are per-response page-object flags. A page handler that wants to toggle
// them takes the request *httpx.Ctx and calls these before returning; the
// engine reads the flags when it renders. The app-wide default for encryption
// is Config.EncryptHistory.
const (
	ctxEncryptHistory = "inertia.encryptHistory"
	ctxClearHistory   = "inertia.clearHistory"
)

// EncryptHistory toggles history-state encryption for THIS response, overriding
// Config.EncryptHistory. Call with no argument (or true) to encrypt a sensitive
// page even when the app default is off; pass false to opt a page out when the
// default is on:
//
//	func NewAccount(c *httpx.Ctx, p nexus.Params[struct{}]) (AccountProps, error) {
//	    inertia.EncryptHistory(c)            // this page holds sensitive data
//	    return AccountProps{...}, nil
//	}
func EncryptHistory(c *httpx.Ctx, on ...bool) {
	v := true
	if len(on) > 0 {
		v = on[0]
	}
	c.Set(ctxEncryptHistory, v)
}

// ClearHistory asks the client to drop any previously-encrypted history state
// on THIS response — typically on logout, so a logged-out user can't reach
// authenticated page data via the back button. It applies to a rendered page;
// pair it with returning the post-logout page (rather than a redirect) so the
// flag rides the page object.
//
//	func NewLogout(c *httpx.Ctx, p nexus.Params[struct{}]) (LoginProps, error) {
//	    session.Destroy(c)
//	    inertia.ClearHistory(c)
//	    return LoginProps{}, nil
//	}
func ClearHistory(c *httpx.Ctx) { c.Set(ctxClearHistory, true) }

// historyFlags resolves the encrypt/clear flags for a render: the per-response
// override set via EncryptHistory/ClearHistory wins over the engine default.
func (e *Engine) historyFlags(c *httpx.Ctx) (encrypt, clear bool) {
	encrypt = e.encryptHistory
	if v, ok := c.Get(ctxEncryptHistory); ok {
		if b, isBool := v.(bool); isBool {
			encrypt = b
		}
	}
	if v, ok := c.Get(ctxClearHistory); ok {
		if b, isBool := v.(bool); isBool {
			clear = b
		}
	}
	return encrypt, clear
}
