package orm

import (
	"context"

	"github.com/paulmanoni/nexus/v2/trace"
)

// A query the ORM refuses never reaches the database, so no sql span
// records it. Under nexus dev each call that can send statements notes
// whether it sent one; failing without having sent any, it records the
// refusal as a failed sql span of the request — the debug toolbar and the
// dashboard's traces show it with the ORM's error.

type runKey struct{}

// runs is ctx noting whether a statement ran under it, and the note; nil
// when not watched (outside nexus dev or a traced request) or when an
// enclosing call already watches it.
func runs(ctx context.Context) (context.Context, *bool) {
	if !devMode {
		return ctx, nil
	}
	if _, ok := ctx.Value(runKey{}).(*bool); ok {
		return ctx, nil
	}
	if _, ok := trace.SpanFromCtx(ctx); !ok {
		return ctx, nil
	}
	ran := new(bool)
	return context.WithValue(ctx, runKey{}, ran), ran
}

func noteRun(ctx context.Context) {
	if ran, ok := ctx.Value(runKey{}).(*bool); ok {
		*ran = true
	}
}

// refused records err as a statement of model's that never ran.
func refused(ctx context.Context, model string, ran *bool, err error) {
	if ran == nil || *ran || err == nil {
		return
	}
	*ran = true
	_, sp := trace.StartSpan(ctx, "sql", trace.Str("sql", "-- not sent: a "+model+" query the ORM refused"), trace.Bool("orm.refused", true))
	sp.End(err)
}
