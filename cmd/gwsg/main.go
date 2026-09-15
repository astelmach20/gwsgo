// Command gwsg is a Google Workspace CLI built on Discovery documents.
//
// Every Google API that publishes a Discovery document is reachable; there is
// no allowlist of supported services.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"

	"github.com/astelmach20/gwsgo/internal/auth"
	"github.com/astelmach20/gwsgo/internal/client"
	"github.com/astelmach20/gwsgo/internal/discovery"
	"github.com/astelmach20/gwsgo/internal/output"
)

// version is overwritten at build time with -ldflags "-X main.version=...".
var version = "dev"

// Exit codes, kept stable so scripts can branch on them.
const (
	exitOK      = 0
	exitFailure = 1
	exitUsage   = 2
	exitAuth    = 3
	exitAPI     = 4
)

const usage = `gwsg %s — Google Workspace CLI

USAGE
    gwsg <service> <resource...> <method> [flags]
    gwsg auth <login|logout|status|print-token>
    gwsg services [substring]
    gwsg schema <service.resource.method>

EXAMPLES
    gwsg drive files list --params '{"pageSize":10}'
    gwsg gmail users messages list --params '{"userId":"me","q":"is:unread"}'
    gwsg calendar events list --params '{"calendarId":"primary"}' --format table
    gwsg vault matters list                  # no registry entry needed
    gwsg drive files create --upload ./big.zip --json '{"name":"big.zip"}'

FLAGS
    --params <JSON>              URL and query parameters
    --json <JSON>                Request body
    --upload <PATH>              File to upload (resumable above 5 MB)
    --upload-content-type <MIME> Override the detected upload MIME type
    --output <PATH>              Write binary/media responses here
    --format <FMT>               json (default), ndjson, table, csv
    --page-all                   Follow pagination to exhaustion
    --subject <EMAIL>            Impersonate a user (service accounts only)
    --user-project <ID>          Set x-goog-user-project for quota attribution
    --api-version <VER>          Pin an API version (or use service:version)
    --dry-run                    Print the request without sending it
    --version                    Print the version

ENVIRONMENT
    GWSG_CLIENT_ID, GWSG_CLIENT_SECRET   OAuth desktop client
    GWSG_TOKEN                           Pre-minted access token
    GOOGLE_APPLICATION_CREDENTIALS       Service account key (pairs with --subject)
    GWSG_SUBJECT                         Default impersonation target
    GWSG_SCOPES                          Override the requested scopes
    GWSG_CONFIG_DIR                      Config and cache location

EXIT CODES
    0 success   1 failure   2 usage   3 auth   4 API error
`

// options holds parsed command-line flags.
type options struct {
	params            string
	body              string
	upload            string
	uploadContentType string
	outputPath        string
	format            string
	subject           string
	userProject       string
	apiVersion        string
	pageAll           bool
	dryRun            bool
}

// parseArgs splits positional arguments from flags.
func parseArgs(args []string) ([]string, *options, error) {
	opts := &options{format: "json"}
	var positional []string

	valueFlags := map[string]*string{
		"--params":              &opts.params,
		"--json":                &opts.body,
		"--upload":              &opts.upload,
		"--upload-content-type": &opts.uploadContentType,
		"--output":              &opts.outputPath,
		"--format":              &opts.format,
		"--subject":             &opts.subject,
		"--user-project":        &opts.userProject,
		"--api-version":         &opts.apiVersion,
	}

	for index := 0; index < len(args); index++ {
		arg := args[index]
		if !strings.HasPrefix(arg, "--") {
			positional = append(positional, arg)
			continue
		}
		name, inline, hasInline := strings.Cut(arg, "=")
		switch name {
		case "--page-all":
			opts.pageAll = true
		case "--dry-run":
			opts.dryRun = true
		default:
			target, known := valueFlags[name]
			if !known {
				return nil, nil, fmt.Errorf("unknown flag %s", name)
			}
			if hasInline {
				*target = inline
				continue
			}
			if index+1 >= len(args) {
				return nil, nil, fmt.Errorf("%s needs a value", name)
			}
			index++
			*target = args[index]
		}
	}
	return positional, opts, nil
}

// decodeJSON parses a JSON flag value, with a readable error.
func decodeJSON(label, raw string) (map[string]any, error) {
	if strings.TrimSpace(raw) == "" {
		return map[string]any{}, nil
	}
	var value map[string]any
	if err := json.Unmarshal([]byte(raw), &value); err != nil {
		return nil, fmt.Errorf("%s is not valid JSON: %w", label, err)
	}
	return value, nil
}

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" || args[0] == "help" {
		fmt.Printf(usage, version)
		return exitOK
	}
	if args[0] == "--version" {
		fmt.Println(version)
		return exitOK
	}

	positional, opts, err := parseArgs(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return exitUsage
	}
	if len(positional) == 0 {
		fmt.Fprintln(os.Stderr, "error: no command given")
		return exitUsage
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	manager := auth.New()
	if opts.subject != "" {
		manager.Subject = opts.subject
	}
	discoveryClient := discovery.New(configCacheDir())

	switch positional[0] {
	case "auth":
		return runAuth(ctx, manager, positional[1:])
	case "services":
		return runServices(ctx, discoveryClient, positional[1:])
	case "schema":
		return runSchema(ctx, discoveryClient, positional[1:])
	}
	return runAPI(ctx, manager, discoveryClient, positional, opts)
}

// configCacheDir is where Discovery documents are cached.
func configCacheDir() string {
	return auth.ConfigDir() + string(os.PathSeparator) + "discovery"
}

// fail prints an error and maps it to an exit code.
func fail(err error) int {
	fmt.Fprintf(os.Stderr, "error: %v\n", err)
	var apiErr *client.Error
	if errors.As(err, &apiErr) {
		if apiErr.Status == 401 || apiErr.Status == 403 {
			return exitAuth
		}
		return exitAPI
	}
	return exitFailure
}

func runAuth(ctx context.Context, manager *auth.Manager, args []string) int {
	action := "status"
	if len(args) > 0 {
		action = args[0]
	}
	switch action {
	case "login":
		if _, err := manager.Login(ctx, os.Stderr); err != nil {
			return fail(err)
		}
		return exitOK
	case "logout":
		removed, err := manager.Logout()
		if err != nil {
			return fail(err)
		}
		if removed {
			fmt.Println("Credential removed.")
		} else {
			fmt.Println("No credential was cached.")
		}
		return exitOK
	case "print-token":
		token, err := manager.AccessToken(ctx)
		if err != nil {
			return fail(err)
		}
		fmt.Println(token)
		return exitOK
	case "status":
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(manager.Status()); err != nil {
			return fail(err)
		}
		return exitOK
	default:
		fmt.Fprintf(os.Stderr, "error: unknown auth command %q\n", action)
		return exitUsage
	}
}

func runServices(ctx context.Context, discoveryClient *discovery.Client, args []string) int {
	entries, err := discoveryClient.Directory(ctx)
	if err != nil {
		return fail(err)
	}
	filter := ""
	if len(args) > 0 {
		filter = strings.ToLower(args[0])
	}
	type row struct{ name, version, title string }
	var rows []row
	for _, entry := range entries {
		if filter != "" && !strings.Contains(strings.ToLower(entry.Name+" "+entry.Title), filter) {
			continue
		}
		rows = append(rows, row{entry.Name, entry.Version, entry.Title})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].name == rows[j].name {
			return rows[i].version < rows[j].version
		}
		return rows[i].name < rows[j].name
	})
	for _, item := range rows {
		fmt.Printf("%-28s %-16s %s\n", item.name, item.version, item.title)
	}
	return exitOK
}

func runSchema(ctx context.Context, discoveryClient *discovery.Client, args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "error: schema needs a target, e.g. drive.files.list")
		return exitUsage
	}
	segments := strings.Split(args[0], ".")
	if len(segments) < 2 {
		fmt.Fprintln(os.Stderr, "error: expected <service>.<resource>.<method>")
		return exitUsage
	}
	service, err := discoveryClient.Resolve(ctx, segments[0])
	if err != nil {
		return fail(err)
	}
	doc, err := discoveryClient.Load(ctx, service)
	if err != nil {
		return fail(err)
	}
	method, _, err := doc.FindMethod(segments[1:])
	if err != nil {
		return fail(err)
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(method); err != nil {
		return fail(err)
	}
	return exitOK
}

func runAPI(ctx context.Context, manager *auth.Manager, discoveryClient *discovery.Client, positional []string, opts *options) int {
	if len(positional) < 2 {
		fmt.Fprintln(os.Stderr, "error: expected <service> <resource...> <method>")
		return exitUsage
	}
	name := positional[0]
	if opts.apiVersion != "" && !strings.Contains(name, ":") {
		name = name + ":" + opts.apiVersion
	}

	service, err := discoveryClient.Resolve(ctx, name)
	if err != nil {
		return fail(err)
	}
	if service.Deprecated {
		fmt.Fprintf(os.Stderr, "warning: %s %s is marked deprecated by Google\n", service.API, service.Version)
	}
	doc, err := discoveryClient.Load(ctx, service)
	if err != nil {
		return fail(err)
	}
	method, _, err := doc.FindMethod(positional[1:])
	if err != nil {
		return fail(err)
	}

	params, err := decodeJSON("--params", opts.params)
	if err != nil {
		return fail(err)
	}
	applySharedDriveDefaults(method, params)

	var body any
	if strings.TrimSpace(opts.body) != "" {
		decoded, decodeErr := decodeJSON("--json", opts.body)
		if decodeErr != nil {
			return fail(decodeErr)
		}
		body = decoded
	}

	format, err := output.Parse(opts.format)
	if err != nil {
		return fail(err)
	}

	if opts.dryRun {
		endpoint, buildErr := client.BuildURL(doc, method, params)
		if buildErr != nil {
			return fail(buildErr)
		}
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")
		encoder.SetEscapeHTML(false)
		_ = encoder.Encode(map[string]any{
			"dry_run": true, "method": method.HTTPMethod, "url": endpoint,
			"body": body, "scopes": method.Scopes,
		})
		return exitOK
	}

	apiClient := client.New(manager)
	apiClient.UserProject = opts.userProject
	request := client.Request{Doc: doc, Method: method, Params: params, Body: body}
	writer := output.New(os.Stdout, format)

	switch {
	case opts.upload != "":
		result, uploadErr := apiClient.Upload(ctx, request, opts.upload, opts.uploadContentType)
		if uploadErr != nil {
			return fail(uploadErr)
		}
		if err := writer.Write(result); err != nil {
			return fail(err)
		}
	case opts.outputPath != "":
		written, downloadErr := apiClient.Download(ctx, request, opts.outputPath)
		if downloadErr != nil {
			return fail(downloadErr)
		}
		fmt.Fprintf(os.Stderr, "wrote %d bytes to %s\n", written, opts.outputPath)
	case opts.pageAll:
		if err := apiClient.All(ctx, request, writer.Write); err != nil {
			return fail(err)
		}
	default:
		result, doErr := apiClient.Do(ctx, request)
		if doErr != nil {
			return fail(doErr)
		}
		if err := writer.Write(result); err != nil {
			return fail(err)
		}
	}
	return exitOK
}

// applySharedDriveDefaults opts every Drive call into shared drive content.
//
// Google defaults supportsAllDrives and includeItemsFromAllDrives to false, so
// a caller who does not know to set them silently cannot see anything stored
// in a shared drive. Defaulting them on matches what a person means by "list
// my files"; an explicit value in --params always wins.
func applySharedDriveDefaults(method *discovery.Method, params map[string]any) {
	for _, name := range []string{"supportsAllDrives", "includeItemsFromAllDrives"} {
		if _, declared := method.Parameters[name]; !declared {
			continue
		}
		if _, explicit := params[name]; explicit {
			continue
		}
		params[name] = true
	}
}
