# deepfreeze

**Note** Use at your own risk!  Also subject to change.

## What is it?

This tool makes an encrypted backup of your mongo database and then sends the encrypted backup off to an s3compatible bucket of your choosing.

It is meant to be used in a cronjob or something for reoccuring backups.  Because of this it actually evaluates the basic health of your cluster before allowing the backup to continue.  If you have a replicaset member down.  It won't continue the backup.   It also prefers secondaries for the backup.

Backups are done utilizing mongodump and age for the encrypted backups.

You can find out more information about age [here](https://age-encryption.org)

## How do I use it?

If you aren't using the docker image you will need to make sure mongodump and age are installed.

You will need an age key pair.  You can generate using `age-keygen`.  Store the private one some place safe and then use the public one.

Right now configuring is done 100% via environment variables.  This is likely to change in the future to use a config file.

There is a k8s file in examples for easy deployment as a cronjob.

Also can run the docker container on its own:

```
docker run --rm -e BACKUP_KEYS=b... geekgonecrazy/deepfreeze
```
Plug in all required environment variables from below

### Environment variables available:

| Environment Variable  | Description | Example Value | Required |
|---|---|---|---|
| BACKUP_KEYS | A comma seperated list of your age public keys. If you only have one no comma needed | age230sdfa32lkj2dfh02c82308h3082h3acashbzjklakjsdf02380as8hdfa  | true |
| CONNECTION_URL | This is the connection string to connect to mongo.  Needs to include {DatabaseName} if you plan to backup multiple databases. | mongodb://user:password@mongo-1,mongo-2,mongo-3/{DatabaseName}?replicaSet=rs01 | true |
| DATABASES | A comma seperated list of the databases you want to backup. | product1,product2 | true |
| S3_ENDPOINT | S3 Endpoint for your s3 compat provider | s3.us-west-000.backblazeb2.com | true |
| S3_BUCKET | S3 bucket at your s3 compat provider | my-encrypted-backups | true |
| S3_ACCESS_ID | Your access id | 000000000300000 | true |
| S3_ACCESS_KEY | Your access key | asdkajsf0382h082h38f0hf | true |
| S3_REGION | The region | us-west-000 | true |
| S3_FOLDER | The folder on the s3 bucket you want to put the backups in | backups | false |
| RC_WEBHOOK | A Rocket.Chat webhook address to send messages about completions or failures to | https://your-rc.com/hooks/{supersecret} | false |


## Example of a messages sent to webhook

```
6:34 PM - Starting Backup Job! Databases: product1

6:34 PM
Backup completed on: product1
Filename: product1-01-29-21-00.34.08.gz.age 
SHA256: e844305563c4ef720d4741c977f42284868b5d7331a96724127679f9e7f6c86a
File Size: 4.630000MB

6:34 PM - Backup Job Finished! Databases: product1
```

## How do I restore from a backup done using this tool?

Put your private key into a file so you can use it with the age tool.  Something like `keyFile`

Then decrypt and pass to mongo

```
cat {file}.gz.age | age -d -i keyFile > decrypted.gz
mongorestore --nsFrom="{db here}.*" --nsTo="{db here}.*" --gzip --archive=decrypted.gz

rm decrypted.gz
```

## What are your plans?

* Move to using a config file
* Allow excluding of collections from the backup
* Maybe assist in setting some expire headers

## FAQ

**Q: Why deepfreeze?**
Because i'm terrible at names. :)

**Q: Why golang and not just a bash script?**
Because for me its what i'm most comfortable with.  So even though it wraps some system commands in some extra logic.. i'm much more efficient when writting in go than bash.
