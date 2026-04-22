package main

import (
	"context"
	"sync"
	"time"

	"nexus/db"
	"nexus/multi"
)

// DB is a project-local alias for nexus/db.Manager, so resolver code and
// wiring use a domain name ("DB") while the underlying type is the full
// multi-driver manager with GORM + failsafe circuit breaker + retry.
type DB = db.Manager

// DBManager wraps a multi.Registry[*DB] AND embeds the current default *DB
// as a field so callers that don't care about routing can skip .Using(...):
//
//	dbs.GetDB().Create(advert)                   // default DB (promoted from *DB)
//	dbs.Using("questions").GetDB().Find(&qs)     // routed explicitly
//
// Because DB is an alias for db.Manager, .GetDB() returns *gorm.DB; from
// there you run normal GORM queries.
type DBManager struct {
	*multi.Registry[*DB] // .Using, .Default, .Names, .Each
	*DB                  // promoted: GetDB, IsConnected, Driver, Start, Stop, Ping
}

func newDBManager(r *multi.Registry[*DB]) *DBManager {
	return &DBManager{Registry: r, DB: r.Default()}
}

// Models ---------------------------------------------------------------------

// Advert is the adverts table row. GORM tags drive AutoMigrate.
type Advert struct {
	ID           uint   `gorm:"primaryKey" json:"id"`
	Title        string `gorm:"size:200"   json:"title"`
	EmployerName string `gorm:"size:200"   json:"employerName"`
}

type Question struct {
	ID   uint   `gorm:"primaryKey" json:"id"`
	Text string `gorm:"size:500"   json:"text"`
}

// GraphQL responses — concrete types (generics like Response[[]Advert]
// produce names go-graph's reflection can't emit as valid GraphQL types).

type AdvertsResponse struct {
	Status  string   `json:"status"`
	Message string   `json:"message"`
	Data    []Advert `json:"data"`
}

type AdvertResponse struct {
	Status  string `json:"status"`
	Message string `json:"message"`
	Data    Advert `json:"data"`
}

type QuestionsResponse struct {
	Status  string     `json:"status"`
	Message string     `json:"message"`
	Data    []Question `json:"data"`
}

func okList(data []Advert, msg string) *AdvertsResponse {
	return &AdvertsResponse{Status: "SUCCESS", Message: msg, Data: data}
}
func okOne(data Advert, msg string) *AdvertResponse {
	return &AdvertResponse{Status: "SUCCESS", Message: msg, Data: data}
}
func okQuestions(data []Question, msg string) *QuestionsResponse {
	return &QuestionsResponse{Status: "SUCCESS", Message: msg, Data: data}
}

// DB constructors ------------------------------------------------------------
//
// Real SQLite via github.com/glebarez/sqlite (pure Go; no CGO). Each .Open
// call creates a separate in-memory database because the DSNs share no
// cache — "main" rows never leak into "questions".

func NewMainDB() *DB {
	m, err := db.Open(db.Config{Driver: db.SQLite, Database: ":memory:"})
	if err != nil {
		panic("main db: " + err.Error())
	}
	m.Start()
	g := m.GetDB()
	if err := g.AutoMigrate(&Advert{}); err != nil {
		panic("main migrate: " + err.Error())
	}
	g.Create(&[]Advert{
		{Title: "Software Engineer", EmployerName: "Acme"},
		{Title: "DevOps Lead", EmployerName: "Globex"},
	})
	return m
}

func NewQuestionsDB() *DB {
	m, err := db.Open(db.Config{Driver: db.SQLite, Database: ":memory:"})
	if err != nil {
		panic("questions db: " + err.Error())
	}
	m.Start()
	g := m.GetDB()
	if err := g.AutoMigrate(&Question{}); err != nil {
		panic("questions migrate: " + err.Error())
	}
	g.Create(&[]Question{
		{Text: "What is the time complexity of hash lookup?"},
		{Text: "Explain CAP theorem in one sentence."},
		{Text: "Why prefer composition over inheritance?"},
	})
	return m
}

// CacheManager ---------------------------------------------------------------

type CacheManager struct {
	mu    sync.RWMutex
	items map[string]any
	exp   map[string]time.Time
}

func NewCacheManager() *CacheManager {
	return &CacheManager{items: map[string]any{}, exp: map[string]time.Time{}}
}

func (c *CacheManager) IsRedisConnected() bool { return false }

func (c *CacheManager) Get(_ context.Context, key string) (any, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if t, ok := c.exp[key]; ok && time.Now().After(t) {
		return nil, false
	}
	v, ok := c.items[key]
	return v, ok
}

func (c *CacheManager) Set(_ context.Context, key string, val any, ttl time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.items[key] = val
	c.exp[key] = time.Now().Add(ttl)
	return nil
}

func (c *CacheManager) Invalidate(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.items, key)
	delete(c.exp, key)
}
