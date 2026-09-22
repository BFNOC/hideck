package qdc507

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
)

type HTTPClient interface {
	Do(*http.Request) (*http.Response, error)
}
type Cache struct {
	directory string
	client    HTTPClient
}

func NewCache(directory string, client HTTPClient) (*Cache, error) {
	if !filepath.IsAbs(directory) || client == nil {
		return nil, fmt.Errorf("qdc507: absolute cache path and HTTP client are required")
	}
	return &Cache{directory: filepath.Join(directory, RuntimeVersion), client: client}, nil
}

func (c *Cache) Directory() string { return c.directory }

// Download is an explicit action, not a side effect of readiness checks. Valid
// files are reused offline; corrupt existing files are reported, not overwritten.
func (c *Cache) Download(ctx context.Context) error {
	if ctx == nil {
		return fmt.Errorf("qdc507: download context is required")
	}
	if err := os.MkdirAll(c.directory, 0700); err != nil {
		return err
	}
	for _, artifact := range Artifacts() {
		if err := c.ensure(ctx, artifact); err != nil {
			return err
		}
	}
	_, err := ReadBundle(os.DirFS(c.directory))
	return err
}

func (c *Cache) ensure(ctx context.Context, artifact Artifact) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	_, err := readArtifact(os.DirFS(c.directory), artifact)
	if err == nil {
		return nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, SourceURL+artifact.Name, nil)
	if err != nil {
		return err
	}
	response, err := c.client.Do(request)
	if err != nil {
		return fmt.Errorf("qdc507: download %s: %w", artifact.Name, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("qdc507: download %s: HTTP %d", artifact.Name, response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, artifact.Size+1))
	if err != nil {
		return err
	}
	if err := verifyBytes(data, artifact); err != nil {
		return err
	}
	return c.store(artifact.Name, data)
}

func (c *Cache) store(name string, data []byte) error {
	f, err := os.CreateTemp(c.directory, "download-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	_, writeErr := f.Write(data)
	syncErr := f.Sync()
	closeErr := f.Close()
	if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
		return err
	}
	// Link installs atomically without replacing another importer/downloader's
	// file. A concurrent winner must still pass the pinned validation.
	if err := os.Link(f.Name(), filepath.Join(c.directory, name)); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return err
		}
		artifact, _ := pinnedArtifact(name)
		_, err = readArtifact(os.DirFS(c.directory), artifact)
		return err
	}
	return nil
}
