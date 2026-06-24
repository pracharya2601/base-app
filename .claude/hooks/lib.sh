#!/usr/bin/env bash
# lib.sh — shared helpers for the base-app Claude Code orchestrator hooks.
#
# Sourced by every hook script. Assumes the caller has already read the hook's
# stdin JSON into the variable $INPUT before sourcing or before calling jget.
#
# Design rules:
#   - Hooks must NEVER crash the harness. We avoid `set -e`; every code path
#     exits 0 unless it INTENTIONALLY blocks (Stop/PostToolUse decision:block).
#   - All model-facing output is built as a plain string, then emitted as JSON
#     via jq so additionalContext is always correctly escaped.
#   - A global kill-switch (CLAUDE_ORCH_DISABLE=1) lets you bypass everything.

PROJECT_DIR="${CLAUDE_PROJECT_DIR:-$(pwd)}"
STATE_DIR="$PROJECT_DIR/.claude/orchestrator/state"
mkdir -p "$STATE_DIR" 2>/dev/null

# orch_disabled — true when the operator has flipped the kill-switch.
orch_disabled() { [ "${CLAUDE_ORCH_DISABLE:-0}" = "1" ]; }

# jget '<jq filter>' — read a field out of the hook's stdin JSON ($INPUT).
# Returns empty string (never errors) when missing or when INPUT is unset.
jget() { printf '%s' "${INPUT:-}" | jq -r "$1 // empty" 2>/dev/null; }

# now — UTC timestamp for the decision log.
now() { date -u +"%Y-%m-%dT%H:%M:%SZ"; }

# olog '<event>' '<detail>' — append one line to the orchestrator decision log.
olog() {
  printf '%s\t%s\t%s\t%s\n' "$(now)" "${SESSION_ID:-?}" "$1" "$2" \
    >> "$STATE_DIR/decisions.log" 2>/dev/null
}

# --- model-facing JSON emitters -------------------------------------------
# Each emits the correct hookSpecificOutput shape for its event and exits 0.

emit_context() { # $1=event name  $2=context text  -> additionalContext (non-blocking)
  jq -n --arg e "$1" --arg c "$2" \
    '{hookSpecificOutput:{hookEventName:$e, additionalContext:$c}}'
  exit 0
}

emit_block_stop() { # $1=reason  -> forces Claude to keep working
  jq -n --arg r "$1" '{decision:"block", reason:$r}'
  exit 0
}

emit_block_tool() { # $1=reason  -> stops the agentic loop after a tool ran
  jq -n --arg r "$1" '{decision:"block", reason:$r}'
  exit 0
}

emit_deny_tool() { # $1=reason  -> PreToolUse: refuse the tool before it runs
  jq -n --arg r "$1" \
    '{hookSpecificOutput:{hookEventName:"PreToolUse", permissionDecision:"deny", permissionDecisionReason:$r}}'
  exit 0
}

# emit_nothing — allow the action, inject nothing.
emit_nothing() { exit 0; }
