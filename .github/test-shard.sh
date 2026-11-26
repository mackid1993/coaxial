#!/bin/sh
# One slice of the test suite (tests.yml runs SHARDS of these side by side, each in its own process, so the
# tests' shared state never meets). Run inside the Go image with the program's folder as the working
# directory. Slice 0 also runs gofmt and go vet. The output goes to /out/out.txt.
set -e
: "${SHARD:?}" "${SHARDS:?}"
if [ "$SHARD" = 0 ]; then
  unformatted=$(gofmt -l .)
  if [ -n "$unformatted" ]; then echo "gofmt wants these files changed:"; echo "$unformatted"; exit 1; fi
  go vet ./...
fi
# Every test, fuzz seed and example by name, dealt out in turn so each slice gets a fair share.
go test -list '.*' ./... | grep -E '^(Test|Fuzz|Example)' | sort -u > /out/all.txt
awk -v s="$SHARD" -v n="$SHARDS" '(NR - 1) % n == s' /out/all.txt > /out/mine.txt
echo "slice $SHARD of $SHARDS: $(wc -l < /out/mine.txt) of $(wc -l < /out/all.txt) tests"
[ -s /out/mine.txt ] || exit 0
pattern="^($(paste -sd '|' /out/mine.txt))\$"
set +e
go test -count=1 -timeout 30m -v -run "$pattern" ./... > /out/out.txt 2>&1
code=$?
grep -E "^(--- FAIL|--- SKIP|FAIL|ok|panic:)" /out/out.txt
if [ $code -ne 0 ]; then
  echo "::group::Output of the failed tests"
  awk '/^=== RUN/{buf=""} {buf=buf $0 "\n"} /^--- FAIL/{printf "%s", buf}' /out/out.txt | tail -400
  echo "::endgroup::"
fi
echo "passed: $(grep -c '^--- PASS' /out/out.txt), failed: $(grep -c '^--- FAIL' /out/out.txt), skipped: $(grep -c '^--- SKIP' /out/out.txt)"
exit $code
