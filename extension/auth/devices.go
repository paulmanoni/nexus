package auth

import (
	"context"
	"errors"
	"sort"
	"time"

	"github.com/paulmanoni/nexus/v2"
	"github.com/paulmanoni/nexus/v2/extension/session"
)

// TokenLister is an optional TokenStore method: the tokens stored for one
// user, by hash. auth.Sessions and auth.Keys need it; the built-in stores
// implement it, and a custom store that doesn't makes them return
// ErrUnsupported.
type TokenLister interface {
	ListUser(ctx context.Context, userID string) (map[string]StoredToken, error)
}

// ErrUnsupported is returned by Sessions and Keys when Config.Tokens can't
// list a user's tokens (it doesn't implement TokenLister).
var ErrUnsupported = errors.New("auth: the TokenStore can't list a user's tokens — implement auth.TokenLister")

// Device is one way a user is signed in — a session, a bearer token or an
// API key — as Sessions and Keys list it. ID names it for RevokeSession
// and Keys.Revoke; it is a hash, never the credential itself.
type Device struct {
	ID      string    `json:"id"`
	Kind    string    `json:"kind"` // "session", "token" or "key"
	Scheme  string    `json:"scheme"`
	Name    string    `json:"name,omitempty"` // an API key's name
	Created time.Time `json:"created,omitzero"`
	Expires time.Time `json:"expires,omitzero"`
	Agent   string    `json:"agent,omitempty"` // the User-Agent it was issued to
	IP      string    `json:"ip,omitempty"`
	Current bool      `json:"current,omitempty"` // the credential of this request
}

const (
	useSession    = "session"
	useKey        = "key"
	sessionRecKey = "_auth_rec"
)

// Sessions lists where userID is signed in: sessions signed in since this
// version, and bearer tokens — not refresh tokens or API keys (Keys.List).
// Newest first.
func Sessions(ctx context.Context, userID string) ([]Device, error) {
	return listDevices(ctx, userID, func(t StoredToken) bool { return t.Use == "" || t.Use == useSession }, true)
}

// RevokeSession ends one of userID's sessions or tokens, by its Device.ID.
func RevokeSession(ctx context.Context, userID, id string) error {
	return revokeDevice(ctx, userID, id, func(t StoredToken) bool { return t.Use == "" || t.Use == useSession })
}

// Keys manages API keys: named, long-lived tokens of an apikey scheme.
//
//	key, dev, err := auth.Keys.Create(ctx, userID, "partners", "billing sync")
//	keys, err := auth.Keys.List(ctx, userID)
//	err = auth.Keys.Revoke(ctx, userID, dev.ID)
var Keys keys

type keys struct{}

// Create issues an API key for userID through the named apikey scheme and
// returns it — once; only its hash is stored — with its Device.
func (keys) Create(ctx context.Context, userID, scheme, name string) (string, *Device, error) {
	st, err := configState(ctx)
	if err != nil {
		return "", nil, err
	}
	sc, ok := st.config.settings.scheme(scheme)
	if !ok || sc.Type != SchemeAPIKey {
		return "", nil, errors.New("auth.Keys.Create: [auth.schemes." + scheme + "] is not an apikey scheme")
	}
	epoch, err := st.epoch(ctx, userID)
	if err != nil {
		return "", nil, err
	}
	tok := newToken()
	t := st.stamp(ctx, StoredToken{UserID: userID, Scheme: sc.name, Epoch: epoch, Use: useKey, Name: name})
	h := hashToken(tok)
	if err := st.config.tokens.Save(ctx, h, t, 0); err != nil {
		return "", nil, err
	}
	d := device(h, t, "")
	return tok, &d, nil
}

// List returns userID's API keys, newest first.
func (keys) List(ctx context.Context, userID string) ([]Device, error) {
	return listDevices(ctx, userID, func(t StoredToken) bool { return t.Use == useKey }, false)
}

// Revoke ends one of userID's API keys, by its Device.ID.
func (keys) Revoke(ctx context.Context, userID, id string) error {
	return revokeDevice(ctx, userID, id, func(t StoredToken) bool { return t.Use == useKey })
}

// stamp records when, to what and from where a credential is issued.
func (st *moduleState) stamp(ctx context.Context, t StoredToken) StoredToken {
	t.Created = time.Now()
	r, _ := ctx.Value(ctxRequestInfo).(requestInfo)
	t.Agent, t.IP = r.agent, nexus.ClientIP(ctx)
	return t
}

func device(hash string, t StoredToken, current string) Device {
	kind := "token"
	switch t.Use {
	case useSession:
		kind = "session"
	case useKey:
		kind = "key"
	}
	return Device{
		ID: hash[:32], Kind: kind, Scheme: t.Scheme, Name: t.Name,
		Created: t.Created, Expires: t.Expires, Agent: t.Agent, IP: t.IP,
		Current: hash == current,
	}
}

// userTokens lists userID's live tokens that pass keep, by hash.
func userTokens(ctx context.Context, st *moduleState, userID string, keep func(StoredToken) bool) (map[string]StoredToken, error) {
	l, ok := st.config.tokens.(TokenLister)
	if !ok {
		return nil, ErrUnsupported
	}
	all, err := l.ListUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	cur, err := st.epoch(ctx, userID)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	out := map[string]StoredToken{}
	for h, t := range all {
		if t.UserID != userID || t.Epoch != cur || !t.Expires.IsZero() && now.After(t.Expires) || !keep(t) {
			continue
		}
		out[h] = t
	}
	return out, nil
}

func listDevices(ctx context.Context, userID string, keep func(StoredToken) bool, markCurrent bool) ([]Device, error) {
	st, err := configState(ctx)
	if err != nil {
		return nil, err
	}
	ts, err := userTokens(ctx, st, userID, keep)
	if err != nil {
		return nil, err
	}
	current := ""
	if markCurrent {
		current = currentHash(ctx)
	}
	out := make([]Device, 0, len(ts))
	for h, t := range ts {
		out = append(out, device(h, t, current))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.After(out[j].Created) })
	return out, nil
}

func revokeDevice(ctx context.Context, userID, id string, keep func(StoredToken) bool) error {
	st, err := configState(ctx)
	if err != nil {
		return err
	}
	ts, err := userTokens(ctx, st, userID, keep)
	if err != nil {
		return err
	}
	for h := range ts {
		if len(id) == 32 && h[:32] == id {
			return st.config.tokens.Delete(ctx, h)
		}
	}
	return nexus.Err(nexus.NotFound, "no such session or key")
}

// currentHash is the store hash of this request's credential: its token's,
// or for a session its record's.
func currentHash(ctx context.Context) string {
	p, ok := ctx.Value(ctxCredentialTok).(presented)
	st, sok := stateFrom(ctx)
	if !ok || !sok || st.config.settings == nil {
		return ""
	}
	if sc, ok := st.config.settings.scheme(p.scheme); ok && sc.Type == SchemeSession {
		return hashToken(session.Get(ctx).ID())
	}
	return hashToken(p.token)
}
