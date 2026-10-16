import assert from 'node:assert/strict';
import test, { type TestContext } from 'node:test';
import { createClient, type ProviderConfig } from './provider.ts';

const config: ProviderConfig = {
  APIKey: 'test-provider-secret', AuthAccountID: '', BaseURL: 'https://api.example.test/v1',
  Model: 'test-model', ServiceTier: '', ThinkingEffort: '', Timeout: 0,
  MaxSteps: 10,
};
const codexConfig = { ...config, AuthAccountID: 'test-account-secret', BaseURL: 'https://chatgpt.com/backend-api/codex' };
const compacted = { id: 'cmp_1', type: 'compaction', encrypted_content: 'opaque-state' };
const usage = { input_tokens: 10, output_tokens: 2, total_tokens: 12 };
const input = [
  { role: 'developer' as const, content: 'Policy' },
  { role: 'user' as const, content: 'Task' },
  { role: 'assistant' as const, content: 'Old answer' },
  { type: 'function_call_output' as const, call_id: 'call_1', output: 'Old tool result' },
  { role: 'system' as const, content: 'System policy' },
];

interface CapturedRequest { url: string; headers: Headers; body: Record<string, unknown> }

function captureFetch(t: TestContext, respond: (request: CapturedRequest, index: number) => Response) {
  const requests: CapturedRequest[] = [];
  t.mock.method(globalThis, 'fetch', async (url: Parameters<typeof fetch>[0], init?: RequestInit) => {
    const request = { url: String(url), headers: new Headers(init?.headers), body: JSON.parse(String(init?.body)) };
    requests.push(request);
    return respond(request, requests.length - 1);
  });
  return requests;
}

function completion(headers?: HeadersInit) {
  return Response.json({ id: 'resp_1', object: 'response', output: [] }, { headers });
}

function compactionStream(count = 1, completed = true) {
  const events = Array.from({ length: count }, (_, index) => ({
    type: 'response.output_item.done', output_index: index,
    item: { ...compacted, id: `cmp_${index + 1}` },
  }));
  const data: unknown[] = [...events];
  if (completed) data.push({ type: 'response.completed', response: { id: 'resp_compact', usage } });
  return new Response(data.map(event => `data: ${JSON.stringify(event)}\n\n`).join('') + 'data: [DONE]\n\n', {
    headers: { 'content-type': 'text/event-stream', 'x-codex-turn-state': 'compacted-turn' },
  });
}

function assertCredentialsOnlyInHeaders(request: CapturedRequest) {
  const body = JSON.stringify(request.body);
  assert.equal(body.includes(config.APIKey), false);
  assert.equal(body.includes(codexConfig.AuthAccountID), false);
  assert.equal(request.headers.get('authorization'), `Bearer ${config.APIKey}`);
}

test('API key requests preserve SDK body and transport without Codex headers', async t => {
  const requests = captureFetch(t, () => completion());
  const client = createClient(config, 'cache-key', () => assert.fail('Unexpected compaction'));
  const body = {
    model: config.Model, input: 'Inspect repository', store: false,
    tools: [{ type: 'function' as const, name: 'exec', description: 'Run JavaScript', parameters: { type: 'object' }, strict: false }],
  };
  const result = await client.responses.create(body);
  assert.equal(result.id, 'resp_1');
  assert.equal(requests.length, 1);
  assert.equal(requests[0].url, `${config.BaseURL}/responses`);
  assert.deepEqual(requests[0].body, body);
  assert.equal(requests[0].headers.get('content-type'), 'application/json');
  for (const header of ['chatgpt-account-id', 'originator', 'session-id', 'thread-id', 'x-codex-turn-state']) {
    assert.equal(requests[0].headers.has(header), false);
  }
  assert.notEqual(requests[0].headers.get('user-agent'), 'codex_cli_rs');
  assertCredentialsOnlyInHeaders(requests[0]);
});

test('Codex credentials and sticky turn state remain request headers', async t => {
  const requests = captureFetch(t, (_request, index) => completion(
    index === 0 ? { 'x-codex-turn-state': 'turn-one' } : index === 2 ? { 'x-codex-turn-state': 'turn-two' } : {},
  ));
  const client = createClient(codexConfig, 'session-key', () => {});
  for (let index = 0; index < 4; index++) await client.responses.create({ model: config.Model, input: 'Task' });
  assert.deepEqual(requests.map(request => request.headers.get('x-codex-turn-state')), [null, 'turn-one', 'turn-one', 'turn-two']);
  for (const request of requests) {
    assert.equal(request.headers.get('chatgpt-account-id'), codexConfig.AuthAccountID);
    assert.equal(request.headers.get('originator'), 'codex_cli_rs');
    assert.equal(request.headers.get('user-agent'), 'codex_cli_rs');
    assert.equal(request.headers.get('session-id'), 'session-key');
    assert.equal(request.headers.get('thread-id'), 'session-key');
    assertCredentialsOnlyInHeaders(request);
  }
});

test('API key compaction preserves the standard compact endpoint', async t => {
  const requests = captureFetch(t, () => Response.json({
    id: 'native_compact', object: 'response.compaction', created_at: 1, output: [compacted], usage,
  }));
  const phases: string[] = [];
  const client = createClient(config, '', phase => { phases.push(phase); });
  const result = await client.responses.compact({ model: config.Model, input });
  assert.equal(result.id, 'native_compact');
  assert.deepEqual(phases, ['compacting']);
  assert.equal(requests.length, 1);
  assert.equal(requests[0].url, `${config.BaseURL}/responses/compact`);
  assert.deepEqual(requests[0].body, { model: config.Model, input });
  assertCredentialsOnlyInHeaders(requests[0]);
});

test('Codex compaction uses Responses SSE and retains task and policy with opaque state', async t => {
  const requests = captureFetch(t, (_request, index) => index === 0 ? compactionStream() : completion());
  const phases: string[] = [];
  const client = createClient(codexConfig, 'session-key', phase => { phases.push(phase); });
  const result = await client.responses.compact({ model: config.Model, instructions: 'Compact now', input });
  assert.deepEqual(phases, ['compacting']);
  assert.equal(requests.length, 1);
  assert.equal(requests[0].url, `${codexConfig.BaseURL}/responses`);
  assert.deepEqual(requests[0].body, {
    model: config.Model, instructions: 'Compact now', store: false, stream: true,
    input: [...input, { type: 'compaction_trigger' }],
  });
  assertCredentialsOnlyInHeaders(requests[0]);
  assert.equal(result.id, 'resp_compact');
  assert.equal(result.object, 'response.compaction');
  assert.deepEqual(result.output, [compacted, { type: 'unknown', providerData: input[0] }, input[1], input[4]]);
  assert.deepEqual(result.usage, usage);
  assert.equal(typeof result.created_at, 'number');
  await client.responses.create({ model: config.Model, input: 'Continue' });
  assert.equal(requests[1].headers.get('x-codex-turn-state'), 'compacted-turn');
});

for (const fault of [
  { name: 'missing completion', count: 1, completed: false },
  { name: 'missing compaction item', count: 0, completed: true },
  { name: 'multiple compaction items', count: 2, completed: true },
]) {
  test(`Codex compaction rejects ${fault.name}`, async t => {
    const requests = captureFetch(t, () => compactionStream(fault.count, fault.completed));
    const client = createClient(codexConfig, '', () => {}).withOptions({ maxRetries: 0 });
    await assert.rejects(client.responses.compact({ model: config.Model, input }), error => {
      assert.ok(error instanceof Error);
      const cause = error.cause instanceof Error ? error.cause : error;
      assert.match(cause.message, /did not return one completed compaction item/);
      return true;
    });
    assert.equal(requests.length, 1);
  });
}
