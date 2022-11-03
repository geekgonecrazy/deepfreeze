package main

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"net"
	"net/http"
	"strings"
	"time"

	rc_models "github.com/RocketChat/Rocket.Chat.Go.SDK/models"

	sh "github.com/codeskyblue/go-sh"
	minio "github.com/minio/minio-go"
	"gopkg.in/mgo.v2"
	"gopkg.in/mgo.v2/bson"
)

func getS3UploadURL(path string, expire time.Duration) (string, error) {
	minioClient, err := minio.NewWithRegion(S3Endpoint, S3AccessID, S3AccessKey, true, S3Region)
	if err != nil {
		return "", err
	}

	url, err := minioClient.PresignedPutObject(S3Bucket, path, expire)
	if err != nil {
		return "", err
	}

	return url.String(), nil
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

func getSession(connectionString string) (*mgo.Session, error) {

	ssl := false
	secondaryPreferred := false

	if strings.Contains(connectionString, "ssl=true") {
		connectionString = strings.Replace(connectionString, "&ssl=true", "", -1)
		connectionString = strings.Replace(connectionString, "?ssl=true&", "?", -1)
		ssl = true
	}

	if strings.Contains(connectionString, "readPreference=secondary") {
		connectionString = strings.Replace(connectionString, "&readPreference=secondary", "", -1)
		connectionString = strings.Replace(connectionString, "?readPreference=secondary", "", -1)
		secondaryPreferred = true
	}

	dialInfo, err := mgo.ParseURL(connectionString)
	if err != nil {
		return nil, err
	}

	if ssl {
		tlsConfig := &tls.Config{}
		dialInfo.DialServer = func(addr *mgo.ServerAddr) (net.Conn, error) {
			conn, err := tls.Dial("tcp", addr.String(), tlsConfig)
			return conn, err
		}
	}

	sess, err := mgo.DialWithInfo(dialInfo)
	if err != nil {
		return nil, err
	}

	if secondaryPreferred {
		sess.SetMode(mgo.SecondaryPreferred, true)
	}

	return sess.Copy(), nil
}

func checkReplicaSetOk(session *mgo.Session) error {
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

	if err := session.DB("admin").Run(bson.M{"replSetGetStatus": 1}, stat); err != nil {
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
