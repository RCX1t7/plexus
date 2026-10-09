/**
 * Plexus bridge plugin for DeepSeek Harness (Cordis plugin entry).
 *
 * Loaded by the Plexus-owned profile (`dsh --profile plexus --patch
 * <profile>/plexus.patch.yml`) in place of DSH's own SDK JSON-RPC server row,
 * and gated on the SDK app's `sdkAppStartup` service (command-line parse +
 * stdin-EOF exit). Stdout carries only bridge JSON-RPC frames.
 *
 * Named exports only (no default export) so the DSH Loader's `unwrapExports`
 * keeps `name`, `inject`, and `apply`. No Config schema export: config is
 * runtime-only test hooks. UNVERIFIED against a live DSH process.
 */

import type { Readable, Writable } from 'node:stream'
import { Bridge } from './bridge.ts'
import { LineRpc } from './rpc.ts'
import type { DshContext } from './dsh.ts'

export { PROTOCOL, BRIDGE_VERSION } from './protocol.ts'

export const name = 'plexus-bridge'
export const inject = ['agents']

export interface BridgeConfig {
  input?: Readable
  output?: Writable
  exit?: (code: number) => void
  forward?: string[]
}

export function apply(ctx: DshContext, config: BridgeConfig = {}): void {
  const rpc = new LineRpc(config.input ?? process.stdin, config.output ?? process.stdout)
  const bridge = new Bridge(ctx, rpc, { exit: config.exit })
  bridge.listen(config.forward)
  const serve = (): (() => Promise<void>) => {
    rpc.start()
    return async () => {
      await bridge.dispose()
      rpc.close()
    }
  }
  if (typeof ctx.effect === 'function') ctx.effect(serve, 'plexus-bridge.serve')
  else serve()
}
