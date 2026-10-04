package auth

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/paulmanoni/nexus/v2"
	"github.com/paulmanoni/nexus/v2/extension/session"
	"github.com/paulmanoni/nexus/v2/middleware/secure"
)

// errNoConfigPath is returned by the sign-in functions in an app without
// Config.Users.
var errNoConfigPath = errors.New("auth: SignIn, SignOut, Login and SetPassword need auth.Module(auth.Config{Users: …}) and a request it handled")

func configState(ctx context.Context) (*moduleState, error) {
	st, ok := stateFrom(ctx)
	if !ok || st.config.settings == nil {
		return nil, errNoConfigPath
	}
	return st, nil
}

// Login checks a sign-in: Users.FindLogin, the password against
// [auth.passwords] hashers (rehashing an outdated hash through
// PasswordSetter), then LoginChecker. It doesn't sign in — pass the
// identity to SignIn. A wrong login or password is one InvalidInput error
// ("invalid login or password", errors.Is ErrInvalidCredentials), the same
// for an unknown account, in the same time.
//
//	id, err := auth.Login(ctx, auth.Password{Username: in.Email, Password: in.Password})
//	if err != nil { return nil, err }
//	return auth.SignIn(ctx, id)
func Login(ctx context.Context, cred Password) (*Identity, error) {
	st, err := configState(ctx)
	if err != nil {
		return nil, err
	}
	rs, th := st.config.settings, st.config.throttle
	if err := th.check(ctx, cred.Username); err != nil {
		return nil, err
	}
	failed := func() (*Identity, error) {
		th.fail(ctx, cred.Username)
		return nil, invalidLogin()
	}
	id, encoded, err := st.config.users.FindLogin(ctx, cred.Username)
	if err != nil {
		return nil, err
	}
	if id == nil || encoded == "" {
		_, _ = rs.hashers.Hash(cred.Password) // the time a verify would take
		return failed()
	}
	ok, upgrade, verr := rs.hashers.Verify(cred.Password, encoded)
	if verr != nil || !ok {
		return failed()
	}
	// Signing in under an area admits only its kinds — told apart from a
	// wrong password by no one.
	info, _ := ctx.Value(ctxRequestInfo).(requestInfo)
	if a := rs.area(info.path); a != nil && !kindIn(id.Kind, a.Kinds) {
		return failed()
	}
	th.succeed(ctx, cred.Username)
	hashers := rs.hashers
	if upgrade {
		if ps, isSetter := st.config.users.(PasswordSetter); isSetter {
			if fresh, herr := hashers.Hash(cred.Password); herr == nil {
				_ = ps.SetPassword(ctx, id.ID, fresh) // best-effort; the login stands
			}
		}
	}
	if lc, isChecker := st.config.users.(LoginChecker); isChecker {
		if err := lc.CheckLogin(ctx, id); err != nil {
			return nil, err
		}
	}
	return id, nil
}

func invalidLogin() error {
	e := nexus.Invalid().Global("invalid login or password")
	e.Message = "invalid login or password"
	e.Cause = ErrInvalidCredentials
	return e
}

// Credential is what SignIn issued. A session sign-in sets the cookie and
// carries no token; a bearer or API-key sign-in returns the token, once —
// only its hash is stored. Its JSON is the OAuth2 token response shape.
type Credential struct {
	Scheme      string `json:"-"`
	AccessToken string `json:"access_token,omitempty"`
	TokenType   string `json:"token_type,omitempty"`
	ExpiresIn   int    `json:"expires_in,omitempty"` // seconds; 0 = no expiry
	// RefreshToken comes with a bearer scheme that has refresh set;
	// auth.RefreshToken (or the token endpoint) exchanges it for a new pair.
	RefreshToken string `json:"refresh_token,omitempty"`
	// Next is where the signed-in user goes: a valid next (ReturnTo, else
	// the request's ?next=) inside an area their kind may enter, else the
	// home of the area they signed in under, else [auth] home.
	Next string `json:"next,omitempty"`
}

// SignInOption adjusts SignIn.
type SignInOption func(*signIn)

type signIn struct{ scheme, next string }

// ReturnTo names the page to land on after signing in — the next a login
// form posted. It is validated like ?next=; an unsafe one is ignored.
func ReturnTo(next string) SignInOption { return func(s *signIn) { s.next = next } }

// Using signs in through the named scheme ([auth.schemes.<name>]) instead
// of the default — the first session scheme, else the first bearer one.
func Using(scheme string) SignInOption { return func(s *signIn) { s.scheme = scheme } }

// SignIn issues id a credential: a session (its id cycled, so a session
// fixed before sign-in is useless) or a bearer token / API key.
func SignIn(ctx context.Context, id *Identity, opts ...SignInOption) (*Credential, error) {
	st, err := configState(ctx)
	if err != nil {
		return nil, err
	}
	if id == nil || id.ID == "" {
		return nil, errors.New("auth.SignIn: an identity with an ID")
	}
	var o signIn
	for _, opt := range opts {
		opt(&o)
	}
	rs := st.config.settings
	landing := func() string {
		next := o.next
		if safeNext(next) == "" {
			next = Next(ctx)
		}
		return rs.landing(ctx, id, next)
	}
	sc, ok := rs.scheme(o.scheme)
	if o.scheme == "" {
		if sc, ok = rs.firstOf(SchemeSession); !ok {
			sc, ok = rs.firstOf(SchemeBearer)
		}
	}
	if !ok {
		if o.scheme != "" {
			return nil, errors.New("auth.SignIn: no scheme [auth.schemes." + o.scheme + "]")
		}
		return nil, errors.New("auth.SignIn: no session or bearer scheme — name one with auth.Using")
	}
	epoch, err := st.epoch(ctx, id.ID)
	if rs.sessions.Single {
		epoch, err = st.bumpEpoch(ctx, id.ID) // ends the user's other sessions
	}
	if err != nil {
		return nil, err
	}
	if sc.Type == SchemeSession {
		if !session.Present(ctx) {
			return nil, errors.New("auth.SignIn: no session on this request")
		}
		s := session.Get(ctx)
		s.Cycle()
		s.Set(sessionUserKey, id.ID)
		s.Set(sessionEpochKey, epochString(epoch))
		s.Set(sessionSeenKey, strconv.FormatInt(time.Now().UnixMilli(), 10))
		// A record of the session in the token store, so Sessions lists it
		// and RevokeSession can end it.
		ttl := sc.TTL
		if ttl <= 0 {
			ttl = 14 * 24 * time.Hour
		}
		rec := st.stamp(ctx, StoredToken{UserID: id.ID, Scheme: sc.name, Epoch: epoch, Use: useSession, Expires: time.Now().Add(ttl)})
		if err := st.config.tokens.Save(ctx, hashToken(s.ID()), rec, ttl); err != nil {
			return nil, err
		}
		s.Set(sessionRecKey, "1")
		secure.RotateCSRF(ctx)
		return &Credential{Scheme: sc.name, Next: landing()}, nil
	}
	if sc.Type == SchemeJWT {
		return nil, errors.New("auth.SignIn: a jwt scheme only verifies tokens another service issued")
	}
	c, err := st.issue(ctx, sc, id.ID, epoch)
	if err != nil {
		return nil, err
	}
	c.Next = landing()
	return c, nil
}

// issue stores and returns a new token (and, for a bearer scheme with
// refresh, a refresh token) for userID under epoch.
func (st *moduleState) issue(ctx context.Context, sc namedScheme, userID string, epoch int64) (*Credential, error) {
	tok := newToken()
	t := st.stamp(ctx, StoredToken{UserID: userID, Scheme: sc.name, Epoch: epoch})
	ttl := time.Duration(0)
	if sc.Type == SchemeBearer {
		ttl = sc.TTL
		t.Expires = time.Now().Add(ttl)
	}
	if err := st.config.tokens.Save(ctx, hashToken(tok), t, ttl); err != nil {
		return nil, err
	}
	c := &Credential{Scheme: sc.name, AccessToken: tok, ExpiresIn: int(ttl / time.Second)}
	if sc.Type != SchemeBearer {
		return c, nil
	}
	c.TokenType = "Bearer"
	if sc.Refresh > 0 {
		rt := newToken()
		r := st.stamp(ctx, StoredToken{UserID: userID, Scheme: sc.name, Epoch: epoch, Use: "refresh", Expires: time.Now().Add(sc.Refresh)})
		if err := st.config.tokens.Save(ctx, hashToken(rt), r, sc.Refresh); err != nil {
			return nil, err
		}
		c.RefreshToken = rt
	}
	return c, nil
}

// RefreshToken exchanges a refresh token for a new access and refresh token
// pair; the old refresh token stops working. One from before the user's
// last RevokeUser, of a user Load no longer finds, or refused by
// CheckLogin is an Unauthenticated error.
func RefreshToken(ctx context.Context, refresh string) (*Credential, error) {
	st, err := configState(ctx)
	if err != nil {
		return nil, err
	}
	invalid := nexus.Err(nexus.Unauthenticated, "invalid or expired refresh token")
	h := hashToken(refresh)
	t, err := st.config.tokens.Load(ctx, h)
	if err != nil {
		return nil, err
	}
	if t == nil || t.Use != "refresh" {
		return nil, invalid
	}
	sc, ok := st.config.settings.scheme(t.Scheme)
	if !ok {
		return nil, invalid
	}
	if cur, err := st.epoch(ctx, t.UserID); err != nil || cur != t.Epoch {
		return nil, invalid
	}
	if err := st.config.tokens.Delete(ctx, h); err != nil {
		return nil, err
	}
	id, err := st.load(ctx, t.UserID)
	if err != nil {
		return nil, err
	}
	if id == nil {
		return nil, invalid
	}
	if lc, ok := st.config.users.(LoginChecker); ok {
		if err := lc.CheckLogin(ctx, id); err != nil {
			return nil, err
		}
	}
	return st.issue(ctx, sc, t.UserID, t.Epoch)
}

// SignOut ends the credential this request came with: the session is
// destroyed, the token or key revoked. Anonymous requests are a no-op.
func SignOut(ctx context.Context) error {
	st, err := configState(ctx)
	if err != nil {
		return err
	}
	p, ok := ctx.Value(ctxCredentialTok).(presented)
	if !ok {
		return nil
	}
	sc, ok := st.config.settings.scheme(p.scheme)
	if !ok {
		return nil
	}
	if sc.Type == SchemeSession {
		s := session.Get(ctx)
		if s.GetString(sessionRecKey) != "" {
			_ = st.config.tokens.Delete(ctx, hashToken(s.ID()))
		}
		s.Destroy()
		secure.RotateCSRF(ctx)
		return nil
	}
	return st.config.tokens.Delete(ctx, hashToken(p.token))
}

// SetPassword validates plain against [auth.passwords], hashes it and
// stores it through Users' SetPassword. A refused password is an
// InvalidInput error on the "password" field.
func SetPassword(ctx context.Context, id *Identity, plain string) error {
	st, err := configState(ctx)
	if err != nil {
		return err
	}
	ps, ok := st.config.users.(PasswordSetter)
	if !ok {
		return errors.New("auth.SetPassword: Users has no SetPassword(ctx, id, encoded string) error method")
	}
	if err := ValidatePassword(ctx, plain, id, st.config.settings.validator...); err != nil {
		return nexus.Invalid().Field("password", err.Error())
	}
	encoded, err := st.config.settings.hashers.Hash(plain)
	if err != nil {
		return err
	}
	if err := ps.SetPassword(ctx, id.ID, encoded); err != nil {
		return err
	}
	Refresh(ctx, id.ID)
	if st.config.settings.endOnPw {
		n, err := st.bumpEpoch(ctx, id.ID)
		if err != nil {
			return err
		}
		st.restamp(ctx, id.ID, n) // this session stays signed in
	}
	return nil
}

// Refresh drops the cached Users.Load result for userID, so the next
// request loads it again — call it after changing what Load returns
// (roles, a disabled flag) outside SetPassword.
func Refresh(ctx context.Context, userID string) {
	if st, ok := stateFrom(ctx); ok && st.config.loads != nil {
		st.config.loads.drop(userID)
	}
}

// Public opts an endpoint out of the sign-in [auth] default requires — the
// same option as nexus.Public().
func Public() nexus.PublicOption { return nexus.Public() }
