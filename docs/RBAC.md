# RBAC: roles, permissions, and multi-role API rules

How user authorization works in this app, and how to write PocketBase API rules
against it. For the architecture/implementation, see the "User roles & RBAC"
section of `CLAUDE.md` and `roles.go`. This doc is the **usage** guide.

## The data model is a chain of multi-relations

```
users.roles[]           ┐
                        ├→  _roles.permissions[]  →  _permissions.token
_serviceAccounts.roles[]┘   (a role holds many       (each record is one
 (a user OR a key's          permissions)             "<collection>:<action>" string)
  service account holds
  many roles)
```

- `_permissions` — one record per access token: `articles:create`, a collection
  wildcard `articles:*`, or the global `*`. (System collection.)
- `_roles.permissions` — multi-relation → `_permissions`. (System collection.)
- `users.roles` and `_serviceAccounts.roles` — multi-relation → `_roles`. Humans
  carry roles on their user record; **API keys** carry roles on a service account
  (see "API keys are roled too"). Both resolve through the *same* rules.

## Default roles & seeded grants

| Role | Grants |
|---|---|
| `viewer` | `read` on every governed collection |
| `editor` | `read` + `create` + `update` on every governed collection (no delete) |
| `admin`  | `*` (everything, all collections, all actions) |

These are seeded on first boot (`migrateAndSeedRBAC`) and the read/write grants
were extended across the governed collections as a data operation. Grants are
**explicit per-collection** — a brand-new collection's tokens are NOT auto-added
to `viewer`/`editor` (only `admin`'s `*` covers it) until you grant them. Manage
roles/permissions via the standard superuser record API on `_roles` /
`_permissions`, e.g. add `projects:read` to viewer:
```
PATCH /api/collections/_roles/records/<viewer-id>
{ "permissions": [<existing-ids>, "<projects:read permission id>"] }
```

## How multiple roles combine: union, no precedence

When a rule references `@request.auth.roles.permissions.token`, PocketBase walks
the **entire** chain and flattens it into one set — every token, from every
permission, of every role the user holds. There is no "primary role" and no
precedence; holding multiple roles simply **unions** their grants.

Concrete example — a user with `[viewer, editor]`:

```
viewer grants : ["articles:read", "users:read"]
editor grants : ["articles:read", "articles:create", "articles:update"]

@request.auth.roles.permissions.token  resolves to the UNION:
  ["articles:create", "articles:read", "articles:update", "users:read"]
```

Neither role alone has both `articles:create` and `users:read`; the user gets
both because the path flattens across roles.

## The operator that makes it work: `?`

The `?` prefix means **"at least one of the values matches."** That single
character IS the multi-role mechanism:

```
@request.auth.roles.permissions.token ?= "articles:create"
```
reads as *"does ANY token in the flattened set equal `articles:create`?"* — true
if any of the user's roles grants it.

| Operator        | Meaning on a multi-valued path                    |
|-----------------|---------------------------------------------------|
| `?=`            | **any** of the values equals — use this for roles |
| `?~`            | **any** of the values contains (SQL `LIKE`)       |
| `?!=` / `?!~`   | any not-equal / any not-contains                  |
| `=` (no `?`)    | ⚠️ different semantics — do NOT use for role checks |

**Rule of thumb: whenever a rule path crosses a multi-relation (`roles`,
`permissions`), use the `?`-prefixed operator.** Forgetting the `?` is the most
common cause of "my rule denies everyone."

## The auto-generated rule (what `rbac:true` / the create hook writes)

For each governed collection, the four rules look like this (create on
`articles`):

```
@request.auth.roles.permissions.token ?= "articles:create"
 || @request.auth.roles.permissions.token ?= "articles:*"
 || @request.auth.roles.permissions.token ?= "*"
```

Read: *grant if ANY role has the exact token, OR a role has the collection
wildcard `articles:*`, OR a role has global `*`.* Multi-role is handled by the
`?=`; the `||`s just add the wildcard shortcuts. (`read` covers both List and
View; `create`/`update`/`delete` map to the matching actions.)

Denials use native PocketBase codes: create → **400**, update/delete → **404**
(the row is filtered out), list → empty result.

## Writing your own rules

You are not limited to the generated rules — combine these freely:

**Permission-based (the default):**
```
@request.auth.roles.permissions.token ?= "invoices:approve"
```
Add `invoices:approve` as a `_permissions` record, grant it to a role — no schema
change, effective immediately.

**Role-name-based (simpler, if you don't need fine-grained tokens):**
```
@request.auth.roles.name ?= "editor" || @request.auth.roles.name ?= "admin"
```

**Per-record — permission AND row ownership:**
```
@request.auth.roles.permissions.token ?= "articles:update"
 && @request.auth.id = author
```
"has the permission AND owns the row" — i.e. editors may edit *their own*
articles. (Replace `author` with the relation/owner field on the collection.)

**Exclusion:**
```
@request.auth.roles.name ?!= "suspended"
```

## API keys are roled too (same rules, no bypass)

API keys go through the **exact same** native rules as users, so you control a
key's data access with roles instead of trusting it. The mechanism:

- An API key (`_apiKeys`) is linked to a **service account** — a record in the
  `_serviceAccounts` system **auth** collection that carries a `roles` relation
  (identical shape to `users.roles`).
- The key middleware splits by plane:
  - **Control plane** (schema/settings/key-management routes): the key acts as
    the minting **superuser**, gated by its coarse **scopes** (unchanged).
  - **Data plane** (`/api/collections/{c}/records`): the key acts AS its
    **service account**, so `@request.auth.roles.permissions.token` resolves the
    key's roles and the same collection rules apply. **No superuser bypass.**

Because rules don't hardcode the `users` collection, `@request.auth.roles...`
works whether `@request.auth` is a user or a service account.

Mint a roled key:
```
POST /api/superadmin/apikeys
{ "name": "reporting-bot", "roles": ["<viewer-role-id>"], "scopes": ["schema:read"] }
```

Consequences:
- A key with **no roles can't read or write any records** (list returns empty,
  reads 404) — private by default.
- A key can only reach **RBAC-governed** collections per its roles. Collections
  left superuser-only (nil rules) are off-limits to keys entirely — even with the
  `admin` role — because PocketBase blocks non-superusers on nil-rule collections.
- Existing keys were migrated to an `admin`-role service account (full access to
  governed collections); downscope them by editing their service account's roles.
- `GET /api/superadmin/apikeys` lists each key's resolved role names.

## Agentic management (MCP)

RBAC is drivable by an LLM agent through the MCP server, with guardrails baked
into the tool descriptions so the agent **asks the user before policy decisions**
instead of inventing access:

- `list_roles` — inspect every role and its tokens (read; pair with
  `list_collections`). The agent should call this first.
- `manage_role` — create/update a role and set its full token list. The
  description flags it as a POLICY DECISION: the agent must ASK the user before
  granting anything not explicitly requested, especially broad grants
  (`*`, `<collection>:*`, write/delete).
- `assign_user_roles` — set a user's roles by name; same ask-the-user guardrail,
  especially for powerful roles.

Backing endpoints (control plane, superuser-bound, scope-gated):
`GET /api/superadmin/roles` (`roles:read`), `POST /api/superadmin/roles` and
`POST /api/superadmin/users/roles` (`roles:write`). The MCP key needs those
scopes. Because these are control-plane routes, an API key drives them as the
superuser (scope-gated) — unlike record routes, where it's its service account.

Design intent: the agent is the operator UI for RBAC, but **the human owns the
policy**. Open question the agent should surface rather than decide: whether
`viewer`/`editor` should auto-cover NEW collections (global `*:<action>` tokens)
or stay explicit per-collection.

## Gotchas

1. **Always `?=`, never `=`** when traversing `roles`/`permissions`. The
   non-`?` operators have different multi-value semantics and will surprise you.
2. **`?=` is exact.** `articles:create` does not satisfy a rule asking for
   `articles:*` or `*` unless you include those `||` clauses. The generator adds
   them; hand-written rules must add them too if you want wildcards honored.
3. **Tokens are plain strings**, matched by value. Creating a token record does
   nothing on its own — it only matters once a collection's rule references it
   AND a role grants it.
4. **Every non-system table is RBAC-governed automatically** — new ones via the
   create hook, existing/ungoverned ones via `backfillRBAC` at startup. They're
   reachable only by superusers, holders of `*` (the `admin` role), or roles
   granted the table's tokens. Collections with explicit rules (e.g. `users`) are
   never touched. To keep a collection superuser-only, give it an explicit
   non-nil rule.
