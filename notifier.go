package nexus

import "github.com/paulmanoni/nexus/live"

// Notifier is the cross-session fan-out primitive used by the
// live template engine: Notify() wakes every connected session
// so a mutation in one tab triggers a re-render in every other.
//
// This is a type alias for live.Notifier — re-exported here so
// user code that just wants to depend on the notifier in a
// constructor doesn't need to also import the live sub-package.
// Same type, methods, and identity; *nexus.Notifier and
// *live.Notifier are interchangeable.
//
// fxEarlyOptions auto-provides a singleton, so a constructor
// can just take a *Notifier param and fx fills it in.
type Notifier = live.Notifier

// NewNotifier constructs a fresh Notifier. Useful for tests or
// for apps that want to drive notifications outside the fx
// container. Production apps don't need to call this — nexus.Run
// puts a singleton into the graph via fxEarlyOptions.
func NewNotifier() *Notifier { return live.New() }
