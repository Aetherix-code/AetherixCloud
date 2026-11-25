package ingress

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// AdminClient talks to Caddy's admin API.
type AdminClient struct {
	// Base is e.g. "http://127.0.0.1:2019".
	Base string
	HTTP *http.Client
}

// NewAdminClient returns a client for the admin address "host:port".
func NewAdminClient(addr string) *AdminClient {
	if addr == "" {
		addr = Options{}.admin()
	}
	if !strings.Contains(addr, "://") {
		addr = "http://" + addr
	}
	return &AdminClient{Base: addr, HTTP: &http.Client{Timeout: 30 * time.Second}}
}

// Load replaces the running configuration (graceful: in-flight requests on
// the old config finish).
func (a *AdminClient) Load(ctx context.Context, cfg any) error {
	body, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.Base+"/load", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("caddy /load: %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	return nil
}

// Ready reports whether the admin API answers.
func (a *AdminClient) Ready(ctx context.Context) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.Base+"/config/", nil)
	if err != nil {
		return false
	}
	resp, err := a.HTTP.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode/100 == 2
}
