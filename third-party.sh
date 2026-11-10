#!/bin/sh
# SPDX-License-Identifier: LGPL-2.1-or-later
# Print THIRD_PARTY.md for Icelos.app from ffmpeg-sources.txt.
#
# Usage: ./third-party.sh [version] [gpl|lgpl]
#
# gpl, the default, describes the development build, which is not distributed. lgpl describes the
# Mac App Store edition, whose FFmpeg is built without x264 and x265 (see build-ffmpeg.sh).
set -eu
here="$(cd "$(dirname "$0")" && pwd)"
version="${1:-dev}"
license="${2:-gpl}"
case "$license" in
gpl)
	terms="This is a development build and is not distributed. Its FFmpeg is built
with the GPL and version 3 options, so the bundled \`ffmpeg\` and \`ffprobe\` binaries fall under
GPL-3.0-or-later."
	source_tar=ffmpeg_source
	;;
lgpl)
	terms="This is the Mac App Store edition. Its FFmpeg is built with the version 3
option and without the GPL option or any GPL library, so the bundled \`ffmpeg\` and \`ffprobe\`
binaries are distributed under LGPL-3.0-or-later, the GNU Lesser General Public License. The
corresponding source and build recipe are offered at https://github.com/dcadolph/icelos-ffmpeg.
Set ICELOS_FFMPEG_LICENSE=lgpl when running \`build-ffmpeg.sh\` to build this edition's
binaries from the corresponding source."
	source_tar=ffmpeg_lgpl_source
	;;
*)
	echo "third-party: unknown license $license (want gpl or lgpl)" >&2
	exit 2
	;;
esac
case "$version" in
dev | "") ref=main ;;
v*) ref="icelos-$version" ;;
*) ref="icelos-v$version" ;;
esac

cat <<HEAD
# Third-party software

Icelos.app bundles FFmpeg and the libraries listed below. Each is distributed under its own
license, named in the table. $terms The license text is in \`LICENSE\` in the source repository.

HEAD

awk -F '\t' -v license="$license" '
	/^#/ || NF < 5 { next }
	license == "lgpl" && ($1 == "x264" || $1 == "x265") { next }
	license == "lgpl" && $1 == "ffmpeg" { $3 = "LGPL-3.0-or-later" }
	{
		n++
		name[n] = $1; ver[n] = $2; lic[n] = $3; url[n] = $4
		if (length($1) > w1) w1 = length($1)
		if (length($2) > w2) w2 = length($2)
		if (length($3) > w3) w3 = length($3)
		if (length($4) > w4) w4 = length($4)
	}
	function row(a, b, c, d) {
		printf "| %-*s | %-*s | %-*s | %-*s |\n", w1, a, w2, b, w3, c, w4, d
	}
	function rule(w,   s) {
		s = ""
		while (length(s) < w) s = s "-"
		return s
	}
	END {
		if (w1 < 4) w1 = 4
		if (w2 < 7) w2 = 7
		if (w3 < 7) w3 = 7
		if (w4 < 6) w4 = 6
		row("Name", "Version", "License", "Source")
		row(rule(w1), rule(w2), rule(w3), rule(w4))
		for (i = 1; i <= n; i++) row(name[i], ver[i], lic[i], url[i])
	}
' "$here/ffmpeg-sources.txt"

cat <<TAIL

The corresponding source for the bundled binaries is attached to the matching icelos-v<version>
release as \`Icelos_<version>_${source_tar}.tar\` at
https://github.com/dcadolph/icelos-ffmpeg/releases. It holds every tarball above with
\`build-ffmpeg.sh\` and \`ffmpeg-sources.txt\`, which build the binaries from them with no
download. The same scripts are at https://github.com/dcadolph/icelos-ffmpeg/tree/$ref.
VideoToolbox, AudioToolbox, zlib, bzip2, and iconv come from macOS.
TAIL

if [ "$license" = lgpl ]; then
	cat <<FTL

FreeType is used under the FreeType License. Portions of this software are copyright © 2025 The
FreeType Project (www.freetype.org). All rights reserved.
FTL
fi
