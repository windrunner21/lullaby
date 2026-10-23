#!/bin/sh
# build-ffmpeg.sh builds a lean ffmpeg for lullaby: the audio formats songs
# come in, the Opus encoder, and the protocols lullaby uses, in one static
# file with no libraries to load. BENCHMARKS.md has what that saves.
#
# It cross-compiles with zig, which it downloads, so any Linux machine can
# build it for any other:
#
#	scripts/build-ffmpeg.sh                             # bin/ffmpeg-linux-arm64
#	TARGET=x86_64-linux-musl scripts/build-ffmpeg.sh    # bin/ffmpeg-linux-amd64
#
# It needs curl, tar, xz, make, perl, a C compiler for the build machine, and
# an nm that reads other machines' files, like llvm-nm. Point Config.FFmpeg
# at the result. CONTRIBUTING.md says why it's built as it is.
set -eu

ffmpeg_version=9.0.2
# From the tarball whose signature checked out with FFmpeg's release key,
# FCF9 86EA 15E6 E293 A564 4F10 B432 2F04 D676 58D8.
ffmpeg_sha256=8c3850283eb25fa026482078a04051e0be17347b09ef81a0849bec15a96e002e
opus_version=1.5.2
opus_sha256=65c1d2f78b9f2fb20082c38cbe47c951ad5839345876e46941612ee87f9a7ce1
# From the tarball whose signature checked out with OpenSSL's key,
# C46E D3F2 CBEF DA1F DAAD A442 64ED 7B1D CCE7 1CB2.
openssl_version=3.5.9
openssl_sha256=603f5602e2eef00d77fbd429d34dcd5822bb301757a1bc9cdb24c670f1eb859a
zig_version=0.17.0
zig_sha256_x86_64=1cbe9df9f27e6b78d14ccbca43b6703a404ef79ef1c463de901d7f088d4e2026
zig_sha256_aarch64=9e8d11661d4ae3bd57702a3832781e23ad151dde5798e16a5ccd503f65234ff8

root=$(cd "$(dirname "$0")/.." && pwd)
target=${TARGET:-aarch64-linux-musl}
arch=${target%%-*}
case $arch in
aarch64) name=linux-arm64 openssl_target=linux-aarch64 ;;
x86_64) name=linux-amd64 openssl_target=linux-x86_64 ;;
*) echo "build-ffmpeg: no name for $arch" >&2; exit 1 ;;
esac
work=${WORK:-$root/build/ffmpeg-$target}
out=${OUT:-$root/bin/ffmpeg-$name}
jobs=$(nproc)
nm=${NM:-$(command -v llvm-nm || echo nm)}
mkdir -p "$work/dl" "$work/src" "$work/tools" "$work/prefix/include" "$work/prefix/lib" "$(dirname "$out")"

# fetch downloads url as file in dl, unless it's there, and checks it.
fetch() {
	[ -f "$work/dl/$1" ] || curl -fsSL -o "$work/dl/$1" "$2"
	echo "$3  $work/dl/$1" | sha256sum -c --quiet -
}

# unpack extracts a download into a fresh directory of its own.
unpack() {
	rm -rf "$work/src/$2"
	mkdir -p "$work/src/$2"
	tar -xf "$work/dl/$1" -C "$work/src/$2" --strip-components=1
}

if [ -z "${ZIG:-}" ]; then
	host=$(uname -m)
	eval "sum=\${zig_sha256_$host:-}"
	[ -n "$sum" ] || { echo "build-ffmpeg: no zig for $host; set ZIG" >&2; exit 1; }
	fetch zig.tar.xz "https://ziglang.org/download/$zig_version/zig-$host-linux-$zig_version.tar.xz" "$sum"
	[ -x "$work/src/zig/zig" ] || unpack zig.tar.xz zig
	ZIG=$work/src/zig/zig
fi

# The tools ffmpeg's and libopus's builds call, aimed at the target.
for tool in cc c++ ar ranlib; do
	case $tool in
	cc | c++) line="exec \"$ZIG\" $tool -target $target \"\$@\"" ;;
	*) line="exec \"$ZIG\" $tool \"\$@\"" ;;
	esac
	printf '#!/bin/sh\n%s\n' "$line" >"$work/tools/$tool"
	chmod +x "$work/tools/$tool"
done
tools=$work/tools
prefix=$work/prefix

fetch opus.tar.gz "https://github.com/xiph/opus/releases/download/v$opus_version/opus-$opus_version.tar.gz" "$opus_sha256"
unpack opus.tar.gz opus
(cd "$work/src/opus" &&
	./configure --host="$target" --prefix="$prefix" --enable-static --disable-shared \
		--disable-doc --disable-extra-programs CC="$tools/cc" AR="$tools/ar" RANLIB="$tools/ranlib" CFLAGS=-O2 >/dev/null &&
	make -j"$jobs" >/dev/null && make install >/dev/null)

# OpenSSL for https, since ffmpeg with it trusts the system's certificates
# by itself: those in /etc/ssl, or SSL_CERT_FILE and SSL_CERT_DIR.
fetch openssl.tar.gz "https://github.com/openssl/openssl/releases/download/openssl-$openssl_version/openssl-$openssl_version.tar.gz" "$openssl_sha256"
unpack openssl.tar.gz openssl
(cd "$work/src/openssl" &&
	CC="$tools/cc" AR="$tools/ar" RANLIB="$tools/ranlib" ./Configure "$openssl_target" \
		no-shared no-dso no-engine no-tests no-docs --prefix="$prefix" --libdir=lib --openssldir=/etc/ssl -O2 >/dev/null &&
	make -j"$jobs" build_libs >/dev/null && make install_dev >/dev/null 2>&1)

fetch ffmpeg.tar.xz "https://ffmpeg.org/releases/ffmpeg-$ffmpeg_version.tar.xz" "$ffmpeg_sha256"
unpack ffmpeg.tar.xz ffmpeg
asm=
[ "$arch" = x86_64 ] && ! command -v nasm >/dev/null && asm=--disable-x86asm
# Songs come as AAC or Opus from YouTube, MP3, AAC or Opus from SoundCloud,
# and anything from other sites through yt-dlp: every common audio format
# and container is in, since each is small. --disable-autodetect would
# leave out threads, which the ffmpeg program needs.
(cd "$work/src/ffmpeg" &&
	PKG_CONFIG_LIBDIR=$prefix/lib/pkgconfig ./configure \
		--enable-cross-compile --arch="$arch" --target-os=linux \
		--cc="$tools/cc" --cxx="$tools/c++" --ar="$tools/ar" --ranlib="$tools/ranlib" --nm="$nm -g" \
		--host-cc="${HOST_CC:-cc}" --pkg-config-flags=--static \
		--extra-cflags="-I$prefix/include -O2" --extra-ldflags="-L$prefix/lib -static -s" \
		--disable-everything --disable-autodetect --enable-pthreads $asm \
		--disable-doc --disable-debug --disable-ffplay --disable-ffprobe \
		--disable-avdevice --disable-swscale --disable-stripping \
		--enable-version3 --enable-libopus --enable-openssl \
		--enable-protocol=pipe,file,http,https,tls,tcp,crypto \
		--enable-demuxer=mov,matroska,ogg,mp3,aac,flac,wav,hls,mpegts,ac3,eac3 \
		--enable-decoder=aac,aac_latm,mp3,mp3float,opus,vorbis,flac,alac,ac3,eac3,pcm_s16le,pcm_s16be,pcm_s24le,pcm_s32le,pcm_f32le \
		--enable-parser=aac,aac_latm,mpegaudio,opus,vorbis,flac,ac3 \
		--enable-encoder=libopus --enable-muxer=ogg \
		--enable-filter=abuffer,abuffersink,aformat,anull,aresample >"$work/configure.log" &&
	make -j"$jobs" ffmpeg >"$work/make.log" 2>&1)

cp "$work/src/ffmpeg/ffmpeg" "$out"
echo "build-ffmpeg: $out"
