#!/usr/bin/env bash
# session_start.sh — SessionStart hook.
# Injects a compact snapshot of repo state + announces that the orchestrator's
# gates are live, so Claude starts every session already grounded.

INPUT=$(cat)
source "${CLAUDE_PROJECT_DIR:-$(pwd)}/.claude/hooks/lib.sh"
orch_disabled && exit 0

SESSION_ID=$(jget '.session_id')
SOURCE=$(jget '.source')
cd "$PROJECT_DIR" 2>/dev/null || exit 0

BRANCH=$(git rev-parse --abbrev-ref HEAD 2>/dev/null || echo "(no git)")
DIRTY=$(git status --porcelain 2>/dev/null | wc -l | tr -d ' ')
COMMITS=$(git log --oneline -5 2>/dev/null)
CHANGED=$(git status --porcelain 2>/dev/null | head -12)

# Fresh session => clear last session's per-turn tracking + flags.
rm -f "$STATE_DIR/edited-go-$SESSION_ID.list" \
      "$STATE_DIR/turn-substantive-$SESSION_ID" 2>/dev/null
olog "session_start" "source=$SOURCE branch=$BRANCH dirty=$DIRTY"

CTX="ORCHESTRATOR ACTIVE — lean, deterministic gates only (.claude/orchestrator/README.md).
This session:
  • Safety net (automatic): Go edits are gofmt-checked on write; the Stop gate blocks finishing while \`go build ./...\` is broken, then nudges to persist durable memory.
  • Specialists (pull-based, use when the task warrants — NOT forced): meta-prompter (scope a fuzzy/large task), code-reviewer (review the diff before shipping), verifier (prove a change runs). Invoke them via the Agent tool yourself when relevant.
  • Heavy/parallel jobs (full audit, cross-package refactor, large PR review): consider a multi-agent Workflow — work one context can't hold. See the README.
  • No per-prompt routing tax: simple asks just get answered. Still honor CLAUDE.md + recalled memory, and ASK before granting access, minting keys, paid AI spend, or destructive/prod ops.

REPO SNAPSHOT ($SOURCE)
  branch: $BRANCH    uncommitted files: $DIRTY
  recent commits:
$(printf '%s' "$COMMITS" | sed 's/^/    /')"

if [ "$DIRTY" != "0" ] && [ -n "$CHANGED" ]; then
  CTX="$CTX
  working-tree changes:
$(printf '%s' "$CHANGED" | sed 's/^/    /')"
fi

CTX="$CTX

This is base-app (PocketBase framework-mode Go platform). Source of truth: CLAUDE.md."

emit_context "SessionStart" "$CTX"
