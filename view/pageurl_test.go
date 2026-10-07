package view

import (
	"context"
	"net/http/httptest"
	"testing"
)

func TestRefererURL(t *testing.T) {
	r := httptest.NewRequest("POST", "http://app.test/_view/shard/x", nil)
	if u := refererURL(r); u.Path != "/_view/shard/x" {
		t.Errorf("no referer: %s", u)
	}
	r.Header.Set("Referer", "http://app.test/users?page=2")
	if u := refererURL(r); u.RequestURI() != "/users?page=2" {
		t.Errorf("same-origin referer: %s", u)
	}
	r.Header.Set("Referer", "http://elsewhere.test/users")
	if u := refererURL(r); u.Path != "/_view/shard/x" {
		t.Errorf("another site's referer: %s", u)
	}
	if u := CurrentURL(context.Background()); u.String() != "" {
		t.Errorf("no page: %s", u)
	}
}
