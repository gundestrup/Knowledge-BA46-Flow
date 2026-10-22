#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TIER="${1:-}"
case "$TIER" in
  basic|intermediate|e2e)
    CORPUS="$ROOT/example-knowledge/$TIER"
    EXPECTED="$CORPUS/expected.json"
    ;;
  llm-eval)
    CORPUS=""
    EXPECTED="$ROOT/example-knowledge/llm-eval/expected.json"
    SELECTION="$ROOT/example-knowledge/llm-eval/selection.json"
    SOURCE_ROOT="${REAL_KB_ROOT:-$ROOT/../engineering-memory}"
    ;;
  *)
    echo "usage: $0 basic|intermediate|e2e|llm-eval" >&2
    exit 2
    ;;
esac

if [[ ! -f "$EXPECTED" ]]; then
  echo "missing golden manifest: $EXPECTED" >&2
  exit 1
fi
if [[ "$TIER" == llm-eval && ! -f "$SELECTION" ]]; then
  echo "missing source allowlist: $SELECTION" >&2
  exit 1
fi
if ! command -v ollama >/dev/null 2>&1; then
  echo "ollama is required for this opt-in live-model test" >&2
  exit 1
fi

ANALYSIS_MODEL="${KBM_OLLAMA_MODEL:-${OLLAMA_MODEL:-gemma3:12b}}"
FAST_MODEL="${KBM_OLLAMA_MODEL_FAST:-${OLLAMA_MODEL_FAST:-phi4-mini:latest}}"
EMBED_MODEL="embeddinggemma:300m"
if [[ "${OLLAMA_NO_ENRICH:-0}" == 1 ]]; then
  FAST_MODEL=""
fi
OLLAMA_URL="${OLLAMA_BASE_URL:-http://127.0.0.1:11434}"
if [[ ! "$OLLAMA_URL" =~ ^http://(127\.0\.0\.1|localhost|\[::1\]):([0-9]{1,5})/?$ ]]; then
  echo "live fixture tests require an HTTP loopback host, numeric port, and no credentials/path" >&2
  exit 1
fi
OLLAMA_HOST="${BASH_REMATCH[1]}:${BASH_REMATCH[2]}"
OLLAMA_PORT="$((10#${BASH_REMATCH[2]}))"
if ((OLLAMA_PORT < 1 || OLLAMA_PORT > 65535)); then
  echo "OLLAMA_BASE_URL port is out of range: $OLLAMA_URL" >&2
  exit 1
fi
export OLLAMA_BASE_URL="$OLLAMA_URL"
export OLLAMA_HOST

INSTALLED_MODELS="$(ollama list)"
for model in "$ANALYSIS_MODEL" "$EMBED_MODEL"; do
  if ! printf '%s\n' "$INSTALLED_MODELS" | awk -v wanted="$model" 'NR > 1 && $1 == wanted { found = 1 } END { exit !found }'; then
    echo "required local Ollama model is not installed: $model" >&2
    exit 1
  fi
done
if [[ -n "$FAST_MODEL" ]] &&
  ! printf '%s\n' "$INSTALLED_MODELS" | awk -v wanted="$FAST_MODEL" 'NR > 1 && $1 == wanted { found = 1 } END { exit !found }'; then
  echo "fast Ollama model is not installed: $FAST_MODEL" >&2
  exit 1
fi

RUNS="$ROOT/data/example-runs"
mkdir -p "$RUNS"
STAMP="$(date +%Y%m%d-%H%M%S)"
RUN_DIR="$RUNS/$STAMP-$TIER"
mkdir "$RUN_DIR"
LOG="$RUN_DIR/run.log"
BEFORE="$RUN_DIR/files-before.sha256"
AFTER="$RUN_DIR/files-after.sha256"
cp "$EXPECTED" "$RUN_DIR/expected.json"
if [[ "$TIER" == llm-eval ]]; then
  CORPUS="$RUN_DIR/corpus"
  (
    cd "$ROOT"
    go run ./scripts/prepare_example_llm_eval \
      -source-root "$SOURCE_ROOT" \
      -manifest "$SELECTION" \
      -destination "$CORPUS"
  ) > "$RUN_DIR/source-files.sha256"
fi

# Merge the tier's golden exclusion config into the starter config so live
# runs scan the same document set as the fake-model corpus tests.
go run "$ROOT/scripts/merge_example_config.go" \
  -base "$ROOT/example-configs/config.example-data.json" \
  -expected "$EXPECTED" \
  -out "$RUN_DIR/config.json"

hash_markdown() {
  (
    cd "$CORPUS"
    find . -type f \( -iname '*.md' -o -iname '*.markdown' \) -exec shasum -a 256 {} + |
      LC_ALL=C sort
  )
}

hash_markdown > "$BEFORE"
export KBM_REPOSITORY_PATH="$CORPUS"
export KBM_DATABASE_PATH="$RUN_DIR/state.db"
export KBM_OLLAMA_MODEL="$ANALYSIS_MODEL"
if [[ -z "$FAST_MODEL" ]]; then
  unset KBM_OLLAMA_MODEL_FAST || true
else
  export KBM_OLLAMA_MODEL_FAST="$FAST_MODEL"
fi

echo "Tier: $TIER"
echo "Analysis model: $ANALYSIS_MODEL"
echo "Enrichment model: ${FAST_MODEL:-disabled}"
echo "Enrichment mode: ${KBM_ENRICHMENT_MODE:-local-first}"
echo "Repository: $CORPUS"
echo "Database: $RUN_DIR/state.db"
echo "Config: $RUN_DIR/config.json"
echo "Log: $LOG"
echo "Analysis-only: no suggestions will be accepted or applied."

set +e
(
  cd "$ROOT"
  go run ./cmd/KBM-flow --config "$RUN_DIR/config.json" run
) 2>&1 | tee "$LOG"
RUN_STATUS="${PIPESTATUS[0]}"
set -e

hash_markdown > "$AFTER"
AUDIT_STATUS=0
if ! cmp -s "$BEFORE" "$AFTER"; then
  echo "Markdown hash audit FAILED; differences:" | tee -a "$LOG" >&2
  diff -u "$BEFORE" "$AFTER" | tee -a "$LOG" >&2 || true
  AUDIT_STATUS=1
else
  echo "Markdown hash audit passed." | tee -a "$LOG"
fi
printf 'run_exit=%s\naudit_exit=%s\n' "$RUN_STATUS" "$AUDIT_STATUS" > "$RUN_DIR/status.txt"

if [[ "$RUN_STATUS" -ne 0 ]]; then
  exit "$RUN_STATUS"
fi
exit "$AUDIT_STATUS"
