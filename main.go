package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"time"
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
	cmd, sub := parseSubcommand(os.Args[1:])

	switch cmd {
	case "freeze":
		loadCommonConfig()
		loadFreezeConfig()
		mode := sub
		if mode == "" {
			mode = "full"
		}
		ctx, cancel := context.WithTimeout(context.Background(), 70*time.Minute)
		defer cancel()
		if err := Freeze(ctx, mode); err != nil {
			log.Fatal("Freeze failed: ", err)
		}
	case "thaw":
		loadCommonConfig()
		loadFreezeConfig() // thaw still needs DATABASES + ConnectionURL fallback
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Hour)
		defer cancel()
		if err := Thaw(ctx); err != nil {
			log.Fatal("Thaw failed: ", err)
		}
	default:
		fmt.Fprintf(os.Stderr, "usage: deepfreeze [freeze [full|incremental] | thaw]\n")
		os.Exit(2)
	}
}

func parseSubcommand(args []string) (cmd, sub string) {
	if len(args) == 0 {
		return "freeze", "full"
	}
	cmd = args[0]
	if len(args) > 1 {
		sub = args[1]
	}
	return cmd, sub
}

func loadCommonConfig() {
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

	if s3folder := os.Getenv("S3_FOLDER"); s3folder != "" {
		S3Folder = s3folder
	}

	WebhookURL = os.Getenv("RC_WEBHOOK")
}

func loadFreezeConfig() {
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
}
