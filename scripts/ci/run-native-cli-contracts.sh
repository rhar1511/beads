#!/usr/bin/env bash
# Run only the selected mandatory private-Dolt CLI contracts, never a whole
# ordinary shard under the real-Dolt opt-in. A missing prerequisite is a failure.
set -euo pipefail
selected=${1:?selected test manifest required}
output=${2:?private evidence output directory required}
: "${BEADS_TEST_BD_BINARY:?exact candidate executable required}"
[[ "$BEADS_TEST_BD_BINARY" = /* && -x "$BEADS_TEST_BD_BINARY" ]]
command -v dolt >/dev/null
mkdir -p "$output"
python3 scripts/ci/audit-native-cli-contracts.py validate "$selected"
run_regex="^($(paste -sd '|' "$selected"))$"
sha256sum "$BEADS_TEST_BD_BINARY" >"$output/binary.sha256"
go version -m "$BEADS_TEST_BD_BINARY" >"$output/binary-build-info.txt"
git rev-parse HEAD >"$output/source-head.txt"
# PROXIED_LOCAL suppresses only TestMain's unrelated shared Docker fixture;
# all selected roots still require and start their own real private Dolt server.
if env BEADS_TEST_ENV_RUN_DOLT=1 BEADS_TEST_PROXIED_LOCAL=1 BEADS_TEST_SKIP= \
  go test -json -race -tags integration,gms_pure_go -count=1 -timeout=30m \
    -run "$run_regex" ./cmd/bd 2>"$output/native.stderr.log" | tee "$output/native.jsonl"; then
  cat "$output/native.stderr.log" >&2
else
  status=$?
  cat "$output/native.stderr.log" >&2
  exit "$status"
fi
python3 scripts/ci/audit-native-cli-contracts.py audit "$selected" "$output/native.jsonl" "$output/proof.json"
