package main

import (
	"fmt"
	"log"
	"os"
	"strings"
	"time"
)

func Backup() error {
	sendRCWebhookTextMessage(fmt.Sprintf("Starting Backup Job!  Databases: %s", strings.Join(Databases, ", ")))

	if err := doBackup(); err != nil {
		sendRCWebhookTextMessage(fmt.Sprintf("Backup Job Failed! Databases: %s Err:\n %s", strings.Join(Databases, ", "), err))
		return err
	}

	sendRCWebhookTextMessage(fmt.Sprintf("Backup Job Finished! Databases: %s", strings.Join(Databases, ", ")))

	return nil
}

func testReplicaSet() error {
	cs := strings.Replace(ConnectionURL, "{DatabaseName}", "test", -1)

	session, err := getSession(cs)
	if err != nil {
		return err
	}

	defer session.Close()

	if err := checkReplicaSetOk(session); err != nil {
		return err
	}

	return nil
}

func doBackup() error {

	log.Println("Testing Replicaset health before trying to do backup")
	if err := testReplicaSet(); err != nil {
		return err
	}

	for _, database := range Databases {
		filename, sha256, fileSize, err := backupDatabase(ConnectionURL, database)
		if err != nil {
			return err
		}

		fileSizeMegaBytes := bytesToMegaBytes(fileSize)

		sendRCWebhookTextMessage(fmt.Sprintf("Backup completed on: %s\nFilename: %s\nSHA256: %s\nFile Size: %fMB ", database, filename, sha256, fileSizeMegaBytes))
	}

	return nil
}

func backupDatabase(connectionString string, database string) (string, string, int64, error) {
	log.Println("Backing up Database: ", database)

	backupTime := time.Now()

	fileName := fmt.Sprintf("%s-%s.gz.age", database, backupTime.Format("01-02-06-15.04.05"))

	log.Println("Getting presigned upload url")

	uploadURL, err := getS3UploadURL(fmt.Sprintf("%s/%s", S3Folder, fileName), 2*time.Hour)
	if err != nil {
		return "", "", 0, err
	}

	backupDirectory := "backups"

	if _, err := os.Stat(backupDirectory); os.IsNotExist(err) {
		if err := os.MkdirAll(backupDirectory, 0777); err != nil {
			return "", "", 0, err
		}
	}

	filePath := fmt.Sprintf("%s/%s", backupDirectory, fileName)

	cs := strings.Replace(connectionString, "{DatabaseName}", database, -1)

	log.Println("Performing Database Backup")

	timeout := 60

	encryptionCommand := fmt.Sprintf("age -r %s", strings.Join(BackupKeys, " -r "))

	dumpCommand := fmt.Sprintf("mongodump --uri='%v' --archive --gzip | %s > %s", cs, encryptionCommand, filePath)

	dumpOutput, err := runCommand(dumpCommand, timeout, false)
	if err != nil {
		return "", "", 0, err
	}

	log.Println(dumpOutput)

	log.Println("Finished Database Backup")

	fileSHAOutput, err := runCommand(fmt.Sprintf("sha256sum %s", filePath), 30, false)
	if err != nil {
		return "", "", 0, err
	}

	log.Println(fileSHAOutput)

	fileSHA := strings.Split(fileSHAOutput, " ")[0]

	log.Println(fileSHA)

	fi, err := os.Stat(filePath)

	fileSize := fi.Size()

	log.Println(fmt.Sprintf("Uploading backup %s", filePath))

	if _, err := runCommand(fmt.Sprintf("curl --upload-file %s \"%s\"", filePath, uploadURL), 60, false); err != nil {
		return "", "", 0, err
	}

	log.Println("Cleaning up backup: ", filePath)

	if _, err := runCommand(fmt.Sprintf("rm %s", filePath), 60, false); err != nil {
		return "", "", 0, err
	}

	log.Println("Finished Backing up Database: ", database)

	return fileName, fileSHA, fileSize, nil
}
