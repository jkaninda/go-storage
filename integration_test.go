//go:build integration

/*
MIT License

Copyright (c) 2024 Jonas Kaninda

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.
*/

package go_storage

// Integration tests against real services started with the docker CLI.
//
//	go test -tags integration ./...
//
// Tests skip when docker is unavailable. Images can be overridden with
// GO_STORAGE_MINIO_IMAGE, GO_STORAGE_AZURITE_IMAGE, GO_STORAGE_FAKEGCS_IMAGE
// and GO_STORAGE_FTP_IMAGE.

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/storage"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"google.golang.org/api/option"
)

func image(env, def string) string {
	if v := os.Getenv(env); v != "" {
		return v
	}
	return def
}

func requireDocker(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := exec.CommandContext(ctx, "docker", "info").Run(); err != nil {
		t.Skipf("docker not available: %v", err)
	}
}

func docker(t *testing.T, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("docker %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// runContainer starts a detached container and removes it at cleanup.
func runContainer(t *testing.T, args ...string) string {
	t.Helper()
	id := docker(t, append([]string{"run", "-d"}, args...)...)
	t.Cleanup(func() {
		if t.Failed() {
			out, _ := exec.Command("docker", "logs", "--tail", "50", id).CombinedOutput()
			t.Logf("container logs:\n%s", out)
		}
		_ = exec.Command("docker", "rm", "-f", "-v", id).Run()
	})
	return id
}

// hostPort returns the host port mapped to the container port.
func hostPort(t *testing.T, id, containerPort string) string {
	t.Helper()
	out := docker(t, "port", id, containerPort)
	line := strings.Split(out, "\n")[0]
	_, port, err := net.SplitHostPort(strings.TrimSpace(line))
	if err != nil {
		t.Fatalf("parse docker port %q: %v", out, err)
	}
	return port
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port
}

const readyTimeout = 60 * time.Second

// eventually retries fn until it succeeds or readyTimeout expires.
func eventually(t *testing.T, what string, fn func() error) {
	t.Helper()
	deadline := time.Now().Add(readyTimeout)
	var err error
	for time.Now().Before(deadline) {
		if err = fn(); err == nil {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("%s: %v", what, err)
}

func TestIntegrationMinIO(t *testing.T) {
	requireDocker(t)
	id := runContainer(t, "-p", "127.0.0.1::9000",
		"-e", "MINIO_ROOT_USER=minioadmin", "-e", "MINIO_ROOT_PASSWORD=minioadmin",
		image("GO_STORAGE_MINIO_IMAGE", "pgsty/minio:latest"), "server", "/data")
	port := hostPort(t, id, "9000/tcp")

	s, err := NewS3Storage(Config{
		S3Bucket:      "conformance",
		S3Endpoint:    "127.0.0.1:" + port,
		S3DisableTLS:  true,
		S3ForcePath:   true,
		S3KeyID:       "minioadmin",
		S3Secret:      "minioadmin",
		S3PartSize:    MinS3PartSize, // 20 MiB stream => real multipart upload
		S3Concurrency: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	eventually(t, "create bucket", func() error {
		_, err := s.client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String("conformance")})
		return err
	})
	runConformance(t, s)

	// Failed multipart uploads must have been aborted.
	out, err := s.client.ListMultipartUploads(ctx, &s3.ListMultipartUploadsInput{Bucket: aws.String("conformance")})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Uploads) != 0 {
		t.Errorf("%d multipart uploads left behind", len(out.Uploads))
	}
}

func TestIntegrationAzurite(t *testing.T) {
	requireDocker(t)
	id := runContainer(t, "-p", "127.0.0.1::10000",
		image("GO_STORAGE_AZURITE_IMAGE", "mcr.microsoft.com/azure-storage/azurite:latest"),
		"azurite-blob", "--blobHost", "0.0.0.0", "--skipApiVersionCheck", "--loose")
	port := hostPort(t, id, "10000/tcp")

	s, err := NewAzureStorage(Config{
		AzureAccount:   "devstoreaccount1",
		AzureKey:       "Eby8vdM02xNOcqFlqUwJPLlmEtlCDXJ1OUzFT50uSRZ6IFsuFq2UVErCz4I6tq/K1SZFPTOtr/KBHBeksoGMGw==",
		AzureContainer: "conformance",
		AzureEndpoint:  "http://127.0.0.1:" + port + "/devstoreaccount1",
	})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "create container", func() error {
		_, err := s.client.CreateContainer(context.Background(), "conformance", nil)
		return err
	})
	runConformance(t, s)
}

func TestIntegrationFakeGCS(t *testing.T) {
	requireDocker(t)
	port := freePort(t)
	addr := "127.0.0.1:" + strconv.Itoa(port)
	runContainer(t, "-p", fmt.Sprintf("%s:4443", addr),
		image("GO_STORAGE_FAKEGCS_IMAGE", "fsouza/fake-gcs-server:latest"),
		"-scheme", "http", "-port", "4443", "-public-host", addr, "-external-url", "http://"+addr, "-backend", "memory")

	ctx := context.Background()
	s, err := newGCSStorage(ctx, "conformance",
		option.WithEndpoint("http://"+addr+"/storage/v1/"),
		option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	eventually(t, "create bucket", func() error {
		err := s.client.Bucket("conformance").Create(ctx, "test-project", &storage.BucketAttrs{})
		var exists bool
		if err != nil && strings.Contains(err.Error(), "409") {
			exists = true
		}
		if exists {
			return nil
		}
		return err
	})
	runConformance(t, s)
}

func TestIntegrationFTP(t *testing.T) {
	requireDocker(t)
	// Passive ports must be identical inside and outside the container.
	minPort := 30000 + rand.Intn(20000)
	maxPort := minPort + 9
	ctrlPort := freePort(t)
	runContainer(t,
		"-p", fmt.Sprintf("127.0.0.1:%d:21", ctrlPort),
		"-p", fmt.Sprintf("127.0.0.1:%d-%d:%d-%d", minPort, maxPort, minPort, maxPort),
		"-e", "USERS=backup|s3cret|/ftp/backup",
		"-e", "ADDRESS=127.0.0.1",
		"-e", "MIN_PORT="+strconv.Itoa(minPort), "-e", "MAX_PORT="+strconv.Itoa(maxPort),
		image("GO_STORAGE_FTP_IMAGE", "delfer/alpine-ftp-server:latest"))

	var s *FTPStorage
	eventually(t, "connect ftp", func() error {
		var err error
		s, err = NewFTPStorage(Config{
			FTPHost:     "127.0.0.1",
			FTPPort:     strconv.Itoa(ctrlPort),
			FTPUsername: "backup",
			FTPPassword: "s3cret",
		})
		if err != nil {
			return err
		}
		if _, err := s.ListDir(context.Background(), ""); err != nil {
			_ = s.Close()
			return err
		}
		return nil
	})
	t.Cleanup(func() { _ = s.Close() })
	runConformance(t, s)

	if _, err := s.PutIf(context.Background(), "x", strings.NewReader("x"), Condition{IfNoneMatch: true}, nil); !errors.Is(err, ErrConditionNotSupported) {
		t.Fatalf("FTP PutIf: %v, want ErrConditionNotSupported", err)
	}
}
