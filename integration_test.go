//go:build integration

package main

import (
	"context"
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

// TestFreezeIncrementalThaw exercises the full chain end-to-end:
//  1. spin up SeaweedFS (S3-compatible) + a single-node Mongo replica set
//  2. seed a doc and run a full backup
//  3. mutate (insert another doc) and run an incremental
//  4. drop the database and run thaw
//  5. assert both docs are present
//
// Run with: go test -tags=integration ./...
//
// Requires on the host: docker, mongodump, mongorestore, age.
func TestFreezeIncrementalThaw(t *testing.T) {
	skipIfHostMissing(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	s3Endpoint := startSeaweedFS(ctx, t)
	mongoURI := startMongoReplicaSet(ctx, t)

	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatalf("generate age key: %v", err)
	}
	pub := identity.Recipient().String()
	priv := identity.String()

	const (
		bucket    = "test-backups"
		accessKey = "testkey"
		secretKey = "testsecret"
		region    = "us-east-1"
		dbName    = "appdata"
	)

	if err := createBucket(s3Endpoint, accessKey, secretKey, region, bucket); err != nil {
		t.Fatalf("create bucket: %v", err)
	}

	// Configure the production globals and env vars the same way main() would.
	BackupKeys = []string{pub}
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

	// Seed initial data.
	seedClient, err := mongo.Connect(options.Client().ApplyURI(mongoURI))
	if err != nil {
		t.Fatalf("connect mongo: %v", err)
	}
	t.Cleanup(func() { _ = seedClient.Disconnect(context.Background()) })

	coll := seedClient.Database(dbName).Collection("widgets")
	if _, err := coll.InsertOne(ctx, bson.M{"name": "pre-full"}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// 1. Full
	if err := Freeze(ctx, "full"); err != nil {
		t.Fatalf("Freeze full: %v", err)
	}

	// 2. Mutate so the incremental has a non-trivial payload to capture.
	if _, err := coll.InsertOne(ctx, bson.M{"name": "post-full-pre-incr"}); err != nil {
		t.Fatalf("mutate: %v", err)
	}

	// 3. Incremental
	if err := Freeze(ctx, "incremental"); err != nil {
		t.Fatalf("Freeze incremental: %v", err)
	}

	// 4. Wipe and thaw.
	if err := seedClient.Database(dbName).Drop(ctx); err != nil {
		t.Fatalf("drop DB: %v", err)
	}

	t.Setenv("BACKUP_IDENTITY", priv)
	if err := Thaw(ctx); err != nil {
		t.Fatalf("Thaw: %v", err)
	}

	// 5. Verify.
	n, err := coll.CountDocuments(ctx, bson.M{})
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 2 {
		t.Errorf("expected 2 docs after thaw, got %d", n)
	}

	if err := coll.FindOne(ctx, bson.M{"name": "post-full-pre-incr"}).Err(); err != nil {
		t.Errorf("expected post-full-pre-incr doc to be restored: %v", err)
	}
	if err := coll.FindOne(ctx, bson.M{"name": "pre-full"}).Err(); err != nil {
		t.Errorf("expected pre-full doc to be restored: %v", err)
	}
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
	// SeaweedFS is sometimes slow to be ready right after the port opens; retry briefly.
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
