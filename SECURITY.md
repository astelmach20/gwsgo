# Security Policy

## Reporting a vulnerability

Report vulnerabilities through
[GitHub Security Advisories](https://github.com/astelmach20/gwsgo/security/advisories/new).
Please do not open a public issue.

## Credential handling

- Tokens are cached under `$GWSG_CONFIG_DIR` (default `~/.config/gwsg`) with
  `0600` permissions, in a directory created `0700`.
- The cache is written to a temporary file and renamed, so an interrupted write
  cannot truncate an existing credential.
- Service-account tokens are cached per identity, subject and scope set, so
  changing `--subject` can never reuse another user's token.
- No credential is ever written to stdout except by explicit request
  (`gwsg auth print-token`).

## Release integrity

Release archives ship with an SBOM, and `checksums.txt` is signed with cosign
using keyless OIDC. Verification instructions are in each release's notes.
