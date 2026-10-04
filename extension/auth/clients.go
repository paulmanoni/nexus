package auth

import (
	"context"
	"crypto/subtle"
	"slices"
	"strings"
)

// Client is an OAuth2 client of the token endpoint.
type Client struct {
	ID string
	// Secret (compared in constant time) or SecretHash (verified with the
	// [auth.passwords] hashers); both empty: a public client.
	Secret     string
	SecretHash string
	// Grants it may use (empty: all): password, refresh_token,
	// client_credentials.
	Grants []string
	// Perms and Kind are the identity of its client_credentials tokens.
	Perms []string
	Kind  string
}

// Clients finds OAuth2 clients — for clients kept in a database.
// Config.Clients sets it; [auth.oauth2.clients.*] are consulted after it.
type Clients interface {
	// Client returns the client with id, nil when there is none.
	Client(ctx context.Context, id string) (*Client, error)
}

// clientPrefix marks a token issued to a client itself (client_credentials):
// its StoredToken.UserID is clientPrefix + the client id.
const clientPrefix = "client:"

func (st *moduleState) client(ctx context.Context, id string) (*Client, error) {
	if id == "" {
		return nil, nil
	}
	if st.cfg.Clients != nil {
		c, err := st.cfg.Clients.Client(ctx, id)
		if err != nil || c != nil {
			return c, err
		}
	}
	cs, ok := st.config.settings.oauth2.Clients[id]
	if !ok {
		return nil, nil
	}
	return &Client{ID: id, Secret: cs.Secret, SecretHash: cs.SecretHash, Grants: cs.Grants, Perms: cs.Perms, Kind: cs.Kind}, nil
}

// clientSecretOK reports whether secret is c's.
func (st *moduleState) clientSecretOK(c *Client, secret string) bool {
	switch {
	case c.SecretHash != "":
		ok, _, err := st.config.settings.hashers.Verify(secret, c.SecretHash)
		return err == nil && ok
	case c.Secret != "":
		return subtle.ConstantTimeCompare([]byte(c.Secret), []byte(secret)) == 1
	}
	return secret == "" // a public client sends none
}

func (c *Client) confidential() bool { return c.Secret != "" || c.SecretHash != "" }

func (c *Client) may(grant string) bool {
	return len(c.Grants) == 0 || slices.Contains(c.Grants, grant)
}

// clientIdentity is the identity of a client_credentials token.
func (st *moduleState) clientIdentity(ctx context.Context, scheme, userID string) (*Identity, error) {
	c, err := st.client(ctx, strings.TrimPrefix(userID, clientPrefix))
	if err != nil {
		return nil, err
	}
	if c == nil {
		return nil, &credentialError{scheme, "the OAuth2 client no longer exists"}
	}
	kind := c.Kind
	if kind == "" {
		kind = "client"
	}
	return &Identity{ID: userID, Kind: kind, Perms: c.Perms, Scheme: scheme}, nil
}
