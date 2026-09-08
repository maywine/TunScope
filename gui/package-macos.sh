#!/bin/sh
set -eu

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
PROJECT_ROOT=$(CDPATH= cd -- "$SCRIPT_DIR/.." && pwd)
VERSION=${1:-0.3.20}
RID=${2:-osx-arm64}
DESTINATION=${3:-"$PROJECT_ROOT/dist"}

case "$VERSION" in
    ''|*[!0-9A-Za-z.-]*) echo "invalid version: $VERSION" >&2; exit 2 ;;
esac
case "$RID" in
    osx-arm64) GO_ARCH=arm64 ;;
    osx-x64) GO_ARCH=amd64 ;;
    *) echo "RID must be osx-arm64 or osx-x64" >&2; exit 2 ;;
esac

MARKETING_VERSION=${VERSION%%-*}
BUILD_VERSION=$(printf '%s' "$MARKETING_VERSION" | tr -cd '0-9.')
case "$BUILD_VERSION" in
    ''|.*|*..*|*.) echo "version must begin with a numeric semantic version: $VERSION" >&2; exit 2 ;;
esac

TEMPORARY_ROOT=$(mktemp -d "${TMPDIR:-/tmp}/tunscope-avalonia.XXXXXX")
trap 'rm -rf -- "$TEMPORARY_ROOT"' EXIT HUP INT TERM
PUBLISH_DIR="$TEMPORARY_ROOT/publish"
PACKAGE_ROOT="$TEMPORARY_ROOT/package"
APP_DIR="$PACKAGE_ROOT/TunScope.app"
MACOS_DIR="$APP_DIR/Contents/MacOS"
RESOURCES_DIR="$APP_DIR/Contents/Resources"
ICONSET_DIR="$TEMPORARY_ROOT/TunScope.iconset"
HELPER="$RESOURCES_DIR/tunscope-helper"

mkdir -p "$PUBLISH_DIR" "$MACOS_DIR" "$RESOURCES_DIR" "$ICONSET_DIR"

cd "$PROJECT_ROOT"
dotnet publish gui/TunScope.GUI.csproj \
    -c Release \
    -r "$RID" \
    --self-contained true \
    -p:Version="$VERSION" \
    -o "$PUBLISH_DIR"

cp -R "$PUBLISH_DIR"/. "$MACOS_DIR"/
chmod 0755 "$MACOS_DIR/TunScope"

CGO_ENABLED=1 GOOS=darwin GOARCH="$GO_ARCH" \
    MACOSX_DEPLOYMENT_TARGET=14.0 \
    go build -trimpath -ldflags "-s -w -X main.version=$VERSION" -o "$HELPER" ./cmd/tunscope
chmod 0755 "$HELPER"

for specification in \
    "16 icon_16x16.png" \
    "32 icon_16x16@2x.png" \
    "32 icon_32x32.png" \
    "64 icon_32x32@2x.png" \
    "128 icon_128x128.png" \
    "256 icon_128x128@2x.png" \
    "256 icon_256x256.png" \
    "512 icon_256x256@2x.png" \
    "512 icon_512x512.png" \
    "1024 icon_512x512@2x.png"
do
    set -- $specification
    sips -z "$1" "$1" "$PROJECT_ROOT/assets/TunScopeIcon.png" --out "$ICONSET_DIR/$2" >/dev/null
done
iconutil -c icns "$ICONSET_DIR" -o "$RESOURCES_DIR/TunScope.icns"

sed \
    -e "s/@MARKETING_VERSION@/$MARKETING_VERSION/g" \
    -e "s/@BUILD_VERSION@/$BUILD_VERSION/g" \
    -e "s/@TUNSCOPE_VERSION@/$VERSION/g" \
    "$SCRIPT_DIR/Platforms/macos/Info.plist.in" > "$APP_DIR/Contents/Info.plist"
plutil -lint "$APP_DIR/Contents/Info.plist" >/dev/null

codesign --force --sign - "$HELPER"
find "$MACOS_DIR" -type f \( -name '*.dylib' -o -perm -0100 \) -exec codesign --force --sign - {} \;
codesign --force --sign - "$APP_DIR"
codesign --verify --deep --strict "$APP_DIR"

OUTPUT_ROOT="$DESTINATION/tunscope-$VERSION-$RID"
mkdir -p "$DESTINATION"
if [ -e "$OUTPUT_ROOT" ]; then
    rm -rf -- "$OUTPUT_ROOT"
fi
mv "$PACKAGE_ROOT" "$OUTPUT_ROOT"

printf 'Created %s/TunScope.app\n' "$OUTPUT_ROOT"
