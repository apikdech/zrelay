<script lang="ts">
  import { onMount } from 'svelte';
  import { createDecoder, openCamera, scanFrames, type Camera, type Decoder } from '../../lib/qr/scan';

  interface Props {
    open?: () => Promise<Camera>;
    decode?: Decoder;
    wait?: (ms: number) => Promise<void>;
    onDecoded: (text: string) => void;
    onClose: () => void;
  }

  let {
    open = openCamera,
    decode = createDecoder(),
    wait = (ms) => new Promise<void>((r) => setTimeout(r, ms)),
    onDecoded,
    onClose,
  }: Props = $props();

  let video: HTMLVideoElement | undefined = $state();
  let error = $state('');

  onMount(() => {
    const ac = new AbortController();
    let camera: Camera | undefined;
    let gone = false;

    (async () => {
      try {
        camera = await open();
        if (gone) return;
        camera.video.className = 'w-full rounded';
        video = camera.video;
        const text = await scanFrames({ grab: () => camera!.grab(), decode, wait, signal: ac.signal });
        onDecoded(text);
      } catch (e) {
        if ((e as Error).name !== 'AbortError') {
          // Only the error name: browsers' messages are not user-facing text.
          error = (e as Error).name === 'NotAllowedError' ? 'Camera permission was denied.' : 'The camera could not be opened.';
        }
      } finally {
        camera?.stop();
      }
    })();

    return () => {
      gone = true;
      ac.abort();
      camera?.stop();
    };
  });
</script>

<section aria-label="QR scanner" class="space-y-2">
  {#if video}
    <!-- The camera's own <video> element is placed here as-is. -->
    <div
      {@attach (host) => {
        host.append(video!);
      }}
    ></div>
  {/if}
  {#if error}
    <p role="alert" class="text-sm text-amber-400">{error} Paste the key instead.</p>
  {/if}
  <button type="button" class="rounded bg-neutral-800 px-3 py-1 text-sm" onclick={onClose}>Close scanner</button>
</section>
