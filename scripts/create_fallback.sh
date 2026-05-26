#!/bin/bash
# ==============================================================================
# StreamSwitch - Fallback File Creator
# Creates an H.265 MPEGTS fallback file compatible with the stream switcher
# ==============================================================================

set -e

# Colors
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
CYAN='\033[0;36m'
NC='\033[0m'

echo -e "${CYAN}╔═══════════════════════════════════════════════╗${NC}"
echo -e "${CYAN}║   StreamSwitch - Fallback File Creator         ║${NC}"
echo -e "${CYAN}╚═══════════════════════════════════════════════╝${NC}"
echo ""

# Defaults
OUTPUT="fallback.ts"
RESOLUTION="1920x1080"
FPS=30
DURATION=60
VIDEO_BITRATE="1000k"
AUDIO_RATE=48000
KEYINT=60  # 2 seconds at 30fps

usage() {
    echo "Usage: $0 [OPTIONS] <input_file>"
    echo ""
    echo "Creates an H.265 MPEGTS fallback file for StreamSwitch."
    echo "Input can be an image (PNG/JPG) or a video file."
    echo ""
    echo "Options:"
    echo "  -o FILE       Output file (default: fallback.ts)"
    echo "  -r WxH        Resolution (default: 1920x1080)"
    echo "  -f FPS        Frame rate (default: 30)"
    echo "  -d SECONDS    Duration in seconds (default: 60)"
    echo "  -b BITRATE    Video bitrate (default: 1000k)"
    echo "  -h            Show this help"
    echo ""
    echo "Examples:"
    echo "  $0 brb.png                    # From image"
    echo "  $0 -d 120 brb_video.mp4       # From video, 2 minutes"
    echo "  $0 -b 2000k -r 1920x1080 bg.jpg"
}

while getopts "o:r:f:d:b:h" opt; do
    case $opt in
        o) OUTPUT="$OPTARG" ;;
        r) RESOLUTION="$OPTARG" ;;
        f) FPS="$OPTARG" ;;
        d) DURATION="$OPTARG" ;;
        b) VIDEO_BITRATE="$OPTARG" ;;
        h) usage; exit 0 ;;
        *) usage; exit 1 ;;
    esac
done

shift $((OPTIND - 1))

if [ $# -lt 1 ]; then
    echo -e "${RED}ERROR: Input file required${NC}"
    echo ""
    usage
    exit 1
fi

INPUT="$1"

if [ ! -f "$INPUT" ]; then
    echo -e "${RED}ERROR: File not found: $INPUT${NC}"
    exit 1
fi

# Check if ffmpeg is available
if ! command -v ffmpeg &> /dev/null; then
    echo -e "${RED}ERROR: ffmpeg not found. Install ffmpeg first.${NC}"
    exit 1
fi

# Detect input type
MIME=$(file --mime-type -b "$INPUT" 2>/dev/null || echo "unknown")
IS_IMAGE=false

case "$MIME" in
    image/*)
        IS_IMAGE=true
        echo -e "${GREEN}Input type: Image${NC}"
        ;;
    video/*)
        echo -e "${GREEN}Input type: Video${NC}"
        ;;
    *)
        echo -e "${YELLOW}WARNING: Unknown file type ($MIME), treating as video${NC}"
        ;;
esac

KEYINT=$((FPS * 2))  # 2 second keyframe interval

echo -e "Resolution:  ${CYAN}$RESOLUTION${NC}"
echo -e "FPS:         ${CYAN}$FPS${NC}"
echo -e "Duration:    ${CYAN}${DURATION}s${NC}"
echo -e "Bitrate:     ${CYAN}$VIDEO_BITRATE${NC}"
echo -e "Keyint:      ${CYAN}$KEYINT (2 seconds)${NC}"
echo -e "Output:      ${CYAN}$OUTPUT${NC}"
echo ""

if $IS_IMAGE; then
    echo -e "${YELLOW}Creating fallback from image...${NC}"
    ffmpeg -y -hide_banner \
        -loop 1 -i "$INPUT" \
        -f lavfi -i "anullsrc=r=${AUDIO_RATE}:cl=stereo" \
        -c:v libx265 -preset medium \
        -b:v "$VIDEO_BITRATE" \
        -x265-params "keyint=${KEYINT}:min-keyint=${KEYINT}:scenecut=0" \
        -r "$FPS" \
        -s "$RESOLUTION" \
        -pix_fmt yuv420p \
        -c:a aac -b:a 128k -ar "$AUDIO_RATE" \
        -t "$DURATION" \
        -f mpegts "$OUTPUT"
else
    echo -e "${YELLOW}Creating fallback from video...${NC}"
    ffmpeg -y -hide_banner \
        -i "$INPUT" \
        -c:v libx265 -preset medium \
        -b:v "$VIDEO_BITRATE" \
        -x265-params "keyint=${KEYINT}:min-keyint=${KEYINT}:scenecut=0" \
        -r "$FPS" \
        -s "$RESOLUTION" \
        -pix_fmt yuv420p \
        -c:a aac -b:a 128k -ar "$AUDIO_RATE" \
        -t "$DURATION" \
        -f mpegts "$OUTPUT"
fi

echo ""

if [ -f "$OUTPUT" ]; then
    SIZE=$(du -h "$OUTPUT" | cut -f1)
    echo -e "${GREEN}✓ Fallback file created: $OUTPUT ($SIZE)${NC}"
    echo ""
    echo -e "Verify with: ${CYAN}ffprobe $OUTPUT${NC}"
    echo -e "Use with:    ${CYAN}streamswitch --fallback $OUTPUT${NC}"
else
    echo -e "${RED}✗ Failed to create fallback file${NC}"
    exit 1
fi
