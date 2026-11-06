// Keyboard shortcuts of the timeline page (ui-spec §4.7).

export type Command = 'toggle' | 'back10' | 'fwd10' | 'prev' | 'next' | 'zoomIn' | 'zoomOut' | 'now';

/** The slice of KeyboardEvent the mapping reads. */
export interface KeyLike {
  key: string;
  ctrlKey: boolean;
  metaKey: boolean;
  altKey: boolean;
  defaultPrevented: boolean;
  target: EventTarget | null;
}

const TEXT_FIELDS = new Set(['INPUT', 'TEXTAREA', 'SELECT']);
// Space activates these when they have focus.
const ACTIVATED_BY_SPACE = new Set(['BUTTON', 'A', 'SUMMARY']);

/**
 * The command a key press stands for, or null when it is not ours: another
 * handler used it, a modifier is held, the user is typing, or a focused
 * control would use Space itself.
 */
export function commandFor(e: KeyLike): Command | null {
  if (e.defaultPrevented || e.ctrlKey || e.metaKey || e.altKey) return null;
  const t = e.target as { tagName?: string; isContentEditable?: boolean } | null;
  const tag = t?.tagName?.toUpperCase() ?? '';
  if (TEXT_FIELDS.has(tag) || t?.isContentEditable) return null;
  switch (e.key) {
    case ' ':
      return ACTIVATED_BY_SPACE.has(tag) ? null : 'toggle';
    case 'ArrowLeft':
      return 'back10';
    case 'ArrowRight':
      return 'fwd10';
    case 'j':
    case 'J':
      return 'prev';
    case 'k':
    case 'K':
      return 'next';
    case '[':
      return 'zoomOut';
    case ']':
      return 'zoomIn';
    case 't':
    case 'T':
      return 'now';
    default:
      return null;
  }
}

/**
 * The clip before (`J`) or after (`K`) the current one in time, whatever the
 * order of `clips`. Stays on the first or last clip instead of wrapping; with
 * no usable selection it starts at the newest clip.
 */
export function adjacentClip(clips: readonly { id: string; startMs: number }[], currentId: string | null, dir: 'prev' | 'next'): string | null {
  if (clips.length === 0) return null;
  const sorted = [...clips].sort((a, b) => a.startMs - b.startMs);
  const i = currentId === null ? -1 : sorted.findIndex((c) => c.id === currentId);
  if (i < 0) return sorted[sorted.length - 1]!.id;
  const j = Math.min(Math.max(i + (dir === 'next' ? 1 : -1), 0), sorted.length - 1);
  return sorted[j]!.id;
}
