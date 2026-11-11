#!/bin/sh
# Runs the Java adapter. Compiles it once into .classes when the source is newer.
set -e
HERE=$(cd "$(dirname "$0")" && pwd)
JAVA_BIN=${JAVA_HOME:+$JAVA_HOME/bin}
JAVA_BIN=${JAVA_BIN:-/opt/homebrew/opt/openjdk/bin}
CP="$HERE/lib/javaparser-core.jar"
OUT="$HERE/.classes/java"
if [ ! -f "$OUT/JavaAdapter.class" ] || [ "$HERE/JavaAdapter.java" -nt "$OUT/JavaAdapter.class" ]; then
  mkdir -p "$OUT"
  "$JAVA_BIN/javac" -nowarn -d "$OUT" -cp "$CP" "$HERE/JavaAdapter.java" >&2
fi
exec "$JAVA_BIN/java" -Dgraybox.lib="$HERE/lib" -cp "$OUT:$CP" JavaAdapter "$@"
