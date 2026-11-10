#!/usr/bin/env bash
# SPDX-License-Identifier: LGPL-2.1-or-later
# Refuse an FFmpeg tool that carries GPL code, so no GPL binary reaches the Mac App Store edition.
#
# Usage: ./gpl-guard.sh <ffmpeg or ffprobe>
#
# Four reads, each printed with its verdict:
#   configuration  the -version configuration line has no --enable-gpl, --enable-libx264,
#                  --enable-libx265, or --enable-nonfree.
#   license        the first three lines of -L name the GNU Lesser General Public License and not
#                  the GNU General Public License.
#   binary         nm finds no x264 or x265 symbol and a scan of every byte finds neither
#                  library's banner. otool -L cannot see either library, because both link
#                  statically, and strings reads only a Mach-O file's sections. The plain text
#                  "x264 - core" is not a marker: FFmpeg's own H.264 decoder carries it.
#   encoders       -encoders lists no libx264, libx264rgb, or libx265.
#
# Exits 0 only when every read is clean, 1 when any read finds GPL code or cannot run, 2 on usage.
set -uo pipefail

if [ $# -ne 1 ] || [ ! -f "$1" ]; then
	echo "usage: gpl-guard.sh <ffmpeg or ffprobe>" >&2
	exit 2
fi
tool="$1"
failed=0

# verdict prints one read's result and records a failure.
verdict() {
	if [ "$2" = clean ]; then
		echo "gpl-guard: $1: clean"
	else
		echo "gpl-guard: $1: GPL found: $2"
		failed=1
	fi
}

if ! version="$("$tool" -hide_banner -version 2>&1)"; then
	verdict configuration "cannot run $tool -version"
else
	config="$(printf '%s\n' "$version" | grep '^configuration:' || true)"
	hits="$(printf '%s\n' "$config" | grep -oE -- '--enable-(gpl|libx264|libx265|nonfree)( |$)' |
		tr -d ' ' | tr '\n' ' ' || true)"
	if [ -z "$config" ]; then
		verdict configuration "no configuration line in -version"
	elif [ -n "$hits" ]; then
		verdict configuration "${hits% }"
	else
		verdict configuration clean
	fi
fi

if ! notice="$("$tool" -hide_banner -L 2>&1 | head -n 3)"; then
	verdict license "cannot run $tool -L"
elif printf '%s\n' "$notice" | grep -qF 'GNU General Public License'; then
	verdict license "-L names the GNU General Public License"
elif ! printf '%s\n' "$notice" | grep -qF 'GNU Lesser General Public License'; then
	verdict license "-L does not name the GNU Lesser General Public License"
else
	verdict license clean
fi

symbols="$(nm "$tool" 2>/dev/null |
	grep -oE '_(ff_libx264(rgb)?_encoder|ff_libx265_encoder|x265_encoder_open[_0-9]*|x264_(8|10)_[A-Za-z0-9_]+)$' |
	head -n 3 | tr '\n' ' ' || true)"
banners="$(LC_ALL=C grep -a -oE \
	'videolan\.org/x264\.html|H\.264/MPEG-4 AVC codec - Copy|HEVC encoder version' "$tool" |
	sort -u | tr '\n' ' ' || true)"
if [ -n "$symbols$banners" ]; then
	found=""
	[ -n "$symbols" ] && found="symbols ${symbols% }"
	[ -n "$banners" ] && found="${found:+$found; }strings ${banners% }"
	verdict binary "$found"
else
	verdict binary clean
fi

if ! encoders="$("$tool" -hide_banner -encoders 2>&1)"; then
	# ffprobe has no -encoders option, and a build without encoders lists none.
	case "$(basename "$tool")" in
	ffprobe*) verdict encoders clean ;;
	*) verdict encoders "cannot run $tool -encoders" ;;
	esac
else
	listed="$(printf '%s\n' "$encoders" | awk '{print $2}' | grep -xE 'libx264(rgb)?|libx265' |
		tr '\n' ' ' || true)"
	if [ -n "$listed" ]; then
		verdict encoders "${listed% }"
	else
		verdict encoders clean
	fi
fi

if [ "$failed" = 1 ]; then
	echo "gpl-guard: refused $tool" >&2
	exit 1
fi
echo "gpl-guard: $tool is free of GPL code"
