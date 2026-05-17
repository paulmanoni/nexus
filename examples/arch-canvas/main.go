// Command arch-canvas demonstrates wrapping VueFlow as an
// nl-island inside a live-template page. This is the
// architecture-canvas pattern for the dashboard rewrite: keep
// the rich-client widget as Vue (no reimplementation in vanilla
// JS or live-template), let the surrounding page — toolbar,
// status bar, drawers — be live-template.
//
// What you see when you boot it:
//
//   - http://localhost:8082 SSRs the page chrome + an empty
//     div carrying nl-island="ArchCanvas". The browser fetches
//     /islands/vueflow-canvas.js (one fetch; the file's two
//     esm.sh imports follow), bootstraps Vue inside that div.
//   - Toolbar buttons mutate server-side Graph state. Each
//     mutation calls notifier.Notify(), the live session re-
//     renders, the :props JSON on <nl-island/> changes, the
//     island's updated() callback reactively swaps the
//     nodes/edges refs, VueFlow incremental-updates the SVG.
//     Pan / zoom / selection state survives.
//   - "Focus first node" exercises the server → island push
//     path: ctx.PushIsland("ArchCanvas", "focus-node", {id})
//     fires the island's channel.on("focus-node") listener.
//
// Run:
//
//	go run ./examples/arch-canvas
package main

import (
	"embed"
	"fmt"
	"math/rand/v2"
	"strconv"
	"sync"
	"time"

	"github.com/paulmanoni/nexus"
	"github.com/paulmanoni/nexus/live/template"
)

//go:embed templates/*.nlt islands/*.js
var assets embed.FS

// ArchNode / ArchEdge use lowercase JSON tags so the wire
// format matches what the VueFlow island expects (id, label,
// x, y) without a translation table on the JS side.
type ArchNode struct {
	ID    string  `json:"id"`
	Label string  `json:"label"`
	X     float64 `json:"x"`
	Y     float64 `json:"y"`
}

type ArchEdge struct {
	ID       string `json:"id"`
	Source   string `json:"source"`
	Target   string `json:"target"`
	Animated bool   `json:"animated,omitempty"`
}

// Graph is the canonical server-side shape. Lives on the
// component so per-session mutations don't bleed across tabs;
// a real dashboard would back this with a shared registry +
// the notifier for cross-tab sync (see examples/live).
type Graph struct {
	Nodes []ArchNode
	Edges []ArchEdge
}

// ArchPage is the live component for /. Each session starts
// with the seeded graph; toolbar actions mutate the local
// Graph and rely on Ctx.Notify to fire a re-render. The
// :props value on <nl-island/> JSON-encodes the new graph
// shape; the island's updated() picks it up.
type ArchPage struct {
	template.BaseComponent

	mu    sync.Mutex
	Graph Graph

	// nextNodeID / nextEdgeID feed monotonic IDs so VueFlow's
	// keyed reconciliation stays stable across mutations.
	nextNodeID int
	nextEdgeID int
}

func (p *ArchPage) Mount(_ *template.Ctx) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	// Seed with a small graph so the canvas isn't empty on
	// first paint. Three connected nodes is enough to show
	// the layout + edges + selection.
	p.nextNodeID = 3
	p.nextEdgeID = 2
	p.Graph = Graph{
		Nodes: []ArchNode{
			{ID: "n1", Label: "auth", X: 50, Y: 50},
			{ID: "n2", Label: "api", X: 280, Y: 50},
			{ID: "n3", Label: "db", X: 520, Y: 50},
		},
		Edges: []ArchEdge{
			{ID: "e1", Source: "n1", Target: "n2", Animated: true},
			{ID: "e2", Source: "n2", Target: "n3"},
		},
	}
	return nil
}

// GraphProps is what :nl-island-props evaluates to on every
// render. The engine JSON-encodes the returned map; the
// island's mount/updated callbacks receive the parsed value.
func (p *ArchPage) GraphProps() map[string]any {
	p.mu.Lock()
	defer p.mu.Unlock()
	return map[string]any{
		"nodes": p.Graph.Nodes,
		"edges": p.Graph.Edges,
	}
}

// AddNode appends a new node at a random position. Connects
// it to the previous node so the graph stays connected; that
// makes the layout interesting after several adds.
func (p *ArchPage) AddNode(_ *template.Ctx) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.nextNodeID++
	id := "n" + strconv.Itoa(p.nextNodeID)
	label := fmt.Sprintf("svc-%d", p.nextNodeID)
	x := 50 + rand.Float64()*500
	y := 50 + rand.Float64()*300
	p.Graph.Nodes = append(p.Graph.Nodes, ArchNode{ID: id, Label: label, X: x, Y: y})
	// Auto-link to the previous-last node so the new node
	// shows up connected — a totally orphan node would
	// confuse the demo.
	if len(p.Graph.Nodes) >= 2 {
		prev := p.Graph.Nodes[len(p.Graph.Nodes)-2]
		p.nextEdgeID++
		p.Graph.Edges = append(p.Graph.Edges, ArchEdge{
			ID:     "e" + strconv.Itoa(p.nextEdgeID),
			Source: prev.ID,
			Target: id,
		})
	}
}

// AddEdge wires a random pair of distinct nodes. No-op on
// graphs with fewer than two nodes.
func (p *ArchPage) AddEdge(_ *template.Ctx) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.Graph.Nodes) < 2 {
		return
	}
	src := p.Graph.Nodes[rand.IntN(len(p.Graph.Nodes))]
	tgt := p.Graph.Nodes[rand.IntN(len(p.Graph.Nodes))]
	// Avoid self-loops for clearer rendering; not strictly
	// required by VueFlow.
	if src.ID == tgt.ID {
		return
	}
	p.nextEdgeID++
	p.Graph.Edges = append(p.Graph.Edges, ArchEdge{
		ID:     "e" + strconv.Itoa(p.nextEdgeID),
		Source: src.ID,
		Target: tgt.ID,
	})
}

// Randomize jitters every node's position. The most visible
// demonstration that the island's updated() applies a partial
// diff — VueFlow animates the nodes to their new positions
// rather than tearing down + remounting.
func (p *ArchPage) Randomize(_ *template.Ctx) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := range p.Graph.Nodes {
		p.Graph.Nodes[i].X = 50 + rand.Float64()*500
		p.Graph.Nodes[i].Y = 50 + rand.Float64()*300
	}
}

// Reset reverts to the seeded three-node graph. Same effect as
// closing and reopening the tab; useful for poking around then
// restoring a clean state.
func (p *ArchPage) Reset(ctx *template.Ctx) {
	_ = p.Mount(ctx)
}

// FocusFirst exercises the server → island push path. The
// island's channel.on("focus-node") listener fires with the
// id payload; the demo logs to console rather than panning
// the view (real VueFlow integration would call setCenter()
// via the useVueFlow hook).
func (p *ArchPage) FocusFirst(ctx *template.Ctx) {
	p.mu.Lock()
	var id string
	if len(p.Graph.Nodes) > 0 {
		id = p.Graph.Nodes[0].ID
	}
	p.mu.Unlock()
	if id != "" {
		ctx.PushIsland("ArchCanvas", "focus-node", map[string]any{"id": id})
	}
}

var liveModule = nexus.Module("arch",
	template.Module(assets,
		// WithStatic serves the islands/ subdir at /islands/
		// — that's where the browser fetches
		// vueflow-canvas.js from.
		template.WithStatic("islands", ""),
		template.WithIdleTimeout(30*time.Minute),
		template.WithSessionResumption(30*time.Second),
	),
	nexus.AsComponent("Architecture",
		func() (*ArchPage, error) {
			return &ArchPage{}, nil
		},
		template.WithTemplate("templates/Architecture"),
		nexus.Path("/"),
	),
)

func main() {
	nexus.Run(
		nexus.Config{
			Server:    nexus.ServerConfig{Addr: ":8082"},
			Dashboard: nexus.DashboardConfig{Enabled: true, Name: "Arch canvas (live)"},
		},
		liveModule,
	)
}
