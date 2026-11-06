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

## Task 19 — `s3` recoverer

**Automated in this task:** `pkg/storage/s3/recover.go` (`ListIncomplete`, `RecoverCommit`,
`AbortIncomplete`). Against MinIO in Docker (`storagetest.MinIOImage`, `make integration`):
`TestS3Recoverer` (the `storagetest.RunRecoverer` suite, plus a crash after 2 full 5 MiB parts and a
1 MiB+123 partial buffer recovering exactly the first 10 MiB), `TestRecoverZeroPartsAborts`,
`TestListIncompleteRespectsAge` (`olderThan` 0 vs 1h) and `TestAbortIncompleteRemovesUpload` all
pass. Unit tests against an in-process fake (`make test`) cover paginated ListMultipartUploads and
ListParts, element-wise prefix and root filtering, the Initiated age filter with server clock skew,
gapped part numbers, a lost CompleteMultipartUpload response followed by a retried `RecoverCommit`,
a gone upload, zero parts and malformed candidates. A probe against the pinned MinIO showed that
MinIO's ListMultipartUploads honours only an empty prefix or an exact object name (a directory
prefix such as `<root>/clips/cam/` lists nothing), so the driver lists the bucket's uploads with no
prefix and filters client-side. The Synology MinIO was not reachable.

**Manual steps:**
1. On a host on the tailnet, set the relay's S3 credentials without echoing them:
   `read -s AWS_ACCESS_KEY_ID; read -s AWS_SECRET_ACCESS_KEY; export AWS_ACCESS_KEY_ID AWS_SECRET_ACCESS_KEY`,
   and set `EP=https://<synology-minio-host>:<port>`, `B=<bucket>`, `R=<root>` (the values from
   `zrelay.yaml`).
2. Start an upload with the relay credentials:
   `docker run --rm -e AWS_ACCESS_KEY_ID -e AWS_SECRET_ACCESS_KEY amazon/aws-cli:2.22.0 --endpoint-url "$EP" --region us-east-1 s3api create-multipart-upload --bucket "$B" --key "$R/clips/livecheck/2026/11/06/000000000_01JC0000000000000000000000.zrec"`
   and note the `UploadId`.
3. List with no prefix (what the driver sends):
   `docker run --rm -e AWS_ACCESS_KEY_ID -e AWS_SECRET_ACCESS_KEY amazon/aws-cli:2.22.0 --endpoint-url "$EP" --region us-east-1 s3api list-multipart-uploads --bucket "$B"`
4. List with a directory prefix, to record the server's prefix behaviour:
   the same command with `--prefix "$R/clips/livecheck/"`.
5. List the (empty) parts:
   `... s3api list-parts --bucket "$B" --key "$R/clips/livecheck/2026/11/06/000000000_01JC0000000000000000000000.zrec" --upload-id <UploadId>`
6. Abort it:
   `... s3api abort-multipart-upload --bucket "$B" --key "$R/clips/livecheck/2026/11/06/000000000_01JC0000000000000000000000.zrec" --upload-id <UploadId>`,
   then repeat step 3. Finally `unset AWS_ACCESS_KEY_ID AWS_SECRET_ACCESS_KEY`.

**Expected result:** steps 2, 3, 5 and 6 succeed under the relay policy (design §6.5); step 3 lists
the upload with an `Initiated` time in UTC within a few seconds of the host clock; step 5 returns
no parts; after step 6, step 3 no longer lists it. Step 4 may list nothing (MinIO's exact-name
prefix behaviour); that is expected and does not matter to the driver.

**If the result differs, update:**
- Step 3 denied (AccessDenied) while step 4 is allowed: the relay MinIO policy restricts
  `s3:ListBucketMultipartUploads` by an `s3:prefix` condition. Grant it on the bucket without a
  prefix condition in the runbook policy (design §6.5), or, if the server matches byte prefixes,
  send `Prefix: <root>/` in `ListIncomplete` (`pkg/storage/s3/recover.go`) and change the fake's
  prefix rule in `fakeRecoverS3.listUploads` (`pkg/storage/s3/recover_test.go`) to match.
- `Initiated` far from the host clock: the age filter in `ListIncomplete` trusts the server clock
  against `time.Now()`; fix NTP on the NAS, or widen the 2-minute candidate age in the caller
  (Contract §7.4, design §6.5).
- Step 5 or 6 denied: add `s3:ListMultipartUploadParts` / `s3:AbortMultipartUpload` to the relay
  policy (design §6.5).

**Status:** not run
