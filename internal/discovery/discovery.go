// Package discovery fetches, caches and navigates Google API Discovery documents.
//
// Discovery documents are machine-readable descriptions of every Google API:
// their resources, methods, HTTP verbs, URL paths, parameters and OAuth scopes.
// gwsg builds its command surface from them at runtime, so every Workspace API
// is reachable without any per-service code.
//
// There is deliberately no allowlist of "known" services. Aliases below are
// conveniences for APIs whose Discovery name differs from what a human would
// type; anything not listed falls through to a directory lookup.
package discovery

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	// DirectoryURL lists every API Google publishes a Discovery document for.
	DirectoryURL = "https://discovery.googleapis.com/discovery/v1/apis"

	// cacheTTL bounds how long a cached Discovery document is trusted.
	cacheTTL = 24 * time.Hour
)

// aliases map friendly names onto Discovery (api, version) pairs. These exist
// only because the Discovery name is unguessable, never to restrict access.
var aliases = map[string]struct{ API, Version string }{
	"admin":          {"admin", "directory_v1"},
	"directory":      {"admin", "directory_v1"},
	"reports":        {"admin", "reports_v1"},
	"admin-reports":  {"admin", "reports_v1"},
	"datatransfer":   {"admin", "datatransfer_v1"},
	"events":         {"workspaceevents", ""},
	"postmaster":     {"gmailpostmastertools", ""},
	"labels":         {"drivelabels", ""},
	"activity":       {"driveactivity", ""},
	"alerts":         {"alertcenter", ""},
	"identity":       {"cloudidentity", ""},
	"groupssettings": {"groupssettings", ""},
	"marketplace":    {"appsmarket", ""},
	"addons":         {"gsuiteaddons", ""},
}

// Schema is a JSON Schema node from a Discovery document.
type Schema struct {
	ID                   string            `json:"id,omitempty"`
	Type                 string            `json:"type,omitempty"`
	Format               string            `json:"format,omitempty"`
	Description          string            `json:"description,omitempty"`
	Ref                  string            `json:"$ref,omitempty"`
	Properties           map[string]Schema `json:"properties,omitempty"`
	Items                *Schema           `json:"items,omitempty"`
	Enum                 []string          `json:"enum,omitempty"`
	EnumDescriptions     []string          `json:"enumDescriptions,omitempty"`
	AdditionalProperties *Schema           `json:"additionalProperties,omitempty"`
}

// Parameter describes a single method parameter.
type Parameter struct {
	Type        string   `json:"type,omitempty"`
	Format      string   `json:"format,omitempty"`
	Description string   `json:"description,omitempty"`
	Location    string   `json:"location,omitempty"`
	Required    bool     `json:"required,omitempty"`
	Repeated    bool     `json:"repeated,omitempty"`
	Enum        []string `json:"enum,omitempty"`
	Default     string   `json:"default,omitempty"`
}

// MediaUploadProtocol describes one upload protocol for a method.
type MediaUploadProtocol struct {
	Path      string `json:"path,omitempty"`
	Multipart bool   `json:"multipart,omitempty"`
}

// MediaUpload captures both simple/multipart and resumable upload support.
//
// The Rust CLI parses only "simple", which is why it cannot upload a file
// larger than the multipart ceiling. We keep both.
type MediaUpload struct {
	Accept    []string `json:"accept,omitempty"`
	MaxSize   string   `json:"maxSize,omitempty"`
	Protocols struct {
		Simple    *MediaUploadProtocol `json:"simple,omitempty"`
		Resumable *MediaUploadProtocol `json:"resumable,omitempty"`
	} `json:"protocols"`
}

// Method is a single callable API method.
type Method struct {
	ID             string               `json:"id,omitempty"`
	Description    string               `json:"description,omitempty"`
	HTTPMethod     string               `json:"httpMethod,omitempty"`
	Path           string               `json:"path,omitempty"`
	FlatPath       string               `json:"flatPath,omitempty"`
	Parameters     map[string]Parameter `json:"parameters,omitempty"`
	ParameterOrder []string             `json:"parameterOrder,omitempty"`
	Request        *struct {
		Ref string `json:"$ref,omitempty"`
	} `json:"request,omitempty"`
	Response *struct {
		Ref string `json:"$ref,omitempty"`
	} `json:"response,omitempty"`
	Scopes                []string     `json:"scopes,omitempty"`
	SupportsMediaDownload bool         `json:"supportsMediaDownload,omitempty"`
	SupportsMediaUpload   bool         `json:"supportsMediaUpload,omitempty"`
	MediaUpload           *MediaUpload `json:"mediaUpload,omitempty"`
}

// Resource is a (possibly nested) group of methods.
type Resource struct {
	Methods   map[string]Method   `json:"methods,omitempty"`
	Resources map[string]Resource `json:"resources,omitempty"`
}

// Document is a Discovery REST description.
type Document struct {
	Name        string               `json:"name"`
	Version     string               `json:"version"`
	Title       string               `json:"title,omitempty"`
	Description string               `json:"description,omitempty"`
	RootURL     string               `json:"rootUrl"`
	ServicePath string               `json:"servicePath,omitempty"`
	BaseURL     string               `json:"baseUrl,omitempty"`
	Schemas     map[string]Schema    `json:"schemas,omitempty"`
	Resources   map[string]Resource  `json:"resources,omitempty"`
	Parameters  map[string]Parameter `json:"parameters,omitempty"`
	Auth        *struct {
		OAuth2 *struct {
			Scopes map[string]struct {
				Description string `json:"description"`
			} `json:"scopes"`
		} `json:"oauth2,omitempty"`
	} `json:"auth,omitempty"`
}

// directoryEntry is one row of the Discovery directory listing.
type directoryEntry struct {
	Name              string   `json:"name"`
	Version           string   `json:"version"`
	Title             string   `json:"title"`
	DiscoveryRestURL  string   `json:"discoveryRestUrl"`
	Preferred         bool     `json:"preferred"`
	DocumentationLink string   `json:"documentationLink"`
	Labels            []string `json:"labels,omitempty"`
}

// Deprecated reports whether Google has flagged this API version as going away.
func (e directoryEntry) Deprecated() bool {
	for _, label := range e.Labels {
		if label == "deprecated" {
			return true
		}
	}
	return false
}

// Client fetches Discovery documents, caching them on disk.
type Client struct {
	HTTP     *http.Client
	CacheDir string
	TTL      time.Duration
	Now      func() time.Time
	// directoryURL overrides the directory endpoint. Tests set it; production
	// leaves it empty and uses DirectoryURL.
	directoryURL string
}

// directoryEndpoint is the URL the client reads the API directory from.
func (c *Client) directoryEndpoint() string {
	if c.directoryURL != "" {
		return c.directoryURL
	}
	return DirectoryURL
}

// New builds a Client caching under the given directory.
func New(cacheDir string) *Client {
	return &Client{
		HTTP:     &http.Client{Timeout: 30 * time.Second},
		CacheDir: cacheDir,
		TTL:      cacheTTL,
		Now:      time.Now,
	}
}

// fetch retrieves a URL, returning its body.
func (c *Client) fetch(ctx context.Context, target string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("discovery request for %s returned HTTP %d", target, resp.StatusCode)
	}
	return raw, nil
}

// cached reads a cache entry if it exists and is younger than the TTL.
func (c *Client) cached(name string) []byte {
	if c.CacheDir == "" {
		return nil
	}
	path := filepath.Join(c.CacheDir, name)
	info, err := os.Stat(path)
	if err != nil || c.Now().Sub(info.ModTime()) > c.TTL {
		return nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	return raw
}

// store writes a cache entry, ignoring failures: a cache miss is never fatal.
func (c *Client) store(name string, raw []byte) {
	if c.CacheDir == "" {
		return
	}
	if err := os.MkdirAll(c.CacheDir, 0o700); err != nil {
		return
	}
	path := filepath.Join(c.CacheDir, name)
	tmp := path + ".tmp"
	if os.WriteFile(tmp, raw, 0o600) == nil {
		_ = os.Rename(tmp, path)
	}
}

// Service identifies a resolved API version and where to fetch its document.
type Service struct {
	API        string
	Version    string
	RestURL    string
	Title      string
	Deprecated bool
}

// Resolve turns a user-supplied service name into a concrete API version.
//
// Accepts "drive", "drive:v2", or any API Google publishes. Unknown names are
// looked up in the Discovery directory and resolved to the preferred version,
// so new APIs work the day Google ships them.
//
// The directory entry's discoveryRestUrl is carried through deliberately. It
// cannot be derived from the API name: drive/v3 is served from
// www.googleapis.com and calendar/v3 from calendar-json.googleapis.com, so any
// client that builds "https://<api>.googleapis.com/$discovery/rest" gets a 404
// on two of the most-used APIs in Workspace.
func (c *Client) Resolve(ctx context.Context, name string) (Service, error) {
	api, version := name, ""
	if base, explicit, found := strings.Cut(name, ":"); found {
		api, version = base, explicit
	}
	if alias, ok := aliases[api]; ok {
		api = alias.API
		if version == "" {
			version = alias.Version
		}
	}

	entries, err := c.Directory(ctx)
	if err != nil {
		// Offline with an explicit version: fall back to the legacy URL shape.
		if version != "" {
			return Service{API: api, Version: version, RestURL: legacyRestURL(api, version)}, nil
		}
		return Service{}, err
	}

	var candidates []directoryEntry
	for _, entry := range entries {
		if entry.Name != api {
			continue
		}
		if version != "" && entry.Version != version {
			continue
		}
		candidates = append(candidates, entry)
	}
	if len(candidates) == 0 {
		if version != "" {
			// Not published in the directory, but the caller was explicit.
			return Service{API: api, Version: version, RestURL: legacyRestURL(api, version)}, nil
		}
		return Service{}, fmt.Errorf("no Google API named %q; run `gwsg services` to list them, "+
			"or pass an explicit version as %s:<version>", api, api)
	}

	chosen := candidates[0]
	for _, entry := range candidates {
		if entry.Preferred {
			chosen = entry
			break
		}
	}
	if len(candidates) > 1 && !chosen.Preferred {
		sort.Slice(candidates, func(i, j int) bool { return candidates[i].Version > candidates[j].Version })
		chosen = candidates[0]
	}
	return Service{
		API:        chosen.Name,
		Version:    chosen.Version,
		RestURL:    chosen.DiscoveryRestURL,
		Title:      chosen.Title,
		Deprecated: chosen.Deprecated(),
	}, nil
}

// legacyRestURL is the historical per-API document location, used only as a
// fallback when the directory is unavailable or does not list a version.
func legacyRestURL(api, version string) string {
	return fmt.Sprintf("https://www.googleapis.com/discovery/v1/apis/%s/%s/rest", api, version)
}

// Directory returns the full list of published Google APIs.
func (c *Client) Directory(ctx context.Context) ([]directoryEntry, error) {
	var payload struct {
		Items []directoryEntry `json:"items"`
	}
	if raw := c.cached("directory.json"); raw != nil {
		if json.Unmarshal(raw, &payload) == nil && len(payload.Items) > 0 {
			return payload.Items, nil
		}
	}
	raw, err := c.fetch(ctx, c.directoryEndpoint())
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, fmt.Errorf("could not parse the Discovery directory: %w", err)
	}
	c.store("directory.json", raw)
	return payload.Items, nil
}

// Load fetches (or reuses) the Discovery document for a resolved service.
func (c *Client) Load(ctx context.Context, service Service) (*Document, error) {
	cacheName := fmt.Sprintf("%s.%s.json", service.API, service.Version)
	if raw := c.cached(cacheName); raw != nil {
		var doc Document
		if json.Unmarshal(raw, &doc) == nil && doc.RootURL != "" {
			return &doc, nil
		}
	}

	sources := []string{}
	if service.RestURL != "" {
		sources = append(sources, service.RestURL)
	}
	sources = append(sources, legacyRestURL(service.API, service.Version))

	var lastErr error
	for _, source := range sources {
		raw, err := c.fetch(ctx, source)
		if err != nil {
			lastErr = err
			continue
		}
		var doc Document
		if err := json.Unmarshal(raw, &doc); err != nil {
			lastErr = fmt.Errorf("could not parse the Discovery document for %s/%s: %w",
				service.API, service.Version, err)
			continue
		}
		if doc.RootURL == "" {
			lastErr = fmt.Errorf("Discovery document for %s/%s has no rootUrl", service.API, service.Version)
			continue
		}
		c.store(cacheName, raw)
		return &doc, nil
	}
	return nil, lastErr
}

// FindMethod walks resource path segments to a method, e.g.
// ["users","messages","list"] within the Gmail document.
func (d *Document) FindMethod(segments []string) (*Method, []string, error) {
	if len(segments) == 0 {
		return nil, nil, fmt.Errorf("no resource or method given")
	}
	resources := d.Resources
	var walked []string
	for index, segment := range segments {
		resource, ok := resources[segment]
		if ok && index < len(segments)-1 {
			walked = append(walked, segment)
			resources = resource.Resources
			// A method may live directly on this resource; check on the next pass.
			if method, found := resource.Methods[segments[index+1]]; found && index+2 == len(segments) {
				return &method, append(walked, segments[index+1]), nil
			}
			continue
		}
		if index > 0 {
			return nil, walked, fmt.Errorf("%q is not a resource or method of %s",
				segment, strings.Join(walked, " "))
		}
		return nil, walked, fmt.Errorf("%q is not a resource of %s %s", segment, d.Name, d.Version)
	}
	return nil, walked, fmt.Errorf("%q names a resource, not a method", strings.Join(segments, " "))
}

// ResourceNames lists the top-level resources of a document, sorted.
func (d *Document) ResourceNames() []string {
	names := make([]string, 0, len(d.Resources))
	for name := range d.Resources {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// MethodNames lists the methods and sub-resources reachable at a resource path.
func (d *Document) MethodNames(segments []string) (methods, subResources []string) {
	resources := d.Resources
	var current *Resource
	for _, segment := range segments {
		resource, ok := resources[segment]
		if !ok {
			return nil, nil
		}
		current = &resource
		resources = resource.Resources
	}
	if current == nil {
		return nil, d.ResourceNames()
	}
	for name := range current.Methods {
		methods = append(methods, name)
	}
	for name := range current.Resources {
		subResources = append(subResources, name)
	}
	sort.Strings(methods)
	sort.Strings(subResources)
	return methods, subResources
}
