# DSH bridge plugin (embedded)

`plexus-bridge.min.mjs` here is the built DSH-side bridge plugin: one minified
ESM file, zero runtime dependencies (only `node:` builtins), ~28 KB. `go:embed`
(`//go:embed all:bridge` in install.go) bundles this directory into
`plexus.exe`; the Windows build stays well under the 25 MiB gate.

When the file is present:
- `Bundled()` reports true.
- The setup page's "Install the plugin" button, and every DSH session start,
  write the whole `plexus` profile (plugin + overlay + package.json + stamp) to
  `<DSH_HOME>/profiles/plexus/` via `InstallAt` (atomic, only when changed).

Protocol: `docs/DSH_BRIDGE.md` (Go side) and the plugin's `PLUGIN-PROTOCOL.md`.

## Source and rebuild

The plugin's TypeScript source, tests and build live outside this repo, in the
team workspace: `plexus-team/adapters/dsh-plugin/plugin/` (`src/`, `test/`,
`scripts/build.mjs`). Rebuild with `bun run build` there; it writes
`plexus-bridge.min.mjs` here and checks the bundle has no non-`node:` imports.
Keep `BridgeVersion` (install.go) and the plugin's `package.json` version in
sync — the version stamps the profile package.json (required by DSH).
