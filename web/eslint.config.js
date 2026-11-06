import js from '@eslint/js';
import svelte from 'eslint-plugin-svelte';
import globals from 'globals';
import svelteParser from 'svelte-eslint-parser';
import ts from 'typescript-eslint';

// Markup injection is banned outright: an XSS in the viewer could capture the
// master key (Contract D8). `{@html}` and the DOM sinks below are errors.
const htmlSinks = [
  {
    selector: "AssignmentExpression[left.type='MemberExpression'][left.property.name=/^(innerHTML|outerHTML)$/]",
    message: 'Assigning innerHTML/outerHTML is banned; use textContent or DOM APIs.',
  },
  {
    selector:
      "AssignmentExpression[left.type='MemberExpression'][left.computed=true][left.property.value=/^(innerHTML|outerHTML)$/]",
    message: 'Assigning innerHTML/outerHTML is banned; use textContent or DOM APIs.',
  },
  {
    selector: "CallExpression[callee.property.name='insertAdjacentHTML']",
    message: 'insertAdjacentHTML is banned; use DOM APIs.',
  },
  {
    selector: "CallExpression[callee.property.name='write'][callee.object.name='document']",
    message: 'document.write is banned.',
  },
];

export default ts.config(
  { ignores: ['dist/', 'node_modules/', 'test-results/', 'playwright-report/'] },
  js.configs.recommended,
  ...ts.configs.recommended,
  ...svelte.configs['flat/recommended'],
  {
    languageOptions: { globals: { ...globals.browser, ...globals.node } },
    rules: {
      'svelte/no-at-html-tags': 'error',
      'no-restricted-syntax': ['error', ...htmlSinks],
    },
  },
  {
    files: ['**/*.svelte'],
    languageOptions: { parser: svelteParser, parserOptions: { parser: ts.parser } },
  },
);
