package main

import (
	"context"
	"sync"
	"time"

	"github.com/paulmanoni/nexus/db"
	"github.com/paulmanoni/nexus/resource"
)

// DB is a project-local alias for nexus/db.Manager. Typed wrappers below
// (MainDB, QuestionsDB) embed it so resolvers can call .GetDB() / .Driver()
// / .IsConnected without unwrapping.
type DB = db.Manager

// MainDB is the typed handle for the primary DB. Resolvers that touch it
// take `*MainDB` by type — fx routes the right instance, and it's
// impossible to typo a string name to something that doesn't exist.
type MainDB struct{ *DB }

// NewMainDB seeds an in-memory SQLite with a couple of adverts. Its
// NexusResources method below declares the dashboard entry; the handler's
// *MainDB dep triggers the attachment edge.
func NewMainDB() *MainDB {
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
	return &MainDB{DB: m}
}

// NexusResources advertises this DB as the default database on the
// dashboard. fxmod's auto-mount registers it at boot; the resolver-dep
// scan attaches it to any service that takes *MainDB as a parameter.
func (m *MainDB) NexusResources() []resource.Resource {
	driver := string(m.Driver())
	return []resource.Resource{
		resource.NewDatabase(
			"main", "GORM — "+driver,
			map[string]any{"engine": driver, "schema": "main"},
			m.IsConnected,
			resource.AsDefault(),
		),
	}
}

// QuestionsDB holds the non-default DB so resolvers that touch it name it
// explicitly. Swapping it for a Postgres-backed manager later is a matter
// of changing this constructor — every caller stays compile-time identical.
type QuestionsDB struct{ *DB }

func NewQuestionsDB() *QuestionsDB {
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
	return &QuestionsDB{DB: m}
}

func (q *QuestionsDB) NexusResources() []resource.Resource {
	driver := string(q.Driver())
	return []resource.Resource{
		resource.NewDatabase(
			"questions", "GORM — "+driver,
			map[string]any{"engine": driver, "schema": "questions"},
			q.IsConnected,
		),
	}
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

// CacheManager ---------------------------------------------------------------

type CacheManager struct {
	mu    sync.RWMutex
	items map[string]any
	exp   map[string]time.Time
}

func NewCacheManager() *CacheManager {
	return &CacheManager{items: map[string]any{}, exp: map[string]time.Time{}}
}

// NexusResources returns this manager's session cache as a full
// resource.Resource — with health probing + dynamic details (backend
// flipping between redis/memory based on connectivity). nexus.ProvideResources
// uses the slice for boot registration and service-edge attachment alike.
func (c *CacheManager) NexusResources() []resource.Resource {
	return []resource.Resource{
		resource.NewCache(
			"session", "Redis + in-memory fallback",
			map[string]any{"ttl": "30m"},
			c.IsRedisConnected,
			resource.AsDefault(),
			resource.WithDetails(func() map[string]any {
				backend := "memory"
				if c.IsRedisConnected() {
					backend = "redis"
				}
				return map[string]any{"backend": backend, "ttl": "30m"}
			}),
		),
	}
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
