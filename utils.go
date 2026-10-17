package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	rc_models "github.com/RocketChat/Rocket.Chat.Go.SDK/models"

	sh "github.com/codeskyblue/go-sh"
	minio "github.com/minio/minio-go"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

func newMinioClient() (*minio.Client, error) {
	secure := os.Getenv("S3_INSECURE") != "true"
	return minio.NewWithRegion(S3Endpoint, S3AccessID, S3AccessKey, secure, S3Region)
}

func getS3UploadURL(path string, expire time.Duration) (string, error) {
	c, err := newMinioClient()
	if err != nil {
		return "", err
	}
	url, err := c.PresignedPutObject(S3Bucket, path, expire)
	if err != nil {
		return "", err
	}
	return url.String(), nil
}

func getS3DownloadURL(path string, expire time.Duration) (string, error) {
	c, err := newMinioClient()
	if err != nil {
		return "", err
	}
	url, err := c.PresignedGetObject(S3Bucket, path, expire, nil)
	if err != nil {
		return "", err
	}
	return url.String(), nil
}

func listS3Objects(prefix string) ([]string, error) {
	c, err := newMinioClient()
	if err != nil {
		return nil, err
	}
	doneCh := make(chan struct{})
	defer close(doneCh)
	var keys []string
	for object := range c.ListObjects(S3Bucket, prefix, true, doneCh) {
		if object.Err != nil {
			return nil, object.Err
		}
		keys = append(keys, object.Key)
	}
	return keys, nil
}

func putS3Object(path string, body []byte, contentType string) error {
	c, err := newMinioClient()
	if err != nil {
		return err
	}
	_, err = c.PutObject(S3Bucket, path, bytes.NewReader(body), int64(len(body)), minio.PutObjectOptions{ContentType: contentType})
	return err
}

func getS3Object(path string) ([]byte, error) {
	c, err := newMinioClient()
	if err != nil {
		return nil, err
	}
	obj, err := c.GetObject(S3Bucket, path, minio.GetObjectOptions{})
	if err != nil {
		return nil, err
	}
	defer obj.Close()
	return io.ReadAll(obj)
}

func bytesToMegaBytes(b int64) float64 {
	bytes := float64(b)
	kiloBytes := bytes / 1024
	megaBytes := kiloBytes / 1024

	output := math.Round(megaBytes*100) / 100

	return output
}

func runCommand(command string, timeout int, bash bool) (string, error) {
	log.Println("COMMAND", command)

	shell := "/bin/sh"

	if bash {
		shell = "/bin/bash"
	}

	output, err := sh.Command(shell, "-c", command).SetTimeout(time.Duration(timeout) * time.Minute).CombinedOutput()
	if err != nil {
		errLog := ""
		if len(output) > 0 {
			errLog = strings.Replace(string(output), "\n", " ", -1)
		}

		return "", errors.New(errLog)
	}

	return string(output), nil
}

func triggerWebhook(url string, payload interface{}) {

	jsonText, err := json.Marshal(payload)
	if err != nil {
		fmt.Println(err)
	}

	req, err := http.NewRequest("POST", url, bytes.NewReader(jsonText))
	if err != nil {
		fmt.Println(err)
	}

	req.Header.Add("Content-Type", "application/json")

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		fmt.Println(err)
		return
	}

	defer resp.Body.Close()
}

func sendRCWebhookTextMessage(text string) error {
	log.Println(text)

	if WebhookURL != "" {
		var message = &rc_models.PostMessage{}

		message.Text = text

		triggerWebhook(WebhookURL, message)
	}

	return nil
}

func getMongoClient(_ context.Context, uri string) (*mongo.Client, error) {
	return mongo.Connect(options.Client().ApplyURI(uri))
}

func getOplogHeadTs(ctx context.Context, client *mongo.Client) (bson.Timestamp, error) {
	return queryOplogTs(ctx, client, -1)
}

func getOplogTailTs(ctx context.Context, client *mongo.Client) (bson.Timestamp, error) {
	return queryOplogTs(ctx, client, 1)
}

func queryOplogTs(ctx context.Context, client *mongo.Client, sort int32) (bson.Timestamp, error) {
	coll := client.Database("local").Collection("oplog.rs")
	opts := options.FindOne().SetSort(bson.D{{Key: "$natural", Value: sort}})
	var doc struct {
		Ts bson.Timestamp `bson:"ts"`
	}
	if err := coll.FindOne(ctx, bson.D{}, opts).Decode(&doc); err != nil {
		return bson.Timestamp{}, err
	}
	return doc.Ts, nil
}

func compareTs(a, b bson.Timestamp) int {
	if a.T != b.T {
		if a.T < b.T {
			return -1
		}
		return 1
	}
	if a.I < b.I {
		return -1
	}
	if a.I > b.I {
		return 1
	}
	return 0
}

func tsString(ts bson.Timestamp) string {
	return fmt.Sprintf("%d.%d", ts.T, ts.I)
}

func parseTs(s string) (bson.Timestamp, error) {
	parts := strings.Split(s, ".")
	if len(parts) != 2 {
		return bson.Timestamp{}, fmt.Errorf("malformed timestamp %q", s)
	}
	t, err := strconv.ParseUint(parts[0], 10, 32)
	if err != nil {
		return bson.Timestamp{}, fmt.Errorf("malformed timestamp seconds in %q: %w", s, err)
	}
	i, err := strconv.ParseUint(parts[1], 10, 32)
	if err != nil {
		return bson.Timestamp{}, fmt.Errorf("malformed timestamp ordinal in %q: %w", s, err)
	}
	return bson.Timestamp{T: uint32(t), I: uint32(i)}, nil
}

// parseIncrementalKey extracts the (from, to) timestamps from an oplog incremental S3 key
// of the form "<folder>/oplog/<fromT>.<fromI>_<toT>.<toI>.bson.gz.age".
func parseIncrementalKey(key string) (from, to bson.Timestamp, err error) {
	base := filepath.Base(key)
	base = strings.TrimSuffix(base, ".bson.gz.age")
	parts := strings.Split(base, "_")
	if len(parts) != 2 {
		return bson.Timestamp{}, bson.Timestamp{}, fmt.Errorf("malformed incremental key %q", key)
	}
	from, err = parseTs(parts[0])
	if err != nil {
		return bson.Timestamp{}, bson.Timestamp{}, err
	}
	to, err = parseTs(parts[1])
	return from, to, err
}

func checkReplicaSetOk(ctx context.Context, client *mongo.Client) error {
	log.Println("Ensuring mongo is healthy enough to perform operation")

	type member struct {
		ID       int `bson:"_id"`
		Name     string
		Health   int
		State    int
		StateStr string `bson:"stateStr"`
	}

	type replStatus struct {
		Set     string
		Ok      int
		Members []member
	}

	stat := &replStatus{}

	if err := client.Database("admin").RunCommand(ctx, bson.D{{Key: "replSetGetStatus", Value: 1}}).Decode(stat); err != nil {
		return err
	}

	log.Println(fmt.Sprintf("%+v", stat))

	if stat.Ok == 0 {
		return errors.New("Mongo Not OK")
	}

	healthy := true

	for _, member := range stat.Members {
		if member.Health == 0 {
			log.Println(fmt.Sprintf("Member %s is not healthy. State: %s\n", member.Name, member.StateStr))
			healthy = false
		}
	}

	if !healthy {
		return errors.New("A Mongo Member Not OK")
	}

	return nil
}
