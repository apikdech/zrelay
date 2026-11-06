#!/usr/bin/env python3
"""Regenerates web/static/icons/*.png (stdlib only): a camera lens on the app's dark background.

    python3 web/scripts/gen-icons.py

The icons are committed; this script is only for changing the artwork.
"""
import math
import os
import struct
import zlib

OUT = os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "static", "icons")
BG = (10, 10, 10)
RING = (56, 189, 248)  # sky-400
LENS = (23, 37, 50)
DOT = (248, 250, 252)
SS = 3  # supersampling per axis


def pixel(x, y, size, scale):
    """Colour at (x, y) in the unit square; `scale` shrinks the artwork for the maskable safe zone."""
    c = size / 2
    d = math.hypot(x - c, y - c) / (size / 2) / scale
    if d <= 0.16:
        return DOT
    if d <= 0.50:
        return LENS
    if d <= 0.66:
        return RING
    if d <= 0.78:
        return LENS if d < 0.72 else BG
    return BG


def png(size, scale):
    rows = []
    for py in range(size):
        row = bytearray([0])
        for px in range(size):
            r = g = b = 0
            for sy in range(SS):
                for sx in range(SS):
                    cr, cg, cb = pixel(px + (sx + 0.5) / SS, py + (sy + 0.5) / SS, size, scale)
                    r, g, b = r + cr, g + cg, b + cb
            n = SS * SS
            row += bytes((r // n, g // n, b // n))
        rows.append(bytes(row))

    def chunk(tag, data):
        body = tag + data
        return struct.pack(">I", len(data)) + body + struct.pack(">I", zlib.crc32(body) & 0xFFFFFFFF)

    ihdr = struct.pack(">IIBBBBB", size, size, 8, 2, 0, 0, 0)
    return b"\x89PNG\r\n\x1a\n" + chunk(b"IHDR", ihdr) + chunk(b"IDAT", zlib.compress(b"".join(rows), 9)) + chunk(b"IEND", b"")


def main():
    os.makedirs(OUT, exist_ok=True)
    # "any" icons use the whole canvas; the maskable one keeps the artwork inside the central 80% safe zone.
    for name, size, scale in [
        ("icon-192.png", 192, 1.0),
        ("icon-512.png", 512, 1.0),
        ("icon-maskable-512.png", 512, 0.78),
        ("apple-touch-icon.png", 180, 0.9),
    ]:
        with open(os.path.join(OUT, name), "wb") as f:
            f.write(png(size, scale))


if __name__ == "__main__":
    main()
