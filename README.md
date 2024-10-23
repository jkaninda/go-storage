# Go Storage

- Local
- S3
- SSH
- FTP


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
utils.Fatal("Error creating s3 storage: %s", err)
}
err = s3Storage.Copy(finalFileName)
if err != nil {
utils.Fatal("Error copying file, error %v", err)
}
```