# deepfreeze

**Note** Use at your own risk!  Also subject to change.

## What is it?

This tool makes encrypted backups of your mongo database and ships them to an s3-compatible bucket. It supports two modes:

- **Full backup** (`freeze full`) — a complete `mongodump` per database, encrypted with [age](https://age-encryption.org). Runs daily.
- **Incremental backup** (`freeze incremental`) — a small dump of just the oplog entries since the last successful run. Runs hourly (or every 30 min if you're feeling crazy).

Together they let you keep RPO around an hour without paying for a full dump every hour. Restore is via the `thaw` subcommand, which downloads the latest valid full and replays each incremental in order against a target Mongo instance.

The tool checks replica-set health before any backup; if a member is down, it refuses to run.

## How does the chain work?

1. A **full** writes two objects to S3:
   - `<S3_FOLDER>/<dbname>/full/<utc>.gz.age` — encrypted archive (`mongodump --archive --gzip --oplog`)
   - `<S3_FOLDER>/<dbname>/full/<utc>.manifest.json` — sidecar JSON recording `oplog_start_ts`, sha256, size. Written **after** the archive uploads, so a half-finished full is invisible to thaw.

2. An **incremental** dumps `local.oplog.rs` filtered to entries newer than the last known `to_ts`, encrypts, and writes one object:
   - `<S3_FOLDER>/oplog/<from-ts>_<to-ts>.bson.gz.age`

   Before dumping, it checks that the *oldest* live oplog entry is still ≤ the last `to_ts`. If not, it fails loudly with a `BROKEN_CHAIN` webhook — you've lost coverage and need to run a fresh full.

3. **Thaw** picks the latest manifest-backed full, restores it, then walks the `oplog/` prefix in order, replaying each incremental.

### Oplog window sizing

This is now a load-bearing assumption: your replica set's oplog must comfortably hold ≥24h of writes (cadence of fulls, not incrementals). Inspect with:

```javascript
db.printReplicationInfo()
```

If `log length` is shorter than your full→full interval, resize the oplog or run fulls more often.

## How do I use it?

If you aren't using the docker image you will need `mongodump`, `mongorestore`, and `age` installed.

You'll need an age key pair (`age-keygen`). Keep the private one safe; the public one(s) go in `BACKUP_KEYS`. The private one is needed only for `thaw` (`BACKUP_IDENTITY`).

Configuration is 100% environment variables. Subcommands:

```
deepfreeze freeze full           # full backup of every DB in DATABASES
deepfreeze freeze incremental    # one global oplog dump
deepfreeze thaw                  # restore latest chain to THAW_TARGET_URL
deepfreeze                       # alias for `freeze full` (backwards compat)
```

For Kubernetes, run two CronJobs sharing one image:
- `examples/k8s-cronjob.yaml` — full, daily
- `examples/k8s-cronjob-incremental.yaml` — incremental, hourly

### Environment variables (freeze)

| Variable | Description | Example | Required |
|---|---|---|---|
| `BACKUP_KEYS` | Comma-separated age public keys (recipients). | `age1abc...` | yes |
| `CONNECTION_URL` | Mongo URI; must contain `{DatabaseName}` if you back up multiple DBs. | `mongodb://user:pw@mongo-1,mongo-2,mongo-3/{DatabaseName}?replicaSet=rs01` | yes |
| `DATABASES` | Comma-separated DBs to back up. | `product1,product2` | yes |
| `S3_ENDPOINT` | S3 endpoint host. | `s3.us-west-000.backblazeb2.com` | yes |
| `S3_BUCKET` | Bucket name. | `my-encrypted-backups` | yes |
| `S3_ACCESS_ID` | S3 access ID. | `000000000300000` | yes |
| `S3_ACCESS_KEY` | S3 access key. | `asdkajsf0382h082h38f0hf` | yes |
| `S3_REGION` | Region. | `us-west-000` | yes |
| `S3_FOLDER` | Prefix in the bucket. | `backups` | no (default `backups`) |
| `RC_WEBHOOK` | Rocket.Chat incoming-webhook URL for status messages. | `https://your-rc.com/hooks/{secret}` | no |

### Environment variables (thaw)

`thaw` inherits the freeze variables (it needs S3 + `DATABASES` + `CONNECTION_URL` as a default target). Additionally:

| Variable | Description | Default |
|---|---|---|
| `BACKUP_IDENTITY` | age private key (the matching identity for `BACKUP_KEYS`). | required |
| `THAW_TARGET_URL` | Mongo URI to restore *into*. | falls back to `CONNECTION_URL` |
| `THAW_DATABASES` | Comma-separated subset of DBs to restore. | falls back to `DATABASES` |
| `THAW_FULL_ID` | Substring matched against archive paths to pick a specific full (e.g. `2026-04-29T00-34-00Z`). | `latest` (newest by `oplog_start_ts`) |
| `THAW_TARGET_TIME` | Stop replay at this point. RFC3339 (`2026-04-29T03:00:00Z`), epoch seconds (`1714356000`), or `<seconds>:<ordinal>` for exact BSON Timestamp. | `latest` (no limit) |

## Failure modes

| Scenario | Behaviour |
|---|---|
| Transient incremental failure | K8s `OnFailure` retries up to `backoffLimit`. |
| Whole hourly Job fails | Next hourly run resumes from the previous `to_ts`. **Self-healing.** |
| Many failures, oplog rolls past last `to_ts` | Gap detection trips; webhook prefixed `BROKEN_CHAIN` fires. **Previous full + incrementals remain valid restore points.** Trigger an early full to reset the chain: `kubectl create job --from=cronjob/backup-full chain-reset-$(date +%s)`. |
| Mid-upload S3 failure (full) | Manifest is written last; archives without manifests are ignored. |
| `thaw` with stale incrementals | Restore proceeds and the final webhook reports the recovery point along with a warning if it is far behind wall clock. |

## Manual restore

`deepfreeze thaw` is the recommended path. If you want to drive it by hand:

```bash
# Decrypt the full archive and restore it
curl -sfL "<presigned-get-url>" | age -d -i keyFile | \
  mongorestore --uri='<target>' --archive --gzip --oplogReplay --nsInclude='product1.*'

# For each incremental, in ascending order of from_ts:
curl -sfL "<presigned-get-url>" | age -d -i keyFile | gunzip > oplog.bson
mkdir empty/
mongorestore --uri='<target>' --oplogReplay --oplogFile=./oplog.bson empty/
```

## Example webhook output

```
00:34 — Starting Freeze (full)! Databases: product1
00:34 — Full backup completed: db=product1 key=backups/product1/full/2026-04-29T00-34-12Z.gz.age sha256=e844... size=4.63MB oplog_start=1714353252.1
00:34 — Freeze (full) Finished! Databases: product1
01:00 — Starting Freeze (incremental)! Databases: product1
01:00 — Incremental complete: key=backups/oplog/1714353252.1_1714356812.4.bson.gz.age sha256=2f1c... size=0.18MB span=1714353252.1..1714356812.4
01:00 — Freeze (incremental) Finished! Databases: product1
```

## Testing

There's an end-to-end integration test that exercises the full chain (full → mutate → incremental → drop → thaw → verify) against real containers — Mongo replica set + SeaweedFS S3 — via [testcontainers-go](https://golang.testcontainers.org/).

```
go test -tags=integration -count=1 -v ./...
```

It requires Docker plus `mongodump`, `mongorestore`, and `age` on the host (the production code shells out to all three). If any are missing the test skips with a hint:

```
brew install age mongodb/brew/mongodb-database-tools
```

A plain `go test ./...` (no tag) skips the integration test entirely so contributors without Docker aren't blocked.

## FAQ

**Q: Why deepfreeze?**
Because I'm terrible at names. :)

**Q: Why Go and not a bash script?**
Because for me it's what I'm most comfortable with. Wrapping system commands in extra logic is much faster for me in Go than in bash.
