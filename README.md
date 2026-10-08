# DOS Business Process Automation System

The central, browser-based workflow system for Digital Office Solutions Limited. Staff use it on office computers and on tablets in the field, and every commercial transaction is traced from client to invoice. The scope is defined in the *System Implementation Brief v1.2*.

This is the **starter project**. The platform layer is complete, and one business module, **Clients**, is built end to end as the pattern for the rest.

| Layer | Choice |
|---|---|
| API | Go 1.24, Echo v4, pgx v5 (plain SQL, no ORM) |
| Database | PostgreSQL 16 |
| Web app | React 19, TypeScript, Vite, TanStack Query, React Router |
| Offline | Installable PWA (service worker) and IndexedDB via Dexie |
| Deploy | One container serves the API and the web app on `:8080` |

---

## Quick start

Requirements: Go 1.24+, Node 22+, Docker (only for Postgres).

```bash
cp backend/.env.example backend/.env       # then set JWT_SECRET
make setup                                 # go mod tidy, npm install, start Postgres
make admin EMAIL=you@dos.co.zm NAME="Your Name" POSITION="Business Applications Developer" DEPT=IT
make api                                   # terminal 1: API on :8080, runs migrations
make web                                   # terminal 2: web app on http://localhost:5173
```

> **First run:** `make setup` runs `go mod tidy`. That completes `backend/go.sum` for the `golang.org/x/*` modules. Commit the updated `go.sum`; the Docker build and CI need it.

Run the tests with `make test`. The integration tests (real Postgres, concurrency, the full client lifecycle) run when `TEST_DATABASE_URL` is set, and `.env.example` already points it at the docker-compose test database.

---

## Structure: a modular monolith

```
backend/
  cmd/server/            HTTP server (migrates on start)
  cmd/admin/             maintenance CLI (create-user)
  internal/app/          wiring: builds platform + registers modules   ← add new modules here
  internal/platform/     shared building blocks, no business rules
    auth/                JWT sessions, password hashing, roles → permissions
    audit/               append-only activity log (record timelines)
    numbering/           server-side document numbers (DOS-Q-2026-0001 …)
    events/              in-process event bus between modules
    migrate/             per-module SQL migrations
    httpx/               error format, validation, pagination
    module/              the Module interface every module implements
    migrations/          platform tables: users, audit_log, number_sequences
  internal/users/        staff accounts, login
  internal/clients/      ★ reference module (copy this layout)
frontend/
  src/api/               fetch wrapper + typed errors
  src/auth/              session, permissions
  src/offline/           IndexedDB store, online status
  src/features/clients/  client screens + API calls      ← one folder per module
```

### Module rules

1. **A module owns its tables.** Other modules store its IDs but never query its tables.
2. **Modules call each other through exported interfaces.** For example, `clients.API` has `Get` and `RequireActive`. Modules never import another module's repository.
3. **Side effects travel as events.** For example, `delivery.signed` makes invoicing mark the order ready to invoice.
4. **Shared needs come from `platform`.** Auth, audit, numbering, events, and later PDFs and email are written once.
5. **Every state change happens in one transaction** that also writes the audit entry. Events are published after the commit.

### Adding a module (e.g. Deals)

1. Copy `internal/clients/` to `internal/deals/`. Rename the types, write `migrations/0001_deals.sql`, and add `PermRead`, `PermCreate` and the others.
2. Declare what it needs in `New`, e.g. `func New(d module.Deps, clients clients.API) *Module`.
3. Register it in `internal/app/app.go` → `buildModules()`.
4. Grant the permissions to roles in `internal/platform/auth/permissions.go`.
5. Add `frontend/src/features/deals/` and a route in `App.tsx`, and switch the nav item to `live: true` in `Layout.tsx`.

Migrations are versioned per module, so new modules never renumber existing ones.

---

## What the Clients module covers (brief §4.1)

- Mandatory registration, with a server-allocated reference (`DOS-CL-00001`).
- Duplicate control. A new client is matched against existing ones on normalised name ("ABC Mining Ltd." = "abc mining limited"), TPIN and PACRA number. A possible duplicate is blocked unless a sales manager overrides it with a reason, and the override is audited.
- Contacts, with one primary contact enforced by the database. Delivery sites.
- Default currency, 30-day credit terms by default, and an optional credit limit (stored as an exact decimal).
- Responsible person and position on every record. Handover requires a reason and is recorded.
- Deactivate and reactivate with a reason. Clients are never deleted. `RequireActive` blocks documents for inactive clients.
- Optimistic locking: an edit made on a stale copy returns `409 stale_version` instead of overwriting someone else's change.
- An activity timeline from the audit log. The log is append-only, enforced by a database trigger.
- Offline: client lists and details are cached on the device and readable with no connection.

## Document numbering (brief §7)

`internal/platform/numbering` allocates numbers in the same transaction as the document insert, using a locked counter row. Concurrent requests never share a number, and a failed transaction releases its number. `TestNumbersNeverDuplicateUnderConcurrency` proves this with 40 parallel transactions. Prefixes follow the brief's illustrative table. **Confirm the final prefixes and reset rules with DOS management** (the client prefix `DOS-CL` is not in the brief).

## Offline sync design (for delivery notes and job cards)

The pieces already in place are the PWA shell (the app opens with no network), the IndexedDB store, and an `outbox` table in `src/offline/db.ts`. The design to build in Phase 3:

1. A record created offline gets `localId = crypto.randomUUID()` and status `pending`, and is stored in the outbox with its signature and photos as Blobs.
2. When back online, the outbox posts to `POST /api/sync/{kind}` with `localId` as an idempotency key, so retries never duplicate.
3. The server inserts the record, **allocates the final DOS number**, stores a `localId → id` mapping, and returns the result. The device marks the item `synced`.
4. Signed records are immutable. Corrections create a linked replacement record.
5. Each attempt is written to a sync log with user, device, record, timestamp, result and error (brief §5.2).

## Security notes and next steps

Done: hashed passwords (PBKDF2-SHA256, 600k iterations), role-based permissions checked on every route, an append-only audit log, a server-only numbering engine, security headers, and parameterised SQL throughout.

Still to do before production:

- **Refresh tokens and a device register.** Access tokens currently last `ACCESS_TOKEN_TTL` (default 12h). The brief requires revoking lost tablets (§5.3, §8.2), so add a `devices` table and short access tokens with revocable refresh tokens.
- **Login rate limiting**, and MFA for admin and CEO accounts (§8.2).
- **Audit views of sensitive records** if DOS wants viewing logged as well as changes.
- **PDF and email.** Documents are drawn by a small built-in PDF writer (`internal/platform/pdf`, no outside service or library), and emailed with attachments through `internal/platform/mail`, which keeps a send log. Quotations use both; invoices and delivery notes will too.
- **Transactional outbox** for events that must never be lost (e.g. email after invoice issue).
- **Backups:** scheduled `pg_dump` with tested restores (§8.2).
- **Multi-tenancy (deferred).** The system is single-company today: no table has an organisation ID, document numbering is one shared sequence, and DOS branding and number prefixes are in code. If DOS may sell or host it for other companies, add an `organisations` table, an `organisation_id` on every table set from the signed-in user, Postgres row-level security as a safety net, and per-organisation numbering and branding. Do this **before Quotations and Invoicing**, because every module built on the single-company design has to be reworked.

## Build order (brief §9)

| Phase | Modules |
|---|---|
| 1 Commercial core | ✅ users, ✅ clients → ✅ deals → ✅ quotations (+ PDF) → ✅ customer orders |
| 2 Procurement | ✅ suppliers register + price requests (RFQs) → supplier POs, goods receipt |
| 3 Field execution | delivery notes, job cards, signatures, photos, offline sync |
| 4 Invoicing & tenders | invoices, statements, ageing, tenders |
| 5 Dashboard & rollout | dashboard, reports, training, migration |
