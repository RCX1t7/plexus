/**
 * The slice of DeepSeek Harness (DSH) runtime shapes this plugin touches,
 * declared structurally so the bundle imports no DSH package (zero runtime
 * deps). Source of truth: github.com/deepseek-ai/deepseek-harness @ 5badb15
 * (packages/core/agent/src/runtime-types.ts, packages/core/session/src/types.ts,
 * packages/interaction/user-approval/src/types.ts,
 * packages/interaction/user-questions/src/types.ts). Published 0.2.0-rc.2
 * carries the same events. UNVERIFIED against a live DSH process.
 */

export interface DshUserMessage {
  readonly id: string
  readonly role: 'user'
  readonly content: readonly { type: string; text?: string }[]
  readonly source: { kind: string }
}

export interface DshSession {
  readonly id: string
  readonly header?: { id?: string; parentSession?: string }
}

export interface DshAgent {
  readonly id: string
  readonly session: DshSession
  readonly status?: 'idle' | 'running'
  readonly ctx?: DshContext
  followup(message: DshUserMessage): void
  steer(message: DshUserMessage): void
  inject(message: DshUserMessage): void
  cancel(cause: { kind: 'user' }, options?: { keepInbox?: boolean }): void
  whenIdle(): Promise<void>
}

export interface DshAgentHandle {
  agent: DshAgent
  dispose(): Promise<void>
}

export interface DshSessionEvent {
  type: string
  seq: number
  time?: number
  data?: any
}

export interface DshContext {
  on(name: string, listener: (...args: any[]) => any, prepend?: boolean): () => void
  get?(name: string): any
  effect?(fn: () => (() => unknown) | void, label?: string): void
  agents: {
    create(o: {
      sessionId: string
      meta?: { cwd?: string }
      agentOptions?: Record<string, unknown>
      setup?: (agentCtx: any, agent?: DshAgent) => unknown
    }): Promise<DshAgentHandle>
    resume(o: {
      resumeSessionId: string
      agentOptions?: Record<string, unknown>
      setup?: (agentCtx: any, agent?: DshAgent) => unknown
    }): Promise<DshAgentHandle>
  }
  root?: { fiber?: { dispose(): Promise<void> } }
  [k: string]: any
}

export type ApprovalOutcome = 'allowed-once' | 'rejected' | 'cancelled' | 'unavailable'

export interface ApprovalRequest {
  agent: DshAgent
  toolName: string
  callId?: string
  reason?: string
  displayReason?: { en: string; [locale: string]: string }
  signal?: AbortSignal
}

export interface AskQuestionItem {
  id: string
  question: string
  detail?: string
  header?: string
  options?: { label: string; description?: string }[]
  multiSelect?: boolean
  intent?: unknown
}

export interface AskQuestionRequest {
  questions: AskQuestionItem[]
  agent?: DshAgent
  signal?: AbortSignal
  wait?: { callId: string; timed?: boolean }
}

export interface AskQuestionAnswer {
  answers: { id: string; selected: string[]; custom?: string }[]
}
