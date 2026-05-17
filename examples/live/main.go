// Command live is a runnable demo of nexus/live/template wired
// through the nexus.Run() entrypoint. Every framework touchpoint
// — the Notifier, the shared PostsRepo, the *template.Engine, the
// two component registrations, the HTTP routes — is declared as
// a nexus option; main() reduces to a single nexus.Run call.
//
// The pattern for each live page:
//
//	nexus.AsComponent("Posts",
//	    func(repo *PostsRepo) (*PostsList, error) {
//	        return &PostsList{repo: repo}, nil
//	    },
//	    template.WithTemplate("templates/posts"),
//	    nexus.Path("/"),
//	)
//
// Constructor params are resolved from the fx graph. The .nlt
// source is loaded from the embed.FS supplied to template.Module.
// nexus.Path mounts the engine's SSR/WS handler at the URL;
// omitting nexus.Path turns the registration into a child-only
// component (loadable from <Tag /> in another template but not
// reachable as a URL).
//
// Run:
//
//	go run ./examples/live
//
// Then open http://localhost:8080 in two tabs and watch them
// stay in sync as you interact. The dashboard is at
// http://localhost:8080/__nexus.
package main

import (
	"embed"
	"fmt"
	"html"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/paulmanoni/nexus"
	"github.com/paulmanoni/nexus/live/template"
)

//go:embed templates/*.nlt
var liveTemplates embed.FS

// Post is the domain row. Exported fields so the template can reach
// p.Title / p.Likes / p.Author through reflection.
type Post struct {
	ID     int
	Title  string
	Author string
	Likes  int
}

// PostsRepo is the shared store every session reads from on render.
// Every mutation calls notifier.Notify so other connected sessions
// re-render and see the change — that's how multi-tab sync works
// here without any pub/sub plumbing in the page code.
type PostsRepo struct {
	mu       sync.RWMutex
	posts    []Post
	notifier *nexus.Notifier
	nextID   atomic.Int64
}

// NewPostsRepo is the fx-friendly constructor. It depends on
// *nexus.Notifier (provided by liveModule) and seeds the store
// with a few system posts so the page isn't empty on first load.
func NewPostsRepo(n *nexus.Notifier) *PostsRepo {
	seed := []Post{
		{ID: 1, Title: "Welcome to live templates", Author: "system"},
		{ID: 2, Title: "Open this URL in two tabs and like things", Author: "system"},
		{ID: 3, Title: "Type below to add your own post", Author: "system"},
	}
	r := &PostsRepo{notifier: n, posts: append([]Post(nil), seed...)}
	maxID := int64(0)
	for _, p := range seed {
		if int64(p.ID) > maxID {
			maxID = int64(p.ID)
		}
	}
	r.nextID.Store(maxID)
	return r
}

// All returns a snapshot copy so callers can iterate without holding
// the lock. Cheap because Post is a small value type.
func (r *PostsRepo) All() []Post {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]Post(nil), r.posts...)
}

func (r *PostsRepo) Like(id int) {
	r.mu.Lock()
	for i := range r.posts {
		if r.posts[i].ID == id {
			r.posts[i].Likes++
			break
		}
	}
	r.mu.Unlock()
	r.notifier.Notify()
}

func (r *PostsRepo) Add(title, author string) {
	title = strings.TrimSpace(title)
	if title == "" {
		return
	}
	r.mu.Lock()
	id := int(r.nextID.Add(1))
	r.posts = append(r.posts, Post{ID: id, Title: title, Author: author, Likes: 0})
	r.mu.Unlock()
	r.notifier.Notify()
}

// PostsList is the live component bound to templates/Posts.nlt.
// Each session holds its own instance — Filter and NewTitle are
// per-tab state — but they all read posts from the shared repo
// on every render.
//
// The repo dependency lands via the AsComponent constructor below
// (fx resolves *PostsRepo and passes it in); the struct itself
// stays plain Go with no framework touchpoint other than the
// embedded BaseComponent.
type PostsList struct {
	template.BaseComponent
	repo *PostsRepo

	Posts    []Post
	Filter   string
	NewTitle string
}

func (c *PostsList) Mount(_ *template.Ctx) error { return nil }

// Refresh pulls the current posts from the shared repo and applies
// the per-session filter. The session calls this before every
// render — both event-triggered and notifier-triggered — so the
// template can read {{ Posts }} as a plain field instead of
// {{ Posts() }} as a method call.
func (c *PostsList) Refresh(_ *template.Ctx) error {
	all := c.repo.All()
	if c.Filter == "" {
		c.Posts = all
		return nil
	}
	needle := strings.ToLower(c.Filter)
	out := all[:0]
	for _, p := range all {
		if strings.Contains(strings.ToLower(p.Title), needle) {
			out = append(out, p)
		}
	}
	c.Posts = out
	return nil
}

// Like increments the like count on a post via the repo. The repo's
// notifier wakes every connected session, including this one — the
// next render pulls the new count from c.repo.All().
//
// The like button lives on the child PostRow component, but events
// always bubble up to the top-level live component (PostsList) for
// handling — child components are pure renders in v1 and don't
// own state or event handlers. The :data-id="Post.ID" on the
// button carries the row identity in the payload.
//
// Also appends to the local Activity stream so this tab sees what
// it just did. ctx.Stream emits a single stream-op frame — no
// re-render of the surrounding template — which is the whole
// point of nl-stream: append-cheap activity feeds.
func (c *PostsList) Like(ctx *template.Ctx, p template.Payload) {
	id := p.Int("id")
	c.repo.Like(id)
	pushActivity(ctx, fmt.Sprintf("liked post #%d", id))
}

// Add creates a new post from the per-session draft. Author is a
// placeholder; a real app would pull it from Ctx (auth middleware
// populates it via Ctx.Params or a future Ctx.User).
func (c *PostsList) Add(ctx *template.Ctx, _ template.Payload) {
	title := strings.TrimSpace(c.NewTitle)
	c.repo.Add(title, "you")
	c.NewTitle = ""
	if title != "" {
		pushActivity(ctx, "added: "+title)
	}
}

// ClearFilter blanks the Filter field. Wired to the filter
// input's @keydown.escape so pressing Esc resets the search.
// The framework's __model handler updates Filter on every
// keystroke; this method just zeroes it server-side and the
// next diff propagates the empty value back to the input.
func (c *PostsList) ClearFilter(_ *template.Ctx) {
	c.Filter = ""
}

// pushActivity formats and ships one log line to the
// "activity" stream container. Pulls a fresh ID from a
// process-wide counter — ids only need to be unique within
// the stream container; overlap across sessions is fine since
// each session has its own container.
var activitySeq atomic.Uint64

func pushActivity(ctx *template.Ctx, msg string) {
	id := fmt.Sprintf("act-%d", activitySeq.Add(1))
	li := fmt.Sprintf(`<li id="%s">%s</li>`, id, html.EscapeString(msg))
	ctx.Stream("activity").Append(id, li)
}

// About is the static second page used to demonstrate
// live-navigate. It has no state, no handlers — just a
// rendered fragment with a back link.
type About struct {
	template.BaseComponent
}

func NewAbout() (*About, error) { return &About{}, nil }

// PostRow is the child component rendering one row of the list.
// It has no state or handlers — just a Post prop the parent passes
// down. The like button's @click event bubbles up to the parent's
// Like handler with the post ID in the payload.
type PostRow struct {
	template.BaseComponent
	Post Post
}

func NewPostRow() (*PostRow, error) {
	return &PostRow{}, nil
}

func (c *PostRow) Mount(_ *template.Ctx) error   { return nil }
func (c *PostRow) Refresh(_ *template.Ctx) error { return nil }

// liveModule is the entire wiring surface: providers for the
// notifier and the repo, the template engine module, and one
// AsComponent registration per component. PostRow has no
// nexus.Path option, so it's child-only — referenced from
// Posts.nlt's <PostRow /> tag but not reachable as a URL.
var liveModule = nexus.Module("posts",
	nexus.Provide(NewPostsRepo),
	template.Module(liveTemplates,
		// Production knobs: drop idle tabs after 30 minutes, keep
		// disconnected sessions around for 30 seconds so a
		// network blip doesn't wipe Filter/NewTitle.
		template.WithIdleTimeout(30*time.Minute),
		template.WithSessionResumption(30*time.Second),
	),
	nexus.AsComponent("Posts", func(repo *PostsRepo) (*PostsList, error) {
		return &PostsList{repo: repo}, nil
	}, template.WithTemplate("templates/Posts"), nexus.Path("/")),

	nexus.AsComponent("PostRow", NewPostRow, template.WithTemplate("templates/PostRow")),

	// Second top-level page reachable via <a nl-navigate
	// href="/about">; the click stays inside the live WS
	// channel instead of doing a full reload.
	nexus.AsComponent("About", NewAbout,
		template.WithTemplate("templates/About"),
		nexus.Path("/about"),
	),
)

func main() {
	opts := []nexus.Option{liveModule}
	// Dev-only: watch templates/ on disk and broadcast a reload
	// frame to every connected tab when a .nlt changes. Gate
	// inclusion so production binaries don't open file watchers
	// they can't use (the templates ship inside the embed.FS).
	if os.Getenv("NEXUS_DEV") == "1" {
		opts = append(opts, template.HotReload("examples/live/templates"))
	}
	nexus.Run(
		nexus.Config{
			Server:    nexus.ServerConfig{Addr: ":8080"},
			Dashboard: nexus.DashboardConfig{Enabled: true, Name: "Posts (live)"},
		},
		opts...,
	)
}
