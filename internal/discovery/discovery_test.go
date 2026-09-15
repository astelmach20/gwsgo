package discovery

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func sampleDoc() *Document {
	return &Document{
		Name: "gmail", Version: "v1",
		RootURL: "https://gmail.googleapis.com/", ServicePath: "gmail/v1/",
		Resources: map[string]Resource{
			"users": {
				Methods: map[string]Method{
					"getProfile": {ID: "gmail.users.getProfile", HTTPMethod: "GET"},
				},
				Resources: map[string]Resource{
					"messages": {Methods: map[string]Method{
						"list": {ID: "gmail.users.messages.list", HTTPMethod: "GET"},
					}},
				},
			},
		},
	}
}

func TestFindMethodOnNestedResource(t *testing.T) {
	method, path, err := sampleDoc().FindMethod([]string{"users", "messages", "list"})
	if err != nil {
		t.Fatalf("FindMethod: %v", err)
	}
	if method.ID != "gmail.users.messages.list" {
		t.Errorf("got %q", method.ID)
	}
	if strings.Join(path, ".") != "users.messages.list" {
		t.Errorf("unexpected path %v", path)
	}
}

func TestFindMethodOnTopLevelResource(t *testing.T) {
	method, _, err := sampleDoc().FindMethod([]string{"users", "getProfile"})
	if err != nil {
		t.Fatalf("FindMethod: %v", err)
	}
	if method.ID != "gmail.users.getProfile" {
		t.Errorf("got %q", method.ID)
	}
}

func TestFindMethodRejectsUnknownNames(t *testing.T) {
	if _, _, err := sampleDoc().FindMethod([]string{"users", "nope"}); err == nil {
		t.Error("expected an error for an unknown method")
	}
	if _, _, err := sampleDoc().FindMethod([]string{"nope", "list"}); err == nil {
		t.Error("expected an error for an unknown resource")
	}
}

func TestResourceNamesAreSorted(t *testing.T) {
	doc := &Document{Resources: map[string]Resource{"zeta": {}, "alpha": {}, "mid": {}}}
	got := doc.ResourceNames()
	want := []string{"alpha", "mid", "zeta"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

// directoryServer stands in for Google's Discovery directory.
func directoryServer(t *testing.T, items []directoryEntry) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"items": items})
	}))
}

func TestResolvePrefersThePreferredVersion(t *testing.T) {
	server := directoryServer(t, []directoryEntry{
		{Name: "drive", Version: "v2", DiscoveryRestURL: "https://example.test/v2", Preferred: false},
		{Name: "drive", Version: "v3", DiscoveryRestURL: "https://example.test/v3", Preferred: true},
	})
	defer server.Close()

	c := &Client{HTTP: server.Client(), Now: time.Now, TTL: time.Minute}
	// Point the client at the stub by overriding the cached directory fetch.
	c.directoryURL = server.URL

	service, err := c.Resolve(context.Background(), "drive")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if service.Version != "v3" {
		t.Errorf("got version %q, want v3", service.Version)
	}
	// The rest URL must come from the directory, never be constructed: Drive is
	// served from www.googleapis.com, not drive.googleapis.com.
	if service.RestURL != "https://example.test/v3" {
		t.Errorf("got rest url %q, want the directory value", service.RestURL)
	}
}

func TestResolveHonoursExplicitVersion(t *testing.T) {
	server := directoryServer(t, []directoryEntry{
		{Name: "drive", Version: "v2", DiscoveryRestURL: "https://example.test/v2"},
		{Name: "drive", Version: "v3", DiscoveryRestURL: "https://example.test/v3", Preferred: true},
	})
	defer server.Close()

	c := &Client{HTTP: server.Client(), Now: time.Now, TTL: time.Minute, directoryURL: server.URL}
	service, err := c.Resolve(context.Background(), "drive:v2")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if service.Version != "v2" {
		t.Errorf("got %q, want v2", service.Version)
	}
}

func TestResolveExpandsAliases(t *testing.T) {
	server := directoryServer(t, []directoryEntry{
		{Name: "admin", Version: "directory_v1", DiscoveryRestURL: "https://example.test/d", Preferred: true},
		{Name: "admin", Version: "reports_v1", DiscoveryRestURL: "https://example.test/r"},
	})
	defer server.Close()

	c := &Client{HTTP: server.Client(), Now: time.Now, TTL: time.Minute, directoryURL: server.URL}
	service, err := c.Resolve(context.Background(), "reports")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if service.Version != "reports_v1" {
		t.Errorf("alias reports should map to reports_v1, got %q", service.Version)
	}
}

// An unknown service must produce a helpful error, not an allowlist lecture.
func TestResolveUnknownServiceExplainsItself(t *testing.T) {
	server := directoryServer(t, []directoryEntry{{Name: "drive", Version: "v3", Preferred: true}})
	defer server.Close()

	c := &Client{HTTP: server.Client(), Now: time.Now, TTL: time.Minute, directoryURL: server.URL}
	_, err := c.Resolve(context.Background(), "nosuchapi")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "gwsg services") {
		t.Errorf("error should point at the discovery command, got: %v", err)
	}
}

func TestDeprecatedLabel(t *testing.T) {
	if !(directoryEntry{Labels: []string{"deprecated"}}).Deprecated() {
		t.Error("deprecated label not detected")
	}
	if (directoryEntry{Labels: []string{"limited_availability"}}).Deprecated() {
		t.Error("false positive on an unrelated label")
	}
}
