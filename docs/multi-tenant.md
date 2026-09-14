# Multi-tenant design — `WHERE tenant_id = ?` everywhere

Schema prep is live in prod since 22:27 UTC (`e921fae`): `tenant_id INTEGER DEFAULT 1` on `users`, `node_owners`, `incidents`. Every existing row backfilled to `tenant_id=1`. Zero behavioural change today.

This doc captures the open design questions before the actual filter clauses land at every read site.

---

## Design questions

### Q1. Where does `tenant_id` come from at request time?

Five options on the table:

| # | Source | Pros | Cons |
|---|--------|------|------|
| A | Encoded in the API token (`api_tokens.tenant_id` column, looked up on auth) | Tenant bound to token, can't accidentally cross-tenant with a stolen token | Token mint needs tenant context; existing tokens are tenant-less |
| B | Separate header `X-Tenant-ID` | Easy to switch tenants in dashboard | Easy to spoof unless paired with auth; multi-token UX is confusing |
| C | Subdomain `acme.nodepulse.io` | Browser-friendly cookies, clear tenant boundary per org | DNS/wildcard complexity; one VDS can't realistically host many wildcards |
| D | Path prefix `/api/v1/t/{tenant_id}/...` | Visually obvious, debuggable | Every handler signature changes; rewrite middleware everywhere |
| E | Operator master token → `tenant_id=1` (no scoping), user-token → derive from `users.tenant_id` | Zero-change for existing single-tenant deployments; the 99% case | Doesn't scale to multi-tenant SaaS without an additional scheme |

**Recommended**: `E` for now (works for the 4 users we have today, zero migration risk), `A` as the v1 SaaS path once the first paying tenant arrives. `B/C/D` are not worth the complexity at current scale.

### Q2. Operator invite flow across tenants

The current `POST /api/v1/invites` endpoint is master-token-gated and the `target_user_id` references a global user. With tenants:
- If an operator in `tenant_id=1` invites a user that doesn't exist yet (`auto_create_user=true`), the new user lands in which tenant?
- If `target_user_id` is set, do we verify the target user belongs to the operator's tenant, or any tenant?

**Recommended**:
- `auto_create_user=true` → new user is in the **inviter's** tenant (operator creates a user in their own tenant; cross-tenant onboarding is operator-mediated only)
- `target_user_id` set → if the user exists in a different tenant, return `403 not in your tenant` (operator can only invite into their own tenant). If the user doesn't exist yet, the operator should use `auto_create_user=true` (which forces same-tenant creation)

### Q3. Master token scope

Master token currently has cross-tenant access (`POST /api/v1/invites`, `GET /api/v1/users/audit`, billing, etc.). After multi-tenant:
- Option A: Master stays cross-tenant (operator is a global admin that can manage every tenant). This is what prod has today and what every docstring assumes.
- Option B: Master is bound to a tenant (e.g. `master_tenant_id=1`), and there's a separate "super-master" for cross-tenant ops.

**Recommended**: A. Operators are global. Adding master-per-tenant is YAGNI until we have ≥2 operator-tenants.

### Q4. Public read endpoints

Currently `/api/v1/public/status`, `/api/v1/public/fleet-summary`, `/api/v1/public/feed.atom`, `/api/v1/public/badge`, `/api/v1/public/incidents` return aggregate data across all users/nodes. With multi-tenant:
- Option A: Public endpoints stay aggregate (the whole platform shares one status page).
- Option B: Each tenant gets a public status at `/{tenant_slug}/status`; the existing endpoints continue to show `tenant_id=1` (default).

**Recommended**: B, but tenant_slug is a future column. For the v1 SaaS, add `slug TEXT UNIQUE` to a new `tenants` table when the first paying tenant appears. Until then, public endpoints stay aggregate (single-tenant reality).

### Q5. Plan limits (FreeNodeLimit, FreeProbeLimit, FreeRetentionDay)

Constants right now (`pkg/store/billing.go:97-101`). With multi-tenant:
- Option A: Keep global (every tenant has the same free tier).
- Option B: Move to a `tenant_plans` table with per-tenant overrides.

**Recommended**: A. Per-tenant plan overrides are a real product feature for enterprise, but we don't have that use case yet. One source of truth > config sprawl.

---

## Migration plan (when Q1 is decided)

1. Decide Q1 (currently `E`); implement resolution helper:
   ```go
   func (p *PersistentStore) TenantIDForToken(tokenHash string) (int64, error)
   ```
   Master tokens → `tenant_id=1` always.
2. Add `api_tokens.tenant_id INTEGER DEFAULT 1` (idempotent migration, same shape as `e921fae`).
3. Auth middleware reads tenant_id once per request and stores it in `context.Context`.
4. Read sites get `WHERE tenant_id = ?` clauses, parameterised from context. Top-down: `GetUserNodes`, `ListIncidents`, `GetFleetSummary`, then everything else.
5. Master-token endpoints keep their cross-tenant behaviour (Q3 = A).
6. Tests for cross-tenant isolation: tenant A cannot read tenant B's nodes/incidents.

---

## Open follow-ups (not blocking the design session)

- `tenant_id` column backfill to `tenant_id=1` for new users after the migration runs (DEFAULT handles it; only relevant if we ever add a per-invite-tenant field).
- `nodes` table — currently does not have `tenant_id` because ownership is via `node_owners`. A node can have multiple owners in different tenants (operator shared monitoring). For now, read sites JOIN through `node_owners`. If a tenant ever needs node-level scoping, that's a future column.
- Billing webhook — `CryptoBotWebhook.Payload.InvoiceID` is opaque. The `invoices` table already has `tenant_id` derivable through `user_id`, so the webhook handler can resolve tenant post-hoc. No change needed for the webhook contract itself.

---

## Pending decisions from Eduard

Please pick the recommended defaults (✓) or propose alternatives:

- [ ] Q1 (token / header / subdomain / path / master-implied): **recommended E for now, A as v1 SaaS**
- [ ] Q2 (invite cross-tenant): **recommended — same-tenant only, operator creates users in their own tenant**
- [ ] Q3 (master token scope): **recommended — master stays cross-tenant**
- [ ] Q4 (public endpoints): **recommended — stay aggregate until first paying tenant arrives, then per-tenant slugs**
- [ ] Q5 (plan limits): **recommended — global for now**
