# Go Storage
A simple Go storage client

Supported storage:
- Local
- S3
- SSH
- FTP
- Azure Blob

```go
go get github.com/jkaninda/go-storage
```

### Local Storage
```go
	localStorage := local.NewStorage(local.Config{
		LocalPath:  tmpPath,
		RemotePath: backupDestination,
	})
	err = localStorage.Copy(finalFileName)
	if err != nil {
		log.Fatal("Error copying file, error %v", err)
	}
```

### S3 Storage
```go
s3Storage, err := s3.NewStorage(s3.Config{
    Endpoint:       "",
    Bucket:         "",
    AccessKey:      "",
    SecretKey:      "",
    Region:         "",
    DisableSsl:     "",
    ForcePathStyle: "",
    RemotePath:     "",
    LocalPath:      "",
})
if err != nil {
    log.Fatalf("Error creating s3 storage: %s", err)
}
// Copy file to S3
err = s3Storage.Copy(finalFileName)
if err != nil {
    log.Fatalf("Error copying file, error %v", err)
}
// Download file from S3
err = s3Storage.CopyFrom(finalFileName)
if err != nil {
    log.Fatalf("Error copying file, error %v", err)
}
```

### SSH Storage
```go
sshStorage, err := ssh.NewStorage(ssh.Config{
		Host:       "",
		Port:       "",
		User:       "",
		Password:   "",
		RemotePath: "",
		LocalPath:  "",
	})
	if err != nil {
        log.Fatalf("Error creating SSH storage: %s", err)
	}
	// Copy file to the remote server
	err = sshStorage.Copy(finalFileName)
	if err != nil {
        log.Fatalf("Error copying backup file: %s", err)
	}
// Download file from SSH remote server
err = sshStorage.CopyFrom(finalFileName)
if err != nil {
log.Fatalf("Error copying file, error %v", err)
}
```
### FTP Storage
```go
	ftpStorage, err := ftp.NewStorage(ftp.Config{
		Host:       "",
		Port:       "",
		User:       "",
		Password:   "",
		RemotePath: "",
		LocalPath:  "",
	})
	if err != nil {
        log.Fatalf("Error creating FTP storage: %s", err)
	}
	err = ftpStorage.Copy(finalFileName)
	if err != nil {
        log.Fatalf("Error copying backup file: %s", err)
	}
// Download file from ftp remote server
err = ftpStorage.CopyFrom(finalFileName)
if err != nil {
log.Fatalf("Error copying file, error %v", err)
}
```

### Azure Blob storage

```go
azureStorage, err := azure.NewStorage(azure.Config{
		ContainerName: '',
		AccountName:   '',
		AccountKey:    '',
		RemotePath:    '',
		LocalPath:     '',
	})
	if err != nil {
		log.Fatal("Error creating Azure Blob storage storage: %s", err)
	}
	err = azureStorage.Copy(finalFileName)
	if err != nil {
		log.Fatal("Error copying file: %s", err)
	}

// Download file from Azure Blob remote server
err = azureStorage.CopyFrom(finalFileName)
if err != nil {
log.Fatalf("Error copying file, error %v", err)
}
```