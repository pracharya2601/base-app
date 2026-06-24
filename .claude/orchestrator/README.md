# base-app Claude Code Orchestrator

A **lean**, hook-driven layer wired into Claude Code's lifecycle. Its job is to
make sessions safer without taxing every prompt. It does three things:

1. **A deterministic safety net** — catch build/format mistakes mechanically.
2. **On-demand specialists** — read-only subagents you pull in when a task warrants.
3. **Heavy fan-out** — guidance for multi-agent Workflows on jobs one context can't hold.

> **Design principle:** orchestration should *earn its cost*. The old version
> forced a routing subagent + meta-prompting protocol on **every** substantive
> prompt and hard-denied edits until you complied. That was ceremony, not value —
> it added latency and tokens to requests that never had a fork in the road. It's
> gone. What's left only fires when it pays for itself.

## The loop (what's automatic)

```
SessionStart ──► inject repo snapshot + announce the gates       (.claude/hooks/session_start.sh)
                 (branch, dirty files, recent commits)           cheap, read-only

UserPromptSubmit ─► silent turn marker                           (.claude/hooks/user_prompt_submit.sh)
                 marks a substantive turn (skips acks / commands) so the Stop
                 hook knows to nudge about memory. Injects NOTHING. No routing,
                 no meta-prompt protocol. Invisible during normal use.

PostToolUse(Edit|Write|MultiEdit) ─► react to edits              (.claude/hooks/post_tool_use.sh)
                 track edited .go files for this session
                 + advisory gofmt feedback

Stop ──► VERIFY-GATE + MEMORY REMINDER                           (.claude/hooks/stop.sh)
                 if Go files were edited: BLOCK finishing while gofmt-dirty or
                 `go build ./...` fails (returns the actual error to fix).
                 Green → allow + suggest code-reviewer / verifier before shipping.
                 + on a substantive turn: non-blocking nudge to persist durable memory.
                 Loop-safe: build gate skips on stop_hook_active (150s timeout).
```

The Stop build gate is the crown jewel — it makes "I'm done" mean "it compiles."

## Specialists — pull-based (`.claude/agents/`)

Invoke these via the Agent tool **when the task actually warrants it** — they are
not forced on you. All are read-only (they report; they don't edit).

| Agent | Pull it in when… |
|---|---|
| `meta-prompter` | the request is fuzzy, large, or high-stakes — lock scope + acceptance criteria first |
| `code-reviewer` | you've made a non-trivial change and are about to commit/ship (Go + PocketBase/RBAC aware) |
| `verifier` | a change claims to fix/add behavior and you want it proven by building + running + curling, not just read |

The Stop gate reminds you about `code-reviewer` / `verifier` after Go edits — act
on it for real changes, skip it for trivial ones.

## Heavy / parallel jobs — use a Workflow

When a job is too big for one context — a **full security audit of the auth/RBAC
surface**, a **cross-package refactor / call-site migration**, **reviewing a
large diff across many dimensions**, a **broad codebase sweep** — that's where
real multi-agent orchestration pays off (fan-out + adversarial verification +
synthesis). Ask for it explicitly ("use a workflow" / "fan out agents") and it
fans work across many subagents in parallel, then verifies and synthesizes.

Good fits here:
- **Review:** split the diff into dimensions (correctness / security / RBAC / perf), one agent each, then adversarially verify each finding before reporting.
- **Audit:** sweep every collection's rules, every route's auth, every system-collection constraint in parallel.
- **Migrate:** discover all call sites, transform each in isolation, verify the build per change.

This is opt-in and can spawn many agents (token cost scales), so it's never
automatic — you ask for it when the scale justifies it.

## Operating it

- **Disable everything** for a session: `export CLAUDE_ORCH_DISABLE=1`.
- **Decision log:** `.claude/orchestrator/state/decisions.log`
  (tab-separated: `time  session  event  detail`; gitignored).
- **Raise the Stop block cap** (default 8): `export CLAUDE_CODE_STOP_HOOK_BLOCK_CAP=N`.

## Design notes

- Hooks never `set -e`; every path exits 0 unless it *intentionally* blocks
  (Stop `decision:block`), so a hook bug can't wedge the session.
- The Stop gate enforces **correctness only** (build + fmt) — review/verify are
  suggested, not forced, so the gate can't loop. Once the build is green it lets go.
- All wiring lives in `.claude/settings.json`; runtime state is gitignored.

> **Not to be confused with** the *product* orchestrator in
> `internal/orchestrator/` — that's an always-on, DB-backed "AI agent company"
> feature of base-app itself (PM → engineer → reviewer drafting work for human
> approval). This README is purely about the dev-time Claude Code session layer.
