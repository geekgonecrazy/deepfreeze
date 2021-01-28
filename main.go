package main

import (
	"log"
	"os"
	"strings"
)

var (
	BackupKeys    []string
	ConnectionURL string
	Databases     []string
	WebhookURL    string
	S3Endpoint    string
	S3Bucket      string
	S3AccessID    string
	S3AccessKey   string
	S3Region      string
	S3Folder      string = "backups"
)

func main() {
	backupKeys := os.Getenv("BACKUP_KEYS")

	if backupKeys == "" {
		panic("BACKUP_KEYS required. Security first peeps")
	}

	BackupKeys = strings.Split(backupKeys, ",")

	connectionURL := os.Getenv("CONNECTION_URL")

	if connectionURL == "" {
		panic("CONNECTION_URL required.  This is how we find the database")
	}

	ConnectionURL = connectionURL

	databases := os.Getenv("DATABASES")

	if databases == "" {
		panic("DATABASES is not defined.  Need a list of databases to backup")
	}

	Databases = strings.Split(databases, ",")

	s3endpoint := os.Getenv("S3_ENDPOINT")
	s3bucket := os.Getenv("S3_BUCKET")
	s3accessID := os.Getenv("S3_ACCESS_ID")
	s3accessKey := os.Getenv("S3_ACCESS_KEY")
	s3region := os.Getenv("S3_REGION")

	if s3bucket == "" || s3accessID == "" || s3accessKey == "" || s3region == "" || s3endpoint == "" {
		panic("S3 info must be provided to store completed backups")
	}

	S3Bucket = s3bucket
	S3AccessID = s3accessID
	S3AccessKey = s3accessKey
	S3Region = s3region
	S3Endpoint = s3endpoint

	s3folder := os.Getenv("S3_FOLDER")

	if s3folder != "" {
		S3Folder = s3folder
	}

	webhookurl := os.Getenv("RC_WEBHOOK")
	WebhookURL = webhookurl

	if err := Backup(); err != nil {
		log.Fatal("An error occured", err)
	}
}
