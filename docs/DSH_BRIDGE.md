# DSH bridge protocol (Go side), protocol version 1

This document is the contract between Plexus (`internal/adapters/dsh`) and the
DSH-side bridge plugin. **The plugin itself (TypeScript) is not in this
repository yet.** `internal/adapters/dsh/plugin/files/` only holds a placeholder
README. When the plugin is written, its built files (ideally one minified
`plexus-bridge.min.js` plus a manifest) go into that directory. `go:embed` then
bundles them into `plexus.exe`, which has plenty of room under the 25 MiB size
gate. The setup page's "Install DSH plugin" button writes them to
`<DSH_HOME>/plugins/plexus-bridge/`. `DSH_HOME` defaults to `~/.dsh`.

Everything here is **UNVERIFIED** against a real DSH. Only the Go side and its
fake (`internal/fakes/rpc.go`, mode `dsh`) are tested.

## Transport

- **Launch:** Plexus starts DSH as `node <npm root>/@deepseek-ai/dsh/lib/bin.js --profile plexus [bots[].args…]`
  with the partner's workdir as cwd.
  - It never starts the `.cmd` shim. `PLEXUS_DSH_EXE` or `bots[].exe` overrides the path.
  - The process runs in a Job Object (Windows) or process group, and `Close` kills the whole tree.
- **Profile:** the `plexus` profile must load the bridge plugin, and the plugin must take over stdio.
  Whether this happens in a profile file or a plugin hook is the plugin's business.
- **Framing:** JSON-RPC 2.0, one JSON object per line (LF), on stdin/stdout.
  - Lines may be longer than 1 MiB.
  - stderr is drained and discarded. Set `PLEXUS_HARNESS_STDERR=1` to mirror it.
- **Direction:** both sides send requests. Unknown methods are answered with `-32601`.
- **Unknown fields:** ignored, and kept in `Event.Raw`.

## Plexus → DSH (requests)

| Method | Params | Result |
|---|---|---|
| `plexus.initialize` | `{protocol: 1, client: {name: "plexus", version}}` | `{protocol: 1}`. A different number makes Plexus refuse the session. |
| `plexus.session.open` | `{cwd, persona, resume, tools}` | `{sessionId}` |
| `plexus.prompt` | `{sessionId, text, level}` | `{turnId}`, returned **immediately**. The turn then streams `plexus.event`. |
| `plexus.cancel` | `{sessionId}` | `{}`. Interrupts the running turn. |
| `plexus.steer` | `{sessionId, text}` | `{}`. Folds `text` into the running turn (a new message arrived while it was working). |
| `plexus.stopTask` | `{sessionId, taskId}` | `{}`. Stops one background job by the `name` it reported. |
| `plexus.control` | `{sessionId, name, payload}` | any. A raw passthrough for native features. |

### `plexus.session.open`

- `resume`: the bridge's earlier `sessionId` for this (partner, Slack thread), or `""` for a new session.
  - Must restore the native conversation so that context survives restarts.
- `persona`: appended to DSH's own system prompt. It **must not replace** it.
- `tools`: Plexus host tools to register as native DSH tools. Each entry is
  `{name, description, inputSchema}`, where `inputSchema` is a JSON Schema object.
  - Current tools: `plexus_post`, `plexus_delegate`, `plexus_deliver`, `plexus_stop_tree`.
  - When the model calls one, the bridge sends `plexus.tool` (below) and returns the reply to the model as the tool result.
- The bridge must not trim DSH's own tools, MCP servers, skills or subagents.

### `plexus.prompt` and `level`

`level` is one of the following:

- `full`: a turn from Sin or a partner. All of DSH's tools are available.
  - Every tool call still goes through `plexus.permission`. That is how the dangerous-action gate works.
- `readonly`: a stranger's turn (the stranger guard is on). This is the **GuestLock**.
  - The bridge must deny every tool except reading files inside `cwd`, and `plexus_post`.
  - Lock it at DSH's native permission layer, not only by prompt.
  - Plexus also denies non-read requests in `plexus.permission`, but the native lock is what makes this hold for tools that skip the callback.
- `chat`: no tools at all.

`text` already has the turn frame prepended (who sent it: Sin, partner or stranger,
plus the thread and task id). Send it to the model verbatim.

## DSH → Plexus

### `plexus.event` (notification)

The params are a `harness.Event`, plus the bridge's own `turnId`:

```json
{"turnId":"t1","kind":"message","id":"m1","parent_id":"","text":"…"}
```

| `kind` | Meaning / required fields |
|---|---|
| `text_delta` | Streamed provisional text (`text`). Optional. |
| `message` | A committed assistant message (`text`). |
| `tool_use` / `tool_result` | Informational. `tool: {call_id, name, kind, command?, paths?}`, `status`. These count as "work evidence" for the loop guard, so please emit them. |
| `background` | A background job's lifecycle: `name` (the job id later passed to `plexus.stopTask`), `status` = `started` / `running` / `done` / `failed`. May arrive after the turn ended (`turnId` `""`); Plexus then starts an autonomous turn. |
| `final` | The turn finished. `text` = the final answer. Exactly one per turn. |
| `error` | The turn failed (`turnId` set), or the session failed (`turnId` `""`). |
| `extension` | Native-only event (`name`, payload in raw). |

`parent_id` gives subagent lineage (`""` = the root agent).

### `plexus.permission` (request)

```json
{"turnId":"t1","id":"p1","parentId":"","reason":"…",
 "tool":{"call_id":"c1","name":"bash","kind":"shell","command":"git push --force","paths":[]},
 "options":[{"id":"allow","label":"Allow","kind":"allow_once"}]}
```

Reply with `{allow, optionId, always, reason}`.

- **The bridge must block the tool call until the reply arrives, with no timeout.**
  - A dangerous action waits for Sin's click on an approval card, which may take hours.
  - If DSH itself times out, treat it as a deny.
- **`kind`:** one of `read`, `write`, `shell`, `fetch`, `ask`, `meta`, `other`.
  - Fill `command` for shell calls and `paths` for file calls. The dangerous-action classifier reads exactly these fields. Missing fields mean the gate cannot see the call.
- **Coverage:** the hook must fire for **every** tool call, including the ones DSH would auto-allow. Otherwise neither the stranger guard nor the approval gate can see them.

### `plexus.question` (request)

```json
{"turnId":"t1","id":"q","parentId":"","questions":[{"id":"q1","header":"DB","text":"Which DB?","options":[{"label":"SQLite"}],"multi":false,"free_text":true}]}
```

- Reply with `{answers: {"q1": ["SQLite"]}}`.
- Plexus posts the question as text in the Slack thread and waits with no timeout. When a partner or a recovered turn asked, Plexus @mentions Sin. The person who asked (or Sin) answers by replying in the thread.

### `plexus.tool` (request)

```json
{"turnId":"t1","id":"call-7","name":"plexus_post","arguments":{"text":"…"}}
```

- Reply with `{text, isError}`.
- `id` must be unique per call. Plexus uses it for idempotent Slack posts.

## Turn rules

- One turn at a time per session. While a turn runs, Plexus may send `plexus.steer` any number of times.
- After `plexus.cancel`, the bridge should still send `final` or `error` for the turn.
- On stop, Plexus sends `plexus.stopTask` for each running background job and `plexus.cancel`, waits 3 s, and then kills the process tree.

## Capabilities the Go side declares for DSH

`PermissionCallback`, `AskUser`, `BackgroundTasks`, `Subagents`, `Resume`,
`Interrupt`, `SystemPrompt`, `Control`, `PerTaskStop`, `HostTools`, `GuestLock`:
all `Native` **if** the plugin implements this document.
