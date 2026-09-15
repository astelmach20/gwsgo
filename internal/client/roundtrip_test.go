package client

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/astelmach20/gwsgo/internal/discovery"
)

// staticToken is a TokenSource that never fails.
type staticToken struct{}

func (staticToken) AccessToken(context.Context) (string, error) { return "test-token", nil }

// newTestClient points a Client at a stub server with instant backoff.
func newTestClient(server *httptest.Server) *Client {
	c := New(staticToken{})
	c.HTTP = server.Client()
	c.Sleep = func(context.Context, time.Duration) error { return nil }
	return c
}

// docFor builds a Document addressed at the stub server.
func docFor(server *httptest.Server) *discovery.Document {
	return &discovery.Document{Name: "test", Version: "v1", RootURL: server.URL + "/", ServicePath: ""}
}

func listMethod() *discovery.Method {
	return &discovery.Method{ID: "test.items.list", HTTPMethod: "GET", Path: "items"}
}

func TestDoSendsBearerToken(t *testing.T) {
	var gotAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"ok":true}`)
	}))
	defer server.Close()

	c := newTestClient(server)
	if _, err := c.Do(context.Background(), Request{Doc: docFor(server), Method: listMethod()}); err != nil {
		t.Fatalf("Do: %v", err)
	}
	if gotAuth != "Bearer test-token" {
		t.Errorf("got %q", gotAuth)
	}
}

func TestDoSetsUserProjectHeaderWhenConfigured(t *testing.T) {
	var got string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("x-goog-user-project")
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{}`)
	}))
	defer server.Close()

	c := newTestClient(server)
	c.UserProject = "my-project"
	if _, err := c.Do(context.Background(), Request{Doc: docFor(server), Method: listMethod()}); err != nil {
		t.Fatalf("Do: %v", err)
	}
	if got != "my-project" {
		t.Errorf("got %q, want my-project", got)
	}
}

func TestDoRetriesOn429ThenSucceeds(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) < 3 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"ok":true}`)
	}))
	defer server.Close()

	c := newTestClient(server)
	result, err := c.Do(context.Background(), Request{Doc: docFor(server), Method: listMethod()})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if calls.Load() != 3 {
		t.Errorf("expected 3 attempts, got %d", calls.Load())
	}
	if payload, ok := result.(map[string]any); !ok || payload["ok"] != true {
		t.Errorf("unexpected result %v", result)
	}
}

// Google returns backendError/500 routinely; the Rust CLI surfaces these
// straight to the user instead of retrying.
func TestDoRetriesOn500(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) < 2 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"ok":true}`)
	}))
	defer server.Close()

	if _, err := newTestClient(server).Do(context.Background(),
		Request{Doc: docFor(server), Method: listMethod()}); err != nil {
		t.Fatalf("Do: %v", err)
	}
	if calls.Load() != 2 {
		t.Errorf("expected a retry after 500, got %d attempts", calls.Load())
	}
}

func TestDoDoesNotRetryOn404(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"error":{"message":"not found"}}`)
	}))
	defer server.Close()

	_, err := newTestClient(server).Do(context.Background(), Request{Doc: docFor(server), Method: listMethod()})
	if err == nil {
		t.Fatal("expected an error")
	}
	if calls.Load() != 1 {
		t.Errorf("404 must not be retried, got %d attempts", calls.Load())
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error should surface the API message, got %v", err)
	}
}

// The Rust CLI stops after 10 pages with no warning. We must not.
func TestAllFollowsEveryPage(t *testing.T) {
	const totalPages = 25
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page := 1
		if token := r.URL.Query().Get("pageToken"); token != "" {
			fmt.Sscanf(token, "page-%d", &page)
		}
		body := map[string]any{"items": []any{map[string]any{"id": page}}}
		if page < totalPages {
			body["nextPageToken"] = fmt.Sprintf("page-%d", page+1)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	}))
	defer server.Close()

	var pages int
	err := newTestClient(server).All(context.Background(),
		Request{Doc: docFor(server), Method: listMethod()},
		func(any) error { pages++; return nil })
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if pages != totalPages {
		t.Errorf("fetched %d pages, want %d — pagination must not be silently capped", pages, totalPages)
	}
}

// A server that echoes the same token would otherwise spin forever.
func TestAllStopsOnRepeatedPageToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"items":[],"nextPageToken":"same"}`)
	}))
	defer server.Close()

	err := newTestClient(server).All(context.Background(),
		Request{Doc: docFor(server), Method: listMethod()}, func(any) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "stalled") {
		t.Fatalf("expected a stall error, got %v", err)
	}
}

func TestAllPropagatesCallbackErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"items":[]}`)
	}))
	defer server.Close()

	want := fmt.Errorf("writer failed")
	err := newTestClient(server).All(context.Background(),
		Request{Doc: docFor(server), Method: listMethod()}, func(any) error { return want })
	if err == nil {
		t.Fatal("expected the callback error to propagate")
	}
}
