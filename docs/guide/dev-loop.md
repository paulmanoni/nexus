# The dev loop

```bash
nexus dev
```

`nexus dev` builds and runs the app, rebuilds it on save, and runs the project's Vite
beside it when there is a frontend. Open the URL it prints: the app's own origin. The
dashboard is on the same origin at `/__nexus`.

## Build, then swap

When you save, the next binary compiles **while the current one keeps serving**. Only a
successful build replaces the old process, so the app is unavailable for the swap itself
(about 20ms), not for the whole compile.

- **A broken save doesn't take the app down.** The compile error is printed, and the last
  good build keeps serving.
- **A save that doesn't change the binary skips the restart.** Examples are a comment, an
  edit to a `_test.go` file, or a package the app doesn't import. It prints
  `● binary unchanged`, and in-memory state survives.
- **The new binary is pre-run once** before the swap, so the operating system's
  first-launch cost is paid while the old process is still answering. On macOS, code
  signature checks take about 450ms.

Two dev-only defaults keep the link step, which dominates a warm rebuild, small:

- DWARF debug info is stripped. Pass `--debug` for delve or complete panic traces.
- The embedded frontend bundle is replaced with empty files in the dev binary, since dev
  serves `web/dist` from disk anyway. Pass `--no-embed-stub` to embed the real bundle.

## What triggers a rebuild

A rebuild is triggered by changes to:

- Go sources
- `go.mod` and `go.sum`
- `nexus.toml`
- files under an `//go:embed` root

A rebuild is not triggered by:

- `_test.go` files
- `testdata/`
- hidden files
- nested modules with their own `go.mod`, unless the root module `replace`s into one

To keep other paths out, put a `.nexusignore` next to `nexus.toml`. It uses gitignore
syntax:

```
tmp/                    # trailing slash: directories only
generated               # no slash: matches at any depth
internal/mock/*.go      # a slash anchors to the project root
assets/**/snapshots     # ** spans directories
!internal/mock/keep.go  # ! re-includes; later rules win
```

## Keeping state across rebuilds

A rebuild replaces the process, so in-memory maps are lost. `nexus.PreserveDev` hands
the state to the dev loop:

```go
func NewStore() *Store {
    s := &Store{notes: map[int]Note{}}
    nexus.PreserveDev("notes", s)          // no-op outside nexus dev
    return s
}

func (s *Store) SnapshotDev() ([]byte, error) { return json.Marshal(s.notes) }
func (s *Store) RestoreDev(b []byte) error    { return json.Unmarshal(b, &s.notes) }
```

- Without those methods, use `nexus.PreserveDevJSON(name, get, set)`.
- State that already has an on-disk format, such as SQLite, can live in
  `nexus.DevStateDir()`. It returns `""` outside `nexus dev`.
- State survives rebuilds, not Ctrl-C. A failed snapshot or restore is reported and
  skipped, never fatal.

## Frontend in dev

When the frontend directory has a `package.json`, `nexus dev`:

- installs its dependencies on the first run
- starts `vite` in its own process group
- learns Vite's address from the hot file

Vite's `Local:`/`Network:` banner is hidden, because it points at the wrong port. Its
other output is prefixed with `[web]`, and `--verbose` shows everything. On exit, Vite
gets SIGTERM, then SIGKILL after 2 seconds.

`nexus dev --dist` also keeps `web/dist` rebuilt in the background, so a `go build` taken
mid-session embeds the current frontend.

## Logs

JSON log lines are shown as colored columns: time, level, source and message. Configure
the format in `nexus.toml`:

```toml
[runtime.logging]
format   = "pretty"   # pretty | logfmt | pattern | raw
pattern  = "%time  %-5level  %caller  %msg  %fields"
requests = true       # one console line per HTTP request (dev only)
```

Formatting turns off when stdout isn't a terminal, so `nexus dev > log` keeps raw JSON.
Color honors `NO_COLOR`.

## Useful flags

| Flag | Does |
|---|---|
| `--addr host:port` | Overrides the listen address |
| `--open` | Opens a browser once the app answers |
| `--frontend <dir>` | Sets the frontend directory |
| `--dist` | Keeps `web/dist` rebuilt in the background |
| `--debug` | Keeps DWARF for delve |
| `--no-embed-stub` | Embeds the real frontend bundle |
| `--log-format`, `--log-pattern`, `--raw-logs` | Controls log formatting |
| `--go-run` | Uses the legacy `go run` loop |
