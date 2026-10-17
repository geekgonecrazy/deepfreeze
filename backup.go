package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// Manifest is the sidecar JSON written next to a full backup. It records the
// oplog head timestamp captured *before* the dump began, which is the chain
// start for any subsequent incrementals. A full without a manifest is treated
// as incomplete and ignored by the incremental job and by `thaw`.
type Manifest struct {
	Version      int        `json:"version"`
	Type         string     `json:"type"`
	Database     string     `json:"database"`
	CreatedAt    string     `json:"created_at"`
	OplogStartTs ManifestTs `json:"oplog_start_ts"`
	ArchivePath  string     `json:"archive_path"`
	SHA256       string     `json:"sha256"`
	SizeBytes    int64      `json:"size_bytes"`
}

type ManifestTs struct {
	T uint32 `json:"t"`
	I uint32 `json:"i"`
}

func (m ManifestTs) toBSON() bson.Timestamp {
	return bson.Timestamp{T: m.T, I: m.I}
}

func Freeze(ctx context.Context, mode string) error {
	sendRCWebhookTextMessage(fmt.Sprintf("Starting Freeze (%s)! Databases: %s", mode, strings.Join(Databases, ", ")))

	if err := doFreeze(ctx, mode); err != nil {
		sendRCWebhookTextMessage(fmt.Sprintf("Freeze (%s) Failed! Databases: %s Err:\n %s", mode, strings.Join(Databases, ", "), err))
		return err
	}

	sendRCWebhookTextMessage(fmt.Sprintf("Freeze (%s) Finished! Databases: %s", mode, strings.Join(Databases, ", ")))
	return nil
}

func doFreeze(ctx context.Context, mode string) error {
	log.Println("Testing Replicaset health before trying to do backup")
	if err := testReplicaSet(ctx); err != nil {
		return err
	}

	switch mode {
	case "full":
		for _, database := range Databases {
			if err := runFullBackup(ctx, database); err != nil {
				return err
			}
		}
		return nil
	case "incremental":
		return runIncrementalBackup(ctx)
	default:
		return fmt.Errorf("unknown freeze mode %q (expected: full, incremental)", mode)
	}
}

func testReplicaSet(ctx context.Context) error {
	cs := strings.Replace(ConnectionURL, "{DatabaseName}", "test", -1)
	client, err := getMongoClient(ctx, cs)
	if err != nil {
		return err
	}
	defer client.Disconnect(context.Background())
	return checkReplicaSetOk(ctx, client)
}

func runFullBackup(ctx context.Context, database string) error {
	log.Println("Backing up Database: ", database)

	cs := strings.Replace(ConnectionURL, "{DatabaseName}", database, -1)

	// Capture the oplog head ts BEFORE the dump starts. Any writes during the
	// dump are inside mongodump --oplog's archive AND will reappear in the
	// next incremental — replays are idempotent so the overlap is harmless.
	oplogStartTs, err := readOplogHead(ctx, cs)
	if err != nil {
		return fmt.Errorf("read oplog head: %w", err)
	}

	backupTime := time.Now().UTC()
	timestamp := backupTime.Format("2006-01-02T15-04-05Z")
	archiveKey := fmt.Sprintf("%s/%s/full/%s.gz.age", S3Folder, database, timestamp)
	manifestKey := fmt.Sprintf("%s/%s/full/%s.manifest.json", S3Folder, database, timestamp)

	uploadURL, err := getS3UploadURL(archiveKey, 2*time.Hour)
	if err != nil {
		return fmt.Errorf("presigned upload URL: %w", err)
	}

	tmpDir, err := os.MkdirTemp("", "deepfreeze-full-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)

	filePath := filepath.Join(tmpDir, fmt.Sprintf("%s-%s.gz.age", database, timestamp))

	encryptionCommand := fmt.Sprintf("age -r %s", strings.Join(BackupKeys, " -r "))
	dumpCommand := fmt.Sprintf("mongodump --uri='%s' --archive --gzip --oplog | %s > %s", cs, encryptionCommand, filePath)

	log.Println("Performing Database Backup")
	if _, err := runCommand(dumpCommand, 60, false); err != nil {
		return fmt.Errorf("mongodump: %w", err)
	}

	hash, size, err := hashAndSize(filePath)
	if err != nil {
		return fmt.Errorf("hash archive: %w", err)
	}

	log.Printf("Uploading archive %s (%d bytes, sha256=%s)", archiveKey, size, hash)
	if _, err := runCommand(fmt.Sprintf("curl --upload-file %s \"%s\"", filePath, uploadURL), 60, false); err != nil {
		return fmt.Errorf("upload archive: %w", err)
	}

	// Write manifest LAST. A full without a manifest is invisible to thaw
	// and to the incremental job, so a half-finished upload is safely ignored.
	manifest := Manifest{
		Version:      1,
		Type:         "full",
		Database:     database,
		CreatedAt:    backupTime.Format(time.RFC3339),
		OplogStartTs: ManifestTs{T: oplogStartTs.T, I: oplogStartTs.I},
		ArchivePath:  archiveKey,
		SHA256:       hash,
		SizeBytes:    size,
	}
	body, err := json.Marshal(manifest)
	if err != nil {
		return fmt.Errorf("marshal manifest: %w", err)
	}
	if err := putS3Object(manifestKey, body, "application/json"); err != nil {
		return fmt.Errorf("upload manifest: %w", err)
	}

	sendRCWebhookTextMessage(fmt.Sprintf("Full backup completed: db=%s key=%s sha256=%s size=%.2fMB oplog_start=%s",
		database, archiveKey, hash, bytesToMegaBytes(size), tsString(oplogStartTs)))
	return nil
}

func runIncrementalBackup(ctx context.Context) error {
	log.Println("Running incremental backup (oplog dump)")

	if len(Databases) == 0 {
		return errors.New("no databases configured")
	}
	// Oplog is replica-set-global — any DB-substituted URI works for the connection.
	cs := strings.Replace(ConnectionURL, "{DatabaseName}", Databases[0], -1)

	client, err := getMongoClient(ctx, cs)
	if err != nil {
		return fmt.Errorf("connect mongo: %w", err)
	}
	defer client.Disconnect(context.Background())

	fromTs, source, err := findChainStart()
	if err != nil {
		return fmt.Errorf("find chain start: %w", err)
	}
	log.Printf("Chain start ts=%s (source=%s)", tsString(fromTs), source)

	// Gap detection: oldest live oplog entry must be <= fromTs.
	oplogTailTs, err := getOplogTailTs(ctx, client)
	if err != nil {
		return fmt.Errorf("read oplog tail: %w", err)
	}
	if compareTs(oplogTailTs, fromTs) > 0 {
		msg := fmt.Sprintf("BROKEN_CHAIN: oldest oplog ts %s is past last known to_ts %s. Run a full backup to reset the chain.",
			tsString(oplogTailTs), tsString(fromTs))
		sendRCWebhookTextMessage(msg)
		return errors.New(msg)
	}

	oplogHeadTs, err := getOplogHeadTs(ctx, client)
	if err != nil {
		return fmt.Errorf("read oplog head: %w", err)
	}

	if compareTs(oplogHeadTs, fromTs) <= 0 {
		log.Println("No new oplog entries since last incremental; skipping")
		return nil
	}

	tmpDir, err := os.MkdirTemp("", "deepfreeze-incr-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)

	dumpDir := filepath.Join(tmpDir, "dump")
	queryJSON := fmt.Sprintf(`{"ts":{"$gt":{"$timestamp":{"t":%d,"i":%d}}}}`, fromTs.T, fromTs.I)
	dumpCmd := fmt.Sprintf("mongodump --uri='%s' --db=local --collection=oplog.rs --query='%s' --out=%s",
		cs, queryJSON, dumpDir)
	if _, err := runCommand(dumpCmd, 60, false); err != nil {
		return fmt.Errorf("mongodump oplog: %w", err)
	}

	bsonPath := filepath.Join(dumpDir, "local", "oplog.rs.bson")
	if _, err := os.Stat(bsonPath); err != nil {
		return fmt.Errorf("expected oplog dump at %s: %w", bsonPath, err)
	}

	incrName := fmt.Sprintf("%s_%s.bson.gz.age", tsString(fromTs), tsString(oplogHeadTs))
	archiveKey := fmt.Sprintf("%s/oplog/%s", S3Folder, incrName)
	encPath := filepath.Join(tmpDir, incrName)

	encryptionCommand := fmt.Sprintf("age -r %s", strings.Join(BackupKeys, " -r "))
	pipeCmd := fmt.Sprintf("gzip -c %s | %s > %s", bsonPath, encryptionCommand, encPath)
	if _, err := runCommand(pipeCmd, 30, false); err != nil {
		return fmt.Errorf("encrypt/gzip oplog: %w", err)
	}

	hash, size, err := hashAndSize(encPath)
	if err != nil {
		return fmt.Errorf("hash incremental: %w", err)
	}

	uploadURL, err := getS3UploadURL(archiveKey, 2*time.Hour)
	if err != nil {
		return fmt.Errorf("presigned upload URL: %w", err)
	}
	if _, err := runCommand(fmt.Sprintf("curl --upload-file %s \"%s\"", encPath, uploadURL), 60, false); err != nil {
		return fmt.Errorf("upload incremental: %w", err)
	}

	sendRCWebhookTextMessage(fmt.Sprintf("Incremental complete: key=%s sha256=%s size=%.2fMB span=%s..%s",
		archiveKey, hash, bytesToMegaBytes(size), tsString(fromTs), tsString(oplogHeadTs)))
	return nil
}

// findChainStart returns the timestamp from which the next incremental should
// dump (i.e. the previous to_ts). Preference order:
//  1. Latest to_ts among existing oplog/ incrementals.
//  2. Latest full's manifest oplog_start_ts.
// Returns an error if no full backup with a manifest exists yet.
func findChainStart() (bson.Timestamp, string, error) {
	keys, err := listS3Objects(fmt.Sprintf("%s/oplog/", S3Folder))
	if err != nil {
		return bson.Timestamp{}, "", err
	}
	var latestTo bson.Timestamp
	var found bool
	for _, k := range keys {
		_, to, err := parseIncrementalKey(k)
		if err != nil {
			log.Printf("skipping unparseable oplog key %q: %v", k, err)
			continue
		}
		if !found || compareTs(to, latestTo) > 0 {
			latestTo = to
			found = true
		}
	}
	if found {
		return latestTo, "incremental tail", nil
	}

	ts, err := findLatestFullManifestStartTs()
	if err != nil {
		return bson.Timestamp{}, "", err
	}
	return ts, "full manifest", nil
}

func findLatestFullManifestStartTs() (bson.Timestamp, error) {
	var latestTs bson.Timestamp
	var found bool
	for _, db := range Databases {
		keys, err := listS3Objects(fmt.Sprintf("%s/%s/full/", S3Folder, db))
		if err != nil {
			return bson.Timestamp{}, err
		}
		for _, k := range keys {
			if !strings.HasSuffix(k, ".manifest.json") {
				continue
			}
			body, err := getS3Object(k)
			if err != nil {
				log.Printf("skipping unreadable manifest %s: %v", k, err)
				continue
			}
			var m Manifest
			if err := json.Unmarshal(body, &m); err != nil {
				log.Printf("skipping malformed manifest %s: %v", k, err)
				continue
			}
			t := m.OplogStartTs.toBSON()
			if !found || compareTs(t, latestTs) > 0 {
				latestTs = t
				found = true
			}
		}
	}
	if !found {
		return bson.Timestamp{}, errors.New("no full backup with manifest found; run a full backup first")
	}
	return latestTs, nil
}

func readOplogHead(ctx context.Context, uri string) (bson.Timestamp, error) {
	client, err := getMongoClient(ctx, uri)
	if err != nil {
		return bson.Timestamp{}, err
	}
	defer client.Disconnect(context.Background())
	return getOplogHeadTs(ctx, client)
}

func hashAndSize(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}
