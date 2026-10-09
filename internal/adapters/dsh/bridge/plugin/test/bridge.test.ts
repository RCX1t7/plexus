// JSON-RPC handling tests for the bridge, driven over in-memory streams with a
// fake DSH Cordis context (no live DSH). Run: bun test test/
import { describe, expect, test } from 'bun:test'
import { PassThrough } from 'node:stream'
import { apply } from '../src/index.ts'
import { mapAnswers } from '../src/bridge.ts'
// @ts-ignore - plain ESM test double (fake DSH Cordis context)
import { createFakeCtx } from './fakectx.mjs'

type Msg = { id?: number | null; method?: string; params?: any; result?: any; error?: { code: number; message: string } }

function harness(opts: { persisted?: string[]; noGuard?: boolean; defaultModel?: Record<string, unknown> } = {}) {
  const input = new PassThrough()
  const output = new PassThrough()
  const ctx = createFakeCtx(opts)
  const msgs: Msg[] = []
  const waiters: { pred: (m: Msg) => boolean; resolve: (m: Msg) => void }[] = []
  let buf = ''
  output.on('data', (c: Buffer) => {
    buf += c.toString('utf8')
    let i: number
    while ((i = buf.indexOf('\n')) >= 0) {
      const line = buf.slice(0, i); buf = buf.slice(i + 1)
      const m = JSON.parse(line) as Msg
      msgs.push(m)
      for (const w of [...waiters]) if (w.pred(m)) { waiters.splice(waiters.indexOf(w), 1); w.resolve(m) }
    }
  })
  let exitCode: number | undefined
  apply(ctx, { input, output, exit: (c) => { exitCode = c } })
  let next = 1
  const wait = (pred: (m: Msg) => boolean, ms = 2000): Promise<Msg> => {
    const found = msgs.find(pred)
    if (found) return Promise.resolve(found)
    return new Promise((resolve, reject) => {
      const t = setTimeout(() => reject(new Error('timeout waiting for message')), ms)
      waiters.push({ pred, resolve: (m) => { clearTimeout(t); resolve(m) } })
    })
  }
  const call = async (method: string, params: any = {}): Promise<Msg> => {
    const id = next++
    input.write(JSON.stringify({ jsonrpc: '2.0', id, method, params }) + '\n')
    return wait(m => m.id === id && m.method === undefined)
  }
  const reply = (id: number, result: any) => input.write(JSON.stringify({ jsonrpc: '2.0', id, result }) + '\n')
  const events = (turnId?: string) => msgs.filter(m => m.method === 'plexus.event' && (turnId === undefined || m.params.turnId === turnId)).map(m => m.params)
  const terminal = (turnId: string) => wait(m => m.method === 'plexus.event' && m.params.turnId === turnId && (m.params.kind === 'final' || m.params.kind === 'error'))
  const open = async (p: any = {}) => {
    expect((await call('plexus.initialize', { protocol: 1 })).result.protocol).toBe(1)
    const r = await call('plexus.session.open', { cwd: '/tmp', ...p })
    return r.result.sessionId as string
  }
  return { input, ctx, msgs, call, reply, wait, events, terminal, open, exit: () => exitCode }
}

describe('handshake', () => {
  test('capability and version report', async () => {
    const h = harness()
    const r = await h.call('plexus.initialize', { protocol: 1, client: { name: 'plexus', version: 'test' } })
    expect(r.result.bridge.name).toBe('plexus-dsh-bridge')
    expect(r.result.capabilities.permission).toBe('native')
    expect(r.result.capabilities.resume).toBe('native')
    expect(r.result.capabilities.slash_commands).toBe('native')
    expect(r.result.capabilities.goals).toBe('unsupported') // discovered, not hardcoded
    expect(r.result.services).toContain('approval')
  })
  test('protocol mismatch, not-initialized, parse error, unknown method', async () => {
    const h = harness()
    expect((await h.call('plexus.session.open', {})).error?.code).toBe(-32000)
    expect((await h.call('plexus.initialize', { protocol: 99 })).error?.code).toBe(-32005)
    h.input.write('{not json\n')
    expect((await h.wait(m => m.id === null)).error?.code).toBe(-32700)
    await h.call('plexus.initialize', { protocol: 1 })
    expect((await h.call('plexus.nope', {})).error?.code).toBe(-32601)
    expect((await h.call('plexus.prompt', { sessionId: 'missing', text: 'x' })).error?.code).toBe(-32001)
  })
})

describe('turns', () => {
  test('prompt streams deltas, message and exactly one final', async () => {
    const h = harness()
    // raw is opt-in (CR-24: off by default for privacy/size). Request it here.
    expect((await h.call('plexus.initialize', { protocol: 1, raw: 'all' })).result.protocol).toBe(1)
    const sid = (await h.call('plexus.session.open', { cwd: '/tmp', persona: 'You are Tester.' })).result.sessionId
    expect(h.ctx.personas.get(sid)).toBe('You are Tester.')
    const { result } = await h.call('plexus.prompt', { sessionId: sid, text: 'hello' })
    const fin = await h.terminal(result.turnId)
    expect(fin.params.kind).toBe('final')
    expect(fin.params.text).toBe('echo: hello')
    const evs = h.events(result.turnId)
    expect(evs.filter(e => e.kind === 'text_delta').map(e => e.text).join('')).toBe('echo: hello')
    expect(evs.some(e => e.kind === 'message' && e.raw?.type === 'assistant/message')).toBe(true)
    expect(evs.filter(e => e.kind === 'final' || e.kind === 'error').length).toBe(1)
  })
  test('raw native payloads are omitted by default (privacy, CR-24)', async () => {
    const h = harness()
    const sid = await h.open()
    const t = (await h.call('plexus.prompt', { sessionId: sid, text: 'hello' })).result.turnId
    await h.terminal(t)
    expect(h.events(t).every(e => e.raw === undefined)).toBe(true)
  })
  test('permission approve and deny', async () => {
    const h = harness()
    const sid = await h.open()
    for (const allow of [true, false]) {
      const { result } = await h.call('plexus.prompt', { sessionId: sid, text: 'perm rm -rf build' })
      const req = await h.wait(m => m.method === 'plexus.permission' && m.params.turnId === result.turnId)
      expect(req.params.tool.name).toBe('bash')
      expect(req.params.tool.input.command).toBe('rm -rf build')
      expect(req.params.options.map((o: any) => o.kind)).toEqual(['allow_once', 'reject_once'])
      h.reply(req.id as number, { allow, optionId: allow ? 'allow-once' : 'reject-once' })
      const fin = await h.terminal(result.turnId)
      expect(fin.params.text).toBe(allow ? 'allowed' : 'denied:rejected')
    }
  })
  test('question answer mapping', async () => {
    const h = harness()
    const sid = await h.open()
    const { result } = await h.call('plexus.prompt', { sessionId: sid, text: 'ask' })
    const req = await h.wait(m => m.method === 'plexus.question')
    expect(req.params.questions[0]).toMatchObject({ id: 'color', text: 'Which color?', multi: false })
    h.reply(req.id as number, { answers: { color: ['blue'], name: ['Ada'] } })
    const fin = await h.terminal(result.turnId)
    expect(fin.params.text).toBe('answers:[{"id":"color","selected":["blue"]},{"id":"name","selected":[],"custom":"Ada"}]')
  })
  test('cancel mid-turn ends the turn with one error and drops queued prompts', async () => {
    const h = harness()
    const sid = await h.open()
    const slow = (await h.call('plexus.prompt', { sessionId: sid, text: 'slow' })).result.turnId
    await h.wait(m => m.method === 'plexus.event' && m.params.kind === 'message' && m.params.turnId === slow)
    const queued = (await h.call('plexus.prompt', { sessionId: sid, text: 'later' })).result.turnId
    expect((await h.call('plexus.cancel', { sessionId: sid })).result.cancelled).toBe(true)
    const a = await h.terminal(slow)
    expect(a.params).toMatchObject({ kind: 'error', status: 'aborted' })
    const b = await h.terminal(queued)
    expect(b.params).toMatchObject({ kind: 'error', status: 'aborted' })
  })
  test('model failure maps to one error', async () => {
    const h = harness()
    const sid = await h.open()
    const t = (await h.call('plexus.prompt', { sessionId: sid, text: 'fail' })).result.turnId
    expect((await h.terminal(t)).params).toMatchObject({ kind: 'error', status: 'error', text: 'model exploded' })
  })
  test('resume known and unknown session', async () => {
    const h = harness({ persisted: ['s-old'] })
    await h.call('plexus.initialize', { protocol: 1 })
    const ok = await h.call('plexus.session.open', { resume: 's-old' })
    expect(ok.result).toMatchObject({ sessionId: 's-old', resumed: true })
    const bad = await h.call('plexus.session.open', { resume: 's-missing' })
    expect(bad.error?.code).toBe(-32002)
  })
})

describe('passthrough', () => {
  test('unknown session event arrives raw as extension', async () => {
    const h = harness()
    const sid = await h.open()
    const t = (await h.call('plexus.prompt', { sessionId: sid, text: 'unknown' })).result.turnId
    await h.terminal(t)
    const ext = h.events(t).find(e => e.kind === 'extension' && e.name === 'x-custom/thing')
    expect(ext?.raw.data.payload.answer).toBe(42)
  })
  test('subagent lineage and lifecycle', async () => {
    const h = harness()
    const sid = await h.open()
    const t = (await h.call('plexus.prompt', { sessionId: sid, text: 'subagent' })).result.turnId
    await h.terminal(t)
    const child = `${sid}-child`
    const evs = h.events()
    expect(evs.some(e => e.kind === 'background' && e.name === 'subagent' && e.status === 'started' && e.text === child)).toBe(true)
    expect(evs.some(e => e.kind === 'tool_use' && e.parent_id === child && e.tool.input.file_path === 'a.txt')).toBe(true)
    expect(evs.some(e => e.kind === 'background' && e.status === 'finished:completed' && e.text === 'child done')).toBe(true)
    expect(evs.filter(e => e.turnId === t && e.kind === 'final').length).toBe(1)
  })
  test('native slash command, raw control call, steer control', async () => {
    const h = harness()
    const sid = await h.open()
    const r = await h.call('plexus.prompt', { sessionId: sid, text: '/compact now' })
    expect(r.result.command).toBe('compact')
    expect((await h.terminal(r.result.turnId)).params).toMatchObject({ kind: 'final', text: 'compacted' })
    const call = await h.call('plexus.control', { sessionId: sid, name: 'call', payload: { service: 'echo', method: 'say', args: [{ $agent: true }, 7] } })
    expect(call.result).toEqual({ agentId: sid, x: 7 })
    const missing = await h.call('plexus.control', { sessionId: sid, name: 'call', payload: { service: 'nope', method: 'x' } })
    expect(missing.error?.code).toBe(-32003)
    expect((await h.call('plexus.control', { sessionId: sid, name: 'approval.setPolicy', payload: { policy: 'never' } })).result).toEqual({})
    expect(h.ctx.log).toContainEqual(['setPolicy', 'never'])
  })
  test('shutdown exits 0', async () => {
    const h = harness()
    await h.open()
    await h.call('plexus.shutdown', {})
    await new Promise(r => setTimeout(r, 20))
    expect(h.exit()).toBe(0)
  })
})

describe('plexus host tools, guest lock, tasks', () => {
  const tools = [{ name: 'plexus_post', description: 'Post to Slack', inputSchema: { type: 'object', properties: { text: { type: 'string' } } } },
    { name: 'plexus_task', description: 'Start a task', inputSchema: { type: 'object' } }]
  test('host tool call round-trips through plexus.tool', async () => {
    const h = harness()
    const sid = await h.open({ tools })
    const t = (await h.call('plexus.prompt', { sessionId: sid, text: 'tool plexus_post {"text":"hi"}' })).result.turnId
    const req = await h.wait(m => m.method === 'plexus.tool')
    expect(req.params).toMatchObject({ sessionId: sid, turnId: t, guest: false, name: 'plexus_post', arguments: { text: 'hi' } })
    h.reply(req.id as number, { text: 'posted 123' })
    expect((await h.terminal(t)).params.text).toBe('posted 123')
    const t2 = (await h.call('plexus.prompt', { sessionId: sid, text: 'tool plexus_post {}' })).result.turnId
    const req2 = await h.wait(m => m.method === 'plexus.tool' && m.id !== req.id)
    h.reply(req2.id as number, { text: 'bad args', isError: true })
    expect((await h.terminal(t2)).params.text).toBe('bad args')
    expect(h.events(t2).find(e => e.kind === 'tool_result')?.status).toBe('error')
  })
  test('guest turn denies every native tool and every host tool but plexus_post, never asks Plexus, then unlocks', async () => {
    const h = harness()
    const init = await h.call('plexus.initialize', { protocol: 1 })
    expect(init.result.capabilities.guest_lock).toBe('native')
    const sid = (await h.call('plexus.session.open', { tools })).result.sessionId
    for (const tool of ['bash', 'read']) {
      const t = (await h.call('plexus.prompt', { sessionId: sid, text: `try ${tool}`, guest: true })).result.turnId
      expect((await h.terminal(t)).params.text).toStartWith('denied: Tools are disabled')
    }
    const p = (await h.call('plexus.prompt', { sessionId: sid, text: 'perm rm -rf /', guest: true })).result.turnId
    expect((await h.terminal(p)).params.text).toBe('denied:rejected')
    expect(h.msgs.some(m => m.method === 'plexus.permission')).toBe(false)
    // another Plexus host tool is denied for a guest, natively (no plexus.tool)
    const ot = (await h.call('plexus.prompt', { sessionId: sid, text: 'tool plexus_task {}', guest: true })).result.turnId
    expect((await h.terminal(ot)).params.text).toStartWith('denied: Tools are disabled')
    expect(h.msgs.some(m => m.method === 'plexus.tool')).toBe(false)
    const ht = (await h.call('plexus.prompt', { sessionId: sid, text: 'tool plexus_post {"text":"x"}', guest: true })).result.turnId
    const req = await h.wait(m => m.method === 'plexus.tool')
    expect(req.params.guest).toBe(true)
    h.reply(req.id as number, { text: 'ok' })
    expect((await h.terminal(ht)).params.text).toBe('ok')
    const tr = (await h.call('plexus.prompt', { sessionId: sid, text: 'try read' })).result.turnId
    expect((await h.terminal(tr)).params.text).toBe('ran')
  })
  test('a legacy level member never widens a guest turn', async () => {
    const h = harness()
    const sid = await h.open({ tools })
    const t = (await h.call('plexus.prompt', { sessionId: sid, text: 'try bash', guest: true, level: 'full' })).result.turnId
    expect((await h.terminal(t)).params.text).toStartWith('denied: Tools are disabled')
  })
  test('every prompt is refused when DSH has no tools.guard (fail closed)', async () => {
    const h = harness({ noGuard: true })
    const sid = await h.open()
    expect((await h.call('plexus.prompt', { sessionId: sid, text: 'hi', guest: true })).error?.code).toBe(-32007)
    expect((await h.call('plexus.prompt', { sessionId: sid, text: 'hi' })).error?.code).toBe(-32007)
  })
  test('background job and subagent are listed and stoppable', async () => {
    const h = harness()
    const sid = await h.open()
    await h.terminal((await h.call('plexus.prompt', { sessionId: sid, text: 'job' })).result.turnId)
    await h.terminal((await h.call('plexus.prompt', { sessionId: sid, text: 'spawn' })).result.turnId)
    const list = (await h.call('plexus.control', { sessionId: sid, name: 'tasks.list', payload: {} })).result
    expect(list).toEqual([{ id: `${sid}-bg`, kind: 'subagent', parentSessionId: sid }, { id: 'shell-1', kind: 'job', ownerSessionId: sid }])
    expect((await h.call('plexus.control', { sessionId: sid, name: 'task.stop', payload: { id: 'shell-1' } })).result.stopped).toBe('job')
    await h.wait(m => m.method === 'plexus.event' && m.params.name === 'job' && m.params.status === 'finished:killed')
    expect((await h.call('plexus.control', { sessionId: sid, name: 'task.stop', payload: { id: `${sid}-bg` } })).result.stopped).toBe('subagent')
    await h.wait(m => m.method === 'plexus.event' && m.params.name === 'subagent' && m.params.status === 'finished:interrupted')
    expect((await h.call('plexus.control', { sessionId: sid, name: 'tasks.list', payload: {} })).result).toEqual([])
    expect((await h.call('plexus.control', { sessionId: sid, name: 'task.stop', payload: { id: 'nope' } })).error?.code).toBe(-32004)
  })
})

describe('dangerous-action gate (single classifier: Plexus Go internal/danger)', () => {
  test('every side-effecting call is forwarded to Plexus with no local danger judgement; a parked deny is relayed', async () => {
    const h = harness()
    const sid = await h.open({ cwd: '/work/proj' })
    const t = (await h.call('plexus.prompt', { sessionId: sid, text: 'run git push --force origin main' })).result.turnId
    const req = await h.wait(m => m.method === 'plexus.permission')
    expect(req.params).toMatchObject({ turnId: t, tool: { name: 'bash', input: { command: 'git push --force origin main' } } })
    expect(req.params.danger).toBeUndefined() // the bridge does not classify; Plexus does
    // Park (CR-3): Plexus denies at once with a reason and the turn goes on; the
    // reason is relayed verbatim so the model does not retry.
    h.reply(req.id as number, { allow: false, optionId: 'reject-once', reason: '已暂挂，等负责人批准' })
    const fin = await h.terminal(t)
    expect(fin.params.text).toContain('已暂挂，等负责人批准')
    expect(h.events(t).find(e => e.kind === 'tool_result')?.status).toBe('error')
  })
  test('an approved call is allowed once; the next identical call is forwarded again', async () => {
    const h = harness()
    const sid = await h.open({ cwd: '/work/proj' })
    const t = (await h.call('plexus.prompt', { sessionId: sid, text: 'run rm -rf /tmp/cache' })).result.turnId
    const req = await h.wait(m => m.method === 'plexus.permission')
    expect(req.params.tool.input.command).toBe('rm -rf /tmp/cache')
    h.reply(req.id as number, { allow: true, optionId: 'allow-once' })
    expect((await h.terminal(t)).params.text).toBe('ran')
    const t2 = (await h.call('plexus.prompt', { sessionId: sid, text: 'run rm -rf /tmp/cache' })).result.turnId
    const req2 = await h.wait(m => m.method === 'plexus.permission' && m.id !== req.id)
    h.reply(req2.id as number, { allow: false })
    expect((await h.terminal(t2)).params.text).toStartWith('denied:')
  })
  test('a read-only call runs without a permission request', async () => {
    const h = harness()
    const sid = await h.open({ cwd: '/work/proj' })
    const t = (await h.call('plexus.prompt', { sessionId: sid, text: 'try read' })).result.turnId
    expect((await h.terminal(t)).params.text).toBe('ran')
    expect(h.msgs.some(m => m.method === 'plexus.permission')).toBe(false)
  })
  test('a policy that auto-allows ahead of the bridge cannot bypass the gate (guard blocks)', async () => {
    const h = harness()
    const sid = await h.open({ cwd: '/work/proj' })
    h.ctx.on('tools/pre-execute', () => ({ kind: 'allow' }), true) // e.g. a user PreToolUse hook answering "allow"
    const t = (await h.call('plexus.prompt', { sessionId: sid, text: 'run reg add HKLM\\Software\\X /v Y /d 1' })).result.turnId
    expect((await h.terminal(t)).params.text).toStartWith('denied: Blocked:')
    expect(h.msgs.some(m => m.method === 'plexus.permission')).toBe(false)
  })
})

test('mapAnswers: declined, multi, single-select overflow', () => {
  const req = { questions: [{ id: 'a', question: '?', options: [{ label: 'x' }, { label: 'y' }] }, { id: 'b', question: '?', multiSelect: true, options: [{ label: 'x' }, { label: 'y' }] }] }
  expect(mapAnswers(req, null).answers).toEqual([{ id: 'a', selected: [] }, { id: 'b', selected: [] }])
  expect(mapAnswers(req, { a: ['x', 'y'], b: ['x', 'y', 'other'] }).answers).toEqual([{ id: 'a', selected: ['x'] }, { id: 'b', selected: ['x', 'y'], custom: 'other' }])
})

describe('model / effort (SessionOptions.Model/Effort)', () => {
  const dflt = { provider: 'deepseek-official', model: 'deepseek-flash', reasoningEffort: 'medium' }
  const created = (h: any) => h.ctx.log.filter((e: any[]) => e[0] === 'create').map((e: any[]) => e[1])
  test('set: model and effort pass through; provider filled from DSH default', async () => {
    const h = harness({ defaultModel: dflt })
    await h.open({ model: 'deepseek-reasoner', effort: 'high' })
    expect(created(h)).toEqual([{ model: 'deepseek-reasoner', reasoningEffort: 'high', provider: 'deepseek-official' }])
  })
  test('model only: DSH default effort is not borrowed for another model', async () => {
    const h = harness({ defaultModel: dflt })
    await h.open({ model: 'deepseek-reasoner' })
    expect(created(h)).toEqual([{ model: 'deepseek-reasoner', provider: 'deepseek-official' }])
  })
  test('empty: DSH configured default model and effort, and a turn completes', async () => {
    const h = harness({ defaultModel: dflt })
    const sid = await h.open({ model: '', effort: '' })
    expect(created(h)).toEqual([{ model: 'deepseek-flash', provider: 'deepseek-official', reasoningEffort: 'medium' }])
    const t = (await h.call('plexus.prompt', { sessionId: sid, text: 'hello' })).result.turnId
    expect((await h.terminal(t)).params.text).toBe('echo: hello')
  })
  test('effort only: kept, model and provider from the default', async () => {
    const h = harness({ defaultModel: dflt })
    await h.open({ effort: 'low' })
    expect(created(h)).toEqual([{ model: 'deepseek-flash', provider: 'deepseek-official', reasoningEffort: 'low' }])
  })
  test('no default composed and nothing set: DSH decides (no agentOptions)', async () => {
    const h = harness()
    await h.open()
    expect(created(h)).toEqual([null])
  })
})
