import { build } from 'esbuild';
import { readFile } from 'node:fs/promises';

const check = process.argv.includes('--check');
const outfile = 'internal/agent/dist/runtime.mjs';
const result = await build({
  entryPoints: ['runtime/main.ts'],
  bundle: true,
  platform: 'node',
  target: 'node22',
  format: 'esm',
  minify: true,
  external: ['bufferutil', 'utf-8-validate'],
  banner: { js: 'import {createRequire} from "node:module"; const require=createRequire(import.meta.url);' },
  outfile,
  write: !check,
});

if (check && !Buffer.from(result.outputFiles[0].contents).equals(await readFile(outfile))) {
  throw new Error(`Stale ${outfile}; run npm run build and include the generated bundle with the source changes`);
}
