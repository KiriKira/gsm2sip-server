#!/usr/bin/env python3
"""Generate original sine-tone Opus fixtures; no microphone or copyrighted audio."""
import json
from pathlib import Path
import struct
import subprocess

ROOT = Path(__file__).resolve().parent


def packets(ogg):
    position = 0
    pending = bytearray()
    result = []
    while position < len(ogg):
        if ogg[position:position + 4] != b"OggS":
            raise RuntimeError("Invalid generated Ogg stream")
        count = ogg[position + 26]
        segments = ogg[position + 27:position + 27 + count]
        body = position + 27 + count
        for size in segments:
            pending.extend(ogg[body:body + size])
            body += size
            if size < 255:
                value = bytes(pending)
                pending.clear()
                if not value.startswith((b"OpusHead", b"OpusTags")):
                    result.append(value)
        position = body
    if pending or len(result) < 20:
        raise RuntimeError("Incomplete generated Opus packets")
    return result


def main():
    directory = ROOT / "testdata"
    directory.mkdir(exist_ok=True)
    for frequency in (1000, 1500):
        ogg = subprocess.run(["ffmpeg", "-hide_banner", "-loglevel", "error", "-f", "lavfi", "-i",
                              f"sine=frequency={frequency}:sample_rate=48000:duration=1", "-ac", "1", "-c:a", "libopus",
                              "-b:a", "32k", "-vbr", "off", "-frame_duration", "20", "-f", "ogg", "pipe:1"],
                             check=True, stdout=subprocess.PIPE).stdout
        # Independently decode the generated Opus stream before pinning packets.
        decoded = subprocess.run(["ffmpeg", "-hide_banner", "-loglevel", "error", "-i", "pipe:0",
                                  "-f", "s16le", "-ac", "1", "-ar", "48000", "pipe:1"], input=ogg,
                                 check=True, stdout=subprocess.PIPE).stdout
        if len(decoded) < 48000:
            raise RuntimeError("Generated Opus did not decode into sufficient PCM")
        samples = struct.unpack("<" + "h" * (len(decoded) // 2), decoded)
        if max(abs(value) for value in samples) < 100:
            raise RuntimeError("Generated Opus fixture decoded to silence")
        value = {"frequency_hz": frequency, "sample_rate": 48000, "channels": 1,
                 "duration_ms": 20, "packets": [packet.hex() for packet in packets(ogg)[:20]],
                 "generator": subprocess.run(["ffmpeg", "-version"], check=True, stdout=subprocess.PIPE,
                                              text=True).stdout.splitlines()[0]}
        (directory / f"opus-{frequency}.json").write_text(json.dumps(value, indent=2) + "\n")
        print(f"Generated and decoded original {frequency} Hz Opus fixture")


if __name__ == "__main__":
    main()
