# Plexus

[中文说明](README.zh-CN.md)

Plexus puts the coding agents you already use (**Claude Code**, **Codex**, **DSH**, and any
**ACP** agent) into Slack as **partners**. Each partner is its own Slack app with its own
identity, connection, workdir and native session. You give goals in a Slack thread. The
partners discuss, split the work, hand off to each other, ask you when a decision is
really needed, and deliver verifiable results. One `plexus.exe` on your Windows laptop
starts and supervises all of them.

Plexus is a thin bridge. It does not run its own agent loop. Each harness keeps its own
login, model, tools, MCP servers, skills, subagents and background jobs, and Plexus talks
to it through its native protocol (Claude stream-json control protocol, Codex app-server,
DSH bridge, ACP).

> Status: **v0.1 skeleton**. It is unit- and integration-tested against fake harnesses and a
> fake Slack, but **not yet run against real Slack, real harnesses or real Windows**. See
> [Unverified](#unverified).

## Quick start (3 steps)

1. **Build or download** `plexus.exe`:
   ```
   CGO_ENABLED=0 GOOS=windows go build -trimpath -ldflags="-s -w" -o plexus.exe ./cmd/plexus
   ```
2. **Run `plexus setup`.** It detects installed harnesses (without sending any model request),
   adds one partner per detected harness, and opens a one-time `http://localhost:<port>/s/<nonce>/` page.
   On that page:
   1. Enter your Slack user ID (profile → ⋯ → *Copy member ID*). Leave the stranger guard on.
   2. For each partner, click **Create the Slack app**. Slack opens with the app manifest
      prefilled (Socket Mode, interactivity, scopes).
      - Create the app.
      - Under *Basic Information → App-Level Tokens*, generate an `xapp-…` token with
        `connections:write`.
      - *Install to Workspace*.
      - Paste the `xapp-…` token and the `xoxb-…` Bot User OAuth Token, then **Save**.
      - Or fill Client ID / Client Secret and use **Install to workspace** (OAuth via a localhost redirect).
   3. For DSH, click **Install the plugin**. This only works once the bridge plugin is bundled; see [docs/DSH_BRIDGE.md](docs/DSH_BRIDGE.md).
3. **Invite the partners** to a channel (`/invite @Plexus Claude …`), @mention one with a goal, and work in the thread.
   - Optional: `plexus install-task` registers a logon task so Plexus starts hidden with Windows.

Tokens are stored only in a DPAPI-encrypted file (`%APPDATA%\Plexus\secrets.dpapi`).
`config.json` never holds secrets.
On Linux and macOS (development only), set `PLEXUS_INSECURE_DEV_SECRETS=1` to allow a 0600 plain file.

## Commands

| Command | What it does |
|---|---|
| `plexus run [--setup] [--hidden]` | Runs all partners (default). Prints `plexus ready bots=<n>` on stderr once every partner made its first connection attempt. |
| `plexus setup` | Runs and opens the setup page. |
| `plexus detect` | Prints detected harnesses as JSON (version and login *evidence*, never values). |
| `plexus stop <task-id>` | Stops a task tree. It goes through the running hub's local control endpoint, or straight to the store when no hub runs. |
| `plexus install-task [--exe PATH]` | Registers the Windows logon task (`scripts/install-task.ps1`, embedded). |
| `plexus --version` | Prints the version without reading config or opening the store. |

`--config DIR` selects another config directory. `PLEXUS_HOME` overrides both the config
and data directories.

## How work flows

- **A thread is a task.** Your root message (`<channel>:<ts>`) is the task id.
  - Each (partner, thread) pair is bound to one native session, so a partner never answers with amnesia, even after a restart.
- **The partners in a thread see all of its messages.** They decide for themselves whether to answer.
  - When you @mention a partner directly, it replies with its final text.
  - Turns triggered by other partners, background jobs or deliveries are *autonomous*. They post only by calling `plexus_post`, so idle partners stay quiet.
- **New messages steer the running turn.** They use Claude's queued input, Codex `turn/steer` or DSH `plexus.steer`. There is never an "I'm busy" reply.
- **Host tools.** Each partner gets these as native tools:
  - `plexus_post`: post to this thread.
  - `plexus_delegate`: hand off to another partner with a six-field record (below).
  - `plexus_deliver`: post a delivery with artifacts. It computes their SHA-256 and writes `out/MANIFEST.sha256`.
  - `plexus_stop_tree`: only on your turn.
- **Loop guard.** Plexus stops delivering repeated partner chatter that has no tool work in between. A human message or new work resumes it. There is no turn cap, no budget and no timeout.

### Handoff record

Every `plexus_delegate` carries six fields:

| Field | Meaning |
|---|---|
| `task` | One line. |
| `inputs` | Relative paths, permalinks or URLs. |
| `done_when` | Checkable statements. |
| `evidence` | Expected or actual proof. |
| `tried_failed` | Known dead ends. |
| `owner_if_stuck` | Who to ask. Defaults to the delegating partner. |

The record is stored in the local database and rendered as a card in the thread. It is for
clarity and traceability. **It is not an authorization mechanism.**

## Trust model

- **You and your partners trust each other fully.** There are no permission levels, signed
  tokens or approvals between partners. Identity is the Slack `user` / `bot_id` compared with
  `owners` and the partners' Slack user IDs.
- **Stranger guard.** This is the only boundary (on by default; switch it off on the setup page).
  - A stranger is anyone who is neither you nor a partner, including other apps and workflows.
  - A stranger's message only gets a conversation. Core sends the turn at level `chat`, and every adapter must enforce it with the harness's native lock (**GuestLock**): nothing that writes or executes. Reads inside the workdir are allowed, but no fetches, delegation, deliveries or stops.
  - Claude: a PreToolUse hook fires on every tool call, and Plexus denies everything except workdir reads and `plexus_post`.
  - DSH: `level: "chat"` and `guest: true` on the prompt (bridge plugin).
  - **Codex and ACP have no hard native lock**: in read-only sandboxes, they run "safe" commands without asking. So they ignore strangers outright. With the guard on, these partners reply *"I can only take requests from my team here."* and run no turn.
  - A partner's message inherits trust from the turn it was posted from. If that origin is unknown, Plexus fails closed.
- **Dangerous actions need your click.** This is the only approval gate.
  - Plexus has **one** classifier, written in Go. Every adapter, DSH included, sends each tool call through it, after `harness.Normalize` fills in the call's kind, command and paths.
  - The classifier stops these:
    - force push or history rewrite (`git push --force/-f/--force-with-lease/+ref/--mirror/--delete`, also with global flags such as `git -C dir` or `git -c k=v` before `push`; `filter-branch`/`filter-repo`/bfg; `rebase`/`reset`/`commit --amend` when HEAD is already pushed);
    - deleting files outside the workdir;
    - registry, service, scheduled-task, system-directory, machine-environment or installer changes;
    - mail, webhooks, and MCP tools that look like they send messages;
    - a shell call whose command Plexus cannot read (`exec.opaque`).
  - **Parking.** Only that one call is suspended, never the whole turn:
    - The permission callback denies the call at once with `已暂挂，等 Sin 批准` ("parked, waiting for Sin's approval"). The partner carries on with other work or ends its turn.
    - Plexus posts an approval card (**Approve / Deny**) in the thread.
    - After you approve, the identical call is allowed **once** when the partner issues it again. Plexus tells the partner it may retry.
  - **Only your click counts.** You can also reply `approve` / `批准` / `deny` / `拒绝` in the thread.
  - Pending cards survive a restart. They are updated in place and never reposted. A stop turns them into denies.
  - Tune the gate in `config.json`:
    ```json
    "dangerous_actions": {"use_defaults": true, "disable": ["git.rewrite"],
                          "extra_commands": ["^terraform apply"], "extra_mcp_tools": ["deploy"]}
    ```
  - Plexus sees commands, not what scripts do inside (`./deploy.sh`, `make release`). Git aliases or hooks can also turn a plain `git push` into a force push.

## Starting a partner

Plexus checks each partner before it starts it:

- **Guest lock.** With the stranger guard on, the harness must either lock strangers' turns natively (Claude, DSH) or ignore strangers outright (Codex, ACP). If a harness would accept strangers without a native guest lock, the partner **refuses to start**, and the log says why.
- **Approval gate.** The harness needs a blocking permission callback, otherwise the dangerous-action gate cannot hold. Set `"ungated_ok": true` on that partner in `bots[]` to run it anyway at your own risk.
- **`ungated_ok` waives only the approval gate, never the guest lock.**

## Sharing sessions with the desktop apps

You may also open the same sessions in Claude Desktop, the Codex app or DSH. Plexus never writes into a conversation that is live somewhere else.

- **Before every resume**, Plexus checks whether the session is active elsewhere:
  - Claude: a live process in `~/.claude/sessions/*.json` holds the session, or its transcript changed in the last 2 minutes after Plexus last used it.
  - Codex: the app-server reports another active writer.
  - DSH: the session lease is held.
- If the session is active elsewhere, Plexus posts once: *"⏸️ I did not resume this conversation… Close it there (or let it go idle), then send your message again."*
- **Workdir notes.** `<data dir>/locks/workdir-<hash>.lock` records which partner works where. It is only a warning in the log, never an exclusive lock: partners may share a repo.
- **Idle unload.** A thread's harness process exits after 5 minutes without traffic, unless background tasks are still running. Idle partners keep no harness process alive. The next message resumes the session from its saved ID.
- Plexus never uses the desktop DSH profile or `dsh.cmd`. It launches DSH with its own `plexus` profile.
- `sessions`, attach, take, fork and release commands are deferred to a later version.

## Credentials

- DPAPI holds **only Plexus's own Slack tokens** (`xapp`, `xoxb`).
- The Slack OAuth client secret lives only in memory, for at most 10 minutes during setup.
- Plexus never stores, injects or scrubs harness credentials. Each harness uses its own login.
- The only environment variables Plexus removes from a harness's environment are `CLAUDECODE` and `CLAUDE_CODE_SIMPLE`. Values of auth-looking variables are registered with the log redactor and never logged.

## Stop

Three ways to stop:

- Send exactly `stop` or `停` in the task thread.
- Run `plexus stop <task-id>` in Slack or in a terminal.
- Ask in words. The partner calls `plexus_stop_tree` (only on your turn).

Plexus revokes the tree, stops background tasks (`stop_task`) and natively interrupts every
affected session. After 3 s, it kills the process trees (a Job Object on Windows). Pending
approvals become denies. Exactly one "已停止 / stopped" is posted. The tree starts no new
turns until you speak in the thread again; your message starts a new task on the same
native session.

## Restarts

- **Inbound messages:** each one is recorded before it is acknowledged.
- **Outbound posts:** each carries a `request_id` in Slack message metadata. If a send was interrupted, Plexus checks the thread history before resending. It never double-posts.
- **Interrupted turns:** they resume on the native session with a note to check the real state first, so finished work is not redone.

## Adding a harness

Claude Code (`claude_code`), Codex (`codex`) and DSH (`dsh`) are native adapters. The
`gemini_cli` and `dsh_acp` ACP entries are built in. Any other ACP agent needs only config:

```json
"acp_harnesses": [
  {"name": "my_agent", "executables": ["my-agent"], "args": ["--acp"],
   "cred_files": [".my-agent/credentials.json"], "cred_env": ["MY_AGENT_API_KEY"],
   "exe_env": "PLEXUS_MY_AGENT_EXE"}
],
"bots": [
  {"name": "my_agent", "display_name": "My Agent", "harness": "my_agent",
   "enabled": true, "workdir": "C:\\Users\\you\\PlexusWork\\my_agent"}
]
```

ACP partners have no host tools and no GuestLock. They answer @mentions, and their text is
checked by whoever delegated to them.

## Files

| What | Where (Windows) |
|---|---|
| Config (no secrets) | `%APPDATA%\Plexus\config.json` |
| Secrets (DPAPI) | `%APPDATA%\Plexus\secrets.dpapi` |
| Database (bbolt) | `%LOCALAPPDATA%\Plexus\plexus.db` |
| Log (redacted) | `%LOCALAPPDATA%\Plexus\plexus.log` |
| Control endpoint | `%LOCALAPPDATA%\Plexus\control.json` (127.0.0.1 port and a per-run nonce, removed on exit) |
| Partner workdirs | `%USERPROFILE%\PlexusWork\<partner>` (configurable) |

## Unverified

These parts are implemented against the vendors' published docs and tested only against fakes:

- **Platforms:** real Windows (Job Object kill, DPAPI, the logon task, hidden console), real Slack (Socket Mode, message metadata, interactivity buttons, OAuth redirect), real harnesses.
- **Claude Code:** the PreToolUse hook, the `mcp_message` SDK MCP server, `stop_task`, steering via queued input, and the `~/.claude/sessions/*.json` record format used by the resume check.
- **Codex:** `dynamicTools` (experimental), `turn/steer`, the read-only access shape, the `untrusted` approval policy, and the exact error text for "another active writer". Whether its elevated sandbox stays inside the Job Object.
- **DSH:** the bridge plugin is written separately and is **not bundled** in this build. `internal/adapters/dsh/bridge/` is the empty slot it drops into (contract: [docs/DSH_BRIDGE.md](docs/DSH_BRIDGE.md)). DSH needs Node ≥22.19.
- **ACP:** the Gemini CLI ACP flag.
- **Tokens:** DPAPI ties tokens to your Windows user, so any process running as you, including a fully trusted partner, can decrypt them.

## Docs

- [docs/REQUIREMENTS.md](docs/REQUIREMENTS.md): acceptance criteria.
- [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md): design as implemented, including divergences (Chinese).
- [docs/DSH_BRIDGE.md](docs/DSH_BRIDGE.md): the DSH bridge protocol.

## License

MIT. See [LICENSE](LICENSE).
