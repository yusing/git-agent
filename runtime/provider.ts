import OpenAI from 'openai';

export interface ProviderConfig {
  APIKey: string;
  AuthAccountID: string;
  BaseURL: string;
  Model: string;
  ServiceTier: string;
  ThinkingEffort: string;
  Timeout: number;
  MaxSteps: number;
}

// Agents SDK 0.20 trims history before the latest compaction item and its
// portable message union lacks the Responses developer role. Normalize only
// compact endpoint results at the SDK boundary, preserving retained messages.
function compactOutputForSession(output: { type?: string; role?: string }[]) {
  return [
    ...output.filter(item => item.type === 'compaction'),
    ...output.filter(item => item.type !== 'compaction').map(item => item.role === 'developer'
      ? { type: 'unknown', providerData: item }
      : item),
  ];
}

export function createClient(config: ProviderConfig, cacheKey: string, notify: (phase: string) => void) {
  const codex = Boolean(config.AuthAccountID) && new URL(config.BaseURL).origin === 'https://chatgpt.com';
  let turnState = '';
  const client: OpenAI = new OpenAI({
    apiKey: config.APIKey,
    baseURL: config.BaseURL,
    ...(config.Timeout > 0 ? { timeout: config.Timeout / 1_000_000 } : {}),
    defaultHeaders: config.AuthAccountID ? {
      'ChatGPT-Account-ID': config.AuthAccountID,
      originator: 'codex_cli_rs',
      'User-Agent': 'codex_cli_rs',
      ...(codex && cacheKey ? { 'session-id': cacheKey, 'thread-id': cacheKey } : {}),
    } : {},
    fetch: async (url, init) => {
      const headers = new Headers(init?.headers);
      if (codex && turnState) headers.set('x-codex-turn-state', turnState);
      const compact = String(url).endsWith('/responses/compact');
      if (compact) notify('compacting');
      // Codex V2 returns a compaction item on the ordinary Responses stream.
      // Adapt that provider contract to the SDK's standard session compactor.
      // Source: codex-rs/core/src/compact_remote_v2_attempt.rs:168:198@0df4aa67781ae45a3c97befa4db48b734afe46d9
      if (codex && compact) {
        const body = JSON.parse(String(init?.body));
        const input = body.input ?? [];
        const stream = await client.responses.create({
          model: body.model,
          instructions: body.instructions,
          store: false,
          stream: true,
          input: [...input, { type: 'compaction_trigger' }],
        }, { signal: init?.signal ?? undefined });
        const compacted: { type: string }[] = [];
        let completed = false;
        let id = '';
        let usage: unknown;
        for await (const event of stream) {
          if (event.type === 'response.output_item.done' && event.item.type === 'compaction') compacted.push(event.item);
          if (event.type === 'response.completed') {
            completed = true;
            id = event.response.id;
            usage = event.response.usage;
          }
        }
        if (!completed || compacted.length !== 1) throw new Error('Codex compaction did not return one completed compaction item');
        // Keep the user task and client-authored policy alongside opaque state.
        const retained = input.filter((item: { role?: string }) => item.role === 'user' || item.role === 'developer' || item.role === 'system');
        return Response.json({ id, object: 'response.compaction', created_at: Math.floor(Date.now() / 1000), output: compactOutputForSession([...retained, ...compacted]), usage });
      }
      const response = await fetch(url, { ...init, headers });
      if (codex) turnState = response.headers.get('x-codex-turn-state') || turnState;
      if (compact && response.ok) {
        const body = await response.json();
        return Response.json({ ...body, output: compactOutputForSession(body.output ?? []) }, { status: response.status });
      }
      return response;
    },
  });
  return client;
}
