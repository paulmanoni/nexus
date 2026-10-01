package pets

import (
	"sort"

	"github.com/paulmanoni/nexus/view"
)

// The Vue islands under web/src/islands this package places on its pages.
var (
	KindChart     = view.NewIsland[ChartProps]("KindChart")
	AdoptionMeter = view.NewIsland[MeterProps]("AdoptionMeter")
)

// KindCount is one bar of the kinds chart.
type KindCount struct {
	Kind  string `json:"kind"`
	Count int    `json:"count"`
}

// ChartProps are KindChart's props. Query is the page's search signal,
// which the chart reads and sets.
type ChartProps struct {
	Kinds []KindCount          `json:"kinds"`
	Query *view.Signal[string] `json:"query"`
}

// MeterProps are AdoptionMeter's props.
type MeterProps struct {
	Adopted int `json:"adopted"`
	Total   int `json:"total"`
}

func kindChart(pets []Pet, query *view.Signal[string]) ChartProps {
	n := map[string]int{}
	for _, p := range pets {
		n[p.Kind]++
	}
	out := ChartProps{Query: query}
	for k, c := range n {
		out.Kinds = append(out.Kinds, KindCount{k, c})
	}
	sort.Slice(out.Kinds, func(i, j int) bool {
		a, b := out.Kinds[i], out.Kinds[j]
		return a.Count > b.Count || (a.Count == b.Count && a.Kind < b.Kind)
	})
	return out
}
