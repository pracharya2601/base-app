#!/usr/bin/env bash
# post_tool_use.sh — PostToolUse hook (matcher: Edit|Write|MultiEdit).
#
# Fires right after a file is written. Two jobs:
#   1. Track every edited .go file for THIS session so the Stop gate knows what
#      to build/verify.
#   2. Give immediate gofmt feedback on Go files (advisory, non-blocking) — the
#      tool already ran, so we react rather than prevent.
# Kept deliberately light: no full build here (that belongs in the Stop gate).

INPUT=$(cat)
source "${CLAUDE_PROJECT_DIR:-$(pwd)}/.claude/hooks/lib.sh"
orch_disabled && exit 0

SESSION_ID=$(jget '.session_id')
FILE=$(jget '.tool_input.file_path')
[ -z "$FILE" ] && exit 0
cd "$PROJECT_DIR" 2>/dev/null || exit 0

case "$FILE" in
  *.go) ;;                       # only Go files get the treatment
  *) exit 0 ;;
esac

# Record this file in the session's edited-go list (deduped).
LIST="$STATE_DIR/edited-go-$SESSION_ID.list"
grep -qxF "$FILE" "$LIST" 2>/dev/null || printf '%s\n' "$FILE" >> "$LIST"

# Immediate formatting feedback (fast; doesn't compile).
if command -v gofmt >/dev/null 2>&1 && [ -f "$FILE" ]; then
  DIFF=$(gofmt -l "$FILE" 2>/dev/null)
  if [ -n "$DIFF" ]; then
    olog "gofmt_dirty" "$FILE"
    emit_context "PostToolUse" "gofmt: $FILE is not formatted. Run \`gofmt -w $FILE\` before finishing — the Stop gate will flag it otherwise."
  fi
fi

olog "go_edit" "$FILE"
exit 0
