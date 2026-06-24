#!/usr/bin/env bash
# user_prompt_submit.sh — UserPromptSubmit hook. TURN MARKER (silent).
#
# Deliberately minimal. We do NOT force routing or inject a meta-prompting
# protocol on every prompt — that was pure per-prompt tax (latency + tokens) that
# rarely changed the outcome. Routing/specialists are now PULL-based: the model
# pulls in meta-prompter / code-reviewer / verifier / a skill when the task
# actually warrants it, and the Stop gate nudges toward review/verify after real
# work.
#
# This hook's only job: detect a substantive turn (not an ack / slash command)
# and arm the marker the Stop hook keys its memory reminder off. It injects
# nothing into context — it is invisible during normal use.

INPUT=$(cat)
source "${CLAUDE_PROJECT_DIR:-$(pwd)}/.claude/hooks/lib.sh"
orch_disabled && exit 0

SESSION_ID=$(jget '.session_id')
PROMPT=$(jget '.prompt')

SUBSTANTIVE_FLAG="$STATE_DIR/turn-substantive-$SESSION_ID"
rm -f "$SUBSTANTIVE_FLAG" 2>/dev/null

# --- skip trivial / control prompts ---------------------------------------
trimmed=$(printf '%s' "$PROMPT" | tr '[:upper:]' '[:lower:]' | sed 's/^[[:space:]]*//;s/[[:space:]]*$//')
len=${#trimmed}
case "$trimmed" in
  /*) exit 0 ;;                                   # slash command
esac
[ "$len" -lt 6 ] && exit 0                        # tiny acks ("ok", "yes")

# Strip ack/filler words; if nothing substantive remains, it's a control reply
# ("yes go ahead", "ok do it now", "sure, continue") — pass straight through.
ACK_RE='^(yes|no|y|n|ok|okay|k|go|ahead|continue|cont|keep|going|next|stop|done|thanks|thank|ty|thx|yep|nope|sure|proceed|do|it|now|please|lets|just|also|and|so|then|ya|yeah|yup|the|a|to|of|pls)$'
core=$(printf '%s' "$trimmed" | tr -cs "a-z'" '\n' | grep -Ev "$ACK_RE")
[ -z "$core" ] && exit 0

# Substantive turn: arm the Stop memory reminder. Stay silent (no context emit).
: > "$SUBSTANTIVE_FLAG" 2>/dev/null
olog "turn_substantive" "len=$len"
exit 0
