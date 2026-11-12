# Secret key rotation

Status: implemented (2026-11-12, branch `feature/secret-key-rotation`) —
`secret.BoxSet` + `enc:v2:<kid>:` format, dual-key JWT, the leader-only
re-encryption migration, backup key version + `--previous-secret-key`
restore, and the operator runbook in the user guide (§13.1). The
"Current state" section below describes the pre-rotation code; the
"Implementation notes" section at the end records where the
implementation deliberately deviates from this design.

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

- Key ids are derived deterministically from the passphrase: the first 8
  hex characters of `SHA256("kid:"+passphrase)` (`secret.KidFor`). This
  deviates from the sequential `k1`, `k2`, … ids in the original design:
  a kid can then be computed at construction time without reading the
  database, and two panels with the same key always agree on the kid.
  The id is not secret; it only selects which key opens a value. The
  settings table still records the current version as
  `secret_key_version` (the kid, not a sequence number).
- The sealed format gains the id:
  `enc:v2:<kid>:<base64(nonce‖ciphertext)>`. `enc:v1:` values keep their
  old meaning (no id; the key that sealed them is unknown).
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

## Implementation notes (2026-11-12)

What was built, and where it deliberately differs from the design above.

- **BoxSet** (`internal/secret/boxset.go`): `{ current, previous *Box }`
  plus extra keys added with `WithKey` (restore source keys). It keeps
  the `Box` signatures (`Seal`/`Open`/`MustOpen`), so the ~70 call sites
  did not change; only the construction sites and the `Store.Box` field
  type changed. `Seal` always writes `enc:v2:<current-kid>:` and
  upgrades an openable sealed value to the current key; a value that
  cannot be opened is returned unchanged, never destroyed. `Reseal` /
  `ResealText` are the migration primitives: plaintext is returned
  unchanged, an already-current v2 value is a no-op, an unopenable
  token is an error. `Box` itself is unchanged and still backs the set.
- **Config**: `SecretKeyPrevious` (`POLARIS_SECRET_KEY_PREVIOUS`,
  mapped automatically by the `POLARIS_` prefix handling), validated
  like the main key (≥16 chars, no `change-me`) and rejected when equal
  to the main key. `NewBoxSet` enforces the same rule, so a Config
  built in code (tests, embedders) gets it too.
- **Startup validation** (`validateKeyVersion`, called in `panel.New`
  after `store.Open` and after any staged restore is applied): the
  settings `secret_key_version` must name the current or the previous
  kid, otherwise the panel refuses to start. An absent setting means a
  pre-rotation database, which adopts the current kid. The design's
  "refuse when any `enc:v2:` row has an unknown kid" is implemented via
  this setting rather than by scanning rows at startup.
- **Migration** (`internal/panel/secret_migration.go`): leader-only
  (the `startJobs` leader guard, plus a goroutine in `Run` at startup),
  500 rows per batch, resumable progress in the settings table
  (`secret_key_migration`: `{kid, table, last_id, updated, failed,
  failed_rows, done}`). The 18 migrated tables are enumerated in
  `secretMigrationTargets` (settings, cluster_secrets, certificates,
  config_revisions, admins, passkeys, plugins, sub_sources,
  sub_source_nodes, node_providers, sso_providers, dns_providers,
  cdn_profiles, chains, webhooks, refund_jobs, ephemeral_kvs,
  inbounds) — ephemeral short-lived rows are included so a code issued
  before rotation still redeems after it. JSON columns (inbound
  protocol/transport/security/extra, plugin settings) are handled by
  the same token replacement inside the JSON text. Updates are guarded
  (`WHERE id = ? AND col = <old value>`) so a concurrent write wins.
  The CA key file is re-encrypted first (temp file + rename, old copy
  kept as `ca.key.prev` until the migration succeeds).
+- **Settings cache**: the migration updates the settings table directly,
  not through `SetSetting`, so `runSecretMigration` invalidates the
  settings cache on every exit path (`Store.InvalidateSettingsCache`);
  otherwise reads would serve the old ciphertext for up to `settingTTL`
  (1 minute) after the migration. The old ciphertext still opens while
  the previous key is configured, so this is a freshness fix, not a
  correctness one — covered by
  `TestSecretKeyMigrationRefreshesSettingsCache`.
- **Failed rows**: a run that ends with failures stays `done=false`,
  so the next 15-minute tick or restart retries it; the failed rows
  are logged and listed in the progress setting, and never
  overwritten. There is no config for a *third* key: a row sealed with
  a key outside `{current, previous}` stays failed and preserved, and
  the operator must re-create that credential.
- **One-time convergence**: the migration also runs once on a panel
  that never rotated (no previous key configured), upgrading pre-rotation
  `enc:v1:` rows sealed with the current key to `enc:v2:`. After that
  first run it is a no-op until the next rotation.
- **JWT**: `signToken` stamps `kid` on every new token (admin, Telegram,
  user center, session renewal); `parseToken` verifies with the key the
  kid names, falls back to the previous key only for tokens without a
  kid, and never retries a token whose kid names the wrong key. The
  rule-set HMAC (`schemes.go`) keeps using the current key bytes, no kid.
- **Backups**: new archives record the kid in `manifest.json`
  (`keyVersion`). Restoring an old backup accepts the old key via
  `polaris restore --previous-secret-key …` (or
  `POLARIS_SECRET_KEY_PREVIOUS`, or the restore form's
  `previousSecretKey` field). The API path re-seals the archive at
  *stage* time — the restart that applies the restore cannot carry
  request parameters — and the staged archive no longer contains the
  old key; the CLI path re-seals at restore time. The old key is never
  written into a new backup. The staged transform also drops the source
  panel's migration progress and records this panel's key version.
- **Not implemented**: non-leader instances do *not* reload their
  `BoxSet` from the settings table (the design's "learn the new
  version" poll). The supported rollout is the one in the design's
  "simplest safe rollout": deploy both keys to every instance, restart
  them all, then let the leader migrate. A running instance that never
  restarts keeps serving with the keys it started with.

## Test plan (for the implementation PR)

Implemented in `internal/secret/boxset_test.go` and
`internal/panel/secret_rotation_test.go` (all passing, see the branch):

- Unit: `BoxSet` opens v1/v2/plaintext, seals with current, rejects
  unknown `kid`; JWT verifies with both keys, rejects unknown `kid`.
- Integration: start with old key, create sealed rows + a session,
  rotate, run migration, assert all rows re-encrypted, old session
  still valid, new session uses new `kid`, restart without the
  previous key, assert everything still works.
- Failure: kill mid-migration, restart, assert resume + no lost rows;
  restore an old backup with and without the previous key.

Covered as written, except the mid-migration kill is approximated by
running the migration twice (second run is a no-op) and by the
unopenable-row test (failed rows are reported, retried, and never
overwritten); no test actually kills the process mid-run. The backup
restore is covered both ways: without the old key the stage is
refused, with `--previous-secret-key` (the `previousKey` argument) it
restores and re-seals with the new key.
