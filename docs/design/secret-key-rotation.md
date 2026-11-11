# Secret key rotation

Status: design only — no code in this batch changes encryption or JWT
signing. This document records the current state and a rotation design
that can be implemented later without changing the on-disk format of
anything except the sealed values themselves.

## Current state (2026-11-12, commit e30b35f)

- One secret, `POLARIS_SECRET_KEY` (`internal/panel/config.go:39-40`,
  required, at least 16 chars, `change-me` rejected). It has two jobs:
  1. **Field encryption.** `secret.New(cfg.SecretKey)`
     (`internal/panel/server.go:144`) derives a 256-bit key as
     `SHA256(passphrase)` and seals with AES-256-GCM
     (`internal/secret/box.go`). Sealed values carry the prefix
     `enc:v1:` and a random nonce. The single `Box` lives on
     `store.Store.Box` and is used everywhere sensitive columns are
     written or read: CA private key (`server.go:274`), certificate
     keys, REALITY keys, SS master keys, API tokens, TOTP secrets,
     backup passphrase, S3 secret, Shodan key, ACME tokens, CDN
     credentials.
  2. **Session signing.** JWTs are HS256 signed with
     `"jwt:" + cfg.SecretKey` (`server.go:159`, `internal/panel/auth.go:54`),
     TTL 24h (`tokenTTL`), no `kid` header. Verification uses the same
     single key (`auth.go:63`).
- `Box.Open` returns non-`enc:v1:` input unchanged, so plaintext rows
  written before encryption was enabled still read. `Box.Seal` leaves
  already-sealed input alone.
- Backup archives embed the secret key itself
  (`internal/panel/backup.go:62,507-508`), so a backup restores on a
  new server with no extra key material. Old backups therefore contain
  the old key.
- The README states rotation is currently unsupported: losing or
  changing the key makes already-encrypted data undecryptable.
- Multi-master: several panel instances share one DB (and Redis for
  online IPs / rate limits / alert dedup). Any rotation must work
  across instances, not just one process.

## Goals

1. Change the key without downtime and without losing data.
2. Old sessions keep working until they expire (24h), not all at once.
3. If rotation is interrupted (crash, restart), the next start can
   resume or roll back safely; no row is ever left unreadable.
4. Old backups remain restorable.
5. Compromise of the old key stops mattering once rotation completes.

## Non-goals

- Rotating the CA itself (agents are enrolled against the CA; the CA
  private key is only *re-encrypted* here, not replaced). CA rotation
  would force agent re-enrolment and is a separate project.
- Changing the cipher (AES-256-GCM stays) or the KDF. `SHA256(passphrase)`
  is not a password KDF (no salt, no work factor); replacing it with
  scrypt/argon2 would change every sealed value at once and is out of
  scope — note it as a follow-up, not part of rotation.
- Per-field or per-tenant keys.

## Design

### Key versions and the sealed format

- Introduce a key id, e.g. `k1`, `k2`, … (monotonic, stored in the
  settings table as `secret_key_version`). The sealed format gains the
  id: `enc:v2:<kid>:<base64(nonce‖ciphertext)>`. `enc:v1:` values keep
  their old meaning (key id `k1`, the pre-rotation key).
- `secret.Box` becomes a `BoxSet`: `{ current, previous *Box }`.
  - `Seal` always uses `current` and writes `enc:v2:<current-id>:`.
  - `Open` picks the box by the `kid` in the prefix; for `enc:v1:`
    values it tries `previous` first, then `current` (v1 has no id,
    so both must be tried). Unknown `kid` is an error, not a fallback
    to plaintext.
  - Plaintext (no prefix) input still reads unchanged, as today.

### Configuration

- `POLARIS_SECRET_KEY` = the **new** key (current).
- `POLARIS_SECRET_KEY_PREVIOUS` = the **old** key, only during the
  transition. It is supplied via env (never written to the DB or to
  backups) and removed from the environment once rotation completes.
- Startup validation: if any `enc:v2:` row exists whose `kid` is
  neither current nor previous, refuse to start (a half-finished
  rotation from a newer key would otherwise silently lose data).

### Field re-encryption migration

- A leader-only background job (the existing leader guard in
  `startJobs` already ensures one instance runs scheduled jobs).
- It scans sealed columns in batches (e.g. 500 rows per batch,
  smallest set first: settings, certificates, API tokens, CDN
  credentials, TOTP secrets — the full list is every `Box.Seal` /
  `Box.MustOpen` call site). Per row: `Open` with the `BoxSet`,
  `Seal` with `current`, update only if the ciphertext changed.
  Rows that fail to open are logged and skipped, never overwritten.
- Progress is resumable: the job records the last processed table +
  row id in the settings table, so a restart continues instead of
  restarting. It is idempotent: re-running on an already-rotated row
  is a no-op because `Seal` on a `v2` value with the current `kid`
  returns it unchanged.
- The CA private key file in `DataDir` is re-encrypted the same way
  (decrypt with `previous`, encrypt with `current`, write to a temp
  file and rename over it). Do it first, while the old key is still
  available, and keep a copy of the old file until the migration
  reports success.
- Plaintext legacy rows are left alone (they were readable before;
  re-encrypting them would change behaviour for rows that predate
  encryption).

### JWT cutover

- Add a `kid` header to newly issued tokens and verify against the
  key set: try the key named by `kid`, fall back to the single-key
  behaviour for tokens without `kid` (all tokens issued before
  rotation). Do not accept a `kid` that is not current or previous.
- New logins sign with the current key. Old tokens stay valid until
  they expire (at most 24h after rotation starts) or until an admin
  bumps `TokenVersion` (`auth.go`, `claims.Version`), which already
  invalidates all sessions for that admin — the emergency brake if
  the old key is suspected compromised.
- After `max(tokenTTL, migration completion)`, remove
  `POLARIS_SECRET_KEY_PREVIOUS` from the environment and restart:
  from then on only the current key verifies and decrypts. Any
  surviving old token or un-migrated row then fails loudly instead
  of silently falling back.

### Backups during and after rotation

- New backups are written with the current key and record the key
  version in the archive header, so a restore knows which key it
  needs.
- Restoring an old backup (which embeds the old key) must accept the
  old key explicitly, e.g. `polaris restore --previous-secret-key …`
  (or the env var), and the restore then re-encrypts everything with
  the current key as part of the restore. Never write the old key
  into a new backup.
- Keep at least one pre-rotation backup until the migration is
  verified; the staged-restore drill (`restore-drill`) is the check.

### Multi-instance behaviour

- Only the leader runs the migration. Non-leader instances learn the
  new key version from the settings table (`secret_key_version`):
  on each request that touches the `Box`, or on a short poll, they
  reload the `BoxSet` when the version changes. Until they reload,
  they can still read old rows (previous key) but must not write new
  sealed values with a stale current key — writes go through the
  leader's version check, or instances restart as part of the
  rollout. Simplest safe rollout: deploy the new key to all instances
  (both env vars set), restart them, then start the migration.
- If an instance starts with only the new key while `enc:v1:` rows
  still exist, it must refuse to serve writes that need those rows
  (fail closed) rather than overwrite them with undecryptable data.

## Rollback

- Before the previous key is removed from the environment, rollback
  is: stop the migration, set `POLARIS_SECRET_KEY` back to the old
  key, restart. Rows already re-encrypted to `v2` are still readable
  because `Open` tries both keys — as long as the new key is still
  supplied as `POLARIS_SECRET_KEY_PREVIOUS` during the rollback.
- After the previous key is removed, rollback is no longer possible;
  that step is the point of no return and should be a deliberate,
  separate operator action, not part of the migration job.

## Risks

- **Interrupted migration + lost old key**: if the old key is removed
  before every row is re-encrypted, the remaining rows are lost.
  Mitigation: the migration job reports a count of un-migrated rows;
  the "remove previous key" step is blocked while the count is > 0.
- **Backup restores**: an old backup restored without the old key
  fails; operators must know the restore needs the old key. Document
  it in the restore command's help, not just in this doc.
- **JWT `kid` confusion**: never let the `kid` header select a key
  outside the configured set, and never accept `alg: none`.
- **KDF weakness**: rotation does not fix `SHA256(passphrase)`.
  A future change to scrypt/argon2 needs its own migration (it
  changes the derived key for the same passphrase).

## Suggested implementation order

1. `BoxSet` + `enc:v2:<kid>:` format + dual-key JWT verification
   (no migration yet; new writes use v2, reads accept both).
2. Config: `POLARIS_SECRET_KEY_PREVIOUS`, startup validation, and
   the settings-table `secret_key_version`.
3. Leader-only re-encryption job + progress tracking + CA key
   re-encryption.
4. Backup header key version + restore `--previous-secret-key`.
5. Operator runbook: deploy both keys, restart, run migration,
   verify (restore-drill + un-migrated row count = 0), wait 24h,
   remove previous key, restart, verify again.

## Test plan (for the implementation PR)

- Unit: `BoxSet` opens v1/v2/plaintext, seals with current, rejects
  unknown `kid`; JWT verifies with both keys, rejects unknown `kid`.
- Integration: start with old key, create sealed rows + a session,
  rotate, run migration, assert all rows re-encrypted, old session
  still valid, new session uses new `kid`, restart without the
  previous key, assert everything still works.
- Failure: kill mid-migration, restart, assert resume + no lost rows;
  restore an old backup with and without the previous key.
