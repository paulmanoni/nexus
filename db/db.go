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
	"log/slog"
	"net"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/failsafe-go/failsafe-go"
	"github.com/failsafe-go/failsafe-go/circuitbreaker"
	"github.com/failsafe-go/failsafe-go/retrypolicy"
	"github.com/paulmanoni/nexus/v2/resource/connlog"

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
	TimeZone string // IANA TZ: Postgres's session zone, MySQL's loc ("" = the machine's)

	// Session sets server settings on every connection the pool opens:
	// MySQL system variables (foreign_key_checks, sql_mode), Postgres
	// run-time parameters (search_path, statement_timeout), SQLite pragmas
	// (foreign_keys). Values are plain — nexus quotes them for the driver.
	// [databases.<name>.session] in nexus.toml.
	Session map[string]string

	// InterpolateParams is MySQL's: nil or true has the driver escape a
	// query's arguments into its text and send it in one round trip (as
	// Django's MySQL backend does); false prepares each statement on the
	// server first, two round trips. Safe because the DSN forces utf8mb4,
	// never a charset whose escaping can be bypassed. Set on another
	// driver it fails Validate. [databases.<name>] interpolate_params.
	InterpolateParams *bool

	// Pool overrides the driver's default pool field by field: a zero
	// field keeps the default, a negative one lifts the limit.
	// [databases.<name>] max_open / max_idle / conn_max_lifetime /
	// conn_max_idle_time in nexus.toml.
	Pool PoolConfig

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
		q := pgDSNValue
		return fmt.Sprintf("host=%s user=%s password=%s dbname=%s port=%s sslmode=%s TimeZone=%s",
			q(c.Host), q(c.User), q(c.Password), q(c.Database), q(c.Port), q(ssl), q(tz)) + c.postgresSession()
	case MySQL:
		// The driver unescapes the name, so a '?' in it can't add
		// parameters (allowAllFiles=true and the like).
		// loc is the zone the driver reads DATETIMEs in and writes times
		// as: the machine's unless TimeZone says (Django with USE_TZ
		// stores UTC).
		loc := "Local"
		if c.TimeZone != "" {
			loc = url.QueryEscape(c.TimeZone)
		}
		interpolate := ""
		if c.InterpolateParams == nil || *c.InterpolateParams {
			interpolate = "&interpolateParams=true"
		}
		return fmt.Sprintf("%s:%s@tcp(%s:%s)/%s?charset=utf8mb4&parseTime=True&loc=%s%s",
			c.User, c.Password, c.Host, c.Port, url.PathEscape(c.Database), loc, interpolate) + c.mysqlSession()
	case SQLite:
		return c.sqliteDSN()
	}
	return ""
}

// pgDSNValue is a keyword/value connection string value: quoted when it
// is empty or holds whitespace (each byte pgx ends a bare value at: \r,
// \v and \f too), a quote or a backslash, so an empty password can't
// swallow the next keyword and a password can't add keywords.
func pgDSNValue(v string) string {
	if v != "" && !strings.ContainsAny(v, " \t\n\r\v\f'\\") {
		return v
	}
	return "'" + strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(v) + "'"
}

// Driver registry — the database/sql pattern. Importing nexus/db links
// NO engine; each driver is a blank-import subpackage:
//
//	_ "github.com/paulmanoni/nexus/v2/db/postgres"
//	_ "github.com/paulmanoni/nexus/v2/db/mysql"
//	_ "github.com/paulmanoni/nexus/v2/db/sqlite"
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
	return fmt.Sprintf("db: driver %q is not linked into this binary — add the blank import:\n\n\t_ \"github.com/paulmanoni/nexus/v2/db/%s\"\n\n(drivers became opt-in so a Postgres app no longer ships the SQLite engine, and vice versa)", d, d)
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

// poolFor is the driver's default pool with cfg.Pool's set fields over
// it; database/sql reads a value <= 0 as "no limit" (no idle kept, for
// MaxIdle).
func poolFor(cfg Config) PoolConfig {
	p, o := defaultPool(cfg), cfg.Pool
	if o.MaxOpen != 0 {
		p.MaxOpen = max(o.MaxOpen, 0)
	}
	if o.MaxIdle != 0 {
		p.MaxIdle = max(o.MaxIdle, 0)
	}
	if o.ConnMaxLife != 0 {
		p.ConnMaxLife = max(o.ConnMaxLife, 0)
	}
	if o.ConnMaxIdle != 0 {
		p.ConnMaxIdle = max(o.ConnMaxIdle, 0)
	}
	return p
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
	logger      *slog.Logger
	executor    failsafe.Executor[*gorm.DB]
	mu          sync.RWMutex
	db          *gorm.DB
	isConnected bool
	ctx         context.Context
	cancel      context.CancelFunc

	// avail shapes the connect/ping failures that repeat on every
	// maintain() tick into state transitions: one line when the server goes
	// down, widening still-down heartbeats, one line on recovery.
	availOnce sync.Once
	availT    *connlog.Transition

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

// WithLogger attaches a *slog.Logger. Without one, Manager runs silently.
func WithLogger(l *slog.Logger) Option {
	return func(m *Manager) {
		if l != nil {
			m.logger = l
		}
	}
}

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

// Name is the name the Manager was bound under (db.Bind's), empty when
// it was opened directly.
func (m *Manager) Name() string { return m.bindName }

// NewManager builds a Manager without connecting. Call Open or Start next.
func NewManager(cfg Config, opts ...Option) *Manager {
	ctx, cancel := context.WithCancel(context.Background())
	m := &Manager{
		cfg:    cfg,
		pool:   poolFor(cfg),
		logger: slog.New(slog.DiscardHandler),
		ctx:    ctx,
		cancel: cancel,
	}
	for _, opt := range opts {
		opt(m)
	}
	// Built after the options so a WithLogger reaches the default executor
	// (its OnRetry hook), unless WithExecutor supplied one.
	if m.executor == nil {
		m.executor = defaultExecutor(m.logger)
	}
	return m
}

// defaultExecutor is the exact retry + circuit-breaker profile oats uses.
// Infinite retries with exponential backoff (500ms → 2s, 25ms jitter), and
// a circuit breaker that opens after 10 consecutive failures for 10 seconds.
func defaultExecutor(logger *slog.Logger) failsafe.Executor[*gorm.DB] {
	retry := retrypolicy.NewBuilder[*gorm.DB]().
		WithDelay(500*time.Millisecond).
		WithBackoff(2, time.Second).
		WithJitter(25 * time.Millisecond).
		OnRetry(func(e failsafe.ExecutionEvent[*gorm.DB]) {
			// Debug, not Warn: the policy is still retrying, so this is
			// progress rather than a problem. The Manager logs once when
			// the attempts are exhausted, which is the reportable event.
			logger.Debug("db: retrying connect",
				slog.Int("attempt", e.Attempts()),
				slog.Any("error", e.LastError()))
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
	if err := m.cfg.Validate(); err != nil {
		m.markDisconnected()
		return err
	}
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
	fields := []slog.Attr{
		slog.String("name", m.bindName),
		slog.String("driver", string(m.cfg.Driver)),
		slog.String("address", m.cfg.Address()),
	}
	if ev, tf := m.avail().OK(); ev == connlog.EventRecovered {
		// The outage's shape closes the story the down/still-down lines told.
		m.logger.LogAttrs(context.Background(), slog.LevelInfo, "db: reconnected", append(fields, tf...)...)
	} else {
		m.logger.LogAttrs(context.Background(), slog.LevelInfo, "db: connected", fields...)
	}
	return nil
}

// avail returns the availability tracker, named after the binding.
func (m *Manager) avail() *connlog.Transition {
	m.availOnce.Do(func() { m.availT = connlog.NewTransition("db:" + m.bindName) })
	return m.availT
}

// reportUnreachable logs a connect failure as a state transition — the
// moment the server goes down, then widening still-down heartbeats — naming
// the database, where it looked, and what to do about it. The maintain()
// loop retries every 5 seconds, so the unguarded version reprinted the same
// line twelve times a minute per database for as long as the server was
// down — with six databases configured that buried everything else.
func (m *Manager) reportUnreachable(err error) {
	if connlog.IsRetryState(err) {
		return
	}
	ev, tf := m.avail().Fail(err)
	if ev == connlog.EventNone {
		return
	}
	addr := m.cfg.Address()
	fields := []slog.Attr{
		slog.String("name", m.bindName),
		slog.String("driver", string(m.cfg.Driver)),
		slog.String("database", m.cfg.Database),
		slog.String("address", addr),
		slog.String("error", connlog.Cause(err)),
	}
	if hint := connlog.Hint(err, string(m.cfg.Driver), addr); hint != "" {
		fields = append(fields, slog.String("fix", hint))
	}
	fields = append(fields, tf...)
	msg := "db: cannot reach the server, retrying in the background"
	if ev == connlog.EventStillDown {
		msg = "db: still unreachable, retrying in the background"
	}
	m.logger.LogAttrs(context.Background(), slog.LevelWarn, msg, fields...)
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
				// Feed the availability tracker so this is the outage's ONE
				// "went down" line; the connect retries that follow continue
				// as still-down heartbeats instead of re-announcing.
				if ev, tf := m.avail().Fail(err); ev != connlog.EventNone {
					m.logger.LogAttrs(context.Background(), slog.LevelWarn, "db: connection lost", append([]slog.Attr{
						slog.String("name", m.bindName), slog.Any("error", err),
					}, tf...)...)
				}
				m.markDisconnected()
			}
			cancel()
		}
	}
}
