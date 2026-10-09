/**
 * Plexus bridge: maps DSH's in-process services onto the Plexus wire protocol.
 * Thin passthrough by design: every durable session event is forwarded (raw),
 * a few are additionally normalized; every native service stays reachable
 * through `plexus.control`. Nothing here removes a DSH capability.
 *
 * Every behavior in this file is UNVERIFIED against a live DSH process; it is
 * written against DSH source @ 5badb15 / npm 0.2.0-rc.2 and tested with a
 * fake Cordis context (test/fakectx.mjs).
 */

import { randomUUID } from 'node:crypto'
import { readFileSync } from 'node:fs'
import { dirname, join, resolve as resolvePath } from 'node:path'
import {
  BRIDGE_NAME, BRIDGE_VERSION, ClientMethod, ErrorCode, Method, Notification, PROTOCOL,
  type EventKind, type WireEvent, type WireTool,
} from './protocol.ts'
import { LineRpc, RpcError, errorMessage } from './rpc.ts'
import type {
  ApprovalOutcome, ApprovalRequest, AskQuestionAnswer, AskQuestionRequest, DshAgent, DshAgentHandle,
  DshContext, DshSession, DshSessionEvent, DshUserMessage,
} from './dsh.ts'

/** Cordis service names probed for the capability report (presence only). */
const KNOWN_SERVICES = [
  'agents', 'approval', 'userQuestions', 'commands', 'subagents', 'jobs', 'goals', 'skills', 'tools',
  'systemPrompt', 'sessionPersistence', 'sessions', 'sessionQuery', 'llm', 'attachments', 'planMode',
  'compaction', 'permissionPresets', 'mcpResources', 'schedule',
]

/** Cordis lifecycle events forwarded as `extension` (or `background`) when they concern an owned session. */
const DEFAULT_FORWARD = ['agent/status', 'agent/error', 'goal/changed', 'goal/activation-changed', 'subagent/start']

const TOOL_CACHE_MAX = 512
const APPROVED_MAX = 1024
const RESULT_TEXT_MAX = 4096

interface PromptRec {
  turnId: string
  turn?: number
  settled: boolean
  /** Turn started by an external (guest) user: every tool except plexus_post is denied. */
  guest: boolean
}

/** A Plexus host tool as declared in plexus.session.open `tools`. */
interface HostToolSpec {
  name: string
  description: string
  inputSchema: Record<string, unknown>
}

/**
 * The one tool a guest (stranger) turn may use: posting its answer. Everything
 * else -- native tools, reads included, and every other Plexus host tool -- is
 * denied natively. Authority is plexus.prompt `guest` only; there is no level.
 */
const GUEST_TOOL = 'plexus_post'

/** Model-facing denial for tools in a guest turn. */
function guestMay(rec: { hostTools: Set<string> }, name: string): boolean {
  return name === GUEST_TOOL && rec.hostTools.has(name)
}

const GUEST_DENIAL = 'Tools are disabled for this turn: it was started by an external (guest) user. Answer in text only.'

interface SessionRec {
  id: string
  handle: DshAgentHandle
  agent: DshAgent
  prompts: Map<string, PromptRec>
  turnPrompt: Map<number, PromptRec>
  activeTurn?: number
  lastText: Map<number, string>
  attempts: Map<string, number>
  children: Set<string>
  /** Subagent sessions that have started and not yet ended -> their parent session id. */
  runningChildren: Map<string, string>
  /** Live background jobs owned by this tree -> owning session id. */
  jobs: Map<string, string>
  /** Guest prompts not yet settled; non-empty = the tree is guest-locked. */
  guest: Set<string>
  hostTools: Set<string>
  cwd: string
  closing?: Promise<void>
}

interface Options {
  raw: 'all' | 'unmapped' | 'none'
  streamText: boolean
  streamReasoning: boolean
  streamFrames: boolean
}

export interface BridgeHooks {
  /** Process exit (default process.exit). */
  exit?: (code: number) => void
}

export class Bridge {
  private initialized = false
  private shuttingDown = false
  private readonly roots = new Map<string, SessionRec>()
  /** Any owned session id (root or descendant) -> its root record. */
  private readonly owned = new Map<string, SessionRec>()
  /** Tool calls by call id, to give permission requests the arguments DSH omits. */
  private readonly toolCalls = new Map<string, { name: string; input: unknown }>()
  private readonly opts: Options = { raw: 'none', streamText: true, streamReasoning: false, streamFrames: false }
  private seq = 0
  private readonly disposers: (() => void)[] = []
  private guestLock = false
  /** Call ids the owner approved through plexus.permission (one call each, never reused). */
  private readonly approved = new Set<string>()
  private jobsSubscribed = false

  constructor(private readonly ctx: DshContext, private readonly rpc: LineRpc, private readonly hooks: BridgeHooks = {}) {
    rpc.onRequest = (m, p) => this.handle(m, p ?? {})
    // A frame dropped for exceeding the 32 MiB cap (CR-9) is surfaced as a
    // session-level error event rather than silently lost.
    rpc.onFrameError = (message: string): void => { try { this.emit({ sessionId: '', turnId: '', kind: 'error', id: this.nextId(), text: message }) } catch { /* ignore */ } }
  }

  // ---------------------------------------------------------------- listeners

  listen(forward: readonly string[] = DEFAULT_FORWARD): void {
    const on = (name: string, fn: (...a: any[]) => any, prepend = false): void => {
      try { this.disposers.push(this.ctx.on(name, fn, prepend)) } catch { /* event unknown to this DSH build */ }
    }
    // Dangerous-action gate: prepended so it runs before other pre-execute
    // policies; the tools.guard below still blocks unapproved dangerous calls
    // if some earlier listener short-circuits with "allow".
    on('tools/pre-execute', (exec: any, next: () => Promise<any>) => this.onPreExecute(exec, next), true)
    on('tools/result', (exec: any) => { if (exec?.callId !== undefined) this.approved.delete(String(exec.callId)) })
    on('session/event', (session: DshSession, event: DshSessionEvent) => this.onSessionEvent(session, event))
    on('session/created', (session: DshSession) => this.onSessionCreated(session))
    on('agent/inbox/claimed', (p: { agent: DshAgent; message: DshUserMessage; turn: number }) => this.onClaimed(p))
    on('agent/assistant-stream', (p: { agent: DshAgent; frame: any }) => this.onStream(p))
    on('subagent/end', (info: any) => this.onSubagentEnd(info))
    on('approval/request', (req: ApprovalRequest, next: () => Promise<ApprovalOutcome>) => this.onApproval(req, next))
    on('user-questions/request', (req: AskQuestionRequest, next: () => Promise<AskQuestionAnswer>) => this.onQuestion(req, next))
    for (const name of new Set(forward)) on(name, (payload: any) => this.onLifecycle(name, payload))
  }

  async dispose(): Promise<void> {
    while (this.disposers.length > 0) {
      try { this.disposers.pop()?.() } catch { /* ignore */ }
    }
    await Promise.allSettled([...this.roots.values()].map(r => this.closeSession(r)))
  }

  // ---------------------------------------------------------------- requests

  private async handle(method: string, p: Record<string, unknown>): Promise<unknown> {
    if (this.shuttingDown && method !== Method.Shutdown) throw new RpcError(ErrorCode.ShuttingDown, 'bridge is shutting down')
    if (method !== Method.Initialize && method !== Method.Shutdown && !this.initialized) {
      throw new RpcError(ErrorCode.NotInitialized, 'call plexus.initialize first')
    }
    switch (method) {
      case Method.Initialize: return this.initialize(p)
      case Method.SessionOpen: return this.openSession(p)
      case Method.SessionClose: { await this.closeSession(this.root(p)); return {} }
      case Method.Prompt: return this.prompt(p)
      case Method.Steer: return this.steer(p)
      case Method.Cancel: return this.cancel(p)
      case Method.Control: return this.control(p)
      case Method.Shutdown: return this.shutdown()
      default: throw new RpcError(ErrorCode.MethodNotFound, `method not found: ${method}`)
    }
  }

  private async initialize(p: Record<string, unknown>): Promise<unknown> {
    const want = p.protocol
    if (typeof want === 'number' && want !== PROTOCOL) {
      throw new RpcError(ErrorCode.ProtocolMismatch, `bridge speaks protocol ${PROTOCOL}, client wants ${want}`, { protocol: PROTOCOL })
    }
    const stream = (p.stream ?? {}) as Record<string, unknown>
    if (typeof stream.text === 'boolean') this.opts.streamText = stream.text
    if (typeof stream.reasoning === 'boolean') this.opts.streamReasoning = stream.reasoning
    if (typeof stream.frames === 'boolean') this.opts.streamFrames = stream.frames
    if (p.raw === 'none' || p.raw === 'unmapped' || p.raw === 'all') this.opts.raw = p.raw
    if (Array.isArray(p.forward)) {
      // Extra Cordis events the client wants passed through (discovered, not hardcoded).
      this.listenExtra(p.forward.filter((n): n is string => typeof n === 'string'))
    }
    // Same readiness boundary as DSH's own SDK server: wait for the plugin tree
    // (e.g. MCP tool discovery) to settle before declaring ready.
    try { await this.ctx.get?.('loader')?.await?.() } catch { /* best effort */ }
    this.installGuestGuard()
    this.subscribeJobs()
    this.initialized = true
    return {
      protocol: PROTOCOL,
      bridge: { name: BRIDGE_NAME, version: BRIDGE_VERSION },
      dsh: { version: dshVersion(), node: process.version, platform: process.platform },
      services: this.services(),
      capabilities: this.capabilities(),
    }
  }

  /**
   * One global monotonic tool guard (ctx.tools.guard). DSH consults guards for
   * every call whose policy outcome is "allow" (after the pre-execute
   * waterfall and any approval), including read-only tools that never ask:
   *  - guest-locked tree (GuestLock): deny everything except plexus_post;
   *  - any non-read, non-meta, non-host call whose call id onPreExecute did not
   *    clear: deny. The guard makes no danger judgement of its own (the single
   *    classifier is Plexus's Go internal/danger); it only fails closed for
   *    calls that skipped the approval round trip, including calls in an
   *    unknown session, so an unknown agent cannot run side-effecting tools.
   * UNVERIFIED against live DSH (source: core/tools/src/index.ts guard()).
   */
  private installGuestGuard(): void {
    if (this.guestLock) return
    const tools = this.service('tools')
    if (typeof tools?.guard !== 'function') return
    try {
      this.disposers.push(tools.guard((exec: any): string | undefined => {
        const rec = this.owned.get(String(exec?.agent?.session?.id))
        const name = String(exec?.name)
        if (rec !== undefined && rec.guest.size > 0 && !guestMay(rec, name)) return GUEST_DENIAL
        // Single classifier (CR-4): the guard makes no danger judgement of its
        // own. Every non-read, non-meta, non-host call must have been cleared
        // by onPreExecute (its call id recorded). Anything that reached the
        // guard without that clearance - including a call in an unknown
        // session - fails closed.
        const isHost = rec !== undefined && rec.hostTools.has(name)
        if (!isHost && needsGate(name, exec?.arguments) && !this.approved.has(String(exec?.callId ?? ''))) {
          return 'Blocked: this action was not approved by Plexus. Side-effecting tools must be cleared before they run.'
        }
        return undefined
      }))
      this.guestLock = true
    } catch { /* guard API absent in this DSH build */ }
  }

  /**
   * tools/pre-execute (prepended): dangerous calls are sent to Plexus as a
   * plexus.permission request with `danger` set and wait for the owner's
   * answer; there is no timeout and no automatic approval. Allow -> the call
   * id is recorded (consumed by the guard / approval answerer) and the rest
   * of DSH's policy chain still runs (it may still deny). Deny -> deny.
   */
  private async onPreExecute(exec: any, next: () => Promise<any>): Promise<any> {
    const sid = String(exec?.agent?.session?.id)
    const rec = this.owned.get(sid)
    const name = String(exec?.name)
    if (rec !== undefined && rec.guest.size > 0 && !guestMay(rec, name)) return { kind: 'deny', reason: GUEST_DENIAL }
    if (rec === undefined) return next() // unknown session: the guard fails closed
    // Single classifier (CR-4): forward EVERY call to Plexus except reads,
    // Plexus host tools and DSH-internal meta tools. Plexus (its one Go
    // classifier) answers allow / deny; a dangerous call is denied at once with
    // a parked reason (CR-3), which we relay verbatim so the model does not
    // retry. There is no local danger judgement and no local timeout.
    if (rec.hostTools.has(name) || !needsGate(name, exec?.arguments)) return next()
    if (this.rpc.isClosed) return { kind: 'deny', reason: 'Plexus is not connected to approve this action.' }
    const callId = String(exec?.callId ?? '')
    if (callId === '') return { kind: 'deny', reason: 'This tool call has no id; Plexus cannot gate it, so it is denied (CR-13).' }
    const { outcome, reason } = await this.requestPermission(rec, sid, { call_id: callId, name, input: exec?.arguments }, '', exec?.signal)
    if (outcome === 'cancelled') return { kind: 'cancel' }
    if (outcome !== 'allowed') return { kind: 'deny', reason: reason !== '' ? reason : 'Plexus did not approve this action.' }
    if (this.approved.size >= APPROVED_MAX) this.approved.delete(this.approved.values().next().value!)
    this.approved.add(callId)
    return next()
  }

  /** Background job lifecycle (jobs.events) -> background events + task tracking. */
  private subscribeJobs(): void {
    if (this.jobsSubscribed) return
    const jobs = this.service('jobs')
    if (typeof jobs?.events?.subscribe !== 'function') return
    try {
      this.disposers.push(jobs.events.subscribe({ owners: 'all' }, (ev: any) => this.onJob(ev)))
      this.jobsSubscribed = true
    } catch { /* ignore */ }
  }

  private extraListening = new Set<string>()
  private listenExtra(names: string[]): void {
    for (const name of names) {
      if (this.extraListening.has(name) || DEFAULT_FORWARD.includes(name)) continue
      this.extraListening.add(name)
      try { this.disposers.push(this.ctx.on(name, (payload: any) => this.onLifecycle(name, payload))) } catch { /* unknown */ }
    }
  }

  private service(name: string): any {
    try { return this.ctx.get?.(name) ?? undefined } catch { return undefined }
  }

  private services(): string[] {
    return KNOWN_SERVICES.filter(n => this.service(n) !== undefined)
  }

  private capabilities(): Record<string, string> {
    const has = (n: string): 'native' | 'unsupported' => this.service(n) !== undefined ? 'native' : 'unsupported'
    return {
      prompt: 'native',
      stream_text: 'native',
      permission: has('approval'),
      question: has('userQuestions'),
      cancel: 'native',
      resume: has('sessionPersistence'),
      steer: 'native',
      inject: 'native',
      slash_commands: has('commands'),
      subagents: has('subagents'),
      background_jobs: has('jobs'),
      per_task_stop: has('subagents') === 'native' || has('jobs') === 'native' ? 'native' : 'unsupported',
      host_tools: has('tools'),
      guest_lock: this.guestLock ? 'native' : 'unsupported',
      // Owner gate for dangerous actions: needs both the prepended pre-execute
      // listener (asks) and tools.guard (blocks anything that skipped it).
      danger_gate: this.guestLock ? 'native' : 'partial',
      goals: has('goals'),
      skills: has('skills'),
      persona: has('systemPrompt') === 'native' ? 'native' : 'unsupported',
      control: 'native',
      raw_events: 'native',
    }
  }

  private async openSession(p: Record<string, unknown>): Promise<unknown> {
    const persona = typeof p.persona === 'string' ? p.persona : ''
    const hostTools = parseHostTools(p.tools)
    const agentOptions: Record<string, unknown> = {}
    if (typeof p.model === 'string' && p.model !== '') agentOptions.model = p.model
    if (typeof p.provider === 'string' && p.provider !== '') agentOptions.provider = p.provider
    if (typeof p.effort === 'string' && p.effort !== '') agentOptions.reasoningEffort = p.effort
    // CR-1: stock DSH refuses every turn without a model ("{{model}}" has no
    // value, then "agent has no provider"). Plexus omits model/effort unless
    // the bot config sets them (SessionOptions.Model/Effort), so fill the
    // gaps from DSH's own configured default (agent-default-model):
    //  - no model: the default model, and its effort unless one was given;
    //  - a model but no provider: the default provider (UNVERIFIED for a
    //    model served only by a non-default provider; set provider then).
    let sel: any
    try { sel = this.service('agentDefaultModel')?.currentSelection?.() } catch { /* not composed: let DSH decide */ }
    if (sel !== null && typeof sel === 'object') {
      if (agentOptions.model === undefined) {
        if (typeof sel.model === 'string' && sel.model !== '') agentOptions.model = sel.model
        if (agentOptions.reasoningEffort === undefined && sel.reasoningEffort !== undefined) agentOptions.reasoningEffort = sel.reasoningEffort
      }
      if (agentOptions.provider === undefined && typeof sel.provider === 'string' && sel.provider !== '') agentOptions.provider = sel.provider
    }
    const setup = (agentCtx: any): void => {
      // Agent-scoped persona section after DSH's own guidance (order 10300 >
      // deployment persona-suffix 10200). UNVERIFIED: order slot and HMR-safety.
      if (persona !== '') agentCtx?.systemPrompt?.section?.({ name: 'plexus:persona', order: 10300, text: persona, interpolate: false })
      // Host tools registered in the agent's own scope (shadow globals, like
      // DSH's MCP client does with raw JSON schemas). UNVERIFIED: visibility
      // inside subagents.
      for (const t of hostTools) agentCtx?.tools?.register?.(this.hostToolDefinition(t))
    }
    const opts = Object.keys(agentOptions).length > 0 ? { agentOptions } : {}
    let handle: DshAgentHandle
    let resumed = false
    if (typeof p.resume === 'string' && p.resume !== '') {
      const live = this.roots.get(p.resume)
      if (live !== undefined) return { sessionId: live.id, resumed: true, commands: this.commandList(live.agent) }
      if (this.service('sessionPersistence') === undefined) {
        throw new RpcError(ErrorCode.ServiceUnavailable, 'this DSH composition has no sessionPersistence; cannot resume')
      }
      try {
        handle = await this.ctx.agents.resume({ resumeSessionId: p.resume, setup, ...opts })
      } catch (error) {
        throw new RpcError(ErrorCode.ResumeFailed, `resume ${p.resume}: ${errorMessage(error)}`)
      }
      resumed = true
    } else {
      const cwd = typeof p.cwd === 'string' && p.cwd !== '' ? resolvePath(p.cwd) : process.cwd()
      handle = await this.ctx.agents.create({ sessionId: randomUUID(), meta: { cwd }, setup, ...opts })
    }
    const id = String(handle.agent.session.id)
    const rec: SessionRec = {
      id, handle, agent: handle.agent, prompts: new Map(), turnPrompt: new Map(),
      lastText: new Map(), attempts: new Map(), children: new Set(), runningChildren: new Map(),
      jobs: new Map(), guest: new Set(), hostTools: new Set(hostTools.map(t => t.name)),
      cwd: typeof p.cwd === 'string' && p.cwd !== '' ? resolvePath(p.cwd) : String((handle.agent.session as any)?.meta?.cwd ?? (handle.agent.session as any)?.header?.cwd ?? process.cwd()) // resumed: SessionHeader.cwd (UNVERIFIED),
    }
    this.roots.set(id, rec)
    this.owned.set(id, rec)
    return { sessionId: id, resumed, commands: this.commandList(rec.agent) }
  }

  private commandList(agent: DshAgent): unknown[] {
    try {
      const list = this.service('commands')?.list?.(agent)
      return Array.isArray(list) ? list.map((c: any) => ({ name: c.name, description: c.description, input: c.input })) : []
    } catch { return [] }
  }

  private root(p: Record<string, unknown>): SessionRec {
    const id = typeof p.sessionId === 'string' ? p.sessionId : ''
    const rec = this.roots.get(id)
    if (rec === undefined) throw new RpcError(ErrorCode.UnknownSession, `unknown session: ${id}`)
    if (rec.closing !== undefined) throw new RpcError(ErrorCode.UnknownSession, `session is closing: ${id}`)
    return rec
  }

  private prompt(p: Record<string, unknown>): unknown {
    const rec = this.root(p)
    const text = typeof p.text === 'string' ? p.text : ''
    // Turn authority is `guest` only (Plexus Turn.Guest); any other member,
    // e.g. a legacy `level`, is ignored and never widens what a turn may do.
    const guest = p.guest === true
    if (!this.guestLock) {
      // Fail closed: without tools.guard neither the guest lock nor the
      // dangerous-action gate can be enforced, so no turn runs at all.
      throw new RpcError(ErrorCode.SafetyGateUnavailable, 'this DSH build has no tools.guard; the dangerous-action gate cannot be enforced, so prompts are refused')
    }
    if (!guest && p.slash !== 'never' && text.startsWith('/')) {
      const cmd = this.tryCommand(rec, text)
      if (cmd !== undefined) return cmd
    }
    const msg = userMessage(text)
    rec.prompts.set(msg.id, { turnId: msg.id, settled: false, guest })
    if (guest) rec.guest.add(msg.id)
    rec.agent.followup(msg)
    return { turnId: msg.id, messageId: msg.id }
  }

  /** Native slash command passthrough: only when DSH itself resolves the name. */
  private tryCommand(rec: SessionRec, line: string): unknown {
    const commands = this.service('commands')
    const name = /^\/(\S+)/.exec(line)?.[1]?.toLowerCase()
    if (commands?.execute === undefined || name === undefined) return undefined
    try { if (commands.find?.(rec.agent, name) === undefined) return undefined } catch { return undefined }
    const turnId = `cmd-${randomUUID()}`
    const pr: PromptRec = { turnId, settled: false, guest: false }
    rec.prompts.set(turnId, pr)
    setImmediate(() => {
      void (async () => {
        try {
          const ac = new AbortController()
          const exec = await commands.execute(rec.agent, line, [], ac.signal)
          if (exec === undefined) {
            this.settle(rec, pr, 'error', 'unknown_command', `DSH did not resolve ${line}`, undefined)
            return
          }
          const kind = exec.result?.kind === 'error' ? 'error' : 'final'
          this.settle(rec, pr, kind, String(exec.result?.kind ?? 'success'), String(exec.result?.text ?? ''), exec)
        } catch (error) {
          this.settle(rec, pr, 'error', 'error', errorMessage(error), undefined)
        }
      })()
    })
    return { turnId, command: name }
  }

  private steer(p: Record<string, unknown>): unknown {
    const rec = this.root(p)
    const msg = userMessage(typeof p.text === 'string' ? p.text : '')
    rec.agent.steer(msg)
    return { messageId: msg.id }
  }

  private cancel(p: Record<string, unknown>): unknown {
    const rec = this.root(p)
    rec.agent.cancel({ kind: 'user' }, p.keepInbox === true ? { keepInbox: true } : undefined)
    this.sweepAfterIdle(rec, 'aborted', 'cancelled before the turn started')
    return { cancelled: true }
  }

  /**
   * Settle prompts that will never get a turn (cancelled out of the inbox, or
   * an agent failure outside a turn) once the agent is quiescent.
   */
  private sweepAfterIdle(rec: SessionRec, status: string, text: string): void {
    void rec.agent.whenIdle().then(() => {
      for (const pr of rec.prompts.values()) {
        if (!pr.settled && pr.turn === undefined && !pr.turnId.startsWith('cmd-')) this.settle(rec, pr, 'error', status, text, undefined)
      }
    }, () => { /* disposed */ })
  }

  private async control(p: Record<string, unknown>): Promise<unknown> {
    const rec = this.root(p)
    const name = typeof p.name === 'string' ? p.name : ''
    const payload = (p.payload !== null && typeof p.payload === 'object' ? p.payload : {}) as Record<string, any>
    const text = typeof payload.text === 'string' ? payload.text : ''
    try {
      switch (name) {
        case 'steer': { const m = userMessage(text); rec.agent.steer(m); return { messageId: m.id } }
        case 'inject': { const m = userMessage(text); rec.agent.inject(m); return { messageId: m.id } }
        case 'followup': { const m = userMessage(text); rec.agent.followup(m); return { messageId: m.id } }
        case 'capabilities': return { services: this.services(), capabilities: this.capabilities(), commands: this.commandList(rec.agent) }
        case 'commands.list': return this.commandList(rec.agent)
        case 'command': {
          const ex = await this.need('commands').execute(rec.agent, String(payload.line ?? ''), [], new AbortController().signal)
          return ex === undefined ? null : ex
        }
        case 'subagent.interrupt':
          this.need('subagents').interrupt(String(payload.sessionId ?? ''), { kind: 'user', parentSessionId: rec.id })
          return {}
        case 'tasks.list': return this.tasks(rec)
        case 'task.stop': return this.stopTask(rec, String(payload.id ?? ''))
        case 'approval.setPolicy':
          this.need('approval').setPolicy(rec.agent, String(payload.policy ?? ''))
          return {}
        case 'call': return await this.rawCall(rec, payload)
        default:
          throw new RpcError(ErrorCode.MethodNotFound, `unknown control ${JSON.stringify(name)}; use "call" for raw service access`)
      }
    } catch (error) {
      if (error instanceof RpcError) throw error
      throw new RpcError(ErrorCode.ControlFailed, `${name}: ${errorMessage(error)}`)
    }
  }

  /** Running subagents and background jobs of this tree (Plexus TaskStopper). */
  private tasks(rec: SessionRec): unknown[] {
    const out: unknown[] = []
    for (const [id, parent] of rec.runningChildren) out.push({ id, kind: 'subagent', parentSessionId: parent })
    for (const [id, owner] of rec.jobs) out.push({ id, kind: 'job', ownerSessionId: owner })
    return out
  }

  private stopTask(rec: SessionRec, id: string): unknown {
    const parent = rec.runningChildren.get(id)
    if (parent !== undefined) {
      this.need('subagents').interrupt(id, { kind: 'user', parentSessionId: parent })
      return { stopped: 'subagent' }
    }
    const owner = rec.jobs.get(id)
    if (owner !== undefined) {
      const r = this.need('jobs').kill(id, owner, 'stopped by Plexus')
      return { stopped: 'job', result: r }
    }
    throw new RpcError(ErrorCode.ControlFailed, `no running task ${JSON.stringify(id)} in this session`)
  }

  private need(name: string): any {
    const s = this.service(name)
    if (s === undefined) throw new RpcError(ErrorCode.ServiceUnavailable, `DSH service ${name} is not composed`)
    return s
  }

  /**
   * Raw escape hatch: `ctx.<service>.<method>(...args)` with placeholders
   * {"$agent":true}, {"$session":true}, {"$signal":true}. Owner-only on the
   * Plexus side. The result must be JSON-serializable (degraded otherwise).
   */
  private async rawCall(rec: SessionRec, payload: Record<string, any>): Promise<unknown> {
    const svcName = String(payload.service ?? '')
    const method = String(payload.method ?? '')
    const svc = this.need(svcName)
    const fn = svc?.[method]
    if (typeof fn !== 'function') throw new RpcError(ErrorCode.MethodNotFound, `${svcName}.${method} is not a function`)
    const args = Array.isArray(payload.args) ? payload.args.map((a: any) => {
      if (a !== null && typeof a === 'object') {
        if (a.$agent === true) return rec.agent
        if (a.$session === true) return rec.agent.session
        if (a.$signal === true) return new AbortController().signal
      }
      return a
    }) : []
    const out = await fn.apply(svc, args)
    return out === undefined ? null : out
  }

  private async closeSession(rec: SessionRec): Promise<void> {
    rec.closing ??= (async () => {
      try { rec.agent.cancel({ kind: 'user' }) } catch { /* ignore */ }
      try { await rec.agent.whenIdle() } catch { /* ignore */ }
      try { await this.service('sessions')?.flush?.(rec.agent.session) } catch { /* ignore */ }
      try { await rec.handle.dispose() } catch { /* ignore */ }
      this.roots.delete(rec.id)
      for (const [sid, r] of this.owned) if (r === rec) this.owned.delete(sid)
    })()
    return rec.closing
  }

  private shutdown(): unknown {
    if (!this.shuttingDown) {
      this.shuttingDown = true
      setImmediate(() => {
        void (async () => {
          await this.dispose()
          try { await this.ctx.root?.fiber?.dispose() } catch { /* ignore */ }
          ;(this.hooks.exit ?? ((c: number) => process.exit(c)))(0)
        })()
      })
    }
    return {}
  }

  // ---------------------------------------------------------------- events

  private emit(ev: WireEvent): void {
    this.rpc.notify(Notification.Event, ev)
  }

  private promptTurnId(rec: SessionRec, turn: number | undefined): string {
    if (turn === undefined) return ''
    return rec.turnPrompt.get(turn)?.turnId ?? ''
  }

  private onSessionCreated(session: DshSession): void {
    const parent = session.header?.parentSession
    if (parent === undefined) return
    const rec = this.owned.get(String(parent))
    if (rec === undefined) return
    const child = String(session.id ?? session.header?.id)
    this.owned.set(child, rec)
    rec.children.add(child)
    rec.runningChildren.set(child, String(parent))
    this.emit({
      sessionId: rec.id, turnId: this.promptTurnId(rec, rec.activeTurn), kind: 'background', id: `sub:${child}:start`,
      parent_id: String(parent) === rec.id ? '' : String(parent), name: 'subagent', status: 'started', text: child,
      raw: { parentSessionId: String(parent), childSessionId: child },
    })
  }

  private onSubagentEnd(info: any): void {
    const child = String(info?.id ?? '')
    const rec = this.owned.get(child)
    if (rec === undefined || child === rec.id) return
    rec.runningChildren.delete(child)
    this.emit({
      sessionId: rec.id, turnId: this.promptTurnId(rec, rec.activeTurn), kind: 'background', id: `sub:${child}:end`,
      parent_id: child, name: 'subagent', status: `finished:${String(info?.stopReason ?? 'unknown')}`,
      text: textOf(info?.lastAssistantMessage), raw: info,
    })
  }

  private onClaimed(p: { agent: DshAgent; message: DshUserMessage; turn: number }): void {
    const rec = this.roots.get(String(p?.agent?.session?.id))
    if (rec === undefined || rec.agent !== p.agent) return
    const pr = rec.prompts.get(String(p.message?.id))
    if (pr === undefined || pr.turn !== undefined) return
    pr.turn = p.turn
    rec.turnPrompt.set(p.turn, pr)
  }

  private onStream(p: { agent: DshAgent; frame: any }): void {
    const sid = String(p?.agent?.session?.id)
    const rec = this.owned.get(sid)
    if (rec === undefined) return
    const f = p.frame
    const child = sid !== rec.id
    if (f?.type === 'start' && !child) rec.attempts.set(String(f.attemptId), Number(f.turn))
    const turn = child ? rec.activeTurn : rec.attempts.get(String(f?.attemptId))
    const turnId = this.promptTurnId(rec, turn)
    if (this.opts.streamFrames) {
      this.emit({ sessionId: rec.id, turnId, kind: 'extension', id: this.nextId(), parent_id: child ? sid : '', name: 'agent/assistant-stream', raw: f })
    } else if (f?.type === 'chunk' && !child) {
      const c = f.chunk
      if (c?.type === 'text-delta' && this.opts.streamText) {
        this.emit({ sessionId: rec.id, turnId, kind: 'text_delta', id: `${sid}:a:${f.attemptId}:${f.index}`, text: String(c.text ?? '') })
      } else if (c?.type === 'reasoning-delta' && this.opts.streamReasoning) {
        this.emit({ sessionId: rec.id, turnId, kind: 'extension', id: `${sid}:a:${f.attemptId}:${f.index}`, name: 'reasoning-delta', text: String(c.text ?? '') })
      }
    }
    if (f?.type === 'end' && !child) rec.attempts.delete(String(f.attemptId))
  }

  private onLifecycle(name: string, payload: any): void {
    const sid = payload?.agent?.session?.id ?? payload?.session?.id ?? payload?.sessionId
    const rec = sid === undefined ? undefined : this.owned.get(String(sid))
    if (rec === undefined) return
    const child = String(sid) !== rec.id
    let text: string | undefined
    let status: string | undefined
    if (name === 'agent/status') status = String(payload.status)
    if (name === 'agent/error') {
      text = errorMessage(payload.error)
      // An in-turn failure is balanced by turn/end; a failure outside any turn
      // may orphan queued prompts, so sweep after quiescence.
      if (!child) this.sweepAfterIdle(rec, 'error', text)
    }
    this.emit({
      sessionId: rec.id, turnId: this.promptTurnId(rec, child ? rec.activeTurn : payload?.turn ?? rec.activeTurn),
      kind: 'extension', id: this.nextId(), parent_id: child ? String(sid) : '', name, status, text, raw: stripLive(payload),
    })
  }

  private onSessionEvent(session: DshSession, event: DshSessionEvent): void {
    const sid = String(session?.id ?? session?.header?.id)
    const rec = this.owned.get(sid)
    if (rec === undefined || event === undefined) return
    const child = sid !== rec.id
    const turn: number | undefined = typeof event.data?.turn === 'number' ? event.data.turn : undefined
    if (!child) {
      if (event.type === 'turn/start' && turn !== undefined) rec.activeTurn = turn
    }
    const turnId = this.promptTurnId(rec, child ? rec.activeTurn : turn)
    const base = { sessionId: rec.id, turnId, id: `${sid}:${event.seq}`, parent_id: child ? sid : '' }
    const raw = this.opts.raw === 'all' ? event : undefined
    switch (event.type) {
      case 'assistant/message': {
        const text = textOf(event.data?.message?.content)
        if (!child && turn !== undefined && text !== '') rec.lastText.set(turn, text)
        if (text !== '') { this.emit({ ...base, kind: 'message', text, status: event.data?.interrupted === true ? 'interrupted' : undefined, raw }); return }
        break
      }
      case 'tool/call': {
        const input = parseArgs(event.data?.arguments)
        const tool: WireTool = { call_id: String(event.data?.callId ?? ''), name: String(event.data?.name ?? ''), input }
        this.cacheTool(tool.call_id ?? '', tool.name, input)
        this.emit({ ...base, kind: 'tool_use', tool, status: 'started', raw })
        return
      }
      case 'tool/result': {
        const msg = event.data?.message
        const callId = String(msg?.toolCallId ?? '')
        const cached = this.toolCalls.get(callId)
        this.toolCalls.delete(callId)
        const text = textOf(msg?.content)
        this.emit({
          ...base, kind: 'tool_result', tool: { call_id: callId, name: cached?.name ?? '' },
          status: msg?.isError === true ? 'error' : 'ok',
          text: text.length > RESULT_TEXT_MAX ? text.slice(0, RESULT_TEXT_MAX) : text, raw,
        })
        return
      }
      case 'turn/end': {
        if (child) {
          this.emit({ ...base, kind: 'background', name: 'subagent-turn', status: String(event.data?.reason?.kind ?? ''), raw })
          return
        }
        this.onTurnEnd(rec, event, turn)
        return
      }
    }
    this.emit({ ...base, kind: 'extension', name: event.type, raw: event })
  }

  private onTurnEnd(rec: SessionRec, event: DshSessionEvent, turn: number | undefined): void {
    const reason = event.data?.reason ?? {}
    const kind = String(reason.kind ?? 'unknown')
    const text = turn === undefined ? '' : rec.lastText.get(turn) ?? ''
    if (turn !== undefined) rec.lastText.delete(turn)
    if (rec.activeTurn === turn) rec.activeTurn = undefined
    const pr = turn === undefined ? undefined : rec.turnPrompt.get(turn)
    const raw = this.opts.raw === 'all' ? event : undefined
    if (pr === undefined) {
      // Autonomous turn (goal round, job wake-up, steer on idle agent...): not
      // a Plexus Send, so it is reported as background, never as final.
      this.emit({ sessionId: rec.id, turnId: '', kind: 'background', id: `${rec.id}:${event.seq}`, name: 'turn', status: kind, text, raw })
      return
    }
    if (turn !== undefined) rec.turnPrompt.delete(turn)
    if (kind === 'completed' || kind === 'max-tokens') {
      this.settle(rec, pr, 'final', kind, text, raw, `${rec.id}:${event.seq}`)
    } else {
      const detail = kind === 'error' ? String(reason.error?.message ?? 'turn failed')
        : kind === 'aborted' ? 'cancelled' : kind
      this.settle(rec, pr, 'error', kind, detail, raw, `${rec.id}:${event.seq}`)
    }
  }

  private settle(rec: SessionRec, pr: PromptRec, kind: EventKind, status: string, text: string, raw: unknown, id?: string): void {
    if (pr.settled) return
    pr.settled = true
    rec.prompts.delete(pr.turnId)
    rec.guest.delete(pr.turnId)
    this.emit({ sessionId: rec.id, turnId: pr.turnId, kind, id: id ?? this.nextId(), status, text, raw })
  }

  private cacheTool(callId: string, name: string, input: unknown): void {
    if (callId === '') return
    if (this.toolCalls.size >= TOOL_CACHE_MAX) {
      const oldest = this.toolCalls.keys().next().value
      if (oldest !== undefined) this.toolCalls.delete(oldest)
    }
    this.toolCalls.set(callId, { name, input })
  }

  private nextId(): string { return `pb-${++this.seq}` }

  // ---------------------------------------------------------------- answerers

  private onApproval(req: ApprovalRequest, next: () => Promise<ApprovalOutcome>): Promise<ApprovalOutcome> {
    const sid = String(req?.agent?.session?.id)
    const rec = this.owned.get(sid)
    if (rec === undefined || this.rpc.isClosed) return next()
    // Guest turn: reject without asking Plexus (the tool guard denies the rest).
    if (rec.guest.size > 0) return Promise.resolve('rejected')
    const callId = req.callId === undefined ? '' : String(req.callId)
    // The owner already approved this exact call in onPreExecute: reuse that
    // single decision instead of asking twice. Nothing else is auto-approved.
    if (callId !== '' && this.approved.has(callId)) return Promise.resolve('allowed-once')
    const cached = callId === '' ? undefined : this.toolCalls.get(callId)
    return this.requestPermission(rec, sid, { call_id: callId, name: req.toolName, input: cached?.input },
      req.displayReason?.en ?? req.reason ?? '', req.signal).then(r =>
      r.outcome === 'allowed' ? 'allowed-once' : r.outcome === 'cancelled' ? 'cancelled' : r.outcome === 'unavailable' ? 'unavailable' : 'rejected')
  }

  /**
   * One plexus.permission round trip. Plexus answers quickly (allow, or deny
   * with a reason - a dangerous call is denied at once and parked on Plexus'
   * side, CR-3); there is no local timeout and no default allow. The reason is
   * relayed to the model verbatim so it does not retry a parked call.
   */
  private requestPermission(rec: SessionRec, sid: string, tool: WireTool, reason: string, signal: AbortSignal | undefined): Promise<{ outcome: 'allowed' | 'denied' | 'cancelled' | 'unavailable'; reason: string }> {
    const id = `perm-${++this.seq}`
    const turnId = this.promptTurnId(rec, rec.activeTurn)
    const params = {
      sessionId: rec.id, turnId, id, parentId: sid === rec.id ? '' : sid, tool, reason,
      options: [
        { id: 'allow-once', label: 'Allow once', kind: 'allow_once' },
        { id: 'reject-once', label: 'Reject', kind: 'reject_once' },
      ],
    }
    const { promise } = this.rpc.request(ClientMethod.Permission, params, signal)
    return promise.then((res: any) => {
      // DSH grants are one-shot only; "always" cannot be honored natively.
      const allowed = res?.allow === true && res?.optionId !== 'reject-once'
      return { outcome: (allowed ? 'allowed' : 'denied') as 'allowed' | 'denied', reason: typeof res?.reason === 'string' ? res.reason : '' }
    }, (error: unknown) => {
      if (signal?.aborted) {
        this.rpc.notify(Notification.RequestCancelled, { sessionId: rec.id, id })
        return { outcome: 'cancelled' as const, reason: '' }
      }
      const denied = error instanceof RpcError && error.code !== ErrorCode.ShuttingDown
      return { outcome: (denied ? 'denied' : 'unavailable') as 'denied' | 'unavailable', reason: '' }
    })
  }

  private onJob(ev: any): void {
    const job = ev?.job
    if (job === undefined || (ev.type !== 'registered' && ev.type !== 'stopping' && ev.type !== 'settled' && ev.type !== 'removed')) return
    const owner = job.owner === undefined ? undefined : String(job.owner)
    const rec = owner === undefined ? undefined : this.owned.get(owner)
    if (rec === undefined || owner === undefined) return
    const id = String(job.id)
    if (ev.type === 'registered') rec.jobs.set(id, owner)
    else if (ev.type === 'settled' || ev.type === 'removed') {
      if (!rec.jobs.delete(id)) return // removal after settlement: already reported
    }
    const status = ev.type === 'registered' ? 'started' : ev.type === 'stopping' ? 'stopping'
      : ev.type === 'settled' ? `finished:${String(job.status)}` : 'removed'
    this.emit({
      sessionId: rec.id, turnId: this.promptTurnId(rec, rec.activeTurn), kind: 'background', id: `job:${id}:${ev.type}`,
      parent_id: owner === rec.id ? '' : owner, name: 'job', status, text: id, raw: ev,
    })
  }

  /** A DSH ToolDefinition whose body is answered by Plexus over plexus.tool. */
  private hostToolDefinition(t: HostToolSpec): Record<string, unknown> {
    return {
      name: t.name,
      description: t.description,
      parameters: t.inputSchema,
      output: { schema: { type: 'string' }, render: (_args: unknown, value: unknown) => [{ type: 'text', text: String(value) }] },
      execute: async (args: unknown, exec: any): Promise<string> => {
        const sid = String(exec?.agent?.session?.id)
        const rec = this.owned.get(sid)
        if (rec === undefined || this.rpc.isClosed) throw new Error('Plexus is not connected')
        const id = `tool-${++this.seq}`
        // plexus.tool uses the top-level {name, arguments} shape from
        // docs/DSH_BRIDGE.md (the Go side reads them top-level).
        const params = {
          sessionId: rec.id, turnId: this.promptTurnId(rec, rec.activeTurn), id, parentId: sid === rec.id ? '' : sid,
          guest: rec.guest.size > 0, callId: String(exec?.callId ?? ''), name: t.name, arguments: args,
        }
        let res: any
        try {
          res = await this.rpc.request(ClientMethod.Tool, params, exec?.signal).promise
        } catch (error) {
          if (exec?.signal?.aborted) this.rpc.notify(Notification.RequestCancelled, { sessionId: rec.id, id })
          throw new Error(`Plexus tool ${t.name} failed: ${errorMessage(error)}`)
        }
        const text = typeof res?.text === 'string' ? res.text : ''
        if (res?.isError === true || res?.is_error === true) throw new Error(text === '' ? `${t.name} failed` : text)
        return text
      },
    }
  }

  private onQuestion(req: AskQuestionRequest, next: () => Promise<AskQuestionAnswer>): Promise<AskQuestionAnswer> {
    const sid = req?.agent === undefined ? '' : String(req.agent.session?.id)
    const rec = this.owned.get(sid)
    if (rec === undefined || this.rpc.isClosed) return next()
    const id = `q-${++this.seq}`
    const params = {
      sessionId: rec.id, turnId: this.promptTurnId(rec, rec.activeTurn), id, parentId: sid === rec.id ? '' : sid,
      questions: req.questions.map(q => ({
        id: q.id, header: q.header, text: q.question, detail: q.detail,
        options: q.options?.map(o => ({ label: o.label, description: o.description })),
        multi: q.multiSelect === true, free_text: true,
      })),
      raw: { questions: req.questions, wait: req.wait },
    }
    const { promise } = this.rpc.request(ClientMethod.Question, params, req.signal)
    return promise.then((res: any) => mapAnswers(req, res?.answers), (error: unknown) => {
      if (req.signal?.aborted) this.rpc.notify(Notification.RequestCancelled, { sessionId: rec.id, id })
      throw error
    })
  }
}

// ---------------------------------------------------------------- helpers

/** A structurally valid DSH user message (createUserMessage's shape: fresh UUID, deep-frozen). */
export function userMessage(text: string): DshUserMessage {
  const content = Object.freeze([Object.freeze({ type: 'text', text })])
  return Object.freeze({ id: randomUUID(), role: 'user' as const, content, source: Object.freeze({ kind: 'user' }) })
}

export function textOf(content: unknown): string {
  if (!Array.isArray(content)) {
    if (content !== null && typeof content === 'object' && Array.isArray((content as any).content)) return textOf((content as any).content)
    return ''
  }
  let out = ''
  for (const b of content) if (b?.type === 'text' && typeof b.text === 'string') out += b.text
  return out
}

function parseHostTools(v: unknown): HostToolSpec[] {
  if (!Array.isArray(v)) return []
  const out: HostToolSpec[] = []
  const seen = new Set<string>()
  for (const t of v) {
    if (t === null || typeof t !== 'object' || typeof t.name !== 'string' || t.name === '' || seen.has(t.name)) continue
    seen.add(t.name)
    const schema = t.inputSchema !== null && typeof t.inputSchema === 'object' ? t.inputSchema : { type: 'object', properties: {} }
    out.push({ name: t.name, description: typeof t.description === 'string' ? t.description : '', inputSchema: schema })
  }
  return out
}

// DSH-internal meta tools: no side effect Plexus gates (messaging between
// agents, planning, todo bookkeeping, sub-agent / job / goal control, skills,
// asking the user). Mirrors internal/adapters/dsh/tools.go internalKind.
const META_TOOLS = new Set([
  'send_message', 'interrupt_agent', 'list_agents', 'workflow', 'exit_plan_mode', 'skill', 'todo_write', 'ask_user_question',
])

function isMetaTool(name: string): boolean {
  return META_TOOLS.has(name) || name.startsWith('subagent') || name.startsWith('job_') || name.endsWith('_goal')
}

/**
 * The coarse kind of a tool call, derived from its name and input the same way
 * the Go side's harness.Normalize does, so both sides agree on what a "read"
 * is. Used only to decide what to forward for approval.
 */
function toolKind(name: string, input: unknown): 'read' | 'write' | 'shell' | 'fetch' | 'meta' | 'other' {
  if (isMetaTool(name)) return 'meta'
  const n = name.toLowerCase()
  const inObj = input !== null && typeof input === 'object' ? input as Record<string, unknown> : undefined
  const hasCmd = inObj !== undefined && ['command', 'cmd', 'commandLine', 'script'].some(k => typeof inObj[k] === 'string' && inObj[k] !== '')
  if (hasCmd || /bash|shell|pwsh|powershell|exec|terminal|run_command/.test(n)) return 'shell'
  if (/write|edit|patch|delete|remove|move|rename|create|mkdir/.test(n)) return 'write'
  if (/fetch|web|http|browse/.test(n)) return 'fetch'
  if (/read|grep|glob|list|search|view|ls/.test(n) && !n.startsWith('mcp')) return 'read'
  return 'other'
}

/**
 * Whether a tool call must be forwarded to Plexus for a gate decision (CR-4).
 * Reads and DSH-internal meta tools are never gated; everything with a possible
 * side effect (write / shell / fetch / unknown) is. Host tools are handled by
 * the caller (they are answered by Plexus directly, not gated).
 */
export function needsGate(name: string, input: unknown): boolean {
  const k = toolKind(name, input)
  return k !== 'read' && k !== 'meta'
}

function parseArgs(args: unknown): unknown {
  if (typeof args !== 'string') return args
  try { return JSON.parse(args) } catch { return args }
}

/** Plexus answers: { qid: [labels or free text] } or null (declined). */
export function mapAnswers(req: AskQuestionRequest, answers: unknown): AskQuestionAnswer {
  const map = (answers !== null && typeof answers === 'object' ? answers : {}) as Record<string, unknown>
  return {
    answers: req.questions.map(q => {
      const given = Array.isArray(map[q.id]) ? (map[q.id] as unknown[]).map(String) : []
      const labels = new Set((q.options ?? []).map(o => o.label))
      const selected = given.filter(g => labels.has(g))
      const free = given.filter(g => !labels.has(g))
      const item: { id: string; selected: string[]; custom?: string } = { id: q.id, selected }
      if (free.length > 0) {
        item.custom = free.join('\n')
        if (q.multiSelect !== true) item.selected = [] // single-select: custom overrides the choice
      } else if (q.multiSelect !== true && selected.length > 1) {
        item.selected = selected.slice(0, 1)
      }
      return item
    }),
  }
}

/** Drop live objects (Agent, Session, Context) from lifecycle payloads before serializing. */
function stripLive(payload: any): unknown {
  if (payload === null || typeof payload !== 'object') return payload
  const out: Record<string, unknown> = {}
  for (const [k, v] of Object.entries(payload)) {
    if (k === 'agent') { out.agentId = (v as any)?.id; out.sessionId = (v as any)?.session?.id; continue }
    if (k === 'session') { out.sessionId = (v as any)?.id; continue }
    if (k === 'signal') continue
    if (k === 'error') { out.error = errorMessage(v); continue }
    out[k] = v
  }
  return out
}

/** DSH version from the launcher package (process.argv[1] = .../@deepseek-ai/dsh/lib/bin.js). */
function dshVersion(): string {
  try {
    const bin = process.argv[1]
    if (bin === undefined) return ''
    const pkg = JSON.parse(readFileSync(join(dirname(bin), '..', 'package.json'), 'utf8'))
    return typeof pkg.version === 'string' ? pkg.version : ''
  } catch { return '' }
}
