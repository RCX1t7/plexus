# Slot for the DSH bridge plugin

Put the built bridge here as `plexus-bridge.min.mjs` (one minified ESM file,
about 37 KB). `go:embed` bundles this directory into `plexus.exe`.

When the file is present:
- `Bundled()` reports true.
- The setup page's "Install the plugin" button, and every DSH session start,
  write it to `<DSH_HOME>/profiles/plexus/` (`InstallAt`).

The adapter engineer's package (`dsh.go`, `session.go`, `install.go`, …) may
replace this whole directory's Go files. Keep the exported names the rest of
Plexus uses: `DSHHome`, `Install`, `InstallAt`, `Installed`, `Bundled`,
`InstallResult`, `ProfileName`.

Protocol: docs/DSH_BRIDGE.md.
