#!/bin/sh
set -eu

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
PROJECT_ROOT=$(CDPATH= cd -- "$SCRIPT_DIR/../.." && pwd)
OUTPUT_DIR="$TARGET_BUILD_DIR/$UNLOCALIZED_RESOURCES_FOLDER_PATH"
OUTPUT="$OUTPUT_DIR/mactun"

ARCH_LIST=${ARCHS:-${CURRENT_ARCH:-$(uname -m)}}
case "$ARCH_LIST" in
    *undefined_arch*) ARCH_LIST=${NATIVE_ARCH_ACTUAL:-$(uname -m)} ;;
esac

TEMP_DIR=$(mktemp -d "${TMPDIR:-/tmp}/mactun-helper.XXXXXX")
trap 'rm -rf "$TEMP_DIR"' EXIT HUP INT TERM
LIPO_INPUTS=""
ARCH_COUNT=0

cd "$PROJECT_ROOT"
for ARCH in $ARCH_LIST; do
    case "$ARCH" in
        arm64) GO_ARCH=arm64 ;;
        x86_64) GO_ARCH=amd64 ;;
        *) echo "unsupported architecture: $ARCH" >&2; exit 1 ;;
    esac
    BINARY="$TEMP_DIR/mactun-$ARCH"
    CGO_ENABLED=1 GOOS=darwin GOARCH="$GO_ARCH" \
        MACOSX_DEPLOYMENT_TARGET="${MACOSX_DEPLOYMENT_TARGET:-13.0}" \
        go build -trimpath -ldflags "-s -w" -o "$BINARY" ./cmd/mactun
    LIPO_INPUTS="$LIPO_INPUTS $BINARY"
    ARCH_COUNT=$((ARCH_COUNT + 1))
done

mkdir -p "$OUTPUT_DIR"
if [ "$ARCH_COUNT" -eq 1 ]; then
    cp $LIPO_INPUTS "$OUTPUT"
else
    xcrun lipo -create $LIPO_INPUTS -output "$OUTPUT"
fi
chmod 0755 "$OUTPUT"
codesign --force --sign - "$OUTPUT"
echo "built $OUTPUT for $ARCH_LIST"
