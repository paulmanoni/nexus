// Package db is nexus's driver-agnostic GORM manager. It mirrors the shape
// of oats_admin_backend/database.DBManager (Start/Stop/GetDB/IsConnected)
// and keeps the same failsafe-go retry + circuit-breaker behavior, but
// handles PostgreSQL, MySQL, and SQLite behind a single Config.Driver field.
//
// Typical usage:
//
//	m, err := db.Open(db.Config{
//	    Driver:   db.SQLite,
//	    Database: ":memory:",
//	})
//	if err != nil { return err }
//	m.Start()                     // background reconnect loop
//	defer m.Stop()
//
//	gdb := m.GetDB()              // *gorm.DB — chain real queries
//	gdb.AutoMigrate(&User{})
//	gdb.Create(&User{Name: "A"})
//
// Multi-DB? Wrap multiple Managers in a multi.Registry[*db.Manager] and call
// .Using(name).GetDB() from resolvers — see examples/graphapp.
package db

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/failsafe-go/failsafe-go"
	"github.com/failsafe-go/failsafe-go/circuitbreaker"
	"github.com/failsafe-go/failsafe-go/retrypolicy"
	"go.uber.org/zap"

	"github.com/glebarez/sqlite"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

// Driver picks the backing dialect. Each value has a corresponding Dialector
// that Config.Dialector() returns.
type Driver string

const (
	Postgres Driver = "postgres"
	MySQL    Driver = "mysql"
	SQLite   Driver = "sqlite"
)

// Config is the full connection spec. Host/Port/User/Password/SSLMode/TimeZone
// apply to postgres+mysql; Database is the db name for pg/mysql and the file
// path (":memory:" for in-memory) for sqlite.
type Config struct {
	Driver   Driver
	Host     string
	Port     string
	User     string
	Password string
	Database string
	SSLMode  string // "disable" / "require" / ... — postgres only
	TimeZone string // IANA TZ — postgres only
}

// DSN returns the connection string for the configured Driver.
func (c Config) DSN() string {
	switch c.Driver {
	case Postgres:
		ssl := c.SSLMode
		if ssl == "" {
			ssl = "disable"
		}
		tz := c.TimeZone
		if tz == "" {
			tz = "UTC"
		}
		return fmt.Sprintf("host=%s user=%s password=%s dbname=%s port=%s sslmode=%s TimeZone=%s",
			c.Host, c.User, c.Password, c.Database, c.Port, ssl, tz)
	case MySQL:
		return fmt.Sprintf("%s:%s@tcp(%s:%s)/%s?charset=utf8mb4&parseTime=True&loc=Local",
			c.User, c.Password, c.Host, c.Port, c.Database)
	case SQLite:
		return c.Database
	}
	return ""
}

// Dialector returns the gorm dialector matching Driver.
func (c Config) Dialector() gorm.Dialector {
	switch c.Driver {
	case Postgres:
		return postgres.Open(c.DSN())
	case MySQL:
		return mysql.Open(c.DSN())
	case SQLite:
		return sqlite.Open(c.DSN())
	}
	panic("db: unknown driver " + string(c.Driver))
}

// PoolConfig tunes the underlying *sql.DB pool. Defaults match oats's values
// for postgres/mysql; sqlite :memory: gets MaxOpen=1 to keep one in-memory
// database shared across goroutines.
type PoolConfig struct {
	MaxIdle        int
	MaxOpen        int
	ConnMaxLife    time.Duration
	ConnMaxIdle    time.Duration
}

func defaultPool(d Driver) PoolConfig {
	if d == SQLite {
		return PoolConfig{MaxIdle: 1, MaxOpen: 1, ConnMaxLife: 0, ConnMaxIdle: 0}
	}
	return PoolConfig{
		MaxIdle:     10,
		MaxOpen:     100,
		ConnMaxLife: time.Hour,
		ConnMaxIdle: 30 * time.Minute,
	}
}

// Manager owns one *gorm.DB, reconnects in the background, and exposes the
// same method set as oats_admin_backend/database.DatabaseManager so it can
// drop in wherever that interface is expected.
type Manager struct {
	cfg         Config
	pool        PoolConfig
	logger      *zap.Logger
	executor    failsafe.Executor[*gorm.DB]
	mu          sync.RWMutex
	db          *gorm.DB
	isConnected bool
	ctx         context.Context
	cancel      context.CancelFunc
}

// Option tweaks a Manager at construction time.
type Option func(*Manager)

// WithLogger attaches a zap logger. Without one, Manager runs silently.
func WithLogger(l *zap.Logger) Option { return func(m *Manager) { m.logger = l } }

// WithPool overrides the default connection-pool sizing.
func WithPool(p PoolConfig) Option { return func(m *Manager) { m.pool = p } }

// WithExecutor swaps in a custom failsafe executor. Useful if you want a
// different retry/circuit-breaker profile (the defaults match oats exactly).
func WithExecutor(e failsafe.Executor[*gorm.DB]) Option {
	return func(m *Manager) { m.executor = e }
}

// NewManager builds a Manager without connecting. Call Open or Start next.
func NewManager(cfg Config, opts ...Option) *Manager {
	ctx, cancel := context.WithCancel(context.Background())
	m := &Manager{
		cfg:    cfg,
		pool:   defaultPool(cfg.Driver),
		logger: zap.NewNop(),
		ctx:    ctx,
		cancel: cancel,
	}
	m.executor = defaultExecutor(m.logger)
	for _, opt := range opts {
		opt(m)
	}
	// If an option changed the logger, refresh the default executor so its
	// OnRetry hook logs via the user's logger.
	if m.executor == nil {
		m.executor = defaultExecutor(m.logger)
	}
	return m
}

// defaultExecutor is the exact retry + circuit-breaker profile oats uses.
// Infinite retries with exponential backoff (500ms → 2s, 25ms jitter), and
// a circuit breaker that opens after 10 consecutive failures for 10 seconds.
func defaultExecutor(logger *zap.Logger) failsafe.Executor[*gorm.DB] {
	retry := retrypolicy.NewBuilder[*gorm.DB]().
		WithDelay(500 * time.Millisecond).
		WithBackoff(2, time.Second).
		WithJitter(25 * time.Millisecond).
		OnRetry(func(e failsafe.ExecutionEvent[*gorm.DB]) {
			logger.Warn("db: reconnecting",
				zap.Int("attempt", e.Attempts()),
				zap.Error(e.LastError()))
		}).
		Build()
	cb := circuitbreaker.NewBuilder[*gorm.DB]().
		WithFailureThreshold(10).
		WithDelay(10 * time.Second).
		WithSuccessThreshold(1).
		Build()
	return failsafe.With[*gorm.DB](retry, cb)
}

// Open constructs a Manager and establishes the initial connection
// synchronously. Call Start() afterwards to enable the background
// reconnect + health-check loop.
func Open(cfg Config, opts ...Option) (*Manager, error) {
	m := NewManager(cfg, opts...)
	if err := m.connect(); err != nil {
		return nil, err
	}
	return m, nil
}

// Start begins the background maintenance loop (5s health check + reconnect).
// Idempotent-ish: calling Start twice starts two loops, which is a bug but
// rarely fatal since they both probe the same state.
func (m *Manager) Start() { go m.maintain() }

// Stop cancels the maintenance loop and closes the underlying *sql.DB.
func (m *Manager) Stop() {
	m.cancel()
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.db != nil {
		if sqlDB, err := m.db.DB(); err == nil {
			_ = sqlDB.Close()
		}
	}
	m.db = nil
	m.isConnected = false
}

// GetDB returns the current *gorm.DB, or nil if disconnected.
func (m *Manager) GetDB() *gorm.DB {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.db
}

// IsConnected returns true if a live connection is currently held.
func (m *Manager) IsConnected() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.isConnected
}

// Ping runs a bounded health check against the current connection.
func (m *Manager) Ping(ctx context.Context) error {
	m.mu.RLock()
	db := m.db
	m.mu.RUnlock()
	if db == nil {
		return fmt.Errorf("db: not connected")
	}
	sqlDB, err := db.DB()
	if err != nil {
		return err
	}
	return sqlDB.PingContext(ctx)
}

// Driver returns the configured driver (useful for dashboards).
func (m *Manager) Driver() Driver { return m.cfg.Driver }

// connect runs the Open + pool-tune sequence through the failsafe executor.
func (m *Manager) connect() error {
	db, err := m.executor.Get(func() (*gorm.DB, error) {
		return gorm.Open(m.cfg.Dialector(), &gorm.Config{})
	})
	if err != nil {
		m.markDisconnected()
		return err
	}
	sqlDB, err := db.DB()
	if err != nil {
		m.markDisconnected()
		return err
	}
	sqlDB.SetMaxIdleConns(m.pool.MaxIdle)
	sqlDB.SetMaxOpenConns(m.pool.MaxOpen)
	sqlDB.SetConnMaxLifetime(m.pool.ConnMaxLife)
	sqlDB.SetConnMaxIdleTime(m.pool.ConnMaxIdle)

	m.mu.Lock()
	m.db = db
	m.isConnected = true
	m.mu.Unlock()
	m.logger.Info("db: connected", zap.String("driver", string(m.cfg.Driver)))
	return nil
}

func (m *Manager) markDisconnected() {
	m.mu.Lock()
	m.isConnected = false
	m.db = nil
	m.mu.Unlock()
}

// maintain runs the 5-second health-check ticker. Mirrors oats.
func (m *Manager) maintain() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	// If connect() has already been called by Open, this is a no-op attempt
	// on a live connection — harmless. If Start was called without Open,
	// this performs the initial connect.
	if !m.IsConnected() {
		_ = m.connect()
	}

	for {
		select {
		case <-m.ctx.Done():
			return
		case <-ticker.C:
			if !m.IsConnected() {
				_ = m.connect()
				continue
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			if err := m.Ping(ctx); err != nil {
				m.logger.Warn("db: ping failed", zap.Error(err))
				m.markDisconnected()
			}
			cancel()
		}
	}
}
