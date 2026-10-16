import { build } from 'esbuild';

await build({
  entryPoints: ['runtime/main.ts'],
  bundle: true,
  platform: 'node',
  target: 'node22',
  format: 'esm',
  minify: true,
  external: ['bufferutil', 'utf-8-validate'],
  banner: { js: 'import {createRequire} from "node:module"; const require=createRequire(import.meta.url);' },
  outfile: 'internal/agent/dist/runtime.mjs',
});
