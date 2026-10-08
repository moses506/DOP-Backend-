# Security

How DOS BPA protects its data, and what must be in place before it is used by a real organisation.

---

## What the system does

### Signing in
- **Passwords** are stored only as PBKDF2-SHA256 hashes (600,000 rounds, per-user salt). Nobody, including administrators, can see them.
- **Password rules** (in line with NIST SP 800-63B):
  - at least 10 characters;
  - not one of the passwords attackers try first (including local ones such as *Lusaka@123*);
  - not a keyboard or number sequence;
  - not built from the person's own name or email.

  Symbol-mixing rules are not forced, because length and uniqueness matter more.
- **Guessing is blocked:**
  - 5 wrong passwords pause sign-in to that account for 15 minutes, and the owner is emailed.
  - Emails with no account are paused the same way, so the response never reveals who has an account.
  - One network address is limited to 30 failures per 15 minutes, plus a request-rate limit on the sign-in and set-up routes.
  - An administrator's password reset, or a set-up link, lifts a pause.
- **Every attempt is recorded** in `login_attempts` (email, address, device, outcome) and kept for 180 days.
- **Set-up and reset links** are 256-bit random, stored only as SHA-256 hashes, work once, and expire after 72 hours (invites) or 24 hours (resets).

### Two-step verification
- **What it is:** after the password, people enter the 6-digit code from an authenticator app (Google or Microsoft Authenticator, or any TOTP app). Built to RFC 6238 and tested against its official vectors.
- **Who must use it:** roles in `MFA_REQUIRED_ROLES`, by default administrators, the CEO and sales managers.
  - They set it up the first time they sign in.
  - Until they do, the server lets them do nothing else.
  - Everyone else can turn it on from **Security** under their name.
- **Protections:**
  - The app's secret is **encrypted in the database** (AES-256-GCM, key from `DATA_ENCRYPTION_KEY`, or `JWT_SECRET` if not set).
  - **A code can't be used twice.**
  - **Wrong codes count towards the same 5-attempt lockout as wrong passwords.** A correct password doesn't reset the count, so knowing the password doesn't allow endless code guessing.
  - The QR code is drawn in the browser, so the secret is never sent to an outside service.
- **Recovery codes:** 10 one-time codes, stored only as hashes and shown once. They can be replaced from the Security page.
- **Lost phone:** an administrator uses **Reset two-step verification** on the Staff page. The person is signed out everywhere and sets it up again at next sign-in. Nobody can reset their own.
- **Alerts:** people are emailed when two-step is turned on, turned off or reset, and when their sign-in is paused.
- **The Security page** shows every device a person is signed in on, so they can sign out any of them.

### Sessions
- The session lives in an **HttpOnly, SameSite=Strict cookie**. It is also HTTPS-only in production. Page scripts can't read it, so injected script can't steal it.
- Every change request also needs the `X-Requested-With: dos-web` header, which other websites can't add. This is protection against cross-site request forgery.
- **Each sign-in is a server-side session.** Every request checks it, so:
  - **Sign out** ends the session on the server, and a copied token stops working at once.
  - **"Sign out on my other devices"** ends every session for the account.
  - Changing or resetting a password, changing someone's role, or deactivating them signs them out everywhere.
- **Idle timeout:** sessions end after 60 minutes without use (`SESSION_IDLE_TIMEOUT`). The app warns two minutes before.
  - Background refreshes don't count as use, so an unattended open tab still times out.
  - The absolute session limit is `ACCESS_TOKEN_TTL` (12 hours).
- Signing out clears the client data cached on the device for offline use.

### Access control
- Every API route checks a named permission, and roles are defined in one place (`internal/platform/auth/permissions.go`).
- Sales executives can change only their own deals. Handing a deal or client to someone else needs a manager and a reason.
- The last administrator can't be removed. Administrators can't change their own role or deactivate themselves.
- **Every change is written to an append-only audit log** (a database trigger blocks edits and deletes), with who, when, what changed and why.

### The web layer
- **Content-Security-Policy:**
  - Only the app's own scripts run, and it talks only to its own server.
  - Framing by other sites is blocked (`frame-ancestors 'none'`, `X-Frame-Options: DENY`).
- Also set:
  - HSTS in production;
  - `nosniff`;
  - a strict referrer policy;
  - a Permissions-Policy, which allows only camera and location (for field work later);
  - cross-origin isolation headers.
- API responses are `Cache-Control: no-store`, so business data isn't left in browser or proxy caches.
- **CORS** allows only the origins listed in `CORS_ORIGINS`.
- **Client address:** the server believes `X-Forwarded-For` only from proxies listed in `TRUSTED_PROXIES`, so nobody can fake their address to dodge the limits.
- **Other safeguards:**
  - all SQL is parameterised;
  - request bodies are capped at 10 MB;
  - errors never expose internals to the browser (they are logged with a request ID);
  - spreadsheet exports escape cells that would otherwise run as Excel formulas.

### Production safety checks
With `APP_ENV=production`, the server **refuses to start** if any of these hold:
- `JWT_SECRET` is short (under 48 characters) or still the example;
- `APP_URL` isn't https;
- `CORS_ORIGINS` contains `*` or an http origin;
- the database URL disables TLS to a remote server;
- the session limits are unreasonably long.

---

## Before go-live: checklist

**Server and network**
- [ ] Serve only over **HTTPS**, behind a reverse proxy such as nginx or Caddy (Caddy obtains certificates automatically). Redirect http to https.
- [ ] Set `TRUSTED_PROXIES=private` when the proxy is on the same machine or a private network, or set the proxy's exact address.
- [ ] Only ports 80/443 open to the internet. **PostgreSQL must not be reachable from the internet.**
- [ ] Keep the operating system patched, with automatic security updates.

**Secrets and configuration**
- [ ] `APP_ENV=production`.
- [ ] `JWT_SECRET` from `openssl rand -base64 48`. Keep it out of git and store it in a password manager. Changing it signs everyone out.
- [ ] `DATA_ENCRYPTION_KEY` from `openssl rand -base64 48`, stored with `JWT_SECRET`. If it's lost or changed, everyone's two-step verification must be reset.
- [ ] `MFA_REQUIRED_ROLES` agreed with management. The default is `admin,ceo,sales_manager`; consider adding `finance`.
- [ ] `.env` readable only by the service account (`chmod 600`).
- [ ] Database password unique and long. The app's database user owns only the app's database, not the server (not a superuser).

**Email**
- [ ] Send from a DOS mailbox (e.g. `workflow@doslimited.com`), not a personal Gmail.
- [ ] SPF, DKIM and DMARC set up on the domain, so security emails aren't treated as spam or spoofed.

**Data**
- [ ] Daily encrypted database backups (`pg_dump`), kept off the server.
- [ ] **Test a restore** before go-live and every quarter.
- [ ] Disk encryption on the database server.

**People**
- [ ] As few administrators as possible, each with their own account (no shared logins).
- [ ] Leavers deactivated on their last day. Deactivation signs them out immediately.
- [ ] Review the sign-in log monthly for repeated lockouts from unknown addresses.

**Upkeep (monthly)**
- [ ] `cd backend && go install golang.org/x/vuln/cmd/govulncheck@latest && govulncheck ./...`
- [ ] `cd frontend && npm audit --omit=dev`
- [ ] Update Go to the latest patch release, rebuild, and redeploy.

---

## Recommended next steps
1. An **administrator screen for the security log**: sign-ins, lockouts and two-step changes across all staff.
2. An independent **penetration test** before handling payment or personal ID data.

## Reporting a problem
If you find a security issue, tell the system administrator directly. Don't put details in a shared chat or ticket.
