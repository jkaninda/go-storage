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

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestStorageInterface(t *testing.T) {
	var _ Storage = (*S3Storage)(nil)
	var _ Storage = (*GCSStorage)(nil)
	var _ Storage = (*AzureStorage)(nil)
	var _ Storage = (*SFTPStorage)(nil)
	var _ Storage = (*FTPStorage)(nil)
	var _ Storage = (*LocalStorage)(nil)
}

func TestCleanKey(t *testing.T) {
	ok := map[string]string{
		"x/y.txt":     "x/y.txt",
		"/a/b.txt":    "a/b.txt",
		"a//b/./c":    "a/b/c",
		`a\b\c.txt`:   "a/b/c.txt",
		"a/../b.txt":  "b.txt",
		"./x":         "x",
		"backup.sql~": "backup.sql~",
	}
	for in, want := range ok {
		got, err := cleanKey(in)
		if err != nil || got != want {
			t.Errorf("cleanKey(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"", "/", ".", "..", "../x", "a/../../x", `..\x`} {
		if got, err := cleanKey(in); err == nil {
			t.Errorf("cleanKey(%q) = %q, want error", in, got)
		}
	}
	if d, err := cleanDir(""); err != nil || d != "" {
		t.Errorf("cleanDir(\"\") = %q, %v", d, err)
	}
	if _, err := cleanPrefix("a/../../b"); err == nil {
		t.Error("cleanPrefix with .. accepted")
	}
}

func TestNewStorageLocalPath(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "base")
	s, err := NewStorage(StorageTypeLocal, Config{LocalPath: dir})
	if err != nil {
		t.Fatal(err)
	}
	if got := s.(*LocalStorage).BasePath(); got != dir {
		t.Fatalf("base path = %q, want %q", got, dir)
	}
}

func TestNewS3StorageOffline(t *testing.T) {
	s, err := NewS3Storage(Config{
		S3Bucket:     "test-bucket",
		S3Endpoint:   "127.0.0.1:9",
		S3DisableTLS: true,
		S3KeyID:      "k",
		S3Secret:     "s",
		S3PartSize:   1,
	})
	if err != nil {
		t.Fatalf("NewS3Storage: %v", err)
	}
	if s.region != DefaultS3Region {
		t.Errorf("region = %q", s.region)
	}
	if got := *s.client.Options().BaseEndpoint; got != "http://127.0.0.1:9" {
		t.Errorf("endpoint = %q", got)
	}
	//nolint:staticcheck // manager is deprecated, see s3.go
	if s.uploader.PartSize != MinS3PartSize || s.uploader.Concurrency != DefaultS3Concurrency {
		t.Errorf("uploader part size/concurrency = %d/%d", s.uploader.PartSize, s.uploader.Concurrency)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.Upload(ctx, filepath.Join(t.TempDir(), "missing.txt"), "remote.txt"); err == nil {
		t.Fatal("Upload of a missing local file succeeded")
	}
}

func TestLocalConformance(t *testing.T) {
	s, err := NewLocalStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	runConformance(t, s)
}

func TestLocalNoLeftovers(t *testing.T) {
	base := t.TempDir()
	s, err := NewLocalStorage(base)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	_, _ = s.Put(ctx, "x/y.bin", &failingReader{r: strings.NewReader(strings.Repeat("a", 100)), n: 10, err: errors.New("boom")}, nil)
	if _, err := s.PutIf(ctx, "x/idx.json", strings.NewReader("1"), Condition{IfNoneMatch: true}, nil); err != nil {
		t.Fatal(err)
	}
	_, _ = s.PutIf(ctx, "x/idx.json", strings.NewReader("2"), Condition{IfNoneMatch: true}, nil)
	entries, err := os.ReadDir(filepath.Join(base, "x"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "idx.json" {
			t.Errorf("leftover file %s", e.Name())
		}
	}
}

// TestLocalPutIfConcurrent checks that concurrent read-modify-write cycles
// with IfMatch never lose an update.
func TestLocalPutIfConcurrent(t *testing.T) {
	s, err := NewLocalStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := s.PutIf(ctx, "idx", strings.NewReader(""), Condition{IfNoneMatch: true}, nil); err != nil {
		t.Fatal(err)
	}
	const workers, perWorker = 8, 10
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perWorker; {
				body, it, err := s.Get(ctx, "idx")
				if err != nil {
					t.Error(err)
					return
				}
				b, _ := io.ReadAll(body)
				_ = body.Close()
				_, err = s.PutIf(ctx, "idx", strings.NewReader(string(b)+"x"), Condition{IfMatch: it.ETag}, nil)
				if errors.Is(err, ErrPreconditionFailed) {
					continue
				}
				if err != nil {
					t.Error(err)
					return
				}
				i++
			}
		}()
	}
	wg.Wait()
	body, _, err := s.Get(ctx, "idx")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = body.Close() }()
	b, _ := io.ReadAll(body)
	if len(b) != workers*perWorker {
		t.Fatalf("lost updates: got %d, want %d", len(b), workers*perWorker)
	}
}

func TestLocalPathTraversal(t *testing.T) {
	base := t.TempDir()
	s, err := NewLocalStorage(filepath.Join(base, "store"))
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"../outside.txt", "a/../../outside.txt", `..\outside.txt`} {
		if _, err := s.Put(context.Background(), k, strings.NewReader("x"), nil); err == nil {
			t.Errorf("Put(%q) succeeded", k)
		}
	}
	if _, err := os.Stat(filepath.Join(base, "outside.txt")); !os.IsNotExist(err) {
		t.Fatal("file written outside base")
	}
}
