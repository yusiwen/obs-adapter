#!/usr/bin/env bash
# End to end check: real OBS Java SDK -> obs-adapter -> MinIO.
#
#   test/e2e/run.sh [env-file] [obs-bucket] [minio-bucket]
#
# Requires: go, javac/java, an env file with OBS_ADAPTER_MINIO_* (see
# configs/minio.env.example), and the OBS Java SDK jar (points OBS_SDK_JAR at it).
set -euo pipefail

cd "$(dirname "$0")/../.."
ROOT=$PWD

ENV_FILE=${1:-$HOME/.config/obs-adapter/minio.env}
OBS_BUCKET=${2:-legacy-bucket}
MINIO_BUCKET=${3:-test}
SDK_JAR=${OBS_SDK_JAR:-}
PORT=18081
BASE=http://127.0.0.1:$PORT

[[ -f $ENV_FILE ]] || { echo "missing env file: $ENV_FILE" >&2; exit 2; }
[[ -n $SDK_JAR && -f $SDK_JAR ]] || { echo "set OBS_SDK_JAR to esdk-obs-java-bundle-<version>.jar" >&2; exit 2; }

# shellcheck disable=SC1090
set -a; . "$ENV_FILE"; set +a
: "${OBS_ADAPTER_MINIO_ENDPOINT:?endpoint missing in $ENV_FILE}"

echo "==> building adapter"
go build -o bin/obs-adapter ./cmd/obs-adapter

ADAPTER_AK=AKTEST1234567890
ADAPTER_SK=SKtestsecretkey0123456789

echo "==> starting adapter on $BASE (upstream ${OBS_ADAPTER_MINIO_ENDPOINT})"
./bin/obs-adapter -listen 127.0.0.1:$PORT -domain obs.test \
  -ak "$ADAPTER_AK" -sk "$ADAPTER_SK" \
  -bucket-alias "$OBS_BUCKET=$MINIO_BUCKET" \
  -auto-create-bucket=true -log-level info > /tmp/obs-adapter-e2e.log 2>&1 &
ADAPTER_PID=$!
trap 'kill $ADAPTER_PID 2>/dev/null || true' EXIT

for _ in $(seq 1 50); do
  if curl -fsS -m 1 "$BASE/__health" >/dev/null 2>&1; then break; fi
  sleep 0.2
done
curl -fsS "$BASE/__health"; echo

echo "==> compiling java clients"
mkdir -p test/e2e/out
javac -cp "$SDK_JAR" -d test/e2e/out test/e2e/*.java

status=0
run() {
  echo
  echo "==> $*"
  java -cp "$SDK_JAR:test/e2e/out" "$@" || status=1
}

run ObsE2E "$BASE" "$OBS_BUCKET" vhost
run ObsE2E "$BASE" "$OBS_BUCKET" path
run ObsLarge "$OBS_BUCKET"
run ObsNegative "$OBS_BUCKET"

echo
if [[ $status -eq 0 ]]; then
  echo "E2E PASSED (adapter log: /tmp/obs-adapter-e2e.log)"
else
  echo "E2E FAILED (adapter log: /tmp/obs-adapter-e2e.log)" >&2
fi
exit $status
