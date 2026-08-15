# `native_build_id` fixtures

Real Android NDK shared objects used by `android_native_build_id_test.go`.

Each ABI directory holds a **pair** of libraries linked from the identical
`lib.c` with two different explicit GNU build-ids:

| file | `NT_GNU_BUILD_ID` descriptor |
| --- | --- |
| `buildid_a.so` | 20 x `0xAA` |
| `buildid_b.so` | 20 x `0xBB` |

Each pair differs in **exactly 20 bytes**, all of them inside the build-id note
descriptor. That is the same shape as the `libdartjni.so` drift that blocked A3,
and the descriptors land at the same file offsets the A3 evidence reported:

| ABI | descriptor offset |
| --- | --- |
| `arm64-v8a` | `0x2e0` |
| `armeabi-v7a` | `0x1fc` |
| `x86_64` | `0x2e0` |

The offsets differ by ABI, which is why the tests can prove that a fixed-offset
implementation is wrong rather than merely asserting it.

## Regenerating byte-for-byte

NDK `27.1.12297006` (`toolchains/llvm/prebuilt/darwin-x86_64/bin/clang`, LLD).
Run from this directory:

```sh
NDK=~/Library/Android/sdk/ndk/27.1.12297006
BIN="$NDK/toolchains/llvm/prebuilt/darwin-x86_64/bin"
A=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
B=bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb

build() { # $1 abi dir, $2 clang target, $3 build-id hex, $4 output file
  mkdir -p "$1"
  "$BIN/clang" --target="$2" -shared -fPIC -O2 -o "$1/$4" lib.c \
    -Wl,--build-id=0x"$3" -Wl,--strip-all
}

build arm64-v8a   aarch64-linux-android21    "$A" buildid_a.so
build arm64-v8a   aarch64-linux-android21    "$B" buildid_b.so
build armeabi-v7a armv7a-linux-androideabi21 "$A" buildid_a.so
build armeabi-v7a armv7a-linux-androideabi21 "$B" buildid_b.so
build x86_64      x86_64-linux-android21     "$A" buildid_a.so
build x86_64      x86_64-linux-android21     "$B" buildid_b.so
```

`-Wl,--strip-all` matches how Android ships native libraries, and keeps the
fixtures under 5 KiB each. No `-g`, so no source paths are embedded.

## Fixtures that are NOT here

The "no build-id note at all" control is not stored here. It reads the real
shipped libraries at
`packages/soroq_flutter/android/src/main/jniLibs/<abi>/libsoroq_runtime_jni.so`,
which carry `.note.android.ident` but no `NT_GNU_BUILD_ID`.
