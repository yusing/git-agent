import { createInterface } from 'node:readline';
import { Agent, Runner, MemorySession, OpenAIProvider, OpenAIResponsesCompactionSession, MaxTurnsExceededError, tool, setTracingDisabled, type AgentInputItem, type RunState } from '@openai/agents';
import { z } from 'zod';
import { createClient, type ProviderConfig } from './provider.ts';
import { executeJavaScript } from './sandbox.ts';

type Item = { type: string; role?: string; content?: string; call_id?: string; name?: string; arguments?: string; output?: string; raw_json?: string; sdk_json?: string };
type Definition = { Name: string; Description: string; Schema: Record<string, unknown> };
interface Start {
  config: ProviderConfig;
  instructions: string;
  input: Item[];
  tools: Definition[];
  cacheKey: string;
  maxSteps: number;
  repair: boolean;
  finalPrompt: string;
  textFormat?: { name: string; schema: Record<string, unknown>; strict: boolean };
}
const lines = createInterface({ input: process.stdin });
const controller = new AbortController();
const pending = new Map<number, { resolve: (value: any) => void; reject: (error: Error) => void }>();
let sequence = 0;
let config: ProviderConfig | undefined;
const emit = (value: unknown) => process.stdout.write(JSON.stringify(value) + '\n');
const sanitize = (value: unknown) => [config?.APIKey, config?.AuthAccountID].filter(Boolean).reduce<string>((text, secret) => text.replaceAll(secret!, '[REDACTED]'), String(value));
function rpc(type: string, value: Record<string, unknown> = {}, signal = controller.signal): Promise<any> {
  signal.throwIfAborted();
  const id = ++sequence;
  return new Promise((resolve, reject) => {
    const abort = () => {
      pending.delete(id);
      emit({ type: 'cancel-tool', id });
      reject(new Error('Operation canceled'));
    };
    signal.addEventListener('abort', abort, { once: true });
    pending.set(id, {
      resolve: result => { signal.removeEventListener('abort', abort); resolve(result); },
      reject: error => { signal.removeEventListener('abort', abort); reject(error); },
    });
    emit({ type, id, ...value });
  });
}
function fromItem(item: Item): AgentInputItem {
  if (item.sdk_json) return JSON.parse(item.sdk_json);
  if (item.raw_json) return { type: 'unknown', providerData: JSON.parse(item.raw_json) };
  // The SDK's portable message union has no developer role. Preserve that
  // Responses role through its supported provider-specific item escape hatch.
  if (item.type === 'message' && item.role === 'developer') return { type: 'unknown', providerData: { type: 'message', role: 'developer', content: [{ type: 'input_text', text: item.content ?? '' }] } };
  if (item.type === 'message' && item.role === 'assistant') return { type: 'message', role: 'assistant', status: 'completed', content: [{ type: 'output_text', text: item.content ?? '' }] };
  if (item.type === 'message' && item.role === 'user') return { type: 'message', role: 'user', content: [{ type: 'input_text', text: item.content ?? '' }] };
  if (item.type === 'message') return { role: item.role, content: item.content } as AgentInputItem;
  if (item.type === 'function_call_output') return { type: 'function_call_result', name: '', callId: item.call_id, status: 'completed', output: item.output } as AgentInputItem;
  if (item.type === 'function_call') return { type: 'function_call', callId: item.call_id, name: item.name, arguments: item.arguments } as AgentInputItem;
  return item as unknown as AgentInputItem;
}
function toItem(item: AgentInputItem): Item {
  return { type: item.type ?? 'message', sdk_json: JSON.stringify(item) };
}
async function run(start: Start) {
  config = start.config;
  setTracingDisabled(true);
  const progress = (phase: string) => emit({ type: 'progress', phase });
  const client = createClient(config, start.cacheKey, progress);
  const session = /^(gpt-|o[1-9]|ft:)/.test(config.Model)
    ? new OpenAIResponsesCompactionSession({ client, model: config.Model, underlyingSession: new MemorySession(), compactionMode: 'input' })
    : new MemorySession();
  let calls = 0;
  let step = 0;
  let repairs = 0;
  const exec = tool({
    name: 'exec',
    description: 'Execute sandboxed JavaScript using the documented read-only tools.* functions. Await independent calls with Promise.all. Emit selected evidence with text(value). No filesystem, network, imports, subprocesses or Node.js globals.',
    parameters: z.object({ code: z.string() }),
    errorFunction: (_context, error) => sanitize(error instanceof Error ? error.message : error),
    execute: async ({ code }) => {
      const started = performance.now();
      progress('executing');
      try {
        const output = await executeJavaScript(code, start.tools.map(t => t.Name), async (name, args, signal) => {
          calls++;
          return rpc('tool', { name, args, step }, signal);
        }, controller.signal);
        await rpc('snapshot');
        return output;
      } finally {
        emit({ type: 'timing', phase: 'tool_batch', duration_ms: performance.now() - started, step });
      }
    },
  });
  const outputType = start.textFormat ? {
    type: 'json_schema' as const, name: start.textFormat.name,
    schema: start.textFormat.schema as any, strict: start.textFormat.strict,
  } : undefined;
  const makeAgent = (withTools: boolean) => new Agent({
    name: 'git-agent',
    model: config!.Model,
    instructions: start.instructions,
    tools: withTools && start.tools.length ? [exec] : [],
    outputType,
    modelSettings: {
      store: false,
      parallelToolCalls: false,
      reasoning: config!.ThinkingEffort ? { effort: config!.ThinkingEffort as any, summary: 'auto' } : undefined,
      providerData: {
        ...(start.cacheKey ? { prompt_cache_key: start.cacheKey } : {}),
        ...(config!.ServiceTier ? { service_tier: config!.ServiceTier } : {}),
      },
    },
  });
  const runner = new Runner({ modelProvider: new OpenAIProvider({ openAIClient: client }), tracingDisabled: true });
  async function generate(input: AgentInputItem[] | RunState<unknown, Agent<unknown, any>>, withTools: boolean, maxTurns: number): Promise<string> {
    try {
      const result = await runner.run(makeAgent(withTools), input, {
        session, stream: true, maxTurns, signal: controller.signal,
        callModelInputFilter: async ({ modelData }) => {
          step++;
          progress('requesting');
          emit({ type: 'trace', kind: 'request', value: { model: config!.Model, instructions: modelData.instructions, input: modelData.input } });
          return modelData;
        },
      });
      for await (const event of result) {
        if (event.type === 'run_item_stream_event') emit({ type: 'trace', kind: event.name, value: event.item.rawItem });
        if (event.type === 'raw_model_stream_event' && event.data.type === 'response_done') {
          emit({ type: 'usage', step, usage: event.data.response.usage });
        }
      }
      await result.completed;
      for (const usage of result.runContext.usage.requestUsageEntries ?? []) {
        if (usage.endpoint === 'responses.compact') emit({ type: 'usage', step, usage });
      }
      return typeof result.finalOutput === 'string' ? result.finalOutput : JSON.stringify(result.finalOutput);
    } catch (error) {
      if (!withTools || !(error instanceof MaxTurnsExceededError) || !error.state) throw error;
      const decision = await rpc('budget', { budget: { MaxSteps: maxTurns } });
      if (decision.ExtendSteps > 0) return generate(error.state, true, maxTurns + decision.ExtendSteps);
      progress('finalizing');
      // The failed run may have persisted a partial turn. Replace that session
      // with the SDK's authoritative history before the final tool-free turn.
      await session.clearSession();
      await session.addItems(error.state.history);
      return generate([{ type: 'message', role: 'user', content: start.finalPrompt }], false, 1);
    }
  }
  let text = await generate(start.input.map(fromItem), true, start.maxSteps);
  let validation = await rpc('validate', { text });
  if (validation.errors?.length) {
    if (!start.repair) throw new Error(`Validation failed: ${validation.errors.join('; ')}`);
    repairs++;
    const started = performance.now();
    progress('repairing');
    text = await generate([{ type: 'message', role: 'user', content: validation.repairPrompt }], false, 1);
    validation = await rpc('validate', { text });
    emit({ type: 'timing', phase: 'repair', duration_ms: performance.now() - started, step });
    if (validation.errors?.length) throw new Error(`Validation failed after repair: ${validation.errors.join('; ')}`);
  }
  emit({ type: 'result', text: validation.text, toolCalls: calls, repairCalls: repairs, history: (await session.getItems()).map(toItem) });
}
let started = false;
lines.on('line', line => {
  try {
    const message = JSON.parse(line);
    if (!started) {
      started = true;
      void run(message).catch(error => emit({ type: 'error', message: sanitize(error.message ?? error) })).finally(() => {
        controller.abort();
        lines.close();
        process.stdin.destroy();
      });
    } else {
      const callback = pending.get(message.id);
      pending.delete(message.id);
      if (message.error) callback?.reject(new Error(message.error));
      else callback?.resolve(message.result);
    }
  } catch {
    controller.abort(new Error('Invalid runtime protocol'));
  }
});
lines.on('close', () => controller.abort(new Error('Runtime input closed')));
