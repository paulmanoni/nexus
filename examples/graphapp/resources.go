package main

import (
	"go.uber.org/fx"

	"nexus"
	"nexus/multi"
	"nexus/resource"
)

// ProvideDBs wires the multi-DB router into the Fx graph. It constructs both
// named instances internally so each one doesn't clash as "*DB" in the
// graph's type registry. Resolvers ask for *DBManager (alias for
// *multi.Registry[*DB]) and route via .Using(name). "main" is the default;
// .Using("") returns it.
func ProvideDBs() *DBManager {
	r := multi.New[*DB]().
		Register("main", NewMainDB(), multi.AsDefault[*DB]()).
		Register("questions", NewQuestionsDB())
	return newDBManager(r)
}

// resourcesModule declares every external resource the app knows about. It
// iterates the multi.Registry via Each so adding a third DB in ProvideDBs
// automatically surfaces as a new dashboard card. It also installs an
// auto-attach hook: any resolver calling dbs.UsingCtx(p.Context, "name")
// attaches that resource to the current service — no manual graphfx.Use.
var resourcesModule = fx.Module("resources",
	fx.Invoke(func(app *nexus.App, dbs *DBManager, cache *CacheManager) {
		app.OnResourceUse(dbs) // auto-attach service→DB on first UsingCtx call
		dbs.Each(func(name string, m *DB) {
			opts := []resource.Option{}
			if name == dbs.DefaultName() {
				opts = append(opts, resource.AsDefault())
			}
			driver := string(m.Driver())
			app.Register(resource.NewDatabase(
				name,
				"GORM — "+driver,
				map[string]any{"engine": driver, "schema": name},
				m.IsConnected,
				opts...,
			))
		})

		app.Register(resource.NewCache(
			"session", "Redis + in-memory fallback",
			map[string]any{"ttl": "30m"},
			cache.IsRedisConnected,
			resource.AsDefault(),
			resource.WithDetails(func() map[string]any {
				backend := "memory"
				if cache.IsRedisConnected() {
					backend = "redis"
				}
				return map[string]any{"backend": backend, "ttl": "30m"}
			}),
		))
	}),
)
