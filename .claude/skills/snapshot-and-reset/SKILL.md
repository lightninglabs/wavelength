---
name: snapshot-and-reset
description: >
  Snapshot a running darepo-client datadir under a descriptive label,
  then reset the live datadir for a fresh wallet against the same operator.
  Use when you want to preserve a daemon's current state (logs, DBs,
  encrypted seed, swap DB) for later debugging while starting clean with a
  new seed — e.g. before reproducing a wonky bug, switching test scenarios,
  or handing off a captured state to another engineer.
argument-hint: "<snapshot-label>   e.g. wonky-operator-key, before-restart-bug"
allowed-tools: Bash, Read, Edit, Write
---

# Snapshot and Reset

Snapshot the active darepo-client datadir to a labeled sibling directory, then
clear the wallet state from the live datadir so the daemon can be
re-initialized with a fresh seed. The snapshot remains self-launchable
(its conf is rewritten to point at its own path) so you can repoint at
it later without flag overrides.

The end state is **two datadirs**:

- `~/.darepod/` — live, no `wallet_seed.enc`, ready for `darepocli create`.
- `~/.darepod-<label>/` — full byte-level copy of the prior state, with a
  rewritten `datadir=` line and a `STATUS.md` marker.

The two daemons cannot run concurrently (they share RPC port 10029).

## When NOT to use this skill

- **`wallet.type=lnd` setups.** With the LND backend the daemon adopts
  LND's wallet identity — there is no local `wallet_seed.enc` to
  preserve, and "fresh" means changing the LND side, not the darepod
  side. See the walletdk on-ramp guide (Appendix B) in the
  `lightninglabs/lightning-infra` repo.
- **You just want a passive backup.** If you only need a copy and won't
  reset the live datadir, run `cp -r ~/.darepod ~/.darepod-<label>` and
  stop there. This skill is for the reset-and-restart flow.

## Phase 1: Validate preconditions

Run these checks and confirm with the user before doing anything destructive.

1. **Locate the daemon.** Find the running process and its config path:
   ```bash
   pgrep -lf darepod
   ps -p <pid> -o command=
   ```
   Note the `--configfile` argument; it points at the live datadir's conf.
   If multiple darepods are running, stop here and ask the user which one.

2. **Inspect the conf.** Read the `datadir=` line and the network. Verify
   the datadir matches what the user expects (typically `~/.darepod`).

3. **Confirm the backend.** `wallet.type=lwwallet` or `wallet.type=btcwallet`
   is fine. `wallet.type=lnd` → stop and refer to the "When NOT to use"
   section above.

4. **Check for an existing snapshot at the target path.** If
   `~/.darepod-<label>/` already exists, ask the user before overwriting.

5. **Present the plan and the snapshot label to the user.** Confirm
   before proceeding to Phase 2.

## Phase 2: Stop the daemon

A clean shutdown flushes the SQLite WAL into the main DBs, giving a
consistent snapshot without needing per-DB `VACUUM INTO` calls.

```bash
kill <pid>
while pgrep -x darepod >/dev/null 2>&1; do sleep 0.3; done
```

If the daemon doesn't exit within ~10 seconds, surface that to the user
rather than escalating to `kill -9` — a hung shutdown probably indicates
an actor that's mid-checkpoint, and forcing it risks DB corruption.

## Phase 3: Snapshot

```bash
cp -r <live-datadir> <live-datadir>-<label>
```

`cp -r` (not `mv`) because the engineering convention is "back up
everything, keep the original path stable." The live datadir continues
to exist under its original name; only its contents are reset in Phase 4.

After copying, rewrite the snapshot's `darepod.conf` so it self-references:

```
datadir=<live-datadir>         →   datadir=<live-datadir>-<label>
```

Use `Edit`; do not stream the conf through `sed` (the file is small and
the line is unique).

Drop a `STATUS.md` at `<live-datadir>-<label>/STATUS.md` with:

- Status (SNAPSHOT — do not run concurrently).
- Capture date and the reason / label.
- Network and operator.
- Pointer back to the live datadir.
- The exact `darepod --configfile ...` command to repoint at this
  snapshot, including the reminder to stop the live daemon first
  (shared RPC port).
- The reverse command to return to the live instance.

## Phase 4: Reset the live datadir

Remove only the wallet/operator state. **Keep** `darepod.conf` and
`wallet_password` (it auto-unlocks the new seed if the user reuses the
same password during `create`).

```bash
rm -rf <live-datadir>/data <live-datadir>/logs \
       <live-datadir>/swaps.db <live-datadir>/darepod.log
```

After cleanup, the live datadir should contain only:
- `darepod.conf`
- `wallet_password` (optional; remove if the user wants a different
  password for the fresh wallet)

Drop a `STATUS.md` at `<live-datadir>/STATUS.md` describing the fresh
instance and pointing at the snapshot.

## Phase 5: Restart the daemon

Launch with the **same** configfile the previous daemon used:

```bash
<binary-path> --configfile <live-datadir>/darepod.conf
```

Run in the background. Tail the persistent log at
`<live-datadir>/logs/<network>/darepod.log` and verify:

```
[INF] DRPD: Wallet not ready, waiting for InitWallet or UnlockWallet RPC
[INF] DRPD: Daemon ready
[INF] DRPD: gRPC server listening addr=127.0.0.1:10029
```

If the daemon exits during startup, check the migration block — a fresh
DB will replay every migration. That's normal but verbose.

## Phase 6: Hand off to wallet creation

**Do not run `darepocli create` from this skill unless the user
explicitly approves printing the mnemonic to the session transcript.**
The mnemonic is sensitive even on signet/testnet — surface a default of
capturing it to a local file:

```bash
darepocli create --no-tls \
  --wallet_password_file <live-datadir>/wallet_password \
  2> ~/seed-fresh-$(date +%Y%m%d-%H%M%S).txt
```

After approval (signet/testnet sessions only), it's reasonable to run
`create` inline and emit the mnemonic + `identity_pubkey` directly. Do
not do this for mainnet — `Config.Validate()` rejects mainnet on this
deployment, but the rule stands.

Verify with:

```bash
darepocli --no-tls getinfo     # wallet_state=WALLET_STATE_READY
darepocli --no-tls balance     # all zeros
```

## Phase 7: Persist memory

Save a project-type auto-memory entry recording both datadir paths,
labels, and the reason for the snapshot. Future sessions inspecting
`~/.darepod*` will then immediately know which is live and which is
retired.

## Rollback

To return to the snapshot's state:

```bash
kill $(pgrep darepod)
<binary-path> --configfile <live-datadir>-<label>/darepod.conf
```

The snapshot's rewritten `datadir=` line makes this work with no flag
overrides. The new (post-snapshot) wallet's state at `<live-datadir>/`
is untouched.

## Gotchas

- **RPC port conflict.** Both datadirs' confs default to
  `rpc.listenaddr=localhost:10029`. The snapshot's conf is unchanged
  except for the `datadir=` line, so concurrent daemons fail to bind.
  Stop one before starting the other.
- **Different identities, same operator.** Fresh seed → different
  `localMailboxID` derived via `serverconn.PubKeyMailboxID`. The
  operator sees the two instances as distinct clients; no server-side
  conflict.
- **`darepocli` and `darepod` may not be on `$PATH`.** Default install
  via `make install-walletrpc` drops them in `$GOPATH/bin`. Use the
  absolute path the daemon was launched from when in doubt.
- **`wallet_password` reuse.** Carrying over the prior `wallet_password`
  file means the fresh wallet will be encrypted under the same password,
  which auto-unlocks on restart. If the user wants a clean break, delete
  the file in Phase 4 before `create`.
