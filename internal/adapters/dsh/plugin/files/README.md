# Plexus bridge plugin for DSH (placeholder)

The DSH-side bridge plugin (TypeScript) is written by the adapter engineer
and dropped into this directory. Everything here is embedded into
plexus.exe with go:embed, and `plexus setup` installs it into
`%DSH_HOME%\plugins\plexus-bridge\`.

The Go side of the protocol is described in docs/DSH_BRIDGE.md.
