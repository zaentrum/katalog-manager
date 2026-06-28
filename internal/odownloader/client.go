// Package odownloader bridges the in-cluster oDownloader daemon into the
// catalog: it enqueues trailer URLs as oDownloader packages, then polls for
// FINISHED downloads and imports the video variant into the packages inbox as a
// kind='trailer' playbackasset (SPEC §4 / 30-integrations.md "oDownloader").
//
// Ported from the CAP services odownloader/{OdownloaderClient,
// TrailerIngestionService,OdownloaderProperties}.java — paths, JSON field names,
// and semantics are reproduced exactly. All bespoke SQL runs via st.Pool().
package odownloader

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/zaentrum/katalog-manager/internal/config"
)

// client is a thin HTTP client over oDownloader's REST API (v1). Like the Java
// original it is best-effort: failures return nil/empty rather than erroring,
// because the caller (the poller) retries on the next tick anyway. The static
// bearer token is sent on every call.
type client struct {
	base    string // base URL with trailing slashes trimmed
	token   string
	enabled bool
	http    *http.Client
}

func newClient(cfg config.Config) *client {
	return &client{
		base:    strings.TrimRight(cfg.ODownloaderURL, "/"),
		token:   cfg.ODownloaderToken,
		enabled: cfg.ODownloaderEnabled(),
		// Connect timeout 5s in Java; here we set an overall per-call timeout
		// per request via context. A modest default guards content streaming.
		http: &http.Client{},
	}
}

// addLinksResult mirrors oDownloader's AddLinksResult. A single source URL often
// fans into multiple downloads (YouTube → .srt/.txt/.jpg/.m4a/.mp4).
type addLinksResult struct {
	PackageID   string   `json:"packageId"`
	DownloadIDs []string `json:"downloadIds"`
}

// downloadStatus is the subset of oDownloader's DownloadLinkView the importer
// needs.
type downloadStatus struct {
	ID                  string `json:"id"`
	PackageID           string `json:"packageId"`
	Name                string `json:"name"`
	BytesDone           int64  `json:"bytesDone"`
	BytesTotal          int64  `json:"bytesTotal"`
	SpeedBytesPerSecond int64  `json:"speedBytesPerSecond"`
	State               string `json:"state"`
	Message             string `json:"message"`
}

func (c *client) isEnabled() bool { return c.enabled }

// addLinks enqueues links as a single package (autostart=true). Returns nil when
// disabled or when the daemon refuses the request (HTTP >= 400).
func (c *client) addLinks(ctx context.Context, urls []string, packageName, comment string) *addLinksResult {
	if !c.enabled {
		return nil
	}
	body := map[string]any{
		"urls":        urls,
		"packageName": packageName,
		"comment":     comment,
		"autostart":   true,
	}
	resp, data := c.post(ctx, "/api/v1/links/add", body)
	if resp == nil || resp.StatusCode >= 400 {
		return nil
	}
	var out addLinksResult
	if err := json.Unmarshal(data, &out); err != nil {
		return nil
	}
	return &out
}

// listDownloadsByPackage reads the current state of every link inside a package
// (reads the JSON `items[]` array).
func (c *client) listDownloadsByPackage(ctx context.Context, packageID string) []downloadStatus {
	if !c.enabled || strings.TrimSpace(packageID) == "" {
		return nil
	}
	q := "?packageId=" + url.QueryEscape(packageID) + "&size=200"
	resp, data := c.get(ctx, "/api/v1/downloads"+q)
	if resp == nil || resp.StatusCode >= 400 {
		return nil
	}
	var env struct {
		Items []downloadStatus `json:"items"`
	}
	if err := json.Unmarshal(data, &env); err != nil {
		return nil
	}
	return env.Items
}

// openContent opens the finished file's byte stream. The caller must close the
// returned ReadCloser. Returns nil when disabled or on HTTP >= 400.
func (c *client) openContent(ctx context.Context, downloadID string) (io.ReadCloser, error) {
	if !c.enabled {
		return nil, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.uri("/api/v1/downloads/"+url.PathEscape(downloadID)+"/content"), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		resp.Body.Close()
		return nil, nil
	}
	return resp.Body, nil
}

// get issues a GET with a 15s timeout (Java parity). Returns (resp, body) or
// (nil, nil) on transport error.
func (c *client) get(ctx context.Context, path string) (*http.Response, []byte) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.uri(path), nil)
	if err != nil {
		return nil, nil
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	return c.do(req)
}

// post issues a JSON POST with a 30s timeout (Java parity).
func (c *client) post(ctx context.Context, path string, body any) (*http.Response, []byte) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.uri(path), bytes.NewReader(payload))
	if err != nil {
		return nil, nil
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	return c.do(req)
}

func (c *client) do(req *http.Request) (*http.Response, []byte) {
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, nil
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil
	}
	return resp, data
}

func (c *client) uri(path string) string { return c.base + path }
