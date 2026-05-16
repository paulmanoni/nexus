// Command live is a runnable demo of nexus/live/template. It serves a
// posts page at / where every connection is a live session: clicking
// like, filtering, or adding a new post in one browser tab updates
// every other tab connected to the same URL via sparse diffs over a
// WebSocket. The shared PostsRepo + live.Notifier are the multi-tab
// sync mechanism — no global JS state, no GraphQL subscriptions.
//
// Run:
//
//	go run ./examples/live
//
// Then open http://localhost:8080 in two tabs and watch them stay in
// sync as you interact.
package main

import (
	_ "embed"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/paulmanoni/nexus/live"
	"github.com/paulmanoni/nexus/live/template"
)

//go:embed posts.nlt
var postsTemplate []byte

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
	notifier *live.Notifier
	nextID   atomic.Int64
}

func NewPostsRepo(n *live.Notifier, seed []Post) *PostsRepo {
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

// PostsList is the live component bound to posts.nlt. Each session
// holds its own instance — Filter and NewTitle are per-tab state —
// but they all read posts from the shared repo on every render.
//
// The repo dependency is injected via the factory passed to
// engine.Register, so this struct stays plain Go with no framework
// touchpoint other than the embedded BaseComponent.
type PostsList struct {
	template.BaseComponent
	repo *PostsRepo

	Filter   string
	NewTitle string
}

// Posts is what the template reads via {{ Posts() }} and nl-for. We
// don't store the slice on the struct because that would let it
// drift from the shared repo when another tab mutates state; instead
// we recompute from the repo on every render. The filter is applied
// here so the template stays declarative.
//
// Template authors call this as a method — {{ Posts() }} — because
// the evaluator resolves bare-identifier methods as function values,
// not as zero-arg calls. The parens are the "yes really, call it"
// signal.
func (c *PostsList) Posts() []Post {
	all := c.repo.All()
	if c.Filter == "" {
		return all
	}
	needle := strings.ToLower(c.Filter)
	out := all[:0]
	for _, p := range all {
		if strings.Contains(strings.ToLower(p.Title), needle) {
			out = append(out, p)
		}
	}
	return out
}

// Mount runs once per session at WS join. Nothing to seed here:
// Posts is computed from the repo on every render, so the very
// first render already shows the current data.
func (c *PostsList) Mount(ctx *template.Ctx) error { return nil }

// UpdateFilter handles every keystroke in the search input. Event
// name "updateFilter" maps to method "UpdateFilter" via title-casing
// in session.go.
func (c *PostsList) UpdateFilter(ctx *template.Ctx, p template.Payload) {
	c.Filter = p.String("value")
}

// UpdateTitle tracks the "new post" input. Stored per-session so the
// shared repo doesn't see draft state.
func (c *PostsList) UpdateTitle(ctx *template.Ctx, p template.Payload) {
	c.NewTitle = p.String("value")
}

// Like increments the like count on a post via the repo. The repo's
// notifier wakes every connected session, including this one — the
// next render pulls the new count from c.repo.All().
func (c *PostsList) Like(ctx *template.Ctx, p template.Payload) {
	c.repo.Like(p.Int("id"))
}

// Add creates a new post from the per-session draft. Author is a
// placeholder; a real app would pull it from Ctx (auth middleware
// populates it via Ctx.Params or a future Ctx.User).
func (c *PostsList) Add(ctx *template.Ctx, _ template.Payload) {
	c.repo.Add(c.NewTitle, "you")
	c.NewTitle = ""
}

func main() {
	notifier := live.New()
	repo := NewPostsRepo(notifier, []Post{
		{ID: 1, Title: "Welcome to live templates", Author: "system"},
		{ID: 2, Title: "Open this URL in two tabs and like things", Author: "system"},
		{ID: 3, Title: "Type below to add your own post", Author: "system"},
	})

	engine := template.New(template.WithNotifier(notifier))
	if err := engine.Register("Posts", postsTemplate, func() template.Component {
		return &PostsList{repo: repo}
	}); err != nil {
		log.Fatalf("register: %v", err)
	}

	mux := http.NewServeMux()
	mux.Handle(template.ScriptPath, engine.Script())
	mux.Handle("/", engine.Handler("Posts"))

	addr := ":8080"
	fmt.Printf("listening on http://localhost%s — open in two tabs\n", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatalf("serve: %v", err)
	}
}
