// Package db is nexus's driver-agnostic GORM manager. It mirrors the shape
// of the legacy DBManager (Start/Stop/GetDB/IsConnected)
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
	"net"
	"strings"
	"sync"
	"time"

	"github.com/failsafe-go/failsafe-go"
	"github.com/failsafe-go/failsafe-go/circuitbreaker"
	"github.com/failsafe-go/failsafe-go/retrypolicy"
	"github.com/paulmanoni/nexus/internal/logx"
	"go.uber.org/zap"

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

	// LogLevel controls SQL/GORM logging. Empty is auto — warn-level under
	// `nexus dev` / a development environment, silent otherwise (a
	// production binary stays quiet by default). Override with
	// "silent"/"false"/"off", "error", "warn"/"true"/"on", or "info"/"all".
	// Case-insensitive. See resolveGormLogger.
	LogLevel string
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

// Driver registry — the database/sql pattern. Importing nexus/db links
// NO engine; each driver is a blank-import subpackage:
//
//	_ "github.com/paulmanoni/nexus/db/postgres"
//	_ "github.com/paulmanoni/nexus/db/mysql"
//	_ "github.com/paulmanoni/nexus/db/sqlite"
//
// Before this, db.go imported all three unconditionally, so every app
// with a database linked the transpiled-C SQLite engine (~5MB), the
// MySQL driver, and pgx — regardless of which one it opened.
var (
	dialectorMu sync.RWMutex
	dialectors  = map[Driver]func(dsn string) gorm.Dialector{}
)

// RegisterDriver installs the dialector constructor for a driver. The
// db/sqlite, db/mysql and db/postgres subpackages call it from init();
// custom gorm dialects may register their own Driver name the same way.
func RegisterDriver(d Driver, open func(dsn string) gorm.Dialector) {
	dialectorMu.Lock()
	dialectors[d] = open
	dialectorMu.Unlock()
}

// driverLinked reports whether a dialector is registered for d.
func driverLinked(d Driver) bool {
	dialectorMu.RLock()
	defer dialectorMu.RUnlock()
	return dialectors[d] != nil
}

// Dialector returns the gorm dialector matching Driver.
// Address is the host:port the driver dials, in a form that is safe to log —
// unlike DSN, which carries the password.
func (c Config) Address() string {
	if c.Driver == SQLite {
		return c.Database
	}
	host := c.Host
	if host == "" {
		host = "localhost"
	}
	if c.Port == "" {
		return host
	}
	return net.JoinHostPort(host, c.Port)
}

func (c Config) Dialector() gorm.Dialector {
	dialectorMu.RLock()
	open := dialectors[c.Driver]
	dialectorMu.RUnlock()
	if open == nil {
		panic(missingDriverMsg(c.Driver))
	}
	return open(c.DSN())
}

func missingDriverMsg(d Driver) string {
	return fmt.Sprintf("db: driver %q is not linked into this binary — add the blank import:\n\n\t_ \"github.com/paulmanoni/nexus/db/%s\"\n\n(drivers became opt-in so a Postgres app no longer ships the SQLite engine, and vice versa)", d, d)
}

// PoolConfig tunes the underlying *sql.DB pool. Postgres/MySQL get a
// server-sized pool; sqlite :memory: gets MaxOpen=1 so every goroutine
// shares the one in-memory database; FILE-backed sqlite gets a small
// pool — with WAL (which the scaffold DSNs enable) concurrent readers
// are the point, and forcing one connection made every query in the
// process queue behind every other.
type PoolConfig struct {
	MaxIdle     int
	MaxOpen     int
	ConnMaxLife time.Duration
	ConnMaxIdle time.Duration
}

func defaultPool(cfg Config) PoolConfig {
	if cfg.Driver == SQLite {
		if strings.Contains(cfg.DSN(), ":memory:") {
			return PoolConfig{MaxIdle: 1, MaxOpen: 1, ConnMaxLife: 0, ConnMaxIdle: 0}
		}
		// Modest by design: SQLite has one writer at a time, so a wide
		// pool only helps reads. Set a busy_timeout pragma in the DSN
		// (the scaffolds do) so a write finding the file locked waits
		// instead of failing with SQLITE_BUSY.
		return PoolConfig{MaxIdle: 2, MaxOpen: 4, ConnMaxLife: 0, ConnMaxIdle: 0}
	}
	return PoolConfig{
		MaxIdle:     10,
		MaxOpen:     100,
		ConnMaxLife: time.Hour,
		ConnMaxIdle: 30 * time.Minute,
	}
}

// Manager owns one *gorm.DB, reconnects in the background, and exposes the
// same method set as the legacy DatabaseManager interface so it can
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

	// downFor collapses the connect failure that repeats on every
	// maintain() tick while a server is unreachable.
	downFor logx.RepeatGuard

	// envNames + bindName drive NexusEnv / NexusServices when the
	// auto-walk fires. Populated via WithEnvNames / WithBindName
	// options (or zero, meaning "use defaults"). Static for the
	// life of the Manager — print mode reads these without
	// touching the live DB.
	envNames EnvNames
	bindName string
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

// WithEnvNames stamps the env-var names this Manager's Config came
// from onto the Manager so NexusEnv / NexusServices can declare
// them in the manifest. Empty fields fall back to DefaultEnvNames.
//
// Use when the app reads from env-var names that don't follow the
// framework convention (e.g. oats reads PASSWORD instead of
// DB_PASSWORD). Apps that build their Config via LoadConfig pass
// the same EnvNames here.
func WithEnvNames(names EnvNames) Option { return func(m *Manager) { m.envNames = names } }

// WithBindName names this Manager in the manifest. Defaults to
// "main"; multi-DB apps override per Manager so each shows up as
// a distinct slot in the orchestration canvas.
func WithBindName(name string) Option { return func(m *Manager) { m.bindName = name } }

// NewManager builds a Manager without connecting. Call Open or Start next.
func NewManager(cfg Config, opts ...Option) *Manager {
	ctx, cancel := context.WithCancel(context.Background())
	m := &Manager{
		cfg:    cfg,
		pool:   defaultPool(cfg),
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
		WithDelay(500*time.Millisecond).
		WithBackoff(2, time.Second).
		WithJitter(25 * time.Millisecond).
		OnRetry(func(e failsafe.ExecutionEvent[*gorm.DB]) {
			// Debug, not Warn: the policy is still retrying, so this is
			// progress rather than a problem. The Manager logs once when
			// the attempts are exhausted, which is the reportable event.
			logger.Debug("db: retrying connect",
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

// GetCtx returns the manager's internal context. It's canceled by Stop(),
// so goroutines that should die alongside the manager can bind to it:
//
//	go func() {
//	    <-mgr.GetCtx().Done()
//	    // manager has stopped; unwind our side here
//	}()
func (m *Manager) GetCtx() context.Context { return m.ctx }

// ConnectionString returns the DSN for this manager's configured driver
// (same string the Dialector is built from). Useful for diagnostics and
// for tooling that needs to reconnect outside GORM — migration CLIs,
// ad-hoc shell scripts, etc. Contains credentials in the Postgres/MySQL
// forms; don't log it in production.
func (m *Manager) ConnectionString() string { return m.cfg.DSN() }

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
		return gorm.Open(m.cfg.Dialector(), &gorm.Config{
			Logger: resolveGormLogger(m.cfg.LogLevel, m.logger),
		})
	})
	if err != nil {
		m.markDisconnected()
		m.reportUnreachable(err)
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
	// The next outage is news again.
	m.downFor.Reset()
	m.logger.Info("db: connected",
		zap.String("name", m.bindName),
		zap.String("driver", string(m.cfg.Driver)),
		zap.String("address", m.cfg.Address()))
	return nil
}

// reportUnreachable logs a connect failure at most once a minute per distinct
// error, naming the database, where it looked, and what to do about it. The
// maintain() loop retries every 5 seconds, so the unguarded version reprinted
// the same line twelve times a minute per database for as long as the server
// was down — with six databases configured that buried everything else.
func (m *Manager) reportUnreachable(err error) {
	if logx.IsRetryState(err) {
		return
	}
	addr := m.cfg.Address()
	suppressed, ok := m.downFor.Allow(logx.Signature(err), time.Minute)
	if !ok {
		return
	}
	fields := []zap.Field{
		zap.String("name", m.bindName),
		zap.String("driver", string(m.cfg.Driver)),
		zap.String("database", m.cfg.Database),
		zap.String("address", addr),
		zap.String("error", logx.Cause(err)),
	}
	if hint := logx.Hint(err, string(m.cfg.Driver), addr); hint != "" {
		fields = append(fields, zap.String("fix", hint))
	}
	if suppressed > 0 {
		fields = append(fields, zap.Int("repeated", suppressed))
	}
	m.logger.Warn("db: cannot reach the server, retrying in the background", fields...)
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
