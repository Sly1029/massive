#!/usr/bin/env bash
# Run every Go fuzz target for FUZZ_TIME each. Targets are discovered, so a new
# Fuzz function is fuzzed without editing this script. A crash does not stop
# the campaign: every target runs, every failure is reported, and the exit
# status is nonzero if any target failed.
#
#   ./scripts/fuzz.sh                       # all packages
#   ./scripts/fuzz.sh ./internal/plan       # selected packages
#   FUZZ_TIME=5m FUZZ_WORKERS=4 ./scripts/fuzz.sh
set -uo pipefail
cd "$(dirname "$0")/.."

fuzz_time="${FUZZ_TIME:-30s}"
workers="${FUZZ_WORKERS:-2}"
minimize_time="${FUZZ_MINIMIZE_TIME:-30s}"

if [ "$#" -gt 0 ]; then
  packages=("$@")
else
  mapfile -t packages < <(git ls-files '*_test.go' | xargs grep -l '^func Fuzz' | xargs -n1 dirname | sort -u | sed 's|^|./|')
fi

failures=()
for package in "${packages[@]}"; do
  if ! listing="$(go test -list '^Fuzz' "$package")"; then
    failures+=("${package} (test build)")
    continue
  fi
  mapfile -t targets < <(grep '^Fuzz' <<<"$listing")
  for target in "${targets[@]}"; do
    echo "::group::fuzz ${package} ${target}"
    if ! go test "$package" -run '^$' -fuzz "^${target}\$" \
      -fuzztime="$fuzz_time" -fuzzminimizetime="$minimize_time" -parallel="$workers"; then
      failures+=("${package} ${target}")
    fi
    echo "::endgroup::"
  done
done

if [ "${#failures[@]}" -eq 0 ]; then
  echo "all fuzz targets passed"
  exit 0
fi

{
  echo "### Fuzz failures"
  echo
  for failure in "${failures[@]}"; do
    echo "- \`${failure}\`"
  done
  echo
  echo "Minimized inputs are under each package's \`testdata/fuzz/<target>/\`."
  echo "Commit them with the fix: \`go test ./...\` replays them as regressions."
} | tee -a "${GITHUB_STEP_SUMMARY:-/dev/null}"
exit 1
