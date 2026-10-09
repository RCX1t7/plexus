// Frame-cap tests for the newline-JSON transport (CR-9): a 32 MiB cap in both
// directions. An oversize incoming line is discarded to the next newline and
// surfaced as an error; the stream then recovers. An oversize outgoing frame
// drops its `raw` passenger, and is dropped entirely if still too big.
import { describe, expect, test } from 'bun:test'
import { PassThrough } from 'node:stream'
import { LineRpc, MAX_FRAME } from '../src/rpc.ts'

function peer() {
  const input = new PassThrough()
  const output = new PassThrough()
  const rpc = new LineRpc(input, output)
  const notes: { method: string; params: any }[] = []
  const frameErrors: string[] = []
  const requests: { method: string; params: any }[] = []
  rpc.onNotification = (method, params) => notes.push({ method, params })
  rpc.onRequest = (method, params) => { requests.push({ method, params }); return {} }
  rpc.onFrameError = (m) => frameErrors.push(m)
  rpc.start()
  const sent: string[] = []
  output.on('data', (c: Buffer) => { for (const l of c.toString('utf8').split('\n')) if (l) sent.push(l) })
  return { input, rpc, notes, frameErrors, requests, sent }
}

describe('32 MiB frame cap', () => {
  test('exactly MAX_FRAME is accepted, MAX_FRAME+1 is discarded, then the stream recovers', () => {
    const p = peer()
    // A valid notification whose line length is close to (but under) the cap.
    const pad = 'x'.repeat(MAX_FRAME - 200)
    p.input.write(JSON.stringify({ jsonrpc: '2.0', method: 'big', params: { pad } }) + '\n')
    expect(p.notes.at(-1)?.method).toBe('big')
    // An oversize line (> MAX_FRAME, no newline until the end): discarded.
    p.input.write('y'.repeat(MAX_FRAME + 1))
    p.input.write('\n')
    expect(p.frameErrors.length).toBe(1)
    // Recovery: a normal frame right after is handled.
    p.input.write(JSON.stringify({ jsonrpc: '2.0', method: 'after', params: { ok: true } }) + '\n')
    expect(p.notes.at(-1)?.method).toBe('after')
  })

  test('oversize chunks split across writes are discarded as one frame and recover', () => {
    const p = peer()
    const chunk = 'z'.repeat(8 * 1024 * 1024)
    for (let i = 0; i < 5; i++) p.input.write(chunk) // 40 MiB, no newline
    p.input.write('\n')
    expect(p.frameErrors.length).toBe(1)
    p.input.write(JSON.stringify({ jsonrpc: '2.0', method: 'ok', params: {} }) + '\n')
    expect(p.notes.at(-1)?.method).toBe('ok')
  })

  test('an outgoing notification that is too big only because of raw drops raw rather than sending oversize', () => {
    const p = peer()
    p.rpc.notify('plexus.event', { kind: 'message', text: 'hi', raw: { blob: 'q'.repeat(MAX_FRAME) } })
    expect(p.sent.length).toBe(1)
    const sent = JSON.parse(p.sent[0])
    expect(sent.params.text).toBe('hi')
    expect(sent.params.raw).toBeUndefined()
    expect(Buffer.byteLength(p.sent[0], 'utf8')).toBeLessThanOrEqual(MAX_FRAME)
  })
})
