# Engineering conventions

These rules apply to every package. `docs/PLAN.md` is the authoritative spec;
this file covers *how* code is written.

## Layout & dependencies
- Dependency direction: `domain` / `settings` (pure, no I/O) ← `service` ← adapters
  (`api`, `cli`, `mcp`, `storage`, `engine/*`). Adapters never import each other,
  except `cli` → `client`.
- `internal/daemon` is the only composition root: it may import everything and
  wires dependencies together. `cli` may import `daemon` (for `dlm serve`) but
  no other server-side package; tests are exempt.
- No package-level mutable state and no `init()` side effects. Pass dependencies
  explicitly through constructors (`NewX(deps...)`).
- OS-specific code lives only in `internal/platform`. Use `filepath`, never
  hard-coded separators. All builds must succeed with `CGO_ENABLED=0`.
- Before adding a dependency, check its latest version, license (must be
  Apache-2.0 compatible: MIT/BSD/Apache/MPL-2.0/ISC) and current docs.

## Errors
- Wrap with context: `fmt.Errorf("storage: get task %q: %w", id, err)`. Messages
  start with the package name, lower-case, no trailing punctuation.
- Use sentinel errors / typed errors from `internal/domain` for conditions callers
  branch on (`errors.Is` / `errors.As`). Never compare error strings.
- Never `panic` for runtime errors; never swallow errors silently. If an error is
  intentionally ignored, comment why.

## Context, concurrency & lifecycle
- `ctx context.Context` is the first parameter of any function doing I/O or
  blocking work. Respect cancellation.
- Every goroutine has an owner that can stop it and wait for it (context +
  `sync.WaitGroup` / `errgroup`). No fire-and-forget goroutines.
- Never return internal slices/maps that are guarded by a lock; return copies.
- Long-running components expose `Run(ctx) error` or `Start`/`Close` pairs.

## Logging
- `log/slog` only, injected as `*slog.Logger`. Structured key/value attributes
  (`"task_id", id`), never string formatting in messages.
- Never log secrets (tokens, cookie values, passwords, Authorization headers).

## Time
- Code that depends on time takes a clock (`func() time.Time` or a small interface)
  so tests can control it. Store timestamps in UTC.

## JSON / API shapes
- JSON field names are `camelCase`. Enums are lower-case strings
  (`"running"`, `"awaiting_link"`).
- Optional settings use `settings.Optional[T]`; absent and `null` both mean "unset /
  inherit".

## Testing
- Standard `testing` package; table-driven tests with `t.Run`. `github.com/google/go-cmp`
  is allowed for diffs. No other assertion frameworks.
- Tests must be hermetic: no network access beyond `httptest`, temp dirs via
  `t.TempDir()`, configuration via `DLM_HOME`-style injection, never the real
  user directories.
- `go test -race ./...` must pass. Tests must not depend on wall-clock sleeps
  for correctness where a fake clock or synchronisation can be used.

## Tooling (Definition of Done)
- `golangci-lint fmt ./...` (gofumpt + goimports), `golangci-lint run ./...`,
  `go vet ./...`, `go test -race ./...` — all clean.
- Conventional Commits; one task per branch/PR (`Closes #N`); tick the task in
  `docs/PLAN.md` in the same PR.
