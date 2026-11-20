#!/bin/sh
# Regenerates the fake capture source's fixtures with FFmpeg (7.1 or later).
# The outputs are committed, so tests never run this; run it from anywhere
# after changing a command below, then commit the new files.
#
#   clip.h264          640x360, 45 frames at 15 fps, H.264 High, Annex-B,
#                      an IDR every 15 frames, no B-frames, an AUD per frame
#   clip-rotated.h264  360x640, 15 frames, otherwise the same
#   aac/NNN.aac        raw AAC-LC frames (ADTS headers stripped), 48 kHz stereo
#   asc.bin            their AudioSpecificConfig
#   pcm.raw            0.5 s of 48 kHz stereo signed 16-bit little-endian PCM
#   pcm-f32.raw        0.25 s of 48 kHz stereo 32-bit float little-endian PCM
set -eu

cd "$(dirname "$0")"
ff="ffmpeg -hide_banner -loglevel error -y"
x264="keyint=15:min-keyint=15:scenecut=0:bframes=0:rc-lookahead=0:aud=1"

video() { # <size> <frames> <out>
	$ff -f lavfi -i "smptebars=size=$1:rate=15,drawbox=x='mod(t*180\,iw-80)':y='ih/2-40':w=80:h=80:color=white:t=fill" \
		-frames:v "$2" -c:v libx264 -profile:v high -pix_fmt yuv420p -preset veryfast -crf 30 \
		-x264-params "$x264" -f h264 "$3"
}

video 640x360 45 clip.h264
video 360x640 15 clip-rotated.h264

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
$ff -f lavfi -i "sine=frequency=440:sample_rate=48000" -ac 2 -t 1 -c:a aac -b:a 48k -f adts "$tmp/a.adts"
rm -rf aac
mkdir aac
# Split the ADTS stream into its frames, dropping each 7- or 9-byte header.
python3 - "$tmp/a.adts" aac <<'EOF'
import sys
data = open(sys.argv[1], "rb").read()
i = n = 0
while i < len(data):
    assert data[i] == 0xFF and data[i + 1] & 0xF0 == 0xF0, "lost ADTS sync"
    header = 7 if data[i + 1] & 1 else 9
    size = ((data[i + 3] & 3) << 11) | (data[i + 4] << 3) | (data[i + 5] >> 5)
    open("%s/%03d.aac" % (sys.argv[2], n), "wb").write(data[i + header:i + size])
    i += size
    n += 1
EOF
# AAC-LC (object type 2), 48 kHz (index 3), 2 channels: 00010 0011 0010 000.
printf '\021\220' > asc.bin

$ff -f lavfi -i "sine=frequency=660:sample_rate=48000" -ac 2 -t 0.5 -f s16le -c:a pcm_s16le pcm.raw
$ff -f lavfi -i "sine=frequency=660:sample_rate=48000" -ac 2 -t 0.25 -f f32le -c:a pcm_f32le pcm-f32.raw
