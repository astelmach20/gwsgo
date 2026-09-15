# gwsgo

A Google Workspace CLI in Go. Every Google API that publishes a Discovery
document is reachable — there is no allowlist of supported services.

```bash
gwsg drive files list --params '{"pageSize":10}'
gwsg gmail users messages list --params '{"userId":"me","q":"is:unread"}'
gwsg calendar events list --params '{"calendarId":"primary"}' --format table
gwsg vault matters list
gwsg admin users list --params '{"customer":"my_customer"}'
```

Companion to [`ows`](https://github.com/astelmach20/outlook-workspace-cli),
which does the same job for Outlook / Microsoft Graph.

## Why

Google ships an official Rust CLI at
[googleworkspace/cli](https://github.com/googleworkspace/cli). It is
discovery-driven internally, but gates every call behind a hardcoded list of 18
services, so roughly 364 methods across 15 Workspace APIs — Admin Directory,
Vault, Cloud Identity, Drive Labels, Cloud Search and others — are unreachable.
Its documented `<api>:<version>` escape hatch does not work. Its `main` branch
has not merged a commit since 2026-03-31.

`gwsgo` keeps the discovery-driven design and drops the allowlist.

| | googleworkspace/cli | gwsgo |
|---|---|---|
| APIs reachable | 18 hardcoded | every published Discovery API |
| Unlisted API escape hatch | documented, broken | not needed |
| Resumable upload | ✗ (5 MB multipart ceiling) | ✓ |
| Service-account impersonation | ✗ | ✓ `--subject` |
| Pagination | silently capped at 10 pages | uncapped |
| Retry | 429 only | 429 + 5xx, jittered backoff |
| Shared drives | `supportsAllDrives` never sent | on by default |
| Dependencies | 30+ crates | Go standard library only |

## Install

```bash
go install github.com/astelmach20/gwsgo/cmd/gwsg@latest
```

Or download a signed binary from [Releases](https://github.com/astelmach20/gwsgo/releases).

## Authentication

Create an OAuth **Desktop** client in a Google Cloud project, enable the APIs
you intend to call, then:

```bash
export GWSG_CLIENT_ID=...apps.googleusercontent.com
export GWSG_CLIENT_SECRET=...
gwsg auth login
```

The login flow is PKCE over a loopback listener on `127.0.0.1`. Google removed
the out-of-band flow in 2023, so a browser on the same machine is required.
Ship the client secret alongside PKCE: it is not a real secret for a public
client, and Google's token endpoint still expects it for Desktop clients.

Other credential sources, in precedence order:

| Priority | Source | Set via |
|---|---|---|
| 1 | Access token | `GWSG_TOKEN` |
| 2 | Service account | `GOOGLE_APPLICATION_CREDENTIALS` |
| 3 | Cached user credential | `gwsg auth login` |

### Domain-wide delegation

Service accounts have no mailbox and no Drive of their own, so they must
impersonate a user:

```bash
export GOOGLE_APPLICATION_CREDENTIALS=/path/to/key.json
gwsg gmail users messages list --params '{"userId":"me"}' --subject user@example.com
```

Grant the service account's client ID the exact scopes you request under
**Admin console → Security → Access and data control → API controls →
Domain-wide delegation**. `gwsg` prints that instruction, with the scope list,
when Google returns `unauthorized_client`.

## Usage

```
gwsg <service> <resource...> <method> [flags]
gwsg auth <login|logout|status|print-token>
gwsg services [substring]
gwsg schema <service.resource.method>
```

| Flag | Purpose |
|---|---|
| `--params <JSON>` | URL and query parameters |
| `--json <JSON>` | Request body |
| `--upload <PATH>` | Upload a file (resumable above 5 MB) |
| `--output <PATH>` | Write a media/binary response to disk |
| `--format <FMT>` | `json` (default), `ndjson`, `table`, `csv` |
| `--page-all` | Follow pagination to exhaustion |
| `--subject <EMAIL>` | Impersonate a user (service accounts only) |
| `--user-project <ID>` | Set `x-goog-user-project` for quota attribution |
| `--api-version <VER>` | Pin a version (or use `service:version`) |
| `--dry-run` | Print the request without sending it |

### Discovering what exists

```bash
gwsg services vault              # find an API
gwsg schema drive.files.list     # inspect a method's parameters and scopes
gwsg drive files list --dry-run  # see the exact request that would be sent
```

### Version pinning

```bash
gwsg drive:v2 files list
gwsg drive files list --api-version v2
```

Unqualified names resolve to whichever version Google marks `preferred`, so
`gwsg postmaster domains list` picks up Postmaster Tools v2 automatically.

## Design notes

**Discovery-driven.** Google publishes a machine-readable description of every
API: resources, methods, HTTP verbs, paths, parameters and scopes. `gwsg`
fetches and caches those documents and builds each request from them at
runtime. New APIs work the day Google ships them, with no release here.

**Discovery URLs are not constructible.** `https://<api>.googleapis.com/$discovery/rest`
404s for Drive, and Calendar is served from `calendar-json.googleapis.com`.
`gwsg` reads each API's `discoveryRestUrl` from the directory verbatim.

**Pagination is never capped.** A silent page limit turns "list my files" into
"list some of my files" with no signal to the caller.

**Shared drives are visible by default.** Google defaults `supportsAllDrives`
and `includeItemsFromAllDrives` to `false`, which silently hides shared drive
content. `gwsg` sets both unless you pass your own value.

## Exit codes

| Code | Meaning |
|---|---|
| 0 | Success |
| 1 | Failure |
| 2 | Usage error |
| 3 | Authentication or permission error |
| 4 | API error |

## Development

```bash
make            # fmt, vet, test, build
make race       # race detector
make cover      # coverage summary
make snapshot   # local GoReleaser build
```

## Status

Early. The generic API surface, auth (user + service account + DWD), resumable
upload, export and output formats are implemented and tested. Convenience
helpers for the parts no spec can describe — Gmail reply threading and
reply-all recipient math, calendar agendas, Drive export MIME defaults — are
next.

## License

MIT. Not an official Google product.
