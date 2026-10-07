package view

import (
	"context"
	"net/http"
	"net/url"
)

type pageURLKey struct{}

func withPageURL(ctx context.Context, u *url.URL) context.Context {
	if u == nil {
		return ctx
	}
	return context.WithValue(ctx, pageURLKey{}, u)
}

// CurrentURL is the URL of the page ctx renders: the request's for a page,
// the one the browser shows for a live page — kept current as view.Link or
// PushPatch patches it — and for a shard's re-render, the page it sits on.
// Breadcrumbs, an active menu entry or a context processor read it:
//
//	view.ContextProcessor(func(ctx context.Context) Crumbs {
//		return crumbsFor(view.CurrentURL(ctx).Path)
//	})
//
// It is a copy, empty when ctx renders no page.
func CurrentURL(ctx context.Context) *url.URL {
	u, _ := ctx.Value(pageURLKey{}).(*url.URL)
	if u == nil {
		return &url.URL{}
	}
	c := *u
	return &c
}

// refererURL is the page a request came from, when it is this app's: what a
// shard re-render's page is.
func refererURL(r *http.Request) *url.URL {
	ref, err := url.Parse(r.Referer())
	if err != nil || ref.Host == "" || ref.Host != r.Host {
		return r.URL
	}
	return &url.URL{Path: ref.Path, RawPath: ref.RawPath, RawQuery: ref.RawQuery}
}
