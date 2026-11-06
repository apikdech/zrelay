import { ESLint } from 'eslint';
import { describe, expect, it } from 'vitest';

// The SPA must never inject markup: an XSS in the viewer could capture the
// master key (Contract D8). These tests pin the lint rules that ban it.
const eslint = new ESLint({ overrideConfigFile: new URL('./eslint.config.js', import.meta.url).pathname });

async function ruleIds(code: string, filePath: string): Promise<string[]> {
  const [res] = await eslint.lintText(code, { filePath: new URL(filePath, import.meta.url).pathname });
  return (res?.messages ?? []).map((m) => m.ruleId ?? 'parse-error');
}

describe('markup injection is banned', () => {
  it('rejects {@html} in Svelte templates', async () => {
    const ids = await ruleIds('<script lang="ts">let x = "a";</script>\n{@html x}\n', 'src/Bad.svelte');
    expect(ids).toContain('svelte/no-at-html-tags');
  });

  it.each([
    ['innerHTML assignment', 'document.body.innerHTML = "<b>x</b>";'],
    ['outerHTML assignment', 'document.body.outerHTML = "<b>x</b>";'],
    ['bracketed innerHTML', 'document.body["innerHTML"] = "x";'],
    ['insertAdjacentHTML', 'document.body.insertAdjacentHTML("beforeend", "x");'],
  ])('rejects %s in TypeScript', async (_name, code) => {
    const ids = await ruleIds(code + '\n', 'src/bad.ts');
    expect(ids).toContain('no-restricted-syntax');
  });

  it('rejects innerHTML in Svelte script blocks', async () => {
    const ids = await ruleIds('<script lang="ts">document.body.innerHTML = "x";</script>\n', 'src/Bad2.svelte');
    expect(ids).toContain('no-restricted-syntax');
  });

  it('accepts textContent', async () => {
    const ids = await ruleIds('document.body.textContent = "x";\n', 'src/ok.ts');
    expect(ids).toEqual([]);
  });
});
