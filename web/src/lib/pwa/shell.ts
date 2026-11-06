// What the service worker precaches (ui-spec §4.9): the content-hashed app
// shell, and nothing that carries data. Shared by the build plugin and the
// worker so both agree on what the shell is.

/** `assets/<name>-<8-char hash>.<ext>`: Vite's content-hashed build output. */
const HASHED = /^assets\/[^/]+-[A-Za-z0-9_-]{8}\.[A-Za-z0-9]+$/;

/** Request paths that carry data. The worker never intercepts them (design §7.2). */
const DATA = /^\/(api|live)(\/|$)/;

export function isDataPath(pathname: string): boolean {
  return DATA.test(pathname);
}

/** The root-relative URLs of the hashed files among a build's output files (paths relative to the output dir). */
export function shellAssets(files: readonly string[]): string[] {
  return files.filter((f) => HASHED.test(f) && !f.endsWith('.map')).map((f) => '/' + f);
}

/** A short digest of the shell list, order-independent: two builds share a cache name only if they ship the same files. */
export function shellVersion(urls: readonly string[]): string {
  const text = [...urls].sort().join('\n');
  // Two FNV-1a 32-bit passes with different offset bases.
  const pass = (seed: number): string => {
    let h = seed >>> 0;
    for (let i = 0; i < text.length; i++) {
      h ^= text.charCodeAt(i);
      h = Math.imul(h, 0x01000193) >>> 0;
    }
    return h.toString(16).padStart(8, '0');
  };
  return pass(0x811c9dc5) + pass(0x9747b28c);
}
