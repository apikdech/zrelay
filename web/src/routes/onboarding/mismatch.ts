// "This key doesn't match recent recordings" (ui-spec §4.1): compares the
// imported key's id with the key_id of the latest clips.

/** How many of the latest clips are compared. */
export const RECENT_CLIPS = 20;

export interface ClipKeyRef {
  start_time: string;
  video: { key_id: string };
}

export interface Mismatch {
  keyId: string;
  /** Distinct key_ids found on the latest clips, newest first. */
  clipKeyIds: string[];
}

/** null when there is nothing to compare or the key matches at least one recent clip. */
export function keyMismatch(clips: readonly ClipKeyRef[], keyId: string): Mismatch | null {
  const latest = [...clips].sort((a, b) => Date.parse(b.start_time) - Date.parse(a.start_time)).slice(0, RECENT_CLIPS);
  if (latest.length === 0) return null;
  const ids = [...new Set(latest.map((c) => c.video.key_id))];
  if (ids.includes(keyId)) return null;
  return { keyId, clipKeyIds: ids };
}
