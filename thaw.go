package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

func Thaw(ctx context.Context) error {
	targetURL := os.Getenv("THAW_TARGET_URL")
	if targetURL == "" {
		targetURL = ConnectionURL
	}
	if targetURL == "" {
		return errors.New("THAW_TARGET_URL or CONNECTION_URL required")
	}

	identity := os.Getenv("BACKUP_IDENTITY")
	if identity == "" {
		return errors.New("BACKUP_IDENTITY required for thaw (age private key)")
	}

	thawDatabases := Databases
	if t := os.Getenv("THAW_DATABASES"); t != "" {
		thawDatabases = strings.Split(t, ",")
	}
	if len(thawDatabases) == 0 {
		return errors.New("no databases to thaw (set DATABASES or THAW_DATABASES)")
	}

	fullID := os.Getenv("THAW_FULL_ID")
	targetTime := os.Getenv("THAW_TARGET_TIME")

	sendRCWebhookTextMessage(fmt.Sprintf("Starting Thaw! databases=%s full=%s target_time=%s",
		strings.Join(thawDatabases, ","), defaultIfEmpty(fullID, "latest"), defaultIfEmpty(targetTime, "latest")))

	var oplogLimit *bson.Timestamp
	if targetTime != "" && targetTime != "latest" {
		ts, err := parseTargetTime(targetTime)
		if err != nil {
			err = fmt.Errorf("THAW_TARGET_TIME: %w", err)
			sendRCWebhookTextMessage(fmt.Sprintf("Thaw failed: %s", err))
			return err
		}
		oplogLimit = &ts
	}

	tmpDir, err := os.MkdirTemp("", "deepfreeze-thaw-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)

	keyPath := filepath.Join(tmpDir, "identity.txt")
	if err := os.WriteFile(keyPath, []byte(identity), 0o600); err != nil {
		return fmt.Errorf("write identity: %w", err)
	}

	if err := doThaw(ctx, tmpDir, keyPath, targetURL, thawDatabases, fullID, oplogLimit); err != nil {
		sendRCWebhookTextMessage(fmt.Sprintf("Thaw failed: %s", err))
		return err
	}
	return nil
}

func doThaw(_ context.Context, tmpDir, keyPath, targetURL string, databases []string, fullID string, oplogLimit *bson.Timestamp) error {
	fulls, err := resolveFulls(databases, fullID)
	if err != nil {
		return fmt.Errorf("resolve full(s): %w", err)
	}

	for _, full := range fulls {
		log.Printf("Restoring full: %s (oplog_start=%s)", full.ArchivePath, tsString(full.OplogStartTs.toBSON()))
		if err := restoreFull(tmpDir, keyPath, targetURL, full); err != nil {
			return fmt.Errorf("restore full %s: %w", full.ArchivePath, err)
		}
	}

	earliestStart := fulls[0].OplogStartTs.toBSON()
	for _, f := range fulls[1:] {
		if compareTs(f.OplogStartTs.toBSON(), earliestStart) < 0 {
			earliestStart = f.OplogStartTs.toBSON()
		}
	}

	incrs, err := findIncrementalsAfter(earliestStart, oplogLimit)
	if err != nil {
		return fmt.Errorf("list incrementals: %w", err)
	}
	log.Printf("Applying %d incremental(s) starting from %s", len(incrs), tsString(earliestStart))

	lastTo := earliestStart
	for _, incr := range incrs {
		log.Printf("Applying incremental: %s", incr.Key)
		if err := restoreIncremental(tmpDir, keyPath, targetURL, incr.Key, databases, oplogLimit); err != nil {
			return fmt.Errorf("apply %s: %w", incr.Key, err)
		}
		lastTo = incr.To
	}

	recoveryPoint := tsString(lastTo)
	if oplogLimit == nil && len(incrs) > 0 {
		now := uint32(time.Now().Unix())
		if now > lastTo.T {
			lag := now - lastTo.T
			if lag > 600 {
				recoveryPoint += fmt.Sprintf(" (WARNING: %ds behind wall clock — incrementals may be lagging)", lag)
			}
		}
	}
	if len(incrs) == 0 {
		recoveryPoint += " (full only; no incrementals applied)"
	}

	sendRCWebhookTextMessage(fmt.Sprintf("Thaw complete: target=%s databases=%s recovery_point=%s",
		targetURL, strings.Join(databases, ","), recoveryPoint))
	return nil
}

func resolveFulls(databases []string, fullID string) ([]Manifest, error) {
	var out []Manifest
	for _, db := range databases {
		keys, err := listS3Objects(fmt.Sprintf("%s/%s/full/", S3Folder, db))
		if err != nil {
			return nil, err
		}
		var candidates []Manifest
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
			if fullID != "" && fullID != "latest" && !strings.Contains(m.ArchivePath, fullID) {
				continue
			}
			candidates = append(candidates, m)
		}
		if len(candidates) == 0 {
			return nil, fmt.Errorf("no full backup found for database %q (full_id=%q)", db, fullID)
		}
		sort.Slice(candidates, func(i, j int) bool {
			return compareTs(candidates[i].OplogStartTs.toBSON(), candidates[j].OplogStartTs.toBSON()) > 0
		})
		out = append(out, candidates[0])
	}
	return out, nil
}

type incrementalRef struct {
	Key  string
	From bson.Timestamp
	To   bson.Timestamp
}

func findIncrementalsAfter(after bson.Timestamp, limit *bson.Timestamp) ([]incrementalRef, error) {
	keys, err := listS3Objects(fmt.Sprintf("%s/oplog/", S3Folder))
	if err != nil {
		return nil, err
	}
	var out []incrementalRef
	for _, k := range keys {
		from, to, err := parseIncrementalKey(k)
		if err != nil {
			log.Printf("skipping unparseable oplog key %q: %v", k, err)
			continue
		}
		// Drop incrementals fully before `after` (full already covers them via embedded oplog).
		if compareTs(to, after) <= 0 {
			continue
		}
		// Drop incrementals fully past the requested limit.
		if limit != nil && compareTs(from, *limit) > 0 {
			continue
		}
		out = append(out, incrementalRef{Key: k, From: from, To: to})
	}
	sort.Slice(out, func(i, j int) bool {
		return compareTs(out[i].From, out[j].From) < 0
	})
	return out, nil
}

func restoreFull(tmpDir, keyPath, targetURL string, m Manifest) error {
	url, err := getS3DownloadURL(m.ArchivePath, 2*time.Hour)
	if err != nil {
		return fmt.Errorf("presigned download URL: %w", err)
	}
	nsArgs := nsIncludeArgs([]string{m.Database})
	cmd := fmt.Sprintf(
		"set -o pipefail; curl -sfL %q | age -d -i %s | mongorestore --uri=%q --archive --gzip --oplogReplay %s",
		url, shellQuote(keyPath), targetURL, nsArgs,
	)
	if _, err := runCommand(cmd, 60, true); err != nil {
		return err
	}
	_ = tmpDir
	return nil
}

func restoreIncremental(tmpDir, keyPath, targetURL, archiveKey string, databases []string, oplogLimit *bson.Timestamp) error {
	url, err := getS3DownloadURL(archiveKey, 2*time.Hour)
	if err != nil {
		return fmt.Errorf("presigned download URL: %w", err)
	}

	bsonName := strings.TrimSuffix(filepath.Base(archiveKey), ".gz.age")
	bsonPath := filepath.Join(tmpDir, bsonName)
	emptyDir := filepath.Join(tmpDir, "empty_"+bsonName)
	if err := os.MkdirAll(emptyDir, 0o700); err != nil {
		return err
	}
	defer os.Remove(bsonPath)

	decryptCmd := fmt.Sprintf(
		"set -o pipefail; curl -sfL %q | age -d -i %s | gunzip > %s",
		url, shellQuote(keyPath), shellQuote(bsonPath),
	)
	if _, err := runCommand(decryptCmd, 30, true); err != nil {
		return fmt.Errorf("decrypt incremental: %w", err)
	}

	nsArgs := nsIncludeArgs(databases)
	limitArg := ""
	if oplogLimit != nil {
		limitArg = fmt.Sprintf("--oplogLimit=%d:%d", oplogLimit.T, oplogLimit.I)
	}
	restoreCmd := fmt.Sprintf(
		"mongorestore --uri=%q --oplogReplay --oplogFile=%s %s %s %s",
		targetURL, shellQuote(bsonPath), limitArg, nsArgs, shellQuote(emptyDir),
	)
	if _, err := runCommand(restoreCmd, 60, false); err != nil {
		return fmt.Errorf("mongorestore: %w", err)
	}
	return nil
}

func nsIncludeArgs(databases []string) string {
	parts := make([]string, 0, len(databases))
	for _, db := range databases {
		parts = append(parts, fmt.Sprintf("--nsInclude=%q", db+".*"))
	}
	return strings.Join(parts, " ")
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func parseTargetTime(s string) (bson.Timestamp, error) {
	if strings.Contains(s, ":") && !strings.ContainsAny(s, "-T") {
		parts := strings.SplitN(s, ":", 2)
		sec, e1 := strconv.ParseUint(parts[0], 10, 32)
		i, e2 := strconv.ParseUint(parts[1], 10, 32)
		if e1 == nil && e2 == nil {
			return bson.Timestamp{T: uint32(sec), I: uint32(i)}, nil
		}
	}
	if sec, err := strconv.ParseUint(s, 10, 32); err == nil {
		return bson.Timestamp{T: uint32(sec), I: 0}, nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return bson.Timestamp{T: uint32(t.Unix()), I: 0}, nil
	}
	return bson.Timestamp{}, fmt.Errorf("unrecognized target time %q (use RFC3339, epoch seconds, or seconds:ordinal)", s)
}

func defaultIfEmpty(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
