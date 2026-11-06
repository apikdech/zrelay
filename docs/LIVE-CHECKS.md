# Manual live checks

Steps that agents could not run: they need the real Tapo C220, the Synology MinIO, phones,
a 72-hour soak, or a deployed URL. Each entry lists the task, exact commands, the expected
result, and what to update if the result differs. Nothing here has been run yet.

## Task 6 — Access units and the RTSP session

**Automated in this task:** `internal/relay/camera` (`RunRTSP`, access-unit assembly, `Redact`) is
tested against the in-process test RTSP server (`testutil.StartRTSPServer`) with the synthetic
H.264 and H.265 fixtures: first AU is IDR, ≥ 140 AUs over the 10 s fixture with strictly increasing
DTS, `ParamGen` bump and a second `Codec` call on an SPS change, monotonic DTS across an RTP
timestamp wrap (`StartRTPTimestamp = 0xFFFF0000`), `time_source` `rtcp` with sender reports and
`arrival` without, credential redaction, H.265, PCMA audio, AUs before the first IDR dropped, and
return on session loss. All pass under `make vet test`. An opt-in live test,
`TestLiveCameraSession`, was added; it skips unless `ZRELAY_LIVE_RTSP_URL` is set and was not run
against a camera.

**Manual steps:**
1. On a host on the camera's network, from the repository root, enter the camera credentials
   without echoing them: `read -s ZRELAY_LIVE_RTSP_URL` and type
   `rtsp://<user>:<password>@<tapo-ip>:554/stream1`, then `export ZRELAY_LIVE_RTSP_URL`.
2. Run: `ZRELAY_LIVE_RTSP_SECONDS=60 go test -race -count=1 -run TestLiveCameraSession -v ./internal/relay/camera/`
3. Repeat step 2 with `stream2` (sub-stream) in the URL.
4. Run once more with a wrong password and confirm the failure message shows no password or
   `user:password` text.
5. `unset ZRELAY_LIVE_RTSP_URL`.

**Expected result:** the test passes; the log shows exactly one `Codec` line with `kind=h264`
(or `h265` if the camera is set to H.265), the configured resolution (e.g. 2560x1440 for
`stream1` on a C220), and `time_source=rtcp` if the camera sends RTCP sender reports; AU count
about fps × 60; max GOP at or below the camera's I-frame interval; wall drift vs media within
±1 s; audio codec `PCMA` (or `PCMU`) with a non-zero audio packet count when the camera's
microphone is on. Step 4 fails with a message containing `***` or no credentials at all.

**If the result differs, update:**
- `time_source=arrival` on a camera that should send RTCP, or a 2 s delay before the first
  video: `timeSourceWait` in `internal/relay/camera/au.go` (raise it if the first sender report
  arrives later than 2 s of media time) and `TestWallClockUsesRTCPWhenPresent`.
- More than one `Codec` line without a real parameter-set change (e.g. the camera rotates SEI or
  re-sends a byte-different but equivalent SPS): `assembler.inBandParams` in `au.go`, design
  §4.1 (parameter-set generation) and `TestParamGenBumpsOnSPSChange`.
- No audio packets with the microphone on: `setupAudio` in `internal/relay/camera/rtsp.go`
  (the camera may advertise a sample rate or channel count other than 8 kHz mono).
- Session errors or timeouts: `rtspTimeout` in `rtsp.go` and design §4.1.
- RTP wrap (every ~13.25 h at 90 kHz) is only exercised by the long soak; a DTS discontinuity
  there points at `unwrapper` in `au.go` and `TestAccessUnitsMonotonicAcrossRTPWrap`.

**Status:** not run

## Task 6 (review fixes) — Access units and the RTSP session

**Automated in this task:** Fixes after review, all under `make vet test`:
`TestVideoPathStampsAccessUnits` packetizes both fixtures with gortsplib's `rtph264`/`rtph265`
encoders (payload limits 1 MiB, 4000 and 1450 bytes, so every AU fits in one RTP packet, some
do, or none do; plus H.264 with the marker bit cleared) and feeds them through the same
depacketize-and-assemble path as `RunRTSP`; every AU arrives with PTS = DTS = index × 6000 and
the arrival-mode Wall. Before the fix the single-packet case delivered 121 of 150 AUs and the
4000-byte case 90 of 150. `TestCodecAnnouncedWhenChangingAUIsDropped` (a parameter-set change on
a dropped AU is still announced by `Codec` before the first AU of the new `ParamGen`),
`TestWallClockRTCPNeverStepsBack` (Wall never steps back when the sender-report mapping does),
`TestRedactUsernameOnly`, `TestRedactKeepsErrorsIs`, `TestRunRTSPInvalidURL`, `TestSelectVideo`
(errNoVideo) and `TestSelectAudio` (PCMU) pass. None of this ran against a camera.

**Manual steps:**
1. On a host on the camera's network, from the repository root: `read -s ZRELAY_LIVE_RTSP_URL`,
   type `rtsp://<user>:<password>@<tapo-ip>:554/stream2` (the sub-stream, whose small P-frames
   fit in one RTP packet), then `export ZRELAY_LIVE_RTSP_URL`.
2. Point the camera at a static scene (a wall), so P-frames are small.
3. Run: `ZRELAY_LIVE_RTSP_SECONDS=120 go test -race -count=1 -run TestLiveCameraSession -v ./internal/relay/camera/`
4. Repeat step 3 with `stream1`.
5. `unset ZRELAY_LIVE_RTSP_URL`.

**Expected result:** for both streams the AU count is within 2 % of fps × 120 (no GOPs lost to
resynchronisation), exactly one `Codec` line, and the logged wall drift vs media stays within
±1 s.

**If the result differs, update:**
- AU count well below fps × 120 with single-packet AUs: `h264Depacketizer` /
  `markerDepacketizer` in `internal/relay/camera/rtsp.go` (the gortsplib decoder's timestamp
  behaviour may differ from the v5.6.7 model they mirror) and `TestVideoPathStampsAccessUnits`.
- Wall drifting ahead of the camera clock after an NTP step on the camera: the Wall floor in
  `assembler.emit` (`internal/relay/camera/au.go`) and `TestWallClockRTCPNeverStepsBack`.

**Status:** not run

## Task 8 — ONVIF events — rules, tracker, PullPoint client

**Automated in this task:** the Tapo rules, the trigger tracker and the PullPoint client run
against `testutil.StartFakeONVIF` (an in-process camera on ephemeral 127.0.0.1 ports) with the
fake clock: `TestTapoRules`, `TestInitializedActiveSetsStateWithoutNewChange`,
`TestPullTransportErrorRetriesSameSubscription` (5 dropped pulls, 5 `PullRetry`, one
subscription), `TestRenewEvery60s`, `TestFailedRenewResubscribesAfterUnsubscribe`,
`TestResubscribeBackoff` (5, 10, 20, 40, 60, 60 s), `TestNon200PullIsFailure`,
`TestSubscriptionAddressHostRewritten`, `TestRedirectRefused`, `TestDigestUsesCameraClockOffset`,
`TestClockResyncEvery30Minutes`, `TestConnectionCloseHeader`,
`TestReadyFalseAndKindsResetWhenSubscriptionEnds`, `TestUnmatchedTopicCounted` and
`TestResponseCappedAt1MiB` pass with `-race`. The fake mimics Tapo's dropped `PullMessages`
connections, but none of this ran against a real camera.
`TestLivePullPoint` is skipped unless `ZRELAY_LIVE_ONVIF_HOST` is set.

**Manual steps:**
1. On a host on the camera's network, from the repository root:
   `export ZRELAY_LIVE_ONVIF_HOST=<tapo-ip> ZRELAY_LIVE_ONVIF_USER=<camera-account>` and
   `read -s ZRELAY_LIVE_ONVIF_PASSWORD; export ZRELAY_LIVE_ONVIF_PASSWORD`.
2. Run: `ZRELAY_LIVE_ONVIF_SECONDS=150 go test -race -count=1 -run TestLivePullPoint -v ./internal/relay/events/`
3. During the 150 s, walk in front of the C220 (person), then stay out of view; if you can,
   drive a car or pet through the scene, and trigger a Tapo smart event such as baby cry.
4. Read the `signal`, `ready`, `pull retries`, `clock skew` and `unmatched` log lines.
5. `unset ZRELAY_LIVE_ONVIF_HOST ZRELAY_LIVE_ONVIF_USER ZRELAY_LIVE_ONVIF_PASSWORD`.

**Expected result:** `ready=true` once and `ready=false` only at shutdown (Renew every 60 s kept
one subscription alive for 150 s, so the C220 supports Renew); `signal kind=motion active=true`
then `active=false` when the scene settles; `kind=person` for the walk-through; a non-zero
`pull retries` count (Tapo drops idle pulls) with no resubscribe; `clock skew` small; `unmatched`
lists only topics the Tapo rules do not cover.

**If the result differs, update:**
- `ready=false` before shutdown, or a failed Renew: `renewEvery` and `renewBody` in
  `internal/relay/events/onvif.go`, and design §4.3 / relay-spec §4.4 (Renew-as-health).
- Wrong or missing kinds, or `IsVehicle` / the catch-all not seen: `TapoRules()` in
  `internal/relay/events/rules.go`, `TestTapoRules`, and the design §4.3 built-in table.
- Authentication failures: `buildEnvelope` in `internal/relay/events/soap.go` and
  `TestDigestUsesCameraClockOffset`.
- Unexpected topics in `unmatched`: add rules via `TapoRules()` and the table in design §4.3.

**Status:** not run
