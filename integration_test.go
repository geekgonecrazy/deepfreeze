//go:build integration

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
	minio "github.com/minio/minio-go"
	"github.com/testcontainers/testcontainers-go"
	tcmongo "github.com/testcontainers/testcontainers-go/modules/mongodb"
	"github.com/testcontainers/testcontainers-go/wait"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// requiredHostBinaries are invoked via shell by the production code (mongodump
// during freeze, mongorestore + age during thaw). The test runs the real code
// paths, so if any of them are missing on the host, the test cannot run.
var requiredHostBinaries = []string{"mongodump", "mongorestore", "age"}

func skipIfHostMissing(t *testing.T) {
	t.Helper()
	var missing []string
	for _, bin := range requiredHostBinaries {
		if _, err := exec.LookPath(bin); err != nil {
			missing = append(missing, bin)
		}
	}
	if len(missing) > 0 {
		t.Skipf("integration test needs %s on PATH; install via: brew install age mongodb/brew/mongodb-database-tools", strings.Join(missing, ", "))
	}
}

type testEnv struct {
	mongoURI   string
	seedClient *mongo.Client
	coll       *mongo.Collection
	dbName     string
	identity   *age.X25519Identity
}

// setupTest spins up SeaweedFS + a single-node Mongo replica set, configures
// the production globals as main() would, and returns a struct with handles
// to the seeded test data. All container/lifecycle cleanup is registered via
// t.Cleanup.
func setupTest(ctx context.Context, t *testing.T) *testEnv {
	t.Helper()

	const (
		bucket    = "test-backups"
		accessKey = "testkey"
		secretKey = "testsecret"
		region    = "us-east-1"
		dbName    = "appdata"
	)

	s3Endpoint := startSeaweedFS(ctx, t)
	mongoURI := startMongoReplicaSet(ctx, t)

	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatalf("generate age key: %v", err)
	}

	if err := createBucket(s3Endpoint, accessKey, secretKey, region, bucket); err != nil {
		t.Fatalf("create bucket: %v", err)
	}

	BackupKeys = []string{identity.Recipient().String()}
	ConnectionURL = injectDBPlaceholder(mongoURI)
	Databases = []string{dbName}
	S3Endpoint = s3Endpoint
	S3Bucket = bucket
	S3AccessID = accessKey
	S3AccessKey = secretKey
	S3Region = region
	S3Folder = "backups"
	WebhookURL = ""
	t.Setenv("S3_INSECURE", "true")

	seedClient, err := mongo.Connect(options.Client().ApplyURI(mongoURI))
	if err != nil {
		t.Fatalf("connect mongo: %v", err)
	}
	t.Cleanup(func() { _ = seedClient.Disconnect(context.Background()) })

	return &testEnv{
		mongoURI:   mongoURI,
		seedClient: seedClient,
		coll:       seedClient.Database(dbName).Collection("widgets"),
		dbName:     dbName,
		identity:   identity,
	}
}

// TestFreezeIncrementalThaw exercises the full chain end-to-end: full →
// mutate → incremental → drop → thaw, asserting both the pre-full and the
// post-full-pre-incremental documents are present.
//
// Run with: go test -tags=integration ./...
//
// Requires on the host: docker, mongodump, mongorestore, age.
func TestFreezeIncrementalThaw(t *testing.T) {
	skipIfHostMissing(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	env := setupTest(ctx, t)

	if _, err := env.coll.InsertOne(ctx, bson.M{"name": "pre-full"}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if err := Freeze(ctx, "full"); err != nil {
		t.Fatalf("Freeze full: %v", err)
	}

	if _, err := env.coll.InsertOne(ctx, bson.M{"name": "post-full-pre-incr"}); err != nil {
		t.Fatalf("mutate: %v", err)
	}

	if err := Freeze(ctx, "incremental"); err != nil {
		t.Fatalf("Freeze incremental: %v", err)
	}

	if err := env.seedClient.Database(env.dbName).Drop(ctx); err != nil {
		t.Fatalf("drop DB: %v", err)
	}

	t.Setenv("BACKUP_IDENTITY", env.identity.String())
	if err := Thaw(ctx); err != nil {
		t.Fatalf("Thaw: %v", err)
	}

	n, err := env.coll.CountDocuments(ctx, bson.M{})
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 2 {
		t.Errorf("expected 2 docs after thaw, got %d", n)
	}

	if err := env.coll.FindOne(ctx, bson.M{"name": "post-full-pre-incr"}).Err(); err != nil {
		t.Errorf("expected post-full-pre-incr doc to be restored: %v", err)
	}
	if err := env.coll.FindOne(ctx, bson.M{"name": "pre-full"}).Err(); err != nil {
		t.Errorf("expected pre-full doc to be restored: %v", err)
	}
}

// TestIncrementalDetectsBrokenChain simulates a rolled-over oplog by tampering
// with the full's manifest to set oplog_start_ts to a long-past timestamp.
// findChainStart() will return that fabricated value, while the live oplog
// tail is many epochs newer — so gap detection should trip and the
// incremental should fail loudly with a BROKEN_CHAIN error.
//
// We don't need to actually wait for an oplog to roll; the comparison logic
// is independent of how the gap was created.
func TestIncrementalDetectsBrokenChain(t *testing.T) {
	skipIfHostMissing(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	env := setupTest(ctx, t)

	if _, err := env.coll.InsertOne(ctx, bson.M{"name": "before-tamper"}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if err := Freeze(ctx, "full"); err != nil {
		t.Fatalf("Freeze full: %v", err)
	}

	manifestKey := findManifestKey(t, env.dbName)
	body, err := getS3Object(manifestKey)
	if err != nil {
		t.Fatalf("get manifest: %v", err)
	}
	var m Manifest
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("decode manifest: %v", err)
	}
	m.OplogStartTs = ManifestTs{T: 1, I: 1} // 1970, well before any live oplog entry
	tampered, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("encode manifest: %v", err)
	}
	if err := putS3Object(manifestKey, tampered, "application/json"); err != nil {
		t.Fatalf("upload tampered manifest: %v", err)
	}

	err = Freeze(ctx, "incremental")
	if err == nil {
		t.Fatal("expected Freeze incremental to fail with BROKEN_CHAIN, got nil error")
	}
	if !strings.Contains(err.Error(), "BROKEN_CHAIN") {
		t.Errorf("expected error to mention BROKEN_CHAIN, got: %v", err)
	}
}

func findManifestKey(t *testing.T, dbName string) string {
	t.Helper()
	keys, err := listS3Objects(fmt.Sprintf("%s/%s/full/", S3Folder, dbName))
	if err != nil {
		t.Fatalf("list manifests: %v", err)
	}
	for _, k := range keys {
		if strings.HasSuffix(k, ".manifest.json") {
			return k
		}
	}
	t.Fatalf("no manifest found under %s/%s/full/ — keys: %v", S3Folder, dbName, keys)
	return ""
}

func startSeaweedFS(ctx context.Context, t *testing.T) string {
	t.Helper()
	req := testcontainers.ContainerRequest{
		Image:        "chrislusf/seaweedfs:latest",
		Cmd:          []string{"server", "-s3"},
		ExposedPorts: []string{"8333/tcp"},
		WaitingFor: wait.ForAll(
			wait.ForListeningPort("8333/tcp"),
			wait.ForLog("Start Seaweed S3 API Server"),
		).WithStartupTimeoutDefault(90 * time.Second),
	}
	ctr, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	if err != nil {
		t.Fatalf("start seaweedfs: %v", err)
	}
	t.Cleanup(func() { _ = ctr.Terminate(context.Background()) })

	host, err := ctr.Host(ctx)
	if err != nil {
		t.Fatalf("seaweedfs host: %v", err)
	}
	port, err := ctr.MappedPort(ctx, "8333")
	if err != nil {
		t.Fatalf("seaweedfs port: %v", err)
	}
	return fmt.Sprintf("%s:%s", host, port.Port())
}

func startMongoReplicaSet(ctx context.Context, t *testing.T) string {
	t.Helper()
	ctr, err := tcmongo.Run(ctx, "mongo:6", tcmongo.WithReplicaSet("rs0"))
	if err != nil {
		t.Fatalf("start mongo: %v", err)
	}
	t.Cleanup(func() { _ = ctr.Terminate(context.Background()) })
	uri, err := ctr.ConnectionString(ctx)
	if err != nil {
		t.Fatalf("mongo URI: %v", err)
	}
	return uri
}

func createBucket(endpoint, accessKey, secretKey, region, bucket string) error {
	c, err := minio.NewWithRegion(endpoint, accessKey, secretKey, false, region)
	if err != nil {
		return err
	}
	deadline := time.Now().Add(30 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		if err := c.MakeBucket(bucket, region); err == nil {
			return nil
		} else {
			lastErr = err
			time.Sleep(500 * time.Millisecond)
		}
	}
	return fmt.Errorf("create bucket after retry: %w", lastErr)
}

// injectDBPlaceholder transforms a testcontainers Mongo URI like
// "mongodb://host:port/?replicaSet=rs" into the form deepfreeze expects:
// "mongodb://host:port/{DatabaseName}?replicaSet=rs".
func injectDBPlaceholder(uri string) string {
	if strings.Contains(uri, "/?") {
		return strings.Replace(uri, "/?", "/{DatabaseName}?", 1)
	}
	if strings.HasSuffix(uri, "/") {
		return uri + "{DatabaseName}"
	}
	return uri + "/{DatabaseName}"
}
