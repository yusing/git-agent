import variant from '@jitl/quickjs-singlefile-cjs-release-sync';
import {
  newQuickJSWASMModuleFromVariant,
  type QuickJSDeferredPromise,
  type QuickJSWASMModule,
} from 'quickjs-emscripten-core';

// The CJS variant's declarations wrap its default export under NodeNext.
const engine: Promise<QuickJSWASMModule> = newQuickJSWASMModuleFromVariant(variant as unknown as Parameters<typeof newQuickJSWASMModuleFromVariant>[0]);
export type ToolCall = (name: string, args: unknown, signal: AbortSignal) => Promise<unknown>;

// Codex's code-mode boundary exposes promise-returning tools and explicit text
// output in a fresh interpreter. QuickJS supplies a portable WASM boundary here.
// Source: codex-rs/code-mode-runtime/src/runtime/globals.rs:15:73@0df4aa67781ae45a3c97befa4db48b734afe46d9
export async function executeJavaScript(
  code: string,
  names: string[],
  invoke: ToolCall,
  signal: AbortSignal,
  limits = { memory: 64 * 1024 * 1024, milliseconds: 30_000, output: 64 * 1024 },
): Promise<string> {
  signal.throwIfAborted();
  const runtime = (await engine).newRuntime();
  runtime.setMemoryLimit(limits.memory);
  runtime.setMaxStackSize(512 * 1024);
  const deadline = Date.now() + limits.milliseconds;
  runtime.setInterruptHandler(() => signal.aborted || Date.now() >= deadline);
  const vm = runtime.newContext();
  const controller = new AbortController();
  const calls = new Set<Promise<void>>();
  const deferred: QuickJSDeferredPromise[] = [];
  const allowed = new Set(names);
  const output: string[] = [];
  let bytes = 0;
  let alive = true;
  let fail: (reason: unknown) => void = () => {};
  const stopped = new Promise<never>((_, reject) => { fail = reject; });
  // Keep a handler attached while synchronous evaluation is still running.
  void stopped.catch(() => {});
  const abort = () => { controller.abort(signal.reason); fail(signal.reason); };
  signal.addEventListener('abort', abort, { once: true });
  const timer = setTimeout(() => {
    const error = new Error('JavaScript execution time limit exceeded');
    controller.abort(error);
    fail(error);
  }, limits.milliseconds);
  function pump() {
    if (!alive) return;
    const result = runtime.executePendingJobs();
    if (result.error) {
      const error = vm.dump(result.error);
      result.error.dispose();
      fail(new Error(JSON.stringify(error)));
    }
  }
  vm.newFunction('__emit', value => {
    const text = vm.getString(value);
    bytes += Buffer.byteLength(text) + 1;
    if (bytes > limits.output) throw new Error('JavaScript output limit exceeded; emit a smaller result');
    output.push(text);
  }).consume(handle => vm.setProp(vm.global, '__emit', handle));
  vm.newFunction('__call', (nameHandle, argsHandle) => {
    const name = vm.getString(nameHandle);
    if (!allowed.has(name)) throw new Error(`Unknown tool: ${name}`);
    const args = JSON.parse(vm.getString(argsHandle));
    const promise = vm.newPromise();
    deferred.push(promise);
    const pending = Promise.resolve().then(() => invoke(name, args, controller.signal)).then(
      result => {
        if (alive) vm.newString(JSON.stringify(result)).consume(value => promise.resolve(value));
      },
      error => {
        if (alive) vm.newError(String(error.message ?? error)).consume(value => promise.reject(value));
      },
    ).then(pump).catch(fail);
    calls.add(pending);
    void pending.finally(() => calls.delete(pending));
    return promise.handle;
  }).consume(handle => vm.setProp(vm.global, '__call', handle));
  try {
    vm.unwrapResult(vm.evalCode(`
      globalThis.tools = Object.freeze(Object.fromEntries(${JSON.stringify(names)}.map(name =>
        [name, async args => JSON.parse(await __call(name, JSON.stringify(args ?? {})))])));
      globalThis.text = value => __emit(typeof value === 'string' ? value : JSON.stringify(value) ?? 'undefined');
    `)).dispose();
    const handle = vm.unwrapResult(vm.evalCode(`(async () => {\n${code}\n})()`));
    try {
      const completion = vm.resolvePromise(handle);
      pump();
      const result = await Promise.race([completion, stopped]);
      vm.unwrapResult(result).dispose();
      signal.throwIfAborted();
      return output.join('\n');
    } finally {
      handle.dispose();
    }
  } finally {
    alive = false;
    controller.abort(new Error('JavaScript invocation finished'));
    clearTimeout(timer);
    signal.removeEventListener('abort', abort);
    await Promise.allSettled(calls);
    for (const promise of deferred) promise.dispose();
    vm.dispose();
    runtime.dispose();
  }
}
