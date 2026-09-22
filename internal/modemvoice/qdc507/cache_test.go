package qdc507

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type httpFunc func(*http.Request) (*http.Response, error)

func (f httpFunc) Do(r *http.Request) (*http.Response, error) { return f(r) }

func TestCorruptCacheIsReportedWithoutDownloadOrOverwrite(t *testing.T) {
	cache, _ := NewCache(t.TempDir(), httpFunc(func(*http.Request) (*http.Response, error) { t.Fatal("unexpected download"); return nil, nil }))
	if err := os.MkdirAll(cache.Directory(), 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(cache.Directory(), Artifacts()[0].Name)
	if err := os.WriteFile(path, []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := cache.Download(context.Background()); err == nil {
		t.Fatal("accepted corrupt cache")
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "corrupt" {
		t.Fatalf("existing cache changed: %s %v", data, err)
	}
}

func TestFailedDownloadDoesNotInstallFile(t *testing.T) {
	cache, _ := NewCache(t.TempDir(), httpFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != SourceURL+Artifacts()[0].Name {
			t.Fatalf("unversioned URL %s", r.URL)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("not a driver"))}, nil
	}))
	if err := cache.Download(context.Background()); err == nil {
		t.Fatal("accepted unverified download")
	}
	files, err := os.ReadDir(cache.Directory())
	if err != nil || len(files) != 0 {
		t.Fatalf("files=%v err=%v", files, err)
	}
}
