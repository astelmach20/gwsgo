// Package client executes Discovery-described API methods over HTTP.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/astelmach20/gwsgo/internal/discovery"
)

// TokenSource supplies bearer tokens.
type TokenSource interface {
	AccessToken(context.Context) (string, error)
}

// maxRetries bounds automatic retries for throttling and transient failures.
const maxRetries = 5

// pathPlaceholder matches {fileId} and {+name} style path segments.
var pathPlaceholder = regexp.MustCompile(`\{\+?([^}]+)\}`)

// Error is a non-2xx API response.
type Error struct {
	Status  int
	Payload any
}

func (e *Error) Error() string {
	if payload, ok := e.Payload.(map[string]any); ok {
		if inner, ok := payload["error"].(map[string]any); ok {
			if message, ok := inner["message"].(string); ok {
				return fmt.Sprintf("Google API returned HTTP %d: %s", e.Status, message)
			}
		}
	}
	return fmt.Sprintf("Google API returned HTTP %d: %v", e.Status, e.Payload)
}

// Client calls Google APIs described by Discovery documents.
type Client struct {
	HTTP   *http.Client
	Tokens TokenSource
	// UserProject populates x-goog-user-project for quota attribution.
	UserProject string
	Retries     int
	Sleep       func(context.Context, time.Duration) error
	Now         func() time.Time
}

// New builds a Client with production defaults.
func New(tokens TokenSource) *Client {
	return &Client{
		HTTP:    &http.Client{Timeout: 10 * time.Minute},
		Tokens:  tokens,
		Retries: maxRetries,
		Now:     time.Now,
		Sleep: func(ctx context.Context, d time.Duration) error {
			timer := time.NewTimer(d)
			defer timer.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-timer.C:
				return nil
			}
		},
	}
}

// Request is one Discovery-described call.
type Request struct {
	Doc    *discovery.Document
	Method *discovery.Method
	Params map[string]any
	Body   any
}

// BuildURL substitutes path placeholders and appends the remaining parameters
// as the query string. Parameters consumed by the path are not repeated.
func BuildURL(doc *discovery.Document, method *discovery.Method, params map[string]any) (string, error) {
	base := doc.BaseURL
	if base == "" {
		base = strings.TrimRight(doc.RootURL, "/") + "/" + strings.TrimLeft(doc.ServicePath, "/")
	}

	consumed := map[string]bool{}
	var missing []string
	path := pathPlaceholder.ReplaceAllStringFunc(method.Path, func(match string) string {
		name := pathPlaceholder.FindStringSubmatch(match)[1]
		value, ok := params[name]
		if !ok {
			missing = append(missing, name)
			return match
		}
		consumed[name] = true
		// Reserved expansion ({+name}) keeps slashes; plain placeholders escape them.
		if strings.HasPrefix(match, "{+") {
			return stringify(value)
		}
		return url.PathEscape(stringify(value))
	})
	if len(missing) > 0 {
		return "", fmt.Errorf("missing required parameter(s): %s", strings.Join(missing, ", "))
	}

	query := url.Values{}
	for name, value := range params {
		if consumed[name] {
			continue
		}
		switch typed := value.(type) {
		case []any:
			for _, item := range typed {
				query.Add(name, stringify(item))
			}
		default:
			query.Set(name, stringify(value))
		}
	}

	endpoint := strings.TrimRight(base, "/") + "/" + strings.TrimLeft(path, "/")
	if encoded := query.Encode(); encoded != "" {
		endpoint += "?" + encoded
	}
	return endpoint, nil
}

// stringify renders a JSON value as a URL parameter without Go's float noise.
func stringify(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case bool:
		return strconv.FormatBool(typed)
	case float64:
		if typed == float64(int64(typed)) {
			return strconv.FormatInt(int64(typed), 10)
		}
		return strconv.FormatFloat(typed, 'f', -1, 64)
	case nil:
		return ""
	default:
		return fmt.Sprint(typed)
	}
}

// retryable reports whether a status code is worth retrying.
//
// 429 always is. 5xx is retried for any method, because Google's own guidance
// is to back off on backendError regardless of verb, and every mutating
// Workspace method is either idempotent or guarded server-side by a request id.
func retryable(status int) bool {
	return status == http.StatusTooManyRequests || status >= 500
}

// backoff computes the delay before the next attempt, honouring Retry-After.
func (c *Client) backoff(header string, attempt int) time.Duration {
	if seconds, err := strconv.Atoi(strings.TrimSpace(header)); err == nil && seconds >= 0 {
		return time.Duration(seconds) * time.Second
	}
	if when, err := http.ParseTime(strings.TrimSpace(header)); err == nil {
		if delay := time.Until(when); delay > 0 {
			return delay
		}
	}
	// Exponential backoff with full jitter, as Google's quota docs recommend.
	ceiling := time.Duration(1<<attempt) * time.Second
	if ceiling > 32*time.Second {
		ceiling = 32 * time.Second
	}
	return time.Duration(rand.Int63n(int64(ceiling) + 1))
}

// send performs one authenticated HTTP call with retries.
func (c *Client) send(ctx context.Context, method, endpoint string, body []byte, headers map[string]string) (*http.Response, []byte, error) {
	token, err := c.Tokens.AccessToken(ctx)
	if err != nil {
		return nil, nil, err
	}
	for attempt := 0; ; attempt++ {
		var reader io.Reader
		if body != nil {
			reader = bytes.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
		if err != nil {
			return nil, nil, err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Accept", "application/json")
		// Drive only compresses when the User-Agent also advertises gzip.
		req.Header.Set("Accept-Encoding", "gzip")
		req.Header.Set("User-Agent", "gwsg/dev (gzip)")
		if c.UserProject != "" {
			req.Header.Set("x-goog-user-project", c.UserProject)
		}
		for name, value := range headers {
			req.Header.Set(name, value)
		}

		resp, err := c.HTTP.Do(req)
		if err != nil {
			if attempt < c.Retries {
				if sleepErr := c.Sleep(ctx, c.backoff("", attempt)); sleepErr != nil {
					return nil, nil, sleepErr
				}
				continue
			}
			return nil, nil, err
		}
		raw, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil {
			return nil, nil, readErr
		}
		if retryable(resp.StatusCode) && attempt < c.Retries {
			if sleepErr := c.Sleep(ctx, c.backoff(resp.Header.Get("Retry-After"), attempt)); sleepErr != nil {
				return nil, nil, sleepErr
			}
			continue
		}
		return resp, raw, nil
	}
}

// decode turns a response body into JSON, bytes, or an *Error.
func decode(resp *http.Response, raw []byte) (any, error) {
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var payload any
		if json.Unmarshal(raw, &payload) != nil {
			payload = strings.TrimSpace(string(raw))
		}
		return nil, &Error{Status: resp.StatusCode, Payload: payload}
	}
	if len(raw) == 0 {
		return nil, nil
	}
	if strings.Contains(resp.Header.Get("Content-Type"), "json") {
		var payload any
		if err := json.Unmarshal(raw, &payload); err != nil {
			return nil, err
		}
		return payload, nil
	}
	return raw, nil
}

// Do executes a single Discovery-described method.
func (c *Client) Do(ctx context.Context, req Request) (any, error) {
	endpoint, err := BuildURL(req.Doc, req.Method, req.Params)
	if err != nil {
		return nil, err
	}
	var body []byte
	headers := map[string]string{}
	if req.Body != nil {
		if body, err = json.Marshal(req.Body); err != nil {
			return nil, err
		}
		headers["Content-Type"] = "application/json; charset=UTF-8"
	}
	verb := req.Method.HTTPMethod
	if verb == "" {
		verb = http.MethodGet
	}
	resp, raw, err := c.send(ctx, verb, endpoint, body, headers)
	if err != nil {
		return nil, err
	}
	return decode(resp, raw)
}

// pageTokenFields are the response keys Google uses to signal another page.
var pageTokenFields = []string{"nextPageToken", "nextSyncToken"}

// All follows pagination to exhaustion and returns every page.
//
// There is deliberately no page cap. A silent ceiling turns "list my files"
// into "list some of my files" with no signal to the caller, which is a data
// -loss bug dressed up as a default.
func (c *Client) All(ctx context.Context, req Request, onPage func(any) error) error {
	params := map[string]any{}
	for name, value := range req.Params {
		params[name] = value
	}
	seen := map[string]bool{}
	for {
		page, err := c.Do(ctx, Request{Doc: req.Doc, Method: req.Method, Params: params, Body: req.Body})
		if err != nil {
			return err
		}
		if err := onPage(page); err != nil {
			return err
		}
		object, ok := page.(map[string]any)
		if !ok {
			return nil
		}
		var next string
		for _, field := range pageTokenFields {
			if token, ok := object[field].(string); ok && token != "" && field == "nextPageToken" {
				next = token
				break
			}
		}
		if next == "" {
			return nil
		}
		// A server that echoes the same token would otherwise loop forever.
		if seen[next] {
			return fmt.Errorf("pagination stalled: the API repeated page token %q", next)
		}
		seen[next] = true
		params["pageToken"] = next
	}
}
