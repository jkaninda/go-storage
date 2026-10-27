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
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

const conformanceStreamSize = 20 << 20

// deterministicStream writes n pseudo-random bytes into an io.Pipe and
// returns the reader plus the expected SHA-256.
func deterministicStream(n int64, seed int64) (io.Reader, [32]byte) {
	h := sha256.New()
	_, _ = io.Copy(h, io.LimitReader(rand.New(rand.NewSource(seed)), n))
	var sum [32]byte
	copy(sum[:], h.Sum(nil))

	pr, pw := io.Pipe()
	go func() {
		_, err := io.Copy(pw, io.LimitReader(rand.New(rand.NewSource(seed)), n))
		_ = pw.CloseWithError(err)
	}()
	return pr, sum
}

// failingReader returns ok bytes, then err.
type failingReader struct {
	r   io.Reader
	n   int64
	err error
}

func (f *failingReader) Read(p []byte) (int, error) {
	if f.n <= 0 {
		return 0, f.err
	}
	if int64(len(p)) > f.n {
		p = p[:f.n]
	}
	n, err := f.r.Read(p)
	f.n -= int64(n)
	return n, err
}

func keysOf(items []Item, filesOnly bool) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		if filesOnly && it.IsDirectory {
			continue
		}
		out = append(out, it.Key)
	}
	sort.Strings(out)
	return out
}

func putString(t *testing.T, s Storage, key, content string) Item {
	t.Helper()
	it, err := s.Put(context.Background(), key, strings.NewReader(content), &PutOptions{ContentType: "text/plain"})
	if err != nil {
		t.Fatalf("Put(%s): %v", key, err)
	}
	return it
}

func getString(t *testing.T, s Storage, key string) string {
	t.Helper()
	body, _, err := s.Get(context.Background(), key)
	if err != nil {
		t.Fatalf("Get(%s): %v", key, err)
	}
	defer func() { _ = body.Close() }()
	b, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("read %s: %v", key, err)
	}
	return string(b)
}

// runConformance exercises the full Storage contract against s.
func runConformance(t *testing.T, s Storage) {
	root := fmt.Sprintf("conf-%d", time.Now().UnixNano())
	k := func(p string) string { return root + "/" + p }
	for _, tc := range []struct {
		name string
		fn   func(*testing.T, Storage, func(string) string)
	}{
		{"StreamRoundTrip", confStreamRoundTrip},
		{"FailingReaderLeavesNothing", confFailingReaderLeavesNothing},
		{"ListDirBoundary", confListDirBoundary},
		{"ListRecursive", confListRecursive},
		{"Remove", confRemove},
		{"PathEscape", confPathEscape},
		{"PutIf", confPutIf},
		{"UploadDownload", confUploadDownload},
	} {
		t.Run(tc.name, func(t *testing.T) { tc.fn(t, s, k) })
	}
}

func confStreamRoundTrip(t *testing.T, s Storage, k func(string) string) {
	ctx := context.Background()
	r, want := deterministicStream(conformanceStreamSize, 42)
	it, err := s.Put(ctx, k("stream/big.bin"), r, &PutOptions{ContentType: "application/octet-stream", Size: -1})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if it.Size != conformanceStreamSize {
		t.Errorf("Put size = %d, want %d", it.Size, conformanceStreamSize)
	}
	st, err := s.Stat(ctx, k("stream/big.bin"))
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if st.Size != conformanceStreamSize {
		t.Errorf("Stat size = %d", st.Size)
	}
	if st.Key != k("stream/big.bin") {
		t.Errorf("Stat key = %q", st.Key)
	}
	if st.ETag != it.ETag {
		t.Errorf("Stat ETag %q != Put ETag %q", st.ETag, it.ETag)
	}
	body, gi, err := s.Get(ctx, k("stream/big.bin"))
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	h := sha256.New()
	_, err = io.Copy(h, body)
	_ = body.Close()
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if !bytes.Equal(h.Sum(nil), want[:]) {
		t.Fatal("content mismatch")
	}
	if gi.Size != conformanceStreamSize {
		t.Errorf("Get item size = %d", gi.Size)
	}
}

func confFailingReaderLeavesNothing(t *testing.T, s Storage, k func(string) string) {
	ctx := context.Background()
	key := k("fail/broken.bin")
	boom := errors.New("boom")
	r := &failingReader{r: rand.New(rand.NewSource(1)), n: 18 << 20, err: boom}
	if _, err := s.Put(ctx, key, r, nil); err == nil {
		t.Fatal("Put with failing reader succeeded")
	}
	if _, err := s.Stat(ctx, key); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Stat after failed Put: %v, want ErrNotFound", err)
	}
	items, err := s.ListDir(ctx, k("fail"))
	if err != nil {
		t.Fatalf("ListDir: %v", err)
	}
	for _, it := range items {
		if strings.HasSuffix(it.Key, ".partial") || strings.HasSuffix(it.Key, "broken.bin") {
			t.Errorf("leftover after failed Put: %s", it.Key)
		}
	}

	cctx, cancel := context.WithCancel(ctx)
	pr, pw := io.Pipe()
	go func() {
		_, _ = pw.Write(make([]byte, 1<<20))
		cancel()
		_, _ = pw.Write(make([]byte, 1<<20))
		_ = pw.Close()
	}()
	if _, err := s.Put(cctx, k("fail/cancelled.bin"), pr, nil); err == nil {
		t.Fatal("Put with cancelled context succeeded")
	}
	_ = pr.Close()
	if _, err := s.Stat(ctx, k("fail/cancelled.bin")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Stat after cancelled Put: %v, want ErrNotFound", err)
	}
}

func confListDirBoundary(t *testing.T, s Storage, k func(string) string) {
	ctx := context.Background()
	for _, p := range []string{"ld/a/b/1.txt", "ld/a/b/2.txt", "ld/a/b/sub/deep.txt", "ld/a/bc/x.txt", "ld/top.txt"} {
		putString(t, s, k(p), "x")
	}
	want := []string{k("ld/a/b/1.txt"), k("ld/a/b/2.txt")}
	for _, dir := range []string{k("ld/a/b"), k("ld/a/b/")} {
		items, err := s.ListDir(ctx, dir)
		if err != nil {
			t.Fatalf("ListDir(%s): %v", dir, err)
		}
		if got := keysOf(items, false); strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("ListDir(%s) = %v, want %v", dir, got, want)
		}
		for _, it := range items {
			if it.IsDirectory || it.Size != 1 {
				t.Errorf("ListDir item %+v", it)
			}
		}
	}
	items, err := s.ListDir(ctx, k("ld"))
	if err != nil {
		t.Fatal(err)
	}
	if got := keysOf(items, false); len(got) != 1 || got[0] != k("ld/top.txt") {
		t.Errorf("ListDir(ld) = %v", got)
	}
	items, err = s.ListDir(ctx, k("ld/missing"))
	if err != nil || len(items) != 0 {
		t.Errorf("ListDir(missing) = %v, %v", items, err)
	}
}

func confListRecursive(t *testing.T, s Storage, k func(string) string) {
	ctx := context.Background()
	items, err := s.List(ctx, k("ld/a/b/"))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{k("ld/a/b/1.txt"), k("ld/a/b/2.txt"), k("ld/a/b/sub/deep.txt")}
	if got := keysOf(items, true); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("List(ld/a/b/) = %v, want %v", got, want)
	}
	items, err = s.List(ctx, k("ld/"))
	if err != nil {
		t.Fatal(err)
	}
	if got := keysOf(items, true); len(got) != 5 {
		t.Errorf("List(ld/) = %v, want 5 files", got)
	}
}

func confRemove(t *testing.T, s Storage, k func(string) string) {
	ctx := context.Background()
	if err := s.Remove(ctx, k("does/not/exist.txt")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Remove missing: %v, want ErrNotFound", err)
	}
	putString(t, s, k("rm/x.txt"), "x")
	if err := s.Remove(ctx, k("rm/x.txt")); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := s.Stat(ctx, k("rm/x.txt")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Stat after Remove: %v", err)
	}
	if _, _, err := s.Get(ctx, k("rm/x.txt")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get after Remove: %v", err)
	}
}

func confPathEscape(t *testing.T, s Storage, k func(string) string) {
	ctx := context.Background()
	if _, err := s.Put(ctx, "../escape.txt", strings.NewReader("x"), nil); err == nil {
		t.Fatal("Put ../escape.txt succeeded")
	}
	if _, err := s.Stat(ctx, "a/../../escape.txt"); err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("Stat escaping key: %v", err)
	}
}

func confPutIf(t *testing.T, s Storage, k func(string) string) {
	ctx := context.Background()
	key := k("index/index.json")
	it1, err := s.PutIf(ctx, key, strings.NewReader(`{"v":1}`), Condition{IfNoneMatch: true}, &PutOptions{ContentType: "application/json"})
	if errors.Is(err, ErrConditionNotSupported) {
		t.Skip("backend does not support conditional writes")
	}
	if err != nil {
		t.Fatalf("PutIf IfNoneMatch (new): %v", err)
	}
	if _, err := s.PutIf(ctx, key, strings.NewReader(`{"v":2}`), Condition{IfNoneMatch: true}, nil); !errors.Is(err, ErrPreconditionFailed) {
		t.Fatalf("PutIf IfNoneMatch (exists): %v, want ErrPreconditionFailed", err)
	}
	if got := getString(t, s, key); got != `{"v":1}` {
		t.Fatalf("content after rejected PutIf = %q", got)
	}

	it2, err := s.PutIf(ctx, key, strings.NewReader(`{"v":2}`), Condition{IfMatch: it1.ETag}, nil)
	if errors.Is(err, ErrConditionNotSupported) {
		t.Log("IfMatch not supported; skipping IfMatch checks")
		return
	}
	if err != nil {
		t.Fatalf("PutIf IfMatch (fresh): %v", err)
	}
	if it2.ETag == "" || it2.ETag == it1.ETag {
		t.Fatalf("ETag did not change: %q -> %q", it1.ETag, it2.ETag)
	}
	if _, err := s.PutIf(ctx, key, strings.NewReader(`{"v":3}`), Condition{IfMatch: it1.ETag}, nil); !errors.Is(err, ErrPreconditionFailed) {
		t.Fatalf("PutIf IfMatch (stale): %v, want ErrPreconditionFailed", err)
	}
	if got := getString(t, s, key); got != `{"v":2}` {
		t.Fatalf("content = %q, want v2", got)
	}
	st, err := s.Stat(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if st.ETag != it2.ETag {
		t.Fatalf("Stat ETag %q != PutIf ETag %q", st.ETag, it2.ETag)
	}
	if _, err := s.PutIf(ctx, k("index/missing.json"), strings.NewReader("x"), Condition{IfMatch: it2.ETag}, nil); !errors.Is(err, ErrPreconditionFailed) {
		t.Fatalf("PutIf IfMatch on missing key: %v, want ErrPreconditionFailed", err)
	}
}

func confUploadDownload(t *testing.T, s Storage, k func(string) string) {
	ctx := context.Background()
	dir := t.TempDir()
	src := filepath.Join(dir, "src.txt")
	if err := os.WriteFile(src, []byte("hello world"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.Upload(ctx, src, k("ud/file.txt")); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	dst := filepath.Join(dir, "out", "dst.txt")
	if err := s.Download(ctx, k("ud/file.txt"), dst); err != nil {
		t.Fatalf("Download: %v", err)
	}
	b, err := os.ReadFile(dst)
	if err != nil || string(b) != "hello world" {
		t.Fatalf("downloaded %q, %v", b, err)
	}
	if err := s.Download(ctx, k("ud/missing.txt"), filepath.Join(dir, "m.txt")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Download missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "m.txt")); !os.IsNotExist(err) {
		t.Fatal("Download of missing key left a local file")
	}
}
