#!/usr/bin/env bash
# SPDX-License-Identifier: LGPL-2.1-or-later
# Prove gpl-guard.sh passes an LGPL FFmpeg and refuses planted GPL code, so the guard cannot
# quietly stop guarding.
#
# Usage: ./gpl-guard-test.sh <lgpl ffmpeg> [gpl ffmpeg]
#
# Checks, each of which must hold: the LGPL binary passes; a copy with the x264 banner appended is
# refused by the binary read alone; a stub that reports a clean configuration and license but
# lists libx264 under -encoders is refused by the encoder read alone; and, when given, the GPL
# binary is refused by all four reads.
set -euo pipefail

if [ $# -lt 1 ] || [ $# -gt 2 ]; then
	echo "usage: gpl-guard-test.sh <lgpl ffmpeg> [gpl ffmpeg]" >&2
	exit 2
fi
here="$(cd "$(dirname "$0")" && pwd)"
guard="$here/gpl-guard.sh"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
failed=0

# expect runs the guard on a binary and checks its exit status and which reads it refused.
expect() {
	local name="$1" want_rc="$2" want_refused="$3" bin="$4" out rc refused
	set +e
	out="$("$guard" "$bin" 2>&1)"
	rc=$?
	set -e
	refused="$(printf '%s\n' "$out" | sed -n 's/^gpl-guard: \([a-z]*\): GPL found.*/\1/p' |
		tr '\n' ' ')"
	refused="${refused% }"
	if [ "$rc" = "$want_rc" ] && [ "$refused" = "$want_refused" ]; then
		echo "gpl-guard-test: $name: ok (exit $rc, refused: ${refused:-none})"
	else
		echo "gpl-guard-test: $name: FAILED (exit $rc, want $want_rc; refused: ${refused:-none}," \
			"want ${want_refused:-none})"
		printf '%s\n' "$out" | sed 's/^/  /'
		failed=1
	fi
}

expect "LGPL binary passes" 0 "" "$1"

cp "$1" "$work/planted"
printf 'x264 - core 165 - H.264/MPEG-4 AVC codec - Copyleft 2003-2025 - http://www.videolan.org/x264.html' \
	>>"$work/planted"
expect "planted x264 banner is refused" 1 "binary" "$work/planted"

cat >"$work/stub" <<'STUB'
#!/bin/sh
case "$*" in
*-version*) printf 'ffmpeg version 9.0.2\nconfiguration: --enable-version3 --enable-static\n' ;;
*-L*) printf 'ffmpeg is free software.\nGNU Lesser General Public License\nversion 3\n' ;;
*-encoders*) printf 'Encoders:\n ------\n V....D lib%s  planted\n' x264 ;;
esac
STUB
chmod +x "$work/stub"
expect "stub listing libx264 is refused" 1 "encoders" "$work/stub"

if [ $# -eq 2 ]; then
	expect "GPL binary is refused" 1 "configuration license binary encoders" "$2"
fi

if [ "$failed" = 1 ]; then
	exit 1
fi
echo "gpl-guard-test: every check held"
