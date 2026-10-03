// Package appctx holds the request-context key under which nexus stashes the
// *App for code that runs inside a request but can't have the app threaded
// through its signature — a ResponseRenderer, an extension's dashboard route.
// It lives in internal so first-party extensions can read it without the key
// becoming part of the public API.
package appctx

// Key is the httpx.Ctx key holding the request's *nexus.App. Read it with
// c.Get(appctx.Key) and assert *nexus.App.
const Key = "nexus.app"
