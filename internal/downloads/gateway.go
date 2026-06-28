// Package downloads implements the download CQRS split (ADR-019/020).
//
//   - Gateway is the COMMAND side: a thin REST client over the in-cluster
//     download-gateway control plane. It issues add / cancel / list-clients and
//     NEVER touches the DB (the command side never reads or writes downloadjobs).
//   - Consumer is the READ side: it consumes the gateway's
//     stube.download.client.{started,progress,completed,failed} Kafka events and
//     projects them into the downloadjobs read model via the store.
//
// Source of truth: SAP CAP (Java) download/{DownloadGatewayClient,
// DownloadEventConsumer,DownloadKafkaConfig,DownloadGatewayProperties}.java.
package downloads

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/zaentrum/katalog-manager/internal/config"
)

// Gateway is the command-side REST client for the download-gateway.
// It satisfies graph.DownloadGateway (Add/Cancel/Clients).
type Gateway struct {
	cfg  config.Config
	http *http.Client
}

// NewGateway builds the command-side client. When cfg.DownloadGatewayURL is
// blank the gateway is disabled: Add/Cancel return an error and Clients returns
// the literal "[]" (mirroring DownloadGatewayClient).
func NewGateway(cfg config.Config) *Gateway {
	return &Gateway{
		cfg: cfg,
		// Connect timeout 5s in Java's HttpClient; per-call timeouts (15s/5s) are
		// applied via the request context below.
		http: &http.Client{Timeout: 20 * time.Second},
	}
}

func (g *Gateway) enabled() bool { return g.cfg.DownloadGatewayEnabled() }

// Add enqueues a download via POST {url}/api/v1/downloads. The gateway request
// body is snake_case; null title/wantedItemID are sent as "" (per Java). On
// success it returns the gateway's client_job_id (possibly "") and a
// "Queued on <adapter>" message. Disabled or non-2xx -> error.
func (g *Gateway) Add(ctx context.Context, adapter, source, title, wantedItemID string) (clientJobID, message string, err error) {
	if !g.enabled() {
		return "", "", fmt.Errorf("download-gateway integration is disabled (download-gateway.url unset)")
	}
	reqBody, err := json.Marshal(map[string]string{
		"adapter":        adapter,
		"source":         source,
		"title":          title,
		"wanted_item_id": wantedItemID,
	})
	if err != nil {
		return "", "", fmt.Errorf("gateway add error: %v", err)
	}
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodPost,
		g.cfg.DownloadGatewayURL+"/api/v1/downloads", bytes.NewReader(reqBody))
	if err != nil {
		return "", "", fmt.Errorf("gateway add error: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := g.http.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("gateway add error: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		return "", "", fmt.Errorf("gateway add failed: HTTP %d %s", resp.StatusCode, excerpt(body))
	}
	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", "", fmt.Errorf("gateway add error: %v", err)
	}
	clientJobID = asString(parsed["client_job_id"])
	return clientJobID, "Queued on " + adapter, nil
}

// Cancel removes a job by its (adapter, clientJobID) identity via
// DELETE {url}/api/v1/downloads/{adapter}/{clientJobId}. Path segments are
// URL-path encoded with "+" rewritten to "%20" (clientJobId is opaque:
// packageId / infohash). Disabled or non-2xx -> error.
func (g *Gateway) Cancel(ctx context.Context, adapter, clientJobID string) (message string, err error) {
	if !g.enabled() {
		return "", fmt.Errorf("download-gateway integration is disabled (download-gateway.url unset)")
	}
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	u := g.cfg.DownloadGatewayURL + "/api/v1/downloads/" + enc(adapter) + "/" + enc(clientJobID)
	req, err := http.NewRequestWithContext(cctx, http.MethodDelete, u, nil)
	if err != nil {
		return "", fmt.Errorf("gateway remove error: %v", err)
	}
	resp, err := g.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("gateway remove error: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("gateway remove failed: HTTP %d %s", resp.StatusCode, excerpt(body))
	}
	return "Cancelled", nil
}

// Clients returns the gateway's raw JSON body verbatim (e.g. `["odownloader"]`).
// Disabled or any error / non-2xx -> the literal "[]" (never errors), mirroring
// DownloadGatewayClient.clients().
func (g *Gateway) Clients(ctx context.Context) (string, error) {
	if !g.enabled() {
		return "[]", nil
	}
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodGet,
		g.cfg.DownloadGatewayURL+"/api/v1/clients", nil)
	if err != nil {
		return "[]", nil
	}
	req.Header.Set("Accept", "application/json")
	resp, err := g.http.Do(req)
	if err != nil {
		return "[]", nil
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		return "[]", nil
	}
	return string(body), nil
}

// enc encodes a path segment the same way Java does: URL-encode then turn the
// form-encoding "+" (space) into "%20" so it is a valid path segment.
func enc(s string) string {
	return strings.ReplaceAll(url.QueryEscape(s), "+", "%20")
}

func excerpt(body []byte) string {
	s := string(body)
	if len(s) > 200 {
		return s[:200] + "…"
	}
	return s
}

func asString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case nil:
		return ""
	default:
		return fmt.Sprintf("%v", t)
	}
}
