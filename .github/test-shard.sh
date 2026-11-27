#!/bin/sh
# One slice of the test suite (tests.yml runs SHARDS of these side by side, each in its own process, so the
# tests' shared state never meets). Run inside the Go image with the program's folder as the working
# directory. Slice 0 also runs gofmt and go vet. The output goes to /out/out.txt.
#
# Tests are dealt out by how long they took (.github/test-times.txt: "name seconds", made from a CI run's
# "--- PASS" lines), longest first, each to the slice with the least so far, so the slices end together. A test
# not in that file counts as 5 s. Every slice prints each test's time, so the file can be made again.
set -e
: "${SHARD:?}" "${SHARDS:?}"
if [ "$SHARD" = 0 ]; then
  unformatted=$(gofmt -l .)
  if [ -n "$unformatted" ]; then echo "gofmt wants these files changed:"; echo "$unformatted"; exit 1; fi
  go vet ./...
fi
go test -list '.*' ./... | grep -E '^(Test|Fuzz|Example)' | sort -u > /out/all.txt
times=../.github/test-times.txt
[ -f "$times" ] || times=/dev/null
awk -v s="$SHARD" -v n="$SHARDS" '
  FNR == NR { t[$1] = $2; next }
  { name[++k] = $1; w[k] = ($1 in t) ? t[$1] + 0 : 5 }
  END {
    # longest first (a simple selection sort: a few hundred tests), each to the lightest slice
    for (i = 1; i <= k; i++) order[i] = i
    for (i = 1; i <= k; i++) { m = i; for (j = i + 1; j <= k; j++) if (w[order[j]] > w[order[m]] || (w[order[j]] == w[order[m]] && name[order[j]] < name[order[m]])) m = j; x = order[i]; order[i] = order[m]; order[m] = x }
    for (i = 0; i < n; i++) load[i] = 0
    for (i = 1; i <= k; i++) { b = 0; for (j = 1; j < n; j++) if (load[j] < load[b]) b = j; load[b] += w[order[i]]; if (b == s) print name[order[i]] }
  }' "$times" /out/all.txt > /out/mine.txt
echo "slice $SHARD of $SHARDS: $(wc -l < /out/mine.txt) of $(wc -l < /out/all.txt) tests"
[ -s /out/mine.txt ] || exit 0
pattern="^($(paste -sd '|' /out/mine.txt))\$"
set +e
go test -count=1 -timeout 30m -v -run "$pattern" ./... > /out/out.txt 2>&1
code=$?
grep -E "^(--- FAIL|--- SKIP|FAIL|ok|panic:)" /out/out.txt
echo "::group::Time of each test (for .github/test-times.txt)"
grep -E "^--- PASS: " /out/out.txt
echo "::endgroup::"
if [ $code -ne 0 ]; then
  echo "::group::Output of the failed tests"
  awk '/^=== RUN/{buf=""} {buf=buf $0 "\n"} /^--- FAIL/{printf "%s", buf}' /out/out.txt | tail -400
  echo "::endgroup::"
fi
echo "passed: $(grep -c '^--- PASS' /out/out.txt), failed: $(grep -c '^--- FAIL' /out/out.txt), skipped: $(grep -c '^--- SKIP' /out/out.txt)"
exit $code
