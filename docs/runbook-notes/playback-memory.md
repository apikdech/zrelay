# Playback memory notes

Peak JS heap and buffered bytes of the playback engine (`web/src/lib/player`) while playing and
seeking a 300 s live clip, per profile. Targets are from ui-spec §4.3.2: mobile at most 32 MiB of
plaintext in memory at 3 Mbps, desktop at most 64 MiB.

**Nothing in this file has been measured yet.** The procedure is in `docs/LIVE-CHECKS.md`, "Task 36
— Playback engine and demo page". Fill the table from that run; do not estimate.

| Browser | Profile | Engine | Peak JS heap | Peak buffered (s) | Peak buffered (MiB at the clip's bitrate) | Seek to 150 s (ms) | Notes |
|---|---|---|---|---|---|---|---|
| Chrome desktop | desktop | mse | not measured | not measured | not measured | not measured | |
| Safari macOS | desktop | mms or mse | not measured | not measured | not measured | not measured | |
| iPhone Safari | mobile | mms | not measured | not measured | not measured | not measured | |

How the numbers are read:

* Peak JS heap: Chrome DevTools Memory tab ("JS heap" in the Performance monitor), or Safari Web
  Inspector Timelines > JavaScript & Events. The plaintext chunks live in the SourceBuffer, not in
  the JS heap, so also record the buffered span.
* Buffered span and bytes: in the console on the demo page,
  `const v = document.querySelector('[data-testid=player]'); [...Array(v.buffered.length).keys()].map(i => [v.buffered.start(i), v.buffered.end(i)])`;
  multiply the span by the clip's bitrate (`size_bytes * 8 / duration_s`) for bytes.
