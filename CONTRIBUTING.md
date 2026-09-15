# Contributing

## Getting started

```bash
git clone https://github.com/astelmach20/gwsgo
cd gwsgo
make
```

Go is the only prerequisite; there are no third-party dependencies and adding
one needs a good reason.

## Before opening a pull request

```bash
make fmt vet test race lint
```

CI runs the same checks on Linux, macOS and Windows, plus `govulncheck`.

## Conventions

- Commit messages follow [Conventional Commits](https://www.conventionalcommits.org)
  (`feat:`, `fix:`, `docs:`, `chore:`). Release notes are generated from them.
- Exported identifiers carry doc comments.
- Comments explain *why*, not *what*. If a line encodes a Google API quirk, say
  which quirk and cite the behaviour.
- New behaviour ships with tests. Network-dependent code is tested against
  `httptest` servers, never live Google APIs.

## Releasing

Tag and push; GoReleaser builds, signs and publishes.

```bash
git tag -a v0.2.0 -m "v0.2.0"
git push origin v0.2.0
```
