package client

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/astelmach20/gwsgo/internal/discovery"
)

func testDoc() *discovery.Document {
	return &discovery.Document{
		Name: "drive", Version: "v3",
		RootURL: "https://www.googleapis.com/", ServicePath: "drive/v3/",
	}
}

func TestBuildURLSubstitutesPathParams(t *testing.T) {
	method := &discovery.Method{HTTPMethod: "GET", Path: "files/{fileId}"}
	got, err := BuildURL(testDoc(), method, map[string]any{"fileId": "abc123"})
	if err != nil {
		t.Fatalf("BuildURL: %v", err)
	}
	want := "https://www.googleapis.com/drive/v3/files/abc123"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestBuildURLDoesNotRepeatPathParamsInQuery(t *testing.T) {
	method := &discovery.Method{HTTPMethod: "GET", Path: "files/{fileId}"}
	got, err := BuildURL(testDoc(), method, map[string]any{"fileId": "abc", "fields": "id,name"})
	if err != nil {
		t.Fatalf("BuildURL: %v", err)
	}
	if strings.Contains(got, "fileId=") {
		t.Errorf("path parameter leaked into the query string: %s", got)
	}
	if !strings.Contains(got, "fields=id%2Cname") {
		t.Errorf("missing query parameter: %s", got)
	}
}

func TestBuildURLReportsMissingRequiredParams(t *testing.T) {
	method := &discovery.Method{HTTPMethod: "GET", Path: "files/{fileId}"}
	_, err := BuildURL(testDoc(), method, map[string]any{})
	if err == nil || !strings.Contains(err.Error(), "fileId") {
		t.Fatalf("expected a missing-parameter error naming fileId, got %v", err)
	}
}

// Reserved expansion ({+name}) must not escape slashes, or resource names like
// "spaces/abc/messages/def" break.
func TestBuildURLReservedExpansionKeepsSlashes(t *testing.T) {
	doc := &discovery.Document{RootURL: "https://chat.googleapis.com/", ServicePath: "v1/"}
	method := &discovery.Method{HTTPMethod: "GET", Path: "{+name}"}
	got, err := BuildURL(doc, method, map[string]any{"name": "spaces/abc/messages/def"})
	if err != nil {
		t.Fatalf("BuildURL: %v", err)
	}
	if !strings.HasSuffix(got, "v1/spaces/abc/messages/def") {
		t.Errorf("reserved expansion escaped slashes: %s", got)
	}
}

func TestBuildURLEscapesPlainPlaceholders(t *testing.T) {
	doc := &discovery.Document{RootURL: "https://gmail.googleapis.com/", ServicePath: "gmail/v1/"}
	method := &discovery.Method{HTTPMethod: "GET", Path: "users/{userId}/labels/{id}"}
	got, err := BuildURL(doc, method, map[string]any{"userId": "me", "id": "a/b"})
	if err != nil {
		t.Fatalf("BuildURL: %v", err)
	}
	if !strings.Contains(got, "a%2Fb") {
		t.Errorf("plain placeholder should escape slashes: %s", got)
	}
}

func TestStringifyRendersIntegersWithoutFloatNoise(t *testing.T) {
	// JSON numbers decode as float64; 10 must not become "10.000000".
	if got := stringify(float64(10)); got != "10" {
		t.Errorf("got %q, want \"10\"", got)
	}
	if got := stringify(float64(1.5)); got != "1.5" {
		t.Errorf("got %q, want \"1.5\"", got)
	}
	if got := stringify(true); got != "true" {
		t.Errorf("got %q, want \"true\"", got)
	}
}

func TestRetryableStatuses(t *testing.T) {
	for _, status := range []int{http.StatusTooManyRequests, 500, 503} {
		if !retryable(status) {
			t.Errorf("status %d should be retryable", status)
		}
	}
	for _, status := range []int{200, 400, 401, 403, 404} {
		if retryable(status) {
			t.Errorf("status %d should not be retryable", status)
		}
	}
}

func TestBackoffHonoursRetryAfterSeconds(t *testing.T) {
	c := New(nil)
	if got := c.backoff("7", 0); got != 7*time.Second {
		t.Errorf("got %v, want 7s", got)
	}
}

func TestBackoffIsBoundedWithoutHeader(t *testing.T) {
	c := New(nil)
	for attempt := range 10 {
		if got := c.backoff("", attempt); got < 0 || got > 32*time.Second {
			t.Errorf("attempt %d produced out-of-range delay %v", attempt, got)
		}
	}
}

func TestResumeOffsetParsesRangeHeader(t *testing.T) {
	// "bytes=0-42" means 43 bytes are stored, so resume at 43.
	got, err := resumeOffset("bytes=0-42")
	if err != nil {
		t.Fatalf("resumeOffset: %v", err)
	}
	if got != 43 {
		t.Errorf("got %d, want 43", got)
	}
}

func TestResumeOffsetTreatsMissingHeaderAsZero(t *testing.T) {
	got, err := resumeOffset("")
	if err != nil || got != 0 {
		t.Errorf("got (%d, %v), want (0, nil)", got, err)
	}
}

func TestResumeOffsetRejectsGarbage(t *testing.T) {
	if _, err := resumeOffset("nonsense"); err == nil {
		t.Error("expected an error for a malformed Range header")
	}
}

// Chunk sizes must be a multiple of 256 KiB or Google rejects the upload.
func TestDefaultChunkSizeIsAValidMultiple(t *testing.T) {
	if defaultChunkSize%chunkGranularity != 0 {
		t.Errorf("chunk size %d is not a multiple of %d", defaultChunkSize, chunkGranularity)
	}
}
