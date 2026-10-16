import assert from 'node:assert/strict';
import test from 'node:test';
import { executeJavaScript, type ToolCall } from './sandbox.ts';

const limits = { memory: 8 * 1024 * 1024, milliseconds: 1_000, output: 1_024 };
const unexpectedTool: ToolCall = async name => { throw new Error(`Unexpected tool: ${name}`); };

test('Promise.all starts host tools concurrently and preserves input order', { timeout: 5_000 }, async () => {
  let active = 0;
  let peak = 0;
  const completed: string[] = [];
  let releaseFirst!: () => void;
  const first = new Promise<void>(resolve => { releaseFirst = resolve; });
  const invoke: ToolCall = async name => {
    peak = Math.max(peak, ++active);
    if (name === 'first') await first;
    completed.push(name);
    active--;
    if (name === 'second') releaseFirst();
    return { name };
  };
  const output = await executeJavaScript(`
    const results = await Promise.all([tools.first({}), tools.second({})]);
    text(results);
  `, ['first', 'second'], invoke, new AbortController().signal, limits);
  assert.equal(peak, 2);
  assert.deepEqual(completed, ['second', 'first']);
  assert.deepEqual(JSON.parse(output), [{ name: 'first' }, { name: 'second' }]);
});

test('host rejection is catchable inside guest code', async () => {
  const output = await executeJavaScript(`
    try { await tools.fail({}); }
    catch (error) { text(error.message); }
    text('continued');
  `, ['fail'], async () => { throw new Error('tool failed'); }, new AbortController().signal, limits);
  assert.equal(output, 'tool failed\ncontinued');
});

test('guest globals and function constructors cannot access host capabilities', async () => {
  const output = await executeJavaScript(`
    text([typeof process, typeof require, typeof fetch, typeof module, typeof Buffer]);
    text(text.constructor('return typeof process')());
    text(await tools.echo.constructor('return typeof process')());
    text(__call.constructor('return typeof process')());
    text(({}).constructor.constructor('return typeof process')());
  `, ['echo'], unexpectedTool, new AbortController().signal, limits);
  assert.deepEqual(output.split('\n'), [
    JSON.stringify(Array(5).fill('undefined')),
    'undefined', 'undefined', 'undefined', 'undefined',
  ]);
});

test('guest cannot dynamically load Node modules', async () => {
  const output = await executeJavaScript(`
    try { await import('node:fs'); text('loaded'); }
    catch { text('blocked'); }
  `, [], unexpectedTool, new AbortController().signal, limits);
  assert.equal(output, 'blocked');
});

test('output limit counts UTF-8 bytes', async () => {
  await assert.rejects(executeJavaScript(`text('é'.repeat(9));`, [], unexpectedTool,
    new AbortController().signal, { ...limits, output: 16 }));
});

test('synchronous loops stop at the execution deadline', { timeout: 5_000 }, async () => {
  await assert.rejects(executeJavaScript('while (true) {}', [], unexpectedTool,
    new AbortController().signal, { ...limits, milliseconds: 30 }));
});

test('guest allocation is bounded by the memory limit', { timeout: 5_000 }, async () => {
  await assert.rejects(executeJavaScript(`const allocation = new Array(1024 * 1024).fill(7); text(allocation.length);`, [], unexpectedTool,
    new AbortController().signal, { ...limits, memory: 2 * 1024 * 1024 }));
});

function pendingTool(onStart: (signal: AbortSignal) => void, onStop: () => void): ToolCall {
  return async (_name, _args, signal) => {
    try {
      await new Promise<never>((_resolve, reject) => {
        signal.addEventListener('abort', () => reject(signal.reason), { once: true });
        onStart(signal);
        if (signal.aborted) reject(signal.reason);
      });
    } finally {
      onStop();
    }
  };
}

test('deadline cancels and settles an outstanding host tool', { timeout: 5_000 }, async () => {
  let toolSignal: AbortSignal | undefined;
  let stopped = false;
  const invoke = pendingTool(signal => { toolSignal = signal; }, () => { stopped = true; });
  await assert.rejects(executeJavaScript('await tools.wait({});', ['wait'], invoke,
    new AbortController().signal, { ...limits, milliseconds: 30 }), /time limit/);
  assert.equal(toolSignal?.aborted, true);
  assert.equal(stopped, true);
});

test('caller cancellation propagates its reason and settles host work', { timeout: 5_000 }, async () => {
  const controller = new AbortController();
  const reason = new Error('caller stopped');
  let toolSignal: AbortSignal | undefined;
  let stopped = false;
  const invoke = pendingTool(signal => {
    toolSignal = signal;
    setImmediate(() => controller.abort(reason));
  }, () => { stopped = true; });
  await assert.rejects(executeJavaScript('await tools.wait({});', ['wait'], invoke,
    controller.signal, limits), error => error === reason);
  assert.equal(toolSignal?.reason, reason);
  assert.equal(stopped, true);
});

test('already cancelled invocation never starts host work', async () => {
  const reason = new Error('cancelled before execution');
  await assert.rejects(executeJavaScript('await tools.wait({});', ['wait'], unexpectedTool,
    AbortSignal.abort(reason), limits), error => error === reason);
});

test('successful completion cancels and settles unawaited host work', { timeout: 5_000 }, async () => {
  let toolSignal: AbortSignal | undefined;
  let stopped = false;
  const invoke = pendingTool(signal => { toolSignal = signal; }, () => { stopped = true; });
  const output = await executeJavaScript(`tools.wait({}); text('done');`, ['wait'], invoke,
    new AbortController().signal, limits);
  assert.equal(output, 'done');
  assert.equal(toolSignal?.aborted, true);
  assert.equal(stopped, true);
});

test('each invocation receives fresh guest state', async () => {
  await executeJavaScript('globalThis.persisted = 123; tools.echo = 123;', ['echo'], unexpectedTool,
    new AbortController().signal, limits);
  const output = await executeJavaScript(`text(typeof persisted); text(typeof tools.echo);`, ['echo'], unexpectedTool,
    new AbortController().signal, limits);
  assert.equal(output, 'undefined\nfunction');
});
