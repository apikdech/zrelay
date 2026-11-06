#!/usr/bin/env bash
# Regenerates the synthetic video fixtures in this directory.
#
# ffmpeg runs only from a pinned Docker image, never from the host.
# Both streams are 640x360 at 15 fps with a closed GOP of 30 frames (an IDR
# every 2 s), no scene-cut keyframes and no B-frames (cameras such as the Tapo
# C220 send none, and the test RTSP server stamps RTP times in decode order).
#
# Usage: testdata/video/gen.sh   (from any directory)
set -euo pipefail

# version-7.1-cli was tried first: its libx265 segfaults (exit 139) on arm64
# hosts (Apple silicon) and under amd64 emulation, so 9.0.2 is pinned instead.
FFMPEG_IMAGE="linuxserver/ffmpeg:version-9.0.2-cli@sha256:822d04f0b2d0c4847430f5cc90f20ea48be866bb502ece63fdeb729400bf5361"

cd "$(dirname "$0")"

docker run --rm -v "$PWD":/out "$FFMPEG_IMAGE" -hide_banner -loglevel error -y \
  -f lavfi -i testsrc2=size=640x360:rate=15 -t 10 \
  -c:v libx264 -profile:v high -pix_fmt yuv420p -bf 0 \
  -g 30 -keyint_min 30 -sc_threshold 0 \
  -bsf:v h264_mp4toannexb /out/h264-640x360-gop2-10s.h264

docker run --rm -v "$PWD":/out "$FFMPEG_IMAGE" -hide_banner -loglevel error -y \
  -f lavfi -i testsrc2=size=640x360:rate=15 -t 5 \
  -c:v libx265 -pix_fmt yuv420p -bf 0 \
  -x265-params keyint=30:min-keyint=30:scenecut=0:bframes=0:open-gop=0:log-level=error \
  -bsf:v hevc_mp4toannexb /out/h265-640x360-gop2-5s.h265

ls -l h264-640x360-gop2-10s.h264 h265-640x360-gop2-5s.h265
