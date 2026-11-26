#!/bin/sh
# Smoke test of a built Coaxial image: the program in the image itself is started (its proxy part, without the
# VPN) in front of a stand-in provider, and a stand-in player logs in and watches a live channel through it,
# as MPEG-TS and as HLS, and opens a catch-up as HLS.
# The image build runs this before the image is given its tag; an image that fails it is never published.
#
#   .github/smoke/smoke.sh <image>
set -u
IMAGE="${1:?usage: smoke.sh <image>}"
HERE="$(cd "$(dirname "$0")" && pwd)"
NET="coaxial-smoke-$$"
fail=0

cleanup() {
  docker rm -f "$NET-proxy" "$NET-provider" >/dev/null 2>&1
  docker network rm "$NET" >/dev/null 2>&1
  rm -f "${SETTINGS:-}"
}
trap cleanup EXIT

# (addresses outside the home-network ranges, as a provider's are)
docker network create --subnet 198.18.210.0/24 "$NET" >/dev/null || exit 1

echo "image: $IMAGE"
version="$(docker run --rm --entrypoint /usr/local/bin/coaxial "$IMAGE" version)" || fail=1
echo "version: ${version:-none}"
case "$version" in v20*) ;; *) echo "FAIL  the program in the image does not say its version"; fail=1 ;; esac

docker run -d --name "$NET-provider" --network "$NET" --ip 198.18.210.10 \
  -v "$HERE/smoke.py:/smoke.py:ro" python:3-alpine python3 -I /smoke.py serve >/dev/null || exit 1

SETTINGS="$(mktemp)"
# (the stand-in provider twice: once for players that watch live TV as MPEG-TS, once for players that use HLS)
printf '%s' '{"player":{"user":"tv","password":"easy"},"providers":[{"name":"Smoke","url":"http://198.18.210.10:8080","user":"realU","password":"realP","port":"18095","connections":"2","live_tv":"ts"},{"name":"Smoke HLS","url":"http://198.18.210.10:8080","user":"realU","password":"realP","port":"18096","connections":"2","live_tv":"hls"}]}' > "$SETTINGS"
chmod 644 "$SETTINGS"
# (the proxy part reads its settings on its standard input, as when the launcher starts it)
docker run -d --name "$NET-proxy" --network "$NET" --ip 198.18.210.20 -v "$SETTINGS:/smoke-settings.json:ro" \
  --entrypoint /bin/sh "$IMAGE" -c 'exec /usr/local/bin/coaxial proxy < /smoke-settings.json' >/dev/null || exit 1

docker run --rm --network "$NET" -v "$HERE/smoke.py:/smoke.py:ro" python:3-alpine \
  python3 -I /smoke.py check http://198.18.210.20:18095 http://198.18.210.20:18096 http://198.18.210.10:8080 || fail=1

if docker logs "$NET-proxy" 2>&1 | grep -E "^panic:|^goroutine [0-9]+ \[|fatal error:"; then
  echo "FAIL  the program crashed"; fail=1
fi
if [ "$(docker inspect -f '{{.State.Running}}' "$NET-proxy" 2>/dev/null)" != "true" ]; then
  echo "FAIL  the program is no longer running"; fail=1
fi
if [ $fail -ne 0 ]; then
  echo "---- the program's log ----"
  docker logs "$NET-proxy" 2>&1 | tail -60
fi
exit $fail
