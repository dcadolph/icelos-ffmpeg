# icelos-ffmpeg

This repository holds the FFmpeg build recipe for Icelos and the receipt verifier.

The recipe is the set of scripts and the pinned source list that build the `ffmpeg` and `ffprobe`
binaries inside Icelos. The receipt verifier, `icelos-verify`, takes a receipt Icelos wrote beside
a converted file and measures the files again to show whether every claim in it still holds.

## Source offer

Every Icelos store release has a matching `icelos-vX.Y.Z` release here, under
[Releases](https://github.com/dcadolph/icelos-ffmpeg/releases). Each one carries two files:

- `Icelos_vX.Y.Z_ffmpeg_lgpl_source.tar` holds every source tarball that version's FFmpeg was
  built from, the build script, the pinned source list, and the GPL guard. It builds the same
  binaries with no download.
- `MANIFEST-lgpl` names the tarballs in it.

The release workflow fetches each tarball from its upstream, checks it against the SHA-256 pinned
in `ffmpeg-sources.txt`, and packs it. These releases stay up. If one cannot be reached, open an
issue here and the same source will be provided at no charge.

The FFmpeg in Icelos is built with the version 3 option and without the GPL option, x264, or x265,
so the binaries fall under LGPL-3.0-or-later. Each library keeps its own license, named in
`ffmpeg-sources.txt`.

## Build FFmpeg

The recipe runs on macOS with the Xcode command line tools and a few build tools:

```sh
brew install cmake nasm meson ninja pkg-config autoconf automake
ICELOS_FFMPEG_LICENSE=lgpl ./build-ffmpeg.sh arm64
```

Use `x86_64` in place of `arm64` for an Intel build. The binaries land in
`.build/ffmpeg-lgpl/<arch>/bin`. The script checks every tarball's SHA-256 before anything builds,
links only against system libraries, and runs `gpl-guard.sh` on both binaries before it reports
success. `ICELOS_JOBS` sets build parallelism.

To build from a release's source tar instead, unpack it and run the same command inside the
unpacked folder. The tarballs are already in place, so nothing is downloaded.

`./build-ffmpeg.sh --fetch-only` downloads and checks the tarballs and stops before building.
Without `ICELOS_FFMPEG_LICENSE=lgpl` the script builds a GPL development variant with x264 and x265,
which Icelos does not distribute.

`./gpl-guard.sh <ffmpeg or ffprobe>` refuses a binary that carries GPL code, and
`./gpl-guard-test.sh <lgpl ffmpeg> [gpl ffmpeg]` proves the guard still refuses planted GPL code.
`./third-party.sh [version] lgpl` prints the third-party notice Icelos ships.

## Verify a receipt

Install the verifier with Go:

```sh
go install github.com/dcadolph/icelos-ffmpeg/cmd/icelos-verify@latest
```

Then point it at a receipt:

```sh
icelos-verify clip.mp4.receipt.json
```

The receipt names its source and output files. By default the verifier looks for both beside the
receipt. `--source` and `--output` point it elsewhere, and `--ffmpeg <dir>` names a folder holding
the `ffmpeg` and `ffprobe` to measure with, in place of the ones on `PATH`.

The verifier re-runs every proof from the plan the receipt carries, then compares the results. The
same checks must run, each must pass or fail as before, and the picture and sound scores must land
within a small margin of the receipt's. It prints one line:

- `holds: N checks re-measured, M not checked` and exits 0 when the receipt holds.
- `does not hold: <check> was <result>, now <result>` and exits 1 at the first difference.
- A reason on stderr and exit 2 when it cannot run, such as a missing file or a receipt written
  before receipts carried their plan.

The verifier only measures. Every ffmpeg command it runs writes to standard output, never to a file,
and a test in `verify/` holds it to that.

## Licenses

The recipe files, `build-ffmpeg.sh`, `ffmpeg-sources.txt`, `gpl-guard.sh`, `gpl-guard-test.sh`,
and `third-party.sh`, are under LGPL-2.1-or-later, in `LICENSE.recipe`. The Go code, including
the verifier, is under GPL-3.0-or-later, in `LICENSE`.

The test clips in `verify/testdata/media` are cut from Sintel, Big Buck Bunny, and Tears of Steel,
© Blender Foundation, under CC BY 3.0. `verify/testdata/media/manifest.json` gives each clip's
source, attribution, and how it was cut.

## Contributing

Pull requests are welcome for the recipe: new library versions, build fixes, and new
architectures. Changes to `verify/` and `probe/` are accepted only from the owner, since a receipt
means what those packages measure.
