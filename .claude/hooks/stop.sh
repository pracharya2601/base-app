#!/usr/bin/env bash
# stop.sh — Stop hook. THE VERIFY-GATE + MEMORY REMINDER.
#
# Two jobs when Claude tries to finish:
#   1. BUILD GATE (blocking): if Go code it touched won't compile or isn't
#      gofmt-clean, refuse to stop and hand back the exact failure. Hard
#      correctness only — review/verify are suggested, never forced, so it can't
#      loop. Skipped on re-entry (stop_hook_active) to stay loop-safe.
#   2. MEMORY REMINDER (non-blocking): on a substantive turn, nudge Claude to
#      persist any durable fact/feedback/decision to its project memory before
#      finishing. Never blocks (memory is a judgment call), so it's safe to run
#      even on re-entry.

INPUT=$(cat)
source "${CLAUDE_PROJECT_DIR:-$(pwd)}/.claude/hooks/lib.sh"
orch_disabled && exit 0

SESSION_ID=$(jget '.session_id')
ACTIVE=$(jget '.stop_hook_active')
cd "$PROJECT_DIR" 2>/dev/null || exit 0

LIST="$STATE_DIR/edited-go-$SESSION_ID.list"
SUBSTANTIVE_FLAG="$STATE_DIR/turn-substantive-$SESSION_ID"

# --- 1) BUILD GATE (only on first Stop of the turn; skipped on re-entry) ---
GREEN_NOTE=""
if [ "$ACTIVE" != "true" ] && [ -s "$LIST" ] && command -v go >/dev/null 2>&1; then
  # Formatting check on the files we actually touched.
  UNFMT=""
  while IFS= read -r f; do
    [ -f "$f" ] || continue
    [ -n "$(gofmt -l "$f" 2>/dev/null)" ] && UNFMT="$UNFMT
  - $f"
  done < "$LIST"

  if [ -n "$UNFMT" ]; then
    olog "stop_blocked" "gofmt"
    emit_block_stop "Before finishing: these Go files you edited are not gofmt-clean:$UNFMT
Run \`gofmt -w\` on them, then stop again."
  fi

  # Build gate. Timeout so a slow/first build can't hang the stop; on timeout we
  # allow the stop (advisory only) rather than trap Claude.
  BUILD_LOG=$(mktemp 2>/dev/null || echo /tmp/orch-build.log)
  if command -v timeout >/dev/null 2>&1; then
    timeout 150 go build ./... >"$BUILD_LOG" 2>&1; RC=$?
    [ "$RC" = "124" ] && { olog "stop_build_timeout" ""; rm -f "$BUILD_LOG"; RC=0; GREEN_NOTE="(build skipped: timed out)"; }
  else
    go build ./... >"$BUILD_LOG" 2>&1; RC=$?
  fi

  if [ "$RC" != "0" ]; then
    ERR=$(tail -25 "$BUILD_LOG"); rm -f "$BUILD_LOG"
    olog "stop_blocked" "build_rc=$RC"
    emit_block_stop "Before finishing: \`go build ./...\` fails. Fix it, then stop again.
(If this is pre-existing breakage unrelated to your change, say so explicitly and stop again to override.)

--- build output (tail) ---
$ERR"
  fi
  rm -f "$BUILD_LOG"
  GOFILES=$(wc -l < "$LIST" | tr -d ' ')
  olog "stop_green" "build_ok files=$GOFILES"
  [ -z "$GREEN_NOTE" ] && GREEN_NOTE="✓ go build clean ($GOFILES Go file(s) edited). Consider the code-reviewer or verifier subagent / \`/code-review\` before shipping."
fi

# --- 2) MEMORY REMINDER (non-blocking; runs even on re-entry) ---------------
MEM_NOTE=""
if [ -f "$SUBSTANTIVE_FLAG" ]; then
  rm -f "$SUBSTANTIVE_FLAG" 2>/dev/null
  olog "stop_memory_nudge" ""
  MEM_NOTE="Memory check — if this turn established something durable (a user preference, feedback on how to work, a project goal/constraint, or a key decision that isn't already in the code/CLAUDE.md), persist it now: write the memory file AND add its one-line MEMORY.md index entry. Skip if nothing non-obvious came up; don't save what the repo already records."
fi

# --- emit a single combined systemMessage if there's anything to say ---------
MSG=""
[ -n "$GREEN_NOTE" ] && MSG="Orchestrator gate: $GREEN_NOTE"
if [ -n "$MEM_NOTE" ]; then
  [ -n "$MSG" ] && MSG="$MSG
"
  MSG="$MSG$MEM_NOTE"
fi
[ -n "$MSG" ] && jq -n --arg m "$MSG" '{systemMessage:$m}'
exit 0
