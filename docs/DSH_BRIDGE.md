# DSH bridge protocol (Go side), protocol version 1

This document is the contract between Plexus (`internal/adapters/dsh`) and the
DSH-side bridge plugin. The plugin's TypeScript source, tests and build live
outside this repository (`plexus-team/adapters/dsh-plugin/plugin/`). The built
file, `internal/adapters/dsh/bridge/plexus-bridge.min.mjs` (one zero-dependency
ESM file, about 28 KB), is committed here, and `go:embed` bundles it into
`plexus.exe`, which stays well under the 25 MiB size gate.

- **Install location:** the setup page's "Install the plugin" button, and every
  DSH session start, write the whole `plexus` profile to
  `<DSH_HOME>/profiles/plexus/` (`InstallAt`): the plugin, `package.json`, the
  Plexus overlay `plexus.patch.yml`, a version/sha256 stamp, and, created once
  and then left to the user, `cordis.patch.yml` and `pnpm-workspace.yaml`.
  Writes are atomic and happen only when the content changed. `DSH_HOME`
  defaults to `~/.dsh` (`%USERPROFILE%\.dsh` on Windows). Other profiles are
  never touched (DSH Desktop owns `profiles/desktop`).
- **Overlay pins:** approval policy `ask` and the `workspace-write` sandbox; no
  permission preset auto-approves (`danger-full-access` also asks). The
  session log, OTEL telemetry and package-inventory rows, and
  `tool-plugin-manager`, are disabled.
- **Node:** DSH needs Node ≥22.19 (the plugin states `^22.19 || >=24`). Plexus
  checks the version before it starts DSH.

The Go side is tested against its fake (`internal/fakes/rpc.go`, mode `dsh`)
and the plugin against a fake DSH context (bun tests). Both have also been run
live against DSH 0.2.0-rc.2 with a mock model (no real key). Points still
marked UNVERIFIED in the plugin source have not been exercised live.

## Transport

- **Launch:** Plexus starts DSH as `node <npm root>/@deepseek-ai/dsh/lib/bin.js --profile plexus --patch <DSH_HOME>/profiles/plexus/plexus.patch.yml [bots[].args…]`
  with the partner's workdir as cwd.
  - It never starts a `.cmd` shim: not npm's, and never DSH Desktop's `dsh.cmd` or its profile. `PLEXUS_DSH_EXE` or `bots[].exe` overrides the path.
  - The process runs in a Job Object (Windows) or process group.
- **Profile:** the `plexus` profile loads the bridge plugin, which takes over stdio
  in place of DSH's SDK JSON-RPC server (that row is disabled by the overlay).
- **Framing:** JSON-RPC 2.0, one JSON object per line (LF), on stdin/stdout.
  - A line may be up to **32 MiB** (`harness.MaxFrame`). A longer line is
    discarded up to its newline and the session goes on; it is not fatal.
    The plugin enforces the same cap on what it sends and receives.
  - stderr is drained and discarded. Set `PLEXUS_HARNESS_STDERR=1` to mirror it.
- **Direction:** both sides send requests. Unknown methods are answered with `-32601`.
- **Unknown fields:** ignored, and kept in `Event.Raw`.

## Plexus → DSH (requests)

| Method | Params | Result |
|---|---|---|
| `plexus.initialize` | `{protocol: 1, client: {name: "plexus", version}}` | `{protocol: 1, capabilities: {...}, ...}`. A different protocol number, or `capabilities.guest_lock` other than `"native"` (missing included), makes Plexus refuse the session. |
| `plexus.session.open` | `{cwd, persona, resume, tools, model?, effort?}` | `{sessionId}` |
| `plexus.prompt` | `{sessionId, text, guest}` | `{turnId}`, returned **immediately**. The turn then streams `plexus.event`. |
| `plexus.cancel` | `{sessionId}` | `{}`. Interrupts the running turn. |
| `plexus.steer` | `{sessionId, text}` | `{}`. Folds `text` into the running turn (a new message arrived while it was working). |
| `plexus.stopTask` | `{sessionId, taskId}` | `{}`. Stops one background job by the `name` it reported. |
| `plexus.control` | `{sessionId, name, payload}` | any. A raw passthrough for native features. |
| `plexus.shutdown` | `{}` | `{}`; the bridge exits 0. Sent by `Close` (bounded to 2 s) before the process tree is killed. |

### `plexus.initialize` and the start-up rule

DSH accepts strangers, so Plexus starts a DSH session **only** when the bridge
reports `guest_lock: "native"`. That means the plugin installed its
`tools.guard`. A missing or empty `guest_lock` counts as not native: Plexus
fails closed and refuses to start (`dsh bridge guest lock is not native …`). A
bridge whose DSH build has no `tools.guard` reports
`guest_lock: "unsupported"`. If such a bridge were reached anyway, it would
refuse every prompt with `-32007 SafetyGateUnavailable`.

### `plexus.session.open`

- `resume`: the bridge's earlier `sessionId` for this (partner, Slack thread), or `""` for a new session.
  - Must restore the native conversation so that context survives restarts.
- `persona`: appended to DSH's own system prompt. It **must not replace** it.
- `tools`: Plexus host tools to register as native DSH tools. Each entry is
  `{name, description, inputSchema}`, where `inputSchema` is a JSON Schema object.
  - Current tools: `plexus_post`, `plexus_delegate`, `plexus_ack`, `plexus_deliver`, `plexus_review`, `plexus_stop_tree` (the list comes from the Go side).
  - When the model calls one, the bridge sends `plexus.tool` (below) and returns the reply to the model as the tool result.
- `model?`, `effort?`: optional, from `SessionOptions.Model` / `Effort`. Plexus
  omits them when empty, and the bridge then uses DSH's own configured default
  (`agentDefaultModel`):
  - no model: DSH's default model, with its default effort unless `effort` was given;
  - a model but no provider: the bridge uses DSH's **default provider**.
- The bridge must not trim DSH's own tools, MCP servers, skills or subagents.

### `plexus.prompt` and `guest`

`guest` (from `harness.Turn.Guest`) is the only authority flag on the wire.
There is no `level`. A legacy `level` member is ignored by the bridge and never
widens a turn.

- `guest: false`: a turn from Sin or a partner. All of DSH's tools are available.
  - Every non-read, non-meta, non-host tool call still goes through `plexus.permission`. That is how the dangerous-action gate works.
- `guest: true`: a stranger's turn (the stranger guard is on). This is the **GuestLock**.
  - The guest gets **`plexus_post` only**. Every other tool is denied:
    shell, writes, **reads** (no workdir reads at all), fetches, and every other
    Plexus host tool.
  - The lock is native: the bridge denies in DSH's `tools/pre-execute` and in
    `tools.guard`, and rejects DSH approval requests, without asking Plexus.
    Denied calls never reach `plexus.permission`.
  - Native slash commands are not run for guest turns.

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

Events may arrive before the `plexus.prompt` reply, including the turn's
`final`. Plexus maps them to the turn anyway, and an early `final` does not
cancel the pending `plexus.prompt` call.

### `plexus.permission` (request)

```json
{"sessionId":"s1","turnId":"t1","id":"perm-1","parentId":"","reason":"",
 "tool":{"call_id":"c1","name":"bash","input":{"command":"git push --force","workdir":"sub"}},
 "options":[{"id":"allow-once","label":"Allow once","kind":"allow_once"},{"id":"reject-once","label":"Reject","kind":"reject_once"}]}
```

Reply with `{allow, optionId, always, reason}`.

- **Which calls are forwarded:** every tool call except reads, Plexus host
  tools and DSH-internal meta tools (`send_message`, `interrupt_agent`,
  `list_agents`, `workflow`, `exit_plan_mode`, `skill`, `todo_write`,
  `ask_user_question`, `subagent*`, `job_*`, `*_goal`). The plugin makes no
  danger judgement of its own. `tools.guard` denies any forwarded-class call
  whose call id Plexus did not clear, so nothing can skip the round trip.
- **Plexus answers at once.** It never holds the callback open.
  - A dangerous action is **parked**: the reply is `allow:false` with reason `已暂挂，等 Sin 批准`. The bridge relays that reason to the model verbatim, and the turn goes on.
  - After Sin approves on the approval card, the identical call is allowed **once** when the model issues it again ("for this task" grants cover the same rule and target until the task ends).
  - A call with an empty call id is denied.
- **One classifier.** The dangerous-action classifier is Plexus's Go code (`internal/danger`), the same one every adapter uses.
  - `tool` is `{call_id, name, input}`. The adapter derives `kind`, `command`, `paths` and the call's own `workdir`. DSH-internal meta tools are pinned to `meta`. Then `harness.Normalize` runs, and the adapter adds DSH-specific path arguments (`directory`, move `source`, …).
  - The per-call `workdir` (DSH bash/pwsh `workdir`, or `cwd`) becomes `ToolRequest.Workdir`, so relative paths in the command resolve where DSH runs it, not against the session workdir.
  - A shell call whose command Plexus cannot read is treated as dangerous (`exec.opaque`).

### `plexus.question` (request)

```json
{"turnId":"t1","id":"q","parentId":"","questions":[{"id":"q1","header":"DB","text":"Which DB?","options":[{"label":"SQLite"}],"multi":false,"free_text":true}]}
```

- Reply with `{answers: {"q1": ["SQLite"]}}`.
- Plexus posts the question as text in the Slack thread and waits with no timeout. When a partner or a recovered turn asked, Plexus @mentions Sin. The person who asked (or Sin) answers by replying in the thread.
- In a guest turn, `ask_user_question` is denied like every other tool, so no question reaches Plexus.

### `plexus.tool` (request)

```json
{"sessionId":"s1","turnId":"t1","id":"tool-7","parentId":"","guest":false,"callId":"c7","name":"plexus_post","arguments":{"text":"…"}}
```

- Plexus reads the top-level `id`, `name` and `arguments`.
- Reply with `{text, isError}`.
- `id` must be unique per call. Plexus uses it for idempotent Slack posts.

## Session lease (Desktop coexistence)

If `plexus.session.open` with `resume` fails because another process holds the
session (an error mentioning a lease, a lock, an active writer, "in use" or
"already open"), Plexus maps it to `harness.ErrActiveElsewhere`. It then posts
once in the thread that it did not resume the conversation, and writes nothing
to it. Idle sessions are closed after 5 minutes, so Plexus does not hold a
lease while idle.

## Turn rules

- One turn at a time per session. While a turn runs, Plexus may send `plexus.steer` any number of times.
- After `plexus.cancel`, the bridge should still send `final` or `error` for the turn.
- On stop, Plexus sends `plexus.stopTask` for each running background job and `plexus.cancel`, waits 3 s, and then closes the session: `plexus.shutdown` (2 s bound), then the process tree is killed.

## Capabilities the Go side declares for DSH

`PermissionCallback`, `AskUser`, `BackgroundTasks`, `Subagents`, `Resume`,
`Interrupt`, `SystemPrompt`, `Control`, `PerTaskStop`, `HostTools`, `GuestLock`:
`Native`, via the bridge plugin. `SlashCommands`, `Effort` and `StopHook` are
declared `Unsupported`. (`Effort` is still passed through `session.open`;
the capability flag has not been updated.)

## Known limitations

- No Provider in SessionOptions, so DSH's default provider is used.
- The DSH adapter does not yet emit the `frame_dropped` error event for a
  discarded oversize line (the Claude, Codex and ACP adapters do). The frame is
  dropped and the session continues.
- Methods the plugin defines that the Go side does not use: `plexus.session.close`
  (not sent; idle unload and stop close the process instead) and the
  `plexus.request.cancelled` notification (ignored).
