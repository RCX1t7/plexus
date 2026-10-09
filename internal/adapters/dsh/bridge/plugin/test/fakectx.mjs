// Fake DSH Cordis context for bridge tests. Models only the public surface the
// bridge uses (agents.create/resume, followup/steer/inject/cancel/whenIdle,
// session/event, agent/inbox/claimed, agent/assistant-stream, approval/request
// and user-questions/request waterfalls, session/created, subagent/end, the
// commands service). Event shapes follow DSH source @ 5badb15. It is a test
// double, not DSH: behaviors here are what the bridge EXPECTS, UNVERIFIED live.
//
// Scripted "model": the user text selects the behavior.
//   anything        -> streams "echo: <text>" and completes
//   perm <command>  -> bash tool call gated by approval/request
//   ask             -> ask_user_question via user-questions/request
//   slow            -> runs until cancelled
//   unknown         -> emits an unknown session event type
//   subagent        -> spawns a child session with its own events
//   fail            -> turn ends with an error
//   try <tool>      -> calls a built-in tool (through tools.guard guards)
//   tool <name> <json> -> calls a host tool registered via the agent ctx
//   job             -> registers a background job that runs until killed
//   spawn           -> starts a subagent that runs until interrupted
//   run <command>   -> bash tool call through tools/pre-execute + guards

import { randomUUID } from 'node:crypto'

export function createFakeCtx(opts = {}) {
  const listeners = new Map()
  const persisted = new Set(opts.persisted ?? [])
  const agents = new Map()
  const personas = new Map()
  const log = []
  const guards = []
  const jobListeners = []
  const liveJobs = new Map()
  const liveChildren = new Map()
  let jobSeq = 0
  const ctx = {
    guards, liveJobs, liveChildren,
    jobEmit(ev) { for (const fn of [...jobListeners]) fn(ev) },
    startJob(owner) {
      const id = `shell-${++jobSeq}`
      const job = { id, kind: 'shell', label: 'sleep 1000', owner, status: 'running', startedAt: Date.now() }
      liveJobs.set(id, job)
      ctx.jobEmit({ type: 'registered', job })
      return id
    },
    log, personas, agents,
    on(name, fn, prepend) {
      if (!listeners.has(name)) listeners.set(name, [])
      if (prepend === true || prepend?.prepend === true) listeners.get(name).unshift(fn)
      else listeners.get(name).push(fn)
      return () => { const l = listeners.get(name); const i = l.indexOf(fn); if (i >= 0) l.splice(i, 1) }
    },
    emit(name, ...args) { for (const fn of [...(listeners.get(name) ?? [])]) fn(...args) },
    waterfall(name, req, terminal) {
      const list = [...(listeners.get(name) ?? [])]
      const run = (i) => i < list.length ? Promise.resolve(list[i](req, () => run(i + 1))) : Promise.resolve(terminal())
      return run(0)
    },
    effect(fn) { const d = fn(); ctx._dispose = d },
    get(name) { return services[name] },
    agents: {
      async create({ sessionId, meta, setup, agentOptions }) {
        log.push(['create', agentOptions ?? null])
        const agent = new FakeAgent(ctx, sessionId, meta?.cwd, undefined)
        await setup?.(agent.ctx, agent)
        agents.set(sessionId, agent); persisted.add(sessionId)
        ctx.emit('session/created', agent.session)
        return { agent, dispose: async () => { agents.delete(sessionId) } }
      },
      async resume({ resumeSessionId, setup }) {
        if (!persisted.has(resumeSessionId)) throw new Error(`session not found: ${resumeSessionId}`)
        const agent = new FakeAgent(ctx, resumeSessionId, undefined, undefined)
        agent.resumed = true
        await setup?.(agent.ctx, agent)
        agents.set(resumeSessionId, agent)
        return { agent, dispose: async () => { agents.delete(resumeSessionId) } }
      },
    },
    root: { fiber: { dispose: async () => {} } },
  }
  const services = {
    agents: ctx.agents,
    approval: { setPolicy: (agent, p) => log.push(['setPolicy', p]) },
    userQuestions: {},
    sessionPersistence: {},
    sessions: { flush: async () => {} },
    subagents: {
      interrupt: (id, auth) => {
        log.push(['interrupt', id, auth.kind, auth.parentSessionId])
        if (liveChildren.delete(id)) setImmediate(() => ctx.emit('subagent/end', { id, local: true, stopReason: 'interrupted', lastAssistantMessage: [] }))
      },
    },
    tools: { guard: (fn) => { guards.push(fn); return () => guards.splice(guards.indexOf(fn), 1) } },
    jobs: {
      events: { subscribe: (_filter, fn) => { jobListeners.push(fn); return () => jobListeners.splice(jobListeners.indexOf(fn), 1) } },
      kill: (id, caller, reason) => {
        const job = liveJobs.get(id)
        if (job === undefined || job.owner !== caller) throw new Error(`unknown or foreign job ${id}`)
        log.push(['kill', id, reason])
        liveJobs.delete(id)
        setImmediate(() => {
          ctx.jobEmit({ type: 'stopping', job: { ...job, status: 'stopping' } })
          ctx.jobEmit({ type: 'settled', job: { ...job, status: 'killed', detail: reason }, cause: 'kill', awaited: false })
        })
        return 'requested'
      },
    },
    systemPrompt: {},
    commands: {
      list: () => [{ name: 'compact', description: 'Compact the conversation' }],
      find: (agent, name) => name === 'compact' ? { name } : undefined,
      execute: async (agent, line) => line.startsWith('/compact') ? { commandId: 'c1', result: { kind: 'success', text: 'compacted' } } : undefined,
    },
    echo: { say: (agent, x) => ({ agentId: agent.id, x }) },
  }
  if (opts.noGuard) delete services.tools
  // DSH's configured default model (agent-default-model), when composed.
  if (opts.defaultModel !== undefined) services.agentDefaultModel = { currentSelection: () => opts.defaultModel }
  return ctx
}

class FakeAgent {
  constructor(root, sessionId, cwd, parentSession) {
    this.root = root
    this.id = sessionId
    this.cwd = cwd
    this.session = { id: sessionId, header: { id: sessionId, ...(parentSession ? { parentSession } : {}) } }
    this.status = 'idle'
    this.seq = 0
    this.turn = 0
    this.inbox = []
    this.steered = []
    this.idleWaiters = []
    this.current = undefined
    this.tools = new Map()
    this.ctx = {
      systemPrompt: { section: (s) => root.personas.set(sessionId, s.text) },
      tools: { register: (def) => { this.tools.set(def.name, def); return () => this.tools.delete(def.name) } },
    }
  }
  ev(type, data) { this.root.emit('session/event', this.session, { type, seq: this.seq++, time: Date.now(), data }) }
  followup(msg) { this.inbox.push(msg); this.wake() }
  steer(msg) { if (this.current) this.steered.push(msg); else this.followup(msg) }
  inject(msg) { this.ev('user/message', msg) }
  cancel(_cause, opts) {
    if (!opts?.keepInbox) this.inbox.length = 0
    this.current?.ac.abort(new Error('cancelled'))
  }
  whenIdle() {
    if (this.status === 'idle' && this.inbox.length === 0) return Promise.resolve()
    return new Promise(r => this.idleWaiters.push(r))
  }
  wake() {
    if (this.status === 'running') return
    this.status = 'running'
    this.root.emit('agent/status', { agent: this, status: 'running' })
    setImmediate(() => this.drain())
  }
  async drain() {
    while (this.inbox.length > 0) {
      const msg = this.inbox.shift()
      const turn = ++this.turn
      const ac = new AbortController()
      this.current = { ac, turn }
      this.ev('turn/start', { turn })
      this.root.emit('agent/inbox/claimed', { agent: this, message: msg, turn })
      this.ev('user/message', msg)
      let reason
      try { reason = await this.run(msg.content[0].text, turn, ac.signal) } catch (e) {
        reason = ac.signal.aborted ? { kind: 'aborted', reason: { kind: 'user' } } : { kind: 'error', error: { message: String(e.message ?? e), code: 'UNKNOWN' } }
      }
      this.ev('turn/end', { turn, reason })
      this.current = undefined
    }
    this.status = 'idle'
    this.root.emit('agent/status', { agent: this, status: 'idle' })
    for (const w of this.idleWaiters.splice(0)) w()
  }
  say(turn, text, step = 0) {
    const attemptId = randomUUID()
    this.root.emit('agent/assistant-stream', { agent: this, frame: { type: 'start', attemptId, revision: 1, turn, step } })
    const parts = text.match(/.{1,6}/g) ?? ['']
    parts.forEach((t, index) => this.root.emit('agent/assistant-stream', { agent: this, frame: { type: 'chunk', attemptId, revision: 1, index, time: Date.now(), chunk: { type: 'text-delta', index: 0, text: t } } }))
    this.ev('assistant/message', { turn, step, message: { id: randomUUID(), role: 'assistant', content: [{ type: 'text', text }], source: { kind: 'model' } }, stream: [] })
    this.root.emit('agent/assistant-stream', { agent: this, frame: { type: 'end', attemptId, revision: 1, index: parts.length, outcome: { kind: 'committed', eventType: 'assistant/message', seq: this.seq - 1 } } })
  }
  /** One tool call through the registry pipeline: guards, then host or built-in body. */
  async callTool(turn, name, args, signal) {
    const callId = `call-${turn}-${name}`
    this.ev('tool/call', { turn, step: 0, callId, name, arguments: JSON.stringify(args) })
    const exec = Object.freeze({ callId, rootCallId: callId, name, arguments: args, agent: this, signal })
    let text, isError = false
    // DSH order (core/tools/src/index.ts): pre-execute waterfall -> (ask) -> guards only when allowed.
    const decision = await this.root.waterfall('tools/pre-execute', exec, () => ({ kind: 'allow' }))
    const denied = decision.kind === 'deny' ? decision.reason
      : decision.kind === 'cancel' ? 'cancelled'
      : this.root.guards.map(g => g(exec)).find(r => r !== undefined)
    if (denied !== undefined) { text = `denied: ${denied}`; isError = true } else {
      const def = this.tools.get(name)
      try { text = def ? String(await def.execute(args, exec)) : 'ran' } catch (e) { text = String(e.message); isError = true }
    }
    this.ev('tool/result', { turn, step: 0, message: { id: randomUUID(), role: 'tool', toolCallId: callId, content: [{ type: 'text', text }], ...(isError ? { isError: true } : {}), source: { kind: 'tool' } } })
    this.root.emit('tools/result', exec, { isError })
    return text
  }
  async run(text, turn, signal) {
    const words = text.trim().split(/\s+/)
    switch (words[0]) {
      case 'perm': {
        const callId = `call-${turn}`
        const command = words.slice(1).join(' ')
        this.ev('tool/call', { turn, step: 0, callId, name: 'bash', arguments: JSON.stringify({ command, description: 'test' }) })
        const outcome = await this.root.waterfall('approval/request', { agent: this, toolName: 'bash', callId, reason: 'sandbox escalation', signal }, () => 'unavailable')
        const ok = outcome === 'allowed-once'
        this.ev('tool/result', { turn, step: 0, message: { id: randomUUID(), role: 'tool', toolCallId: callId, content: [{ type: 'text', text: ok ? 'ran' : `not run: ${outcome}` }], ...(ok ? {} : { isError: true }), source: { kind: 'tool' } } })
        this.say(turn, ok ? 'allowed' : `denied:${outcome}`, 1)
        return { kind: 'completed' }
      }
      case 'ask': {
        const ans = await this.root.waterfall('user-questions/request', {
          agent: this, signal,
          questions: [{ id: 'color', question: 'Which color?', header: 'Pick', options: [{ label: 'red' }, { label: 'blue', description: 'cool' }] },
            { id: 'name', question: 'Your name?' }],
        }, () => { throw new Error('no answerer') })
        this.say(turn, `answers:${JSON.stringify(ans.answers)}`)
        return { kind: 'completed' }
      }
      case 'slow': {
        this.say(turn, 'working...')
        await new Promise((resolve, reject) => {
          if (signal.aborted) reject(signal.reason)
          signal.addEventListener('abort', () => reject(signal.reason), { once: true })
        })
        return { kind: 'completed' }
      }
      case 'unknown':
        this.ev('x-custom/thing', { turn, payload: { answer: 42 } })
        this.say(turn, 'ok')
        return { kind: 'completed' }
      case 'subagent': {
        const childId = `${this.id}-child`
        const child = new FakeAgent(this.root, childId, this.cwd, this.id)
        this.root.emit('session/created', child.session)
        child.ev('turn/start', { turn: 1 })
        child.ev('tool/call', { turn: 1, step: 0, callId: 'cc1', name: 'read', arguments: '{"file_path":"a.txt"}' })
        child.ev('assistant/message', { turn: 1, step: 0, message: { id: 'm', role: 'assistant', content: [{ type: 'text', text: 'child done' }], source: { kind: 'model' } }, stream: [] })
        child.ev('turn/end', { turn: 1, reason: { kind: 'completed' } })
        this.root.emit('subagent/end', { id: childId, local: true, provider: 'in-process', stopReason: 'completed', lastAssistantMessage: [{ type: 'text', text: 'child done' }] })
        this.say(turn, 'parent done')
        return { kind: 'completed' }
      }
      case 'fail':
        throw new Error('model exploded')
      case 'try': {
        const out = await this.callTool(turn, words[1], words[1] === 'bash' ? { command: 'ls' } : { file_path: 'a.txt' }, signal)
        this.say(turn, out)
        return { kind: 'completed' }
      }
      case 'tool': {
        const out = await this.callTool(turn, words[1], JSON.parse(words.slice(2).join(' ') || '{}'), signal)
        this.say(turn, out)
        return { kind: 'completed' }
      }
      case 'run': {
        const out = await this.callTool(turn, 'bash', { command: text.trim().slice(4) }, signal)
        this.say(turn, out)
        return { kind: 'completed' }
      }
      case 'job': {
        const id = this.root.startJob(this.id)
        this.say(turn, `started ${id}`)
        return { kind: 'completed' }
      }
      case 'spawn': {
        const childId = `${this.id}-bg`
        this.root.liveChildren.set(childId, this.id)
        this.root.emit('session/created', new FakeAgent(this.root, childId, this.cwd, this.id).session)
        this.say(turn, `spawned ${childId}`)
        return { kind: 'completed' }
      }
      default:
        this.say(turn, `echo: ${text}${this.steered.length ? ` +steer:${this.steered.map(m => m.content[0].text).join(',')}` : ''}`)
        this.steered.length = 0
        return { kind: 'completed' }
    }
  }
}
