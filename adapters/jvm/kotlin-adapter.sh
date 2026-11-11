#!/bin/sh
# Runs the Kotlin adapter. Builds it once into .classes when the source is newer.
set -e
HERE=$(cd "$(dirname "$0")" && pwd)
KOTLINC=$(command -v kotlinc || true)
if [ -n "$KOTLIN_HOME" ]; then KOTLINC="$KOTLIN_HOME/bin/kotlinc"; fi
[ -n "$KOTLINC" ] || { echo '{"error":"kotlinc not found; install Kotlin or set KOTLIN_HOME"}'; exit 0; }
# Resolve the distribution's lib directory (Homebrew symlinks kotlinc).
REAL=$(python3 -c 'import os,sys;print(os.path.realpath(sys.argv[1]))' "$KOTLINC")
D=$(dirname "$REAL")
if [ -d "$D/../lib" ]; then KLIB=$(cd "$D/../lib" && pwd); else KLIB=$(cd "$D/../libexec/lib" && pwd); fi
JAVA_BIN=${JAVA_HOME:+$JAVA_HOME/bin}
JAVA_BIN=${JAVA_BIN:-/opt/homebrew/opt/openjdk/bin}
OUT="$HERE/.classes/kotlin"
if [ ! -f "$OUT/KotlinAdapterKt.class" ] || [ "$HERE/KotlinAdapter.kt" -nt "$OUT/KotlinAdapterKt.class" ]; then
  mkdir -p "$OUT"
  "$KOTLINC" -nowarn -cp "$KLIB/kotlin-compiler.jar" -d "$OUT" "$HERE/KotlinAdapter.kt" >&2
fi
exec "$JAVA_BIN/java" -Dgraybox.lib="$HERE/lib" -Dgraybox.kotlinLib="$KLIB" \
  -cp "$OUT:$KLIB/*" KotlinAdapterKt "$@"
