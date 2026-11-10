# Backend implementation notes

All endpoints are prefixed `/api/v1` except `/health`. JSON envelope matches `docs/DESIGN.md`. Fields are snake_case, IDs and rupiah integers. Successful writes return the changed ID/status (fetch list for complete row). Config PATCH endpoints accept complete configuration form bodies.

## Execution

`go run ./cmd/server migrate`, `seed`, `bootstrap`, `serve` (default). Configuration: `DATABASE_URL`, `JWT_SECRET` (>=32 bytes, required for serve), `PORT` (8080), `CORS_ORIGIN` comma-separated allowlist, `UPLOAD_DIR` (uploads), `DEMO_MODE=true`, `DEMO_PASSWORD` (seed), `INITIAL_ADMIN_EMAIL`, `INITIAL_ADMIN_PASSWORD` (bootstrap). Migration SQL is embedded. Production bootstrap only creates first admin, without financial defaults. Register requires name/email/NIK 16 digits/address/phone/password >=10 characters <=72 UTF-8 bytes. Pending members have no member number until activation; identity document and contact data are required for activation.

## Actual status and policy values

- Roles: `admin`, `teller`, `committee`, `member`.
- Savings codes: `principal`, `mandatory`, `voluntary`. Configure minimum balance explicitly to withdraw; configure required amount explicitly for principal/mandatory. `period=once` for principal; mandatory period `YYYY-MM`. One approved deposit per required period. Confirmed deposits/withdrawals post balanced ledger and update locked account atomically. Savings policy is revalidated at approval; policy changes may block pending transactions.
- Savings/payment states: `pending`, `approved`, `rejected`.
- Applications: `submitted`, `surveyed`, `approved`, `rejected`, `disbursed`. Survey recommendation: `eligible`/`ineligible`. Survey accepts text. Only active members submit their own applications, with at least one owned, unused loan document. Member row is locked; any unpaid overdue installment blocks submission (422). Existing non-overdue active loans are counted in response/audit, without inventing an unsupported loan-count limit. Product policy is share-locked during snapshot creation.
- Loans `active`/`paid`; installments `unpaid`/`paid`. Equal principal, final-row remainder, interest half-up; rates are annual bps; maximum loan amount 1e12, tenor 120, annual bps 100000. Monthly due dates clamp independently to month end. Both daily penalty methods cannot be nonzero. Payments require full installment; no partial payment or interest discount invented. Verification rechecks current penalty; a stale pending quote is rejected with `409 payment_amount_changed`. Teller must reject old request, then member submits current total. No greater receipt silently recorded.
- Scheduler runs on server start and hourly; manual `POST /scheduler/run` admin. Penalty totals are recalculated without compounding. In-app H-3 and H-1 reminders plus daily overdue notices deduplicate by installment/civil day. Business dates use Asia/Makassar, including UTC-scanned PostgreSQL dates. Email/WhatsApp/push delivery is not connected.
- SHU: closed years only; `draft → reviewed → finalized → distributed`. Configure weights sum 10000; basis `closing_balance_paid_interest`. Explicit implementation assumption: closing savings balance and paid interest. Zero-weight pool retained (`retained_amount`), not fabricated allocations. Distribution records manual cash payouts and notifications, not gateway transfers. Costs entered for a closed/reviewed year are blocked; a new cost invalidates a draft calculation. Amount pool is interest+penalty income minus recorded costs, never negative.
- Documents: PDF/PNG/JPEG max 5 MB, private random paths, attached downloads, owner/admin/committee access. File type/size checks are not antivirus scanning.
- Password reset endpoints return 503 `delivery_not_configured`; they do not generate fake reset emails. Password change revokes all sessions; logout revokes current session. JWT algorithm allowlist HS256, 8-hour expiry, DB session and current role checked each request.
- Unknown server errors are 500 generic; duplicate constraints 409; invalid FK/check/date data 422. No SQL detail returned.

## Data and verification

Seed requires explicit DEMO mode and a supplied password. Names, funds, financial rules and histories are fiction. Atomic audit marker prevents repeated balances/journals. Dataset includes accounts, confirmed savings, active loans, schedules, paid and overdue installments, pending application/deposit, a fully paid prior-year loan (Rp36,000 paid interest), prior-year draft SHU with nonzero calculation basis and notifications. The dataset makes no claim to external cash settlement.

`go test ./...` covers integer schedules/penalties/date bounds, SHU allocation and CSV formula protection, upload checks/ownership, JWT rejection and role guards. `go vet ./...` and compiled server build run separately. `go build -buildvcs=false` is needed in the provided scratch environment with inaccessible parent VCS metadata. Live PostgreSQL migration/seed/API/concurrency integration requires a real PostgreSQL server and is provided via `../tests/integration.py`; it was not executed in the managed root-only environment where PostgreSQL initdb refuses root. No mock database is substituted.


## Manajemen akun staf (non-demo)

- `POST /api/v1/users` hanya untuk admin; membuat akun `admin`, `teller`, atau `committee`, menyimpan kata sandi dengan bcrypt, dan menulis audit log. Akun `member` dibuat melalui pendaftaran anggota agar data keanggotaan wajib ikut dibuat.
- `PATCH /api/v1/users/:id/password` hanya untuk admin; mereset kata sandi pengguna lain, mencabut seluruh sesi aktif akun tersebut, dan menulis audit log. Admin tidak dapat menggunakan endpoint ini untuk mereset kata sandinya sendiri; gunakan `PATCH /api/v1/auth/password`.
- Instalasi normal menggunakan `INITIAL_ADMIN_EMAIL` dan `INITIAL_ADMIN_PASSWORD` untuk bootstrap administrator pertama. Jangan memakai konfigurasi mode demo pada basis data nyata.
