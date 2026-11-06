import { describe, expect, it } from 'vitest';
import { isDataPath, shellAssets, shellVersion } from './shell';

describe('shellAssets', () => {
  it('keeps the content-hashed build output and nothing else', () => {
    const files = [
      'index.html',
      'sw.js',
      'manifest.webmanifest',
      'icons/icon-192.png',
      'assets/index-Bx3k9QaZ.js',
      'assets/index-Dq1_a-Zx.css',
      'assets/worker-Cc82LmPq.js',
      'assets/index-Bx3k9QaZ.js.map',
      'assets/unhashed.js',
      'assets/sub/x-Bx3k9QaZ.js',
    ];
    expect(shellAssets(files)).toEqual(['/assets/index-Bx3k9QaZ.js', '/assets/index-Dq1_a-Zx.css', '/assets/worker-Cc82LmPq.js']);
  });

  it('never lists a data path, whatever the file is called', () => {
    expect(shellAssets(['api/v1/clips-Bx3k9QaZ.json', 'live/front-Bx3k9QaZ.js'])).toEqual([]);
  });
});

describe('shellVersion', () => {
  it('names a cache by the exact shell it holds', () => {
    const a = shellVersion(['/assets/a-Bx3k9QaZ.js']);
    expect(a).toMatch(/^[0-9a-f]{8,}$/);
    expect(shellVersion(['/assets/a-Bx3k9QaZ.js'])).toBe(a);
    expect(shellVersion(['/assets/a-Bx3k9QaZ.js', '/assets/b-Bx3k9QaZ.js'])).not.toBe(a);
    // The order a bundler lists files in does not matter.
    expect(shellVersion(['/assets/b-Bx3k9QaZ.js', '/assets/a-Bx3k9QaZ.js'])).toBe(
      shellVersion(['/assets/a-Bx3k9QaZ.js', '/assets/b-Bx3k9QaZ.js']),
    );
  });
});

describe('isDataPath', () => {
  it.each(['/api', '/api/', '/api/v1/clips', '/live', '/live/front-door', '/live/front-door/sessions/1'])('%s is data', (p) => {
    expect(isDataPath(p)).toBe(true);
  });
  it.each(['/', '/assets/a-Bx3k9QaZ.js', '/apiary.js', '/liveness', '/sw.js'])('%s is not', (p) => {
    expect(isDataPath(p)).toBe(false);
  });
});
