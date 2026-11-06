// User-facing wording for the integrity states (ui-spec §4.4).
import type { PlayerState } from './engine';

export function initialState(): PlayerState {
  return {
    clipId: null,
    integrity: 'verifying',
    engine: null,
    failedAtMs: null,
    verification: null,
    header: null,
    keyId: null,
    message: null,
  };
}

/** `mm:ss`, rounded to the nearest second. */
export function formatMmss(ms: number): string {
  const total = Math.round(ms / 1000);
  const m = Math.floor(total / 60);
  const s = total % 60;
  return `${String(m).padStart(2, '0')}:${String(s).padStart(2, '0')}`;
}

const utc = (ms: number): string => new Date(ms).toISOString().slice(0, 19).replace('T', ' ') + ' UTC';

export interface IntegrityView {
  kind: PlayerState['integrity'];
  /** Short text for the badge or banner. */
  text: string;
  /** Tooltip. */
  title: string;
  spinner: boolean;
  /** Whether exporting the recording may be offered. */
  export: boolean;
}

export function describeIntegrity(s: PlayerState): IntegrityView {
  switch (s.integrity) {
    case 'verifying':
      // The clip could not be opened (network, no key): there is nothing left to wait for.
      if (s.message !== null) {
        return { kind: 'verifying', text: 'Could not verify this recording', title: 'Not verified', spinner: false, export: false };
      }
      return { kind: 'verifying', text: 'Verifying…', title: 'Verifying the encrypted recording', spinner: true, export: false };
    case 'verified':
      return { kind: 'verified', text: 'Verified', title: 'Encrypted recording verified', spinner: false, export: true };
    case 'mismatch': {
      const h = s.header;
      const shown = h ? ` Showing the encrypted header's values: camera ${h.cameraId}, started ${utc(h.startUnixMs)}.` : '';
      return {
        kind: 'mismatch',
        text: `Metadata mismatch: the unencrypted description of this clip disagrees with its encrypted header.${shown}`,
        title: 'Metadata mismatch',
        spinner: false,
        export: true,
      };
    }
    case 'tampered': {
      const at = s.failedAtMs === null ? '' : ` at ~${formatMmss(s.failedAtMs)}`;
      return {
        kind: 'tampered',
        text: `This recording failed verification${at}`,
        title: 'Tampered or corrupted',
        spinner: false,
        export: false,
      };
    }
    case 'incomplete':
      return {
        kind: 'incomplete',
        text: 'Recording was interrupted',
        title: 'Incomplete: only the verified portion plays',
        spinner: false,
        export: true,
      };
    case 'unknown-key':
      return {
        kind: 'unknown-key',
        text: `Recorded with key ${s.keyId ?? '(unknown)'}; add that key to play`,
        title: 'Unknown key',
        spinner: false,
        export: false,
      };
  }
}
