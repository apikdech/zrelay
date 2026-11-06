// Test page for e2e/vectors.spec.ts: runs the shared vector loop in the
// browser and publishes the results on window.__vectorResults.
import manifest from '../../../testdata/zrec/v1/vectors.json';
import { runAll, type CaseResult, type Manifest } from '../../test-support/vectors-runner';

declare global {
  interface Window {
    __vectorResults?: CaseResult[];
  }
}

const urls = import.meta.glob<string>('../../../testdata/zrec/v1/*.zrec', {
  query: '?url',
  import: 'default',
  eager: true,
});

async function load(file: string): Promise<Uint8Array> {
  const key = Object.keys(urls).find((k) => k.endsWith('/' + file));
  if (!key) throw new Error(`vector file not found: ${file}`);
  const res = await fetch(urls[key]!);
  if (!res.ok) throw new Error(`fetch ${file}: ${res.status}`);
  return new Uint8Array(await res.arrayBuffer());
}

const results = await runAll(manifest as Manifest, load);
window.__vectorResults = results;
document.getElementById('out')!.textContent = results
  .map((r) => `${r.ok ? 'ok  ' : 'FAIL'} ${r.name} ${r.detail}`)
  .join('\n');
