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
# Used only by the polyglot benchmark: AssertJ for the official Java tests,
# and the libraries two exercises import.
fetch org/assertj/assertj-core 3.25.1 assertj-core-3.25.1.jar a3a6e79792f041b61507be9f2a211a81297d7b0f
fetch net/bytebuddy/byte-buddy 1.14.11 byte-buddy-1.14.11.jar 725602eb7c8c56b51b9c21f273f9df5c909d9e7d
fetch org/json/json 20190722 json-20190722.jar 07bce7bacf0ab5e9f894d307a3de8b7f540064d5
fetch io/reactivex/rxjava2/rxjava 2.2.12 rxjava-2.2.12.jar fc5cb9ad87c312f559a38253aa4d897f549f50dc
fetch org/reactivestreams/reactive-streams 1.0.2 reactive-streams-1.0.2.jar 323964c36556eb0e6209f65c1cef72b53b461ab8
ln -sf javaparser-core-3.28.2.jar javaparser-core.jar
