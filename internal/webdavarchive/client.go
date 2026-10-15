package webdavarchive

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"
)

const (
	defaultTimeout = 30 * time.Second
	maxErrorBody   = 4096
)

type ClientConfig struct {
	Endpoint string
	Username string
	Password string
	BasePath string
	Timeout  time.Duration
}

type Client struct {
	baseURL  *url.URL
	username string
	password string
	basePath string
	http     *http.Client
}

type requestOptions struct {
	Method      string
	RemotePath  string
	Body        io.Reader
	Headers     map[string]string
	Accepted    []int
	IncludeBase bool
}

func NewClient(config ClientConfig) (*Client, error) {
	endpoint, err := parseEndpoint(config.Endpoint)
	if err != nil {
		return nil, err
	}
	basePath, err := cleanRelativePath(config.BasePath)
	if err != nil {
		return nil, fmt.Errorf("WebDAV 基础目录无效: %w", err)
	}
	timeout := config.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	return &Client{
		baseURL: endpoint, username: strings.TrimSpace(config.Username), password: config.Password,
		basePath: basePath, http: &http.Client{Timeout: timeout},
	}, nil
}

func parseEndpoint(raw string) (*url.URL, error) {
	endpoint, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || endpoint == nil || endpoint.Host == "" {
		return nil, errors.New("WebDAV 地址无效")
	}
	if endpoint.Scheme != "http" && endpoint.Scheme != "https" {
		return nil, errors.New("WebDAV 地址只支持 http 或 https")
	}
	if endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" {
		return nil, errors.New("WebDAV 地址不能包含凭据、查询参数或片段")
	}
	return endpoint, nil
}

func (c *Client) Probe(ctx context.Context) error {
	var token [8]byte
	if _, err := rand.Read(token[:]); err != nil {
		return fmt.Errorf("生成 WebDAV 探测标识失败: %w", err)
	}
	name := ".hideck-probe-" + hex.EncodeToString(token[:])
	if err := c.Put(ctx, name, []byte("hideck webdav probe\n"), "text/plain; charset=utf-8"); err != nil {
		return err
	}
	if err := c.delete(ctx, name); err != nil {
		return fmt.Errorf("WebDAV 探测文件清理失败: %w", err)
	}
	return nil
}

func (c *Client) Put(ctx context.Context, remotePath string, data []byte, contentType string) error {
	cleaned, err := cleanRelativePath(remotePath)
	if err != nil || cleaned == "" {
		return errors.New("WebDAV 文件路径无效")
	}
	if err := c.ensureDir(ctx, path.Dir(cleaned)); err != nil {
		return err
	}
	headers := map[string]string{"Content-Type": contentType}
	return c.request(ctx, requestOptions{
		Method: http.MethodPut, RemotePath: cleaned, Body: bytes.NewReader(data), Headers: headers,
		Accepted: []int{http.StatusOK, http.StatusCreated, http.StatusNoContent}, IncludeBase: true,
	})
}

func (c *Client) ensureDir(ctx context.Context, relativeDir string) error {
	if relativeDir == "." {
		relativeDir = ""
	}
	joined := strings.Trim(strings.Join([]string{c.basePath, relativeDir}, "/"), "/")
	if joined == "" {
		return nil
	}
	parts := strings.Split(joined, "/")
	for index := range parts {
		directory := strings.Join(parts[:index+1], "/")
		err := c.request(ctx, requestOptions{
			Method: "MKCOL", RemotePath: directory,
			Accepted: []int{http.StatusOK, http.StatusCreated, http.StatusNoContent, http.StatusMethodNotAllowed},
		})
		if err != nil {
			return fmt.Errorf("创建 WebDAV 目录 %q: %w", directory, err)
		}
	}
	return nil
}

func (c *Client) delete(ctx context.Context, remotePath string) error {
	return c.request(ctx, requestOptions{
		Method: http.MethodDelete, RemotePath: remotePath, IncludeBase: true,
		Accepted: []int{http.StatusOK, http.StatusNoContent, http.StatusNotFound},
	})
}

func (c *Client) request(ctx context.Context, options requestOptions) error {
	requestURL, err := c.resolve(options.RemotePath, options.IncludeBase)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, options.Method, requestURL.String(), options.Body)
	if err != nil {
		return fmt.Errorf("创建 WebDAV 请求失败: %w", err)
	}
	if c.username != "" || c.password != "" {
		request.SetBasicAuth(c.username, c.password)
	}
	for key, value := range options.Headers {
		request.Header.Set(key, value)
	}
	response, err := c.http.Do(request)
	if err != nil {
		return fmt.Errorf("WebDAV %s 请求失败: %w", options.Method, err)
	}
	defer response.Body.Close()
	for _, status := range options.Accepted {
		if response.StatusCode == status {
			_, _ = io.Copy(io.Discard, response.Body)
			return nil
		}
	}
	message, _ := io.ReadAll(io.LimitReader(response.Body, maxErrorBody))
	return fmt.Errorf("WebDAV %s 返回 %s: %s", options.Method, response.Status, strings.TrimSpace(string(message)))
}

func (c *Client) resolve(remotePath string, includeBase bool) (*url.URL, error) {
	cleaned, err := cleanRelativePath(remotePath)
	if err != nil {
		return nil, err
	}
	resolved := *c.baseURL
	if includeBase {
		resolved.Path = path.Join(c.baseURL.Path, c.basePath, cleaned)
	} else {
		resolved.Path = path.Join(c.baseURL.Path, cleaned)
	}
	return &resolved, nil
}

func cleanRelativePath(value string) (string, error) {
	value = strings.TrimSpace(strings.ReplaceAll(value, "\\", "/"))
	if value == "" || value == "." {
		return "", nil
	}
	if strings.HasPrefix(value, "/") {
		return "", errors.New("路径必须是相对路径")
	}
	for _, segment := range strings.Split(value, "/") {
		if segment == ".." || segment == "." || segment == "" {
			return "", errors.New("路径包含无效目录段")
		}
	}
	return path.Clean(value), nil
}
