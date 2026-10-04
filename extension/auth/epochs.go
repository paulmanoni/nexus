package auth

import (
	"context"
	"strconv"
	"sync"
	"time"

	"github.com/paulmanoni/nexus/v2"
	"github.com/paulmanoni/nexus/v2/extension/session"
)

// A user's epoch is when their sessions and tokens were last ended: every
// credential records the epoch it was issued under, and one issued before
// the current epoch no longer works. RevokeUser, a password change and
// [auth.sessions] single move it.

const (
	epochKeyPrefix   = "user-epoch:"
	sessionEpochKey  = "_auth_epoch"
	sessionSeenKey   = "_auth_seen"
	ctxAuthEpoch     = ctxCredential(101)
	revokedReason    = "signed out everywhere (auth.RevokeUser or a password change)"
	idleReasonPrefix = "idle for over "
)

// authEpoch is the epoch a request's credential was checked against, kept
// for the connections (WebSocket, live view) that outlive the request.
type authEpoch struct {
	user  string
	epoch int64
}

type epochCache struct {
	ttl time.Duration
	mu  sync.Mutex
	m   map[string]epochEntry
}

type epochEntry struct {
	epoch   int64
	expires time.Time
}

// epoch is userID's current epoch (0 when it never moved), cached for the
// [auth] cache TTL — another replica's RevokeUser reaches this one within it.
func (st *moduleState) epoch(ctx context.Context, userID string) (int64, error) {
	c := st.config.epochs
	if c != nil {
		c.mu.Lock()
		e, ok := c.m[userID]
		c.mu.Unlock()
		if ok && time.Now().Before(e.expires) {
			return e.epoch, nil
		}
	}
	t, err := st.config.tokens.Load(ctx, epochKeyPrefix+userID)
	if err != nil {
		return 0, err
	}
	var n int64
	if t != nil {
		n = t.Epoch
	}
	if c != nil {
		c.mu.Lock()
		c.m[userID] = epochEntry{n, time.Now().Add(c.ttl)}
		c.mu.Unlock()
	}
	return n, nil
}

// bumpEpoch ends every credential userID holds and returns the new epoch.
func (st *moduleState) bumpEpoch(ctx context.Context, userID string) (int64, error) {
	n := time.Now().UnixNano()
	if err := st.config.tokens.Save(ctx, epochKeyPrefix+userID, StoredToken{UserID: userID, Epoch: n}, 0); err != nil {
		return 0, err
	}
	if c := st.config.epochs; c != nil {
		c.mu.Lock()
		c.m[userID] = epochEntry{n, time.Now().Add(c.ttl)}
		c.mu.Unlock()
	}
	return n, nil
}

// RevokeUser ends every session and token userID holds — "sign out
// everywhere". Open WebSocket and live view connections close on their
// next message. Other replicas see it within [auth] cache.
func RevokeUser(ctx context.Context, userID string) error {
	st, err := configState(ctx)
	if err != nil {
		return err
	}
	if _, err := st.bumpEpoch(ctx, userID); err != nil {
		return err
	}
	Refresh(ctx, userID)
	return nil
}

// Revoke ends one bearer token or API key, given the token itself.
func Revoke(ctx context.Context, token string) error {
	st, err := configState(ctx)
	if err != nil {
		return err
	}
	return st.config.tokens.Delete(ctx, hashToken(token))
}

// restamp moves the credential this request came with to epoch n, when it
// belongs to userID — so a password change ends the user's other sessions,
// not this one.
func (st *moduleState) restamp(ctx context.Context, userID string, n int64) {
	id := Current(ctx)
	p, ok := ctx.Value(ctxCredentialTok).(presented)
	if id == nil || id.ID != userID || !ok {
		return
	}
	sc, ok := st.config.settings.scheme(p.scheme)
	if !ok {
		return
	}
	if sc.Type == SchemeSession {
		session.Get(ctx).Set(sessionEpochKey, strconv.FormatInt(n, 10))
		return
	}
	h := hashToken(p.token)
	t, err := st.config.tokens.Load(ctx, h)
	if err != nil || t == nil {
		return
	}
	t.Epoch = n
	ttl := time.Duration(0)
	if !t.Expires.IsZero() {
		ttl = time.Until(t.Expires)
	}
	_ = st.config.tokens.Save(ctx, h, *t, ttl)
}

func init() {
	// A connection keeps the epoch its upgrade request was checked
	// against, and ends once the user's epoch moves past it.
	nexus.RegisterWSCarrier(func(upgrade, conn context.Context) context.Context {
		if e, ok := upgrade.Value(ctxAuthEpoch).(authEpoch); ok {
			conn = context.WithValue(conn, ctxAuthEpoch, e)
		}
		return conn
	})
	nexus.RegisterConnectionCheck(func(ctx context.Context) error {
		e, ok := ctx.Value(ctxAuthEpoch).(authEpoch)
		st, sok := stateFrom(ctx)
		if !ok || !sok || st.config.settings == nil {
			return nil
		}
		cur, err := st.epoch(ctx, e.user)
		if err != nil || cur == e.epoch {
			return nil
		}
		return &nexus.Error{Code: nexus.Unauthenticated, Message: "auth: " + revokedReason, Cause: ErrUnauthenticated}
	})
}
