// Package authdb keeps extension/auth's issued tokens, API keys and session
// records in a SQL database — restart-safe and shared by every replica,
// without a cache. Any dialect GORM speaks.
//
//	type DB struct{ *db.Manager }
//
//	nexus.Boot(
//	    db.BindFromConfig[DB]("main"),
//	    authdb.Bind[DB](),                                   // provides an auth.TokenStore
//	    auth.Module(auth.Config{Users: auth.UseUsers(NewUsers)}),
//	)
//
// The table nexus_auth_tokens is created on first use (NoMigrate to run
// Migrate from your own migrations). It holds SHA-256 hashes, never tokens.
package authdb

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/paulmanoni/nexus/v2"
	"github.com/paulmanoni/nexus/v2/db"
	"github.com/paulmanoni/nexus/v2/extension/auth"
	"github.com/paulmanoni/nexus/v2/internal/bindutil"
)

// ErrUnavailable is returned while the database is disconnected.
var ErrUnavailable = errors.New("authdb: the database is not connected")

// Bind provides an auth.TokenStore on the database T embeds (*db.Manager);
// auth.Module uses it when Config.Tokens is unset.
func Bind[T any](opts ...Option) nexus.Option {
	idx := bindutil.EmbeddedField[T]("authdb.Bind", reflect.TypeFor[*db.Manager](), "type DB struct{ *db.Manager }")
	return nexus.Provide(func(t *T) auth.TokenStore {
		m := bindutil.ManagerOf[*db.Manager](t, idx)
		return New(m.GetDB, opts...)
	})
}

// Option configures the store.
type Option func(*Store)

// NoMigrate skips creating the table on first use.
func NoMigrate() Option { return func(s *Store) { s.migrate = false } }

// Store is an auth.TokenStore (and auth.TokenLister) on a SQL database.
type Store struct {
	get     func() *gorm.DB
	migrate bool

	mu       sync.Mutex
	migrated bool
}

// New returns a store on the database get returns (nil while disconnected).
func New(get func() *gorm.DB, opts ...Option) *Store {
	s := &Store{get: get, migrate: true}
	for _, o := range opts {
		o(s)
	}
	return s
}

// Migrate creates or updates the table.
func Migrate(g *gorm.DB) error { return g.AutoMigrate(&tokenRow{}) }

type tokenRow struct {
	Hash          string `gorm:"primaryKey;size:80"` // a token hash, or "user-epoch:<id>"
	UserID        string `gorm:"size:200;not null;index:idx_nexus_auth_tokens_user"`
	Scheme        string `gorm:"size:100"`
	Expires       *time.Time
	Epoch         int64
	Use           string `gorm:"size:16"`
	Impersonating string `gorm:"size:200"`
	Name          string `gorm:"size:200"`
	Created       *time.Time
	Agent         string `gorm:"size:500"`
	IP            string `gorm:"size:64"`
}

func (tokenRow) TableName() string { return "nexus_auth_tokens" }

func (s *Store) conn(ctx context.Context) (*gorm.DB, error) {
	g := s.get()
	if g == nil {
		return nil, ErrUnavailable
	}
	if s.migrate {
		s.mu.Lock()
		if !s.migrated {
			if err := Migrate(g.WithContext(ctx)); err != nil {
				s.mu.Unlock()
				return nil, fmt.Errorf("authdb: creating the token table: %w", err)
			}
			s.migrated = true
		}
		s.mu.Unlock()
	}
	return g.WithContext(ctx), nil
}

func toRow(hash string, t auth.StoredToken) tokenRow {
	r := tokenRow{Hash: hash, UserID: t.UserID, Scheme: t.Scheme, Epoch: t.Epoch, Use: t.Use,
		Impersonating: t.Impersonating, Name: t.Name, Agent: truncate(t.Agent, 500), IP: t.IP}
	if !t.Expires.IsZero() {
		e := t.Expires
		r.Expires = &e
	}
	if !t.Created.IsZero() {
		c := t.Created
		r.Created = &c
	}
	return r
}

func (r tokenRow) token() auth.StoredToken {
	t := auth.StoredToken{UserID: r.UserID, Scheme: r.Scheme, Epoch: r.Epoch, Use: r.Use,
		Impersonating: r.Impersonating, Name: r.Name, Agent: r.Agent, IP: r.IP}
	if r.Expires != nil {
		t.Expires = *r.Expires
	}
	if r.Created != nil {
		t.Created = *r.Created
	}
	return t
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// Save implements auth.TokenStore.
func (s *Store) Save(ctx context.Context, hash string, t auth.StoredToken, _ time.Duration) error {
	g, err := s.conn(ctx)
	if err != nil {
		return err
	}
	row := toRow(hash, t)
	return g.Clauses(clause.OnConflict{UpdateAll: true}).Create(&row).Error
}

// Load implements auth.TokenStore: nil for an unknown or expired hash.
func (s *Store) Load(ctx context.Context, hash string) (*auth.StoredToken, error) {
	g, err := s.conn(ctx)
	if err != nil {
		return nil, err
	}
	var row tokenRow
	if err := g.Where("hash = ?", hash).Take(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	if row.Expires != nil && time.Now().After(*row.Expires) {
		_ = g.Delete(&tokenRow{}, "hash = ?", hash).Error
		return nil, nil
	}
	t := row.token()
	return &t, nil
}

// Delete implements auth.TokenStore.
func (s *Store) Delete(ctx context.Context, hash string) error {
	g, err := s.conn(ctx)
	if err != nil {
		return err
	}
	return g.Delete(&tokenRow{}, "hash = ?", hash).Error
}

// ListUser implements auth.TokenLister; it also removes the user's expired
// rows.
func (s *Store) ListUser(ctx context.Context, userID string) (map[string]auth.StoredToken, error) {
	g, err := s.conn(ctx)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	_ = g.Where("user_id = ? AND expires IS NOT NULL AND expires < ?", userID, now).Delete(&tokenRow{}).Error
	var rows []tokenRow
	if err := g.Where("user_id = ?", userID).Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make(map[string]auth.StoredToken, len(rows))
	for _, r := range rows {
		if strings.HasPrefix(r.Hash, "user-epoch:") {
			continue
		}
		out[r.Hash] = r.token()
	}
	return out, nil
}

// Purge deletes every expired row — run it from a cron if many tokens
// expire unused.
func (s *Store) Purge(ctx context.Context) error {
	g, err := s.conn(ctx)
	if err != nil {
		return err
	}
	return g.Where("expires IS NOT NULL AND expires < ?", time.Now()).Delete(&tokenRow{}).Error
}
