# Contributing to SpaceHound

Thanks for helping. SpaceHound deletes and moves people's files, so safety
comes before features.

## Ground rules

- **No third-party modules.** Go standard library and macOS system libraries
  through the small cgo layer are the supported dependency set. Building
  requires macOS, Go 1.22+ and Apple's command-line compiler tools. Keep
  `CGO_ENABLED=1`; pure-Go builds cannot provide these filesystem guarantees.
- **Never touch real data in tests.** Tests must only use `t.TempDir()` or
  the `internal/demo` fixture. Don't scan `$HOME`, `/Volumes` or the real
  Trash in tests, and don't run the binary against real data while
  developing. Use `--demo`.
- **Keep the safety model intact.** Any change to Trash, offload, identity
  checks or classification needs tests, and needs [SAFETY.md](SAFETY.md)
  updated if behaviour changes. New mutating actions must use a summary
  screen and typed confirmation. Reopen through held descriptors, validate
  scan identity, and journal intent before capturing originals. Never add
  pathname check-then-rename or metadata-losing fallbacks.
- **Be conservative when classifying.** If a file might be app data,
  mark it `review` or `dangerous`, not `low`.

## Workflow

```sh
make fmt        # gofmt
make vet        # go vet ./...
make test       # go test -race ./...
make build      # ./spacehound
make demo-json  # executable check on a temporary fixture
make demo       # interactive check on a temporary fixture
```

`make check` runs the formatting check, vet, tests and build together. It
must pass before you open a pull request.

`make snapshot` builds darwin/arm64 and darwin/amd64 archives locally, selecting
the matching clang `-arch` for cgo, and writes their checksums to `dist/`.
It does not publish a release. CI runs the race-enabled suite and native
builds before producing both architecture archives.

Safety tests must exercise changed destinations, source replacement during
capture, blocked restoration, interrupted journal entries, metadata loss,
terminal controls and protected scan roots. On kernels/filesystems without
guarded unlink, assert explicit refusal and original retention rather than
skipping the test or introducing a weaker deletion fallback. Test metadata
with creation time distinct from modification time, not just equal times.

## Layout

| Path | Purpose |
|------|---------|
| `cmd/spacehound` | Flags and wiring |
| `internal/scan` | Read-only walker, classifier, duplicates, skip rules |
| `internal/model` | Report types shared by JSON and the UI |
| `internal/fsid` | File identity capture and revalidation |
| `internal/macosfs` | Native descriptor-relative operations and complete fingerprints |
| `internal/fileops` | Journaled source capture and non-overwriting restoration |
| `internal/trash` | Move to Trash (never unlink) |
| `internal/volumes` | External volume discovery and suitability |
| `internal/offload` | Verified copy-before-remove offload |
| `internal/oplog` | Private durable operation/recovery journal |
| `internal/display` | Escaping untrusted text for terminal rendering |
| `internal/diskspace` | Free space and APFS shortfall explanations |
| `internal/term`, `internal/tui` | Raw terminal and UI state machine |
| `internal/demo` | Deterministic temporary fixture |

## Commits

Use short conventional-style subjects (`feat:`, `fix:`, `docs:`, `test:`).
Explain the safety impact in the commit body when there is one.
