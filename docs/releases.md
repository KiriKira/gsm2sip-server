# Server releases

The `Release` workflow publishes Linux binary bundles to GitHub Releases. It
does not deploy to a public server or publish a container image.

## Release triggers

| Trigger | Release tag | Release type |
| --- | --- | --- |
| Push to `main` | `build-<run_number>-<commit_sha7>` | Prerelease |
| Push a `v*` tag | The exact Git tag | Stable release |
| Run manually on `main` with `publish: true` | `build-<run_number>-<commit_sha7>` | Prerelease |
| Run manually with `publish: false`, or from another branch | No release | Build and validation only |

Manual runs default to `publish: true`, but only a run whose ref is `main` can
publish. The workflow checks this in both its release decision and publishing
job. A manual run from another branch still runs the checks and builds both
architectures. Pushes to branches other than `main` do not trigger this
workflow. Re-running a main build keeps the same tag; the publisher accepts an
identical already-published build and rejects a changed payload for that tag.

## Checks before publishing

The workflow runs the server checks against the source commit being released:

- Go tests with the race detector and `go vet`.
- Contract validation.
- All PostgreSQL backup and restore integration tests against PostgreSQL 17,
  using PostgreSQL 17 `pg_dump`, `pg_restore`, and `psql` clients.
- Static `CGO_ENABLED=0` Linux builds for `amd64` and `arm64`.

The database integration tests use an isolated CI PostgreSQL service. The
workflow does not contact a carrier, connect to a production database, or
deploy the generated files.

## Release files

Each release contains:

- `gsm2sip-server-<version>-linux-amd64.tar.gz`
- `gsm2sip-server-<version>-linux-arm64.tar.gz`
- `release-manifest.json`, with the version, source commit, and each archive's name,
  SHA-256 digest, and byte size.
- `SHA256SUMS.txt`, with hashes for both archives and `release-manifest.json`.

Each archive contains the executable `bin/gsm2sip-api`,
`bin/gsm2sip-worker`, and `bin/gsm2sip-admin` binaries; the complete SQL
`migrations/`; backup and quarantine scripts; the server source and Docker
Compose build context; `.env.example`; and the README and operations
documentation. Its `manifest.json` identifies the source commit, version,
Linux architecture, and Go toolchain; its `SHA256SUMS` covers the files inside
the archive. This lets an operator either run the static binaries or use
Compose to build the included source. The source bundle omits `.git`, local
`.env` and `.local` files, database dumps, and `docs/ui-verification` screenshots
and results.

The API needs `MIGRATIONS_DIR` to point to the archive's `migrations/` directory
when run outside Compose. Follow [operations.md](operations.md) for database,
SIP, and recovery configuration. The sample `.env.example` contains development
placeholders; replace them before use. Generate and keep
`SECRETS_ENCRYPTION_KEY` in an external secret manager. It is never generated
into a release archive.

## Verify and unpack

After downloading the release files into one directory, verify their hashes
before extracting an archive:

```sh
sha256sum -c SHA256SUMS.txt
tar -xzf gsm2sip-server-<version>-linux-amd64.tar.gz
cd gsm2sip-server-<version>-linux-amd64
sha256sum -c SHA256SUMS
```

Choose the `arm64` archive for Linux ARM64 hosts. The checksums detect accidental
corruption; they are not a cryptographic signature. Keep database backups on an
encrypted disk as described in [operations.md](operations.md).
