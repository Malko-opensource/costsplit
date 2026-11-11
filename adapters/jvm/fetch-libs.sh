#!/bin/sh
# Downloads the jars the JVM adapters need from Maven Central and checks SHA-1.
set -e
cd "$(dirname "$0")" && mkdir -p lib && cd lib
fetch() { # path version jar sha1
  [ -f "$3" ] || curl -sSfLO "https://repo1.maven.org/maven2/$1/$2/$3"
  echo "$4  $3" | shasum -c -
}
fetch com/github/javaparser/javaparser-core 3.28.2 javaparser-core-3.28.2.jar 0f585a590072b20c6b4be3c35291b6e66a88b995
fetch org/junit/platform/junit-platform-console-standalone 6.1.3 junit-platform-console-standalone-6.1.3.jar bb65bef57072d36433e8a863016163858c926113
ln -sf javaparser-core-3.28.2.jar javaparser-core.jar
