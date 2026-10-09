package db

import (
	"errors"
	"fmt"
	"time"

	"github.com/paulmanoni/nexus/v2/trace"
	"gorm.io/gorm"
)

// traceQueries records each statement GORM runs as an "sql" span of the
// request it runs under (its context: db.WithContext(ctx)), as the nexus
// ORM does — so the dashboard's traces and the dev toolbar list GORM's
// queries too. A statement without a traced context costs a context lookup.
func traceQueries(db *gorm.DB) error {
	const span, start = "nexus:trace_span", "nexus:trace_start"
	before := func(tx *gorm.DB) {
		ctx := tx.Statement.Context
		if ctx == nil {
			return
		}
		if _, ok := trace.SpanFromCtx(ctx); !ok {
			return
		}
		_, sp := trace.StartSpan(ctx, "sql")
		tx.InstanceSet(span, sp)
		tx.InstanceSet(start, time.Now())
	}
	after := func(tx *gorm.DB) {
		v, ok := tx.InstanceGet(span)
		if !ok {
			return
		}
		sp := v.(*trace.Span)
		if t, ok := tx.InstanceGet(start); ok {
			sp.Set("ms", fmt.Sprintf("%.2f", float64(time.Since(t.(time.Time)).Microseconds())/1000))
		}
		sp.Set("sql", tx.Statement.SQL.String())
		sp.Set("args", int64(len(tx.Statement.Vars)))
		err := tx.Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			err = nil
		}
		sp.End(err)
	}
	cb := db.Callback()
	return errors.Join(
		cb.Create().Before("gorm:create").Register("nexus:trace_before", before),
		cb.Create().After("gorm:create").Register("nexus:trace_after", after),
		cb.Query().Before("gorm:query").Register("nexus:trace_before", before),
		cb.Query().After("gorm:query").Register("nexus:trace_after", after),
		cb.Update().Before("gorm:update").Register("nexus:trace_before", before),
		cb.Update().After("gorm:update").Register("nexus:trace_after", after),
		cb.Delete().Before("gorm:delete").Register("nexus:trace_before", before),
		cb.Delete().After("gorm:delete").Register("nexus:trace_after", after),
		cb.Row().Before("gorm:row").Register("nexus:trace_before", before),
		cb.Row().After("gorm:row").Register("nexus:trace_after", after),
		cb.Raw().Before("gorm:raw").Register("nexus:trace_before", before),
		cb.Raw().After("gorm:raw").Register("nexus:trace_after", after),
	)
}
