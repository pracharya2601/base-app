---
name: pocketbase-rbac-ops
description: >
  Operate a running base-app (PocketBase framework-mode) instance — provision
  schema and manage RBAC: roles, permissions, user role assignment, and
  service-account API keys. Use when adding collections, granting or restricting
  access, minting/scoping API keys, or debugging "why can't X read/write Y".
  This is a procedural playbook; the model lives in docs/RBAC.md, the runtime
  verbs in the `pocketbase-superadmin` MCP server.
---

# Operating base-app schema + RBAC

base-app is PocketBase used as a Go framework with a custom superadmin API, a
role/permission system enforced by **native collection rules**, and API keys that
act as **roled service accounts**. This skill is the *how/when*; it points at the
tools and reference rather than copying them.

## Two ways to drive it
- **MCP (preferred)** — the `pocketbase-superadmin` server exposes:
  `list_collections`, `list_field_types`, `provision_schema`, `create_record`,
  `get_settings`, `update_settings`, and the RBAC tools `list_roles`,
  `manage_role`, `assign_user_roles`. Each tool self-describes; obey its
  description (the RBAC ones tell you to ask the user — do).
- **Raw HTTP** — `X-API-Key: pbk_...` or a superuser `Authorization` token, base
  `http://localhost:8090`. Key superadmin endpoints:
  `POST /api/superadmin/provision`, `GET|POST /api/superadmin/roles`,
  `POST /api/superadmin/users/roles`, `GET /api/superadmin/permissions`,
  `POST /api/superadmin/apikeys`. Standard PocketBase under `/api/collections`.

## Mental model — the 5 things you must know
1. **Two planes.** Control plane (schema/settings/keys) = superuser + coarse
   *scopes*. Data plane (`/records`) = **RBAC**: native rules read the caller's
   roles → permissions. Users and API keys flow through the *same* data-plane rules.
2. **Permissions are `"<collection>:<action>"` tokens** (`read`/`create`/`update`/
   `delete`), plus `"<collection>:*"` and global `"*"`. A role holds tokens; a
   user/key holds roles; effective access = the **union** of all their roles.
3. **Govern-by-default.** Every non-system collection is auto-RBAC'd on create and
   at startup. A collection only escapes RBAC if it has an explicit non-nil rule
   (e.g. `users`, for public signup). New tables are reachable only by `admin`
   (`*`) until you grant their tokens.
4. **API keys are roled.** A key acts as a `_serviceAccounts` identity on `/records`.
   No roles ⇒ no record access. Mint with `roles` to scope it.
5. **Denials are native codes, not 403:** create→400, update/delete→**404** (row
   filtered), list→**empty**. In rules, always use `?=` (any-of), never `=`.

## Playbooks

### Add a collection / feature
1. `list_collections` to see what exists. 2. `provision_schema` (idempotent) to
create/extend collections + fields + seed. It **auto-governs** the collection
(tokens + rules) — you do NOT need `rbac:true` for new collections. 3. Decide who
gets access → see "Grant access" (ask the user).

### Grant access — ⚠️ ASK THE USER FIRST
RBAC is privacy. **Do not invent policy.** Before granting anything not explicitly
requested — and ALWAYS before broad grants (`*`, `<collection>:*`, write/delete on
sensitive data) — confirm the intended collections + actions with the user. If the
request is vague ("let viewers see everything"), ask: which collections, and
read-only or write?
1. `list_roles` to see current grants (don't erase them). 2. `manage_role` to set a
role's FULL token list (missing tokens are auto-created). 3. `assign_user_roles`
to put a user in roles (by name; replaces their roles — include ones to keep).

### Mint an API key for a service
`scripts/mint-apikey.sh` (or `POST /api/superadmin/apikeys`). Give it the **least**
roles it needs (`ROLE_NAME=viewer ./scripts/mint-apikey.sh` for read-only). Scopes
gate the control plane; roles gate data. A key with no roles can't touch records —
that's the safe default. Confirm the intended capability with the user.

### Debug "X can't read/write Y"
Check, in order: is Y governed (has RBAC rules)? does any role of X grant
`Y:<action>` / `Y:*` / `*`? is X authenticated (guest ⇒ rules deny)? for a key, does
its service account have roles? Remember update/delete denial shows as 404.

## Hard rules
- **Never broaden access on your own initiative** — ask the user, every time.
- Set a role's permissions by reading current grants first, then writing the full
  desired set (these tools REPLACE, not append).
- Keep a collection superuser-only by giving it an explicit non-nil rule; otherwise
  it's auto-governed.
- The MCP/operator key needs `roles:read roles:write` scopes to manage RBAC.

## Reference
- Model + rule grammar + multi-role + service accounts: `docs/RBAC.md`
- Architecture + endpoints + decisions: `docs/ARCHITECTURE.md`, `docs/DECISIONS.md`,
  `CLAUDE.md`
- Command snippets: `docs/RUNBOOK.md`
