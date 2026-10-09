# DSH bridge plugin

`plexus-bridge.min.mjs` is the built DSH-side bridge plugin: one minified ESM
file, zero runtime dependencies (only `node:` builtins), 28,400 bytes.
`//go:embed bridge/plexus-bridge.min.mjs` in install.go embeds **only that
file** into `plexus.exe` (not the source, tests or `node_modules`); the Windows
build stays well under the 25 MiB gate.

- `Bundled()` reports true.
- The setup page's "Install the plugin" button, and every DSH session start,
  write the whole `plexus` profile (plugin + overlay + package.json + stamp) to
  `<DSH_HOME>/profiles/plexus/` via `InstallAt` (atomic, only when changed).

Protocol: `docs/DSH_BRIDGE.md`.

## Layout

| Path | What |
|---|---|
| `plexus-bridge.min.mjs` | the artifact (generated; do not edit) |
| `plexus-bridge.sha256` | `sha256sum`-format manifest: the artifact's hash first, then the hash of every build input (generated) |
| `plugin/src/` | TypeScript source (`index.ts` entry, `bridge.ts`, `rpc.ts`, `dsh.ts`, `protocol.ts`) |
| `plugin/test/` | Bun tests and the fake DSH Cordis context they run against (`fakectx.mjs`) |
| `plugin/scripts/build.mjs` | the build |
| `plugin/package.json`, `plugin/package-lock.json` | pinned toolchain: `bun` 1.4.2 (the bundler), `typescript` 5.9.3, `@types/node` 20.19.43 |

Recorded artifact sha256:
`409d69263f4e1cfabafa69f4af4d45e0025c7038e9fa195ee7bc07b2cf0b9cbf`

## Rebuild (reproducible)

Requires Node.js >= 20 (verified with v20.19.2) and npm. You do not need a
global Bun: `npm ci` installs the pinned Bun 1.4.2 into `node_modules`, and
`build.mjs` uses only that one (it refuses any other version). Do not use
`--ignore-scripts`: the `bun` package's postinstall links its binary.

```sh
cd internal/adapters/dsh/bridge/plugin
npm ci
node scripts/build.mjs          # rewrites ../plexus-bridge.min.mjs and ../plexus-bridge.sha256
sha256sum ../plexus-bridge.min.mjs
# 409d69263f4e1cfabafa69f4af4d45e0025c7038e9fa195ee7bc07b2cf0b9cbf  ../plexus-bridge.min.mjs
```

The output is byte-identical across clones and machines: Bun is pinned
exactly, the entry is passed as a relative path, there is no source map, and
the banner holds only the package name and version (no time, path or host).
`.gitattributes` here marks everything `-text` so no checkout rewrites line
endings.

To verify without touching the tree (CI): `node scripts/build.mjs --check`
rebuilds to a scratch file and exits 1 unless the result is byte-identical to
the committed artifact and the manifest matches.

Tests and type check: `npm test` (`bun test test/`, pinned Bun) and
`npm run typecheck` (`tsc --noEmit`).

## Go-side checks (no Node needed)

`bridge_test.go` checks that the embedded bytes hash to the recorded sha256,
that the embed holds only the artifact, and that every build input (`src/*.ts`,
`package.json`, the lockfile, `tsconfig.json`, `build.mjs`) still matches the
manifest. Editing the source without rebuilding therefore fails `go test`.

After changing the source: run the build, then commit the source, the artifact
and `plexus-bridge.sha256` together. Keep `BridgeVersion` (install.go) and
`plugin/package.json`'s version in sync: the version stamps the profile
package.json (required by DSH) and the artifact's banner.
