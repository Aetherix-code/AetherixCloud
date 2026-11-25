package backup

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strings"
	"time"
)

// S3Config is an S3-compatible endpoint (AWS, MinIO, R2, ...).
type S3Config struct {
	Endpoint        string `json:"endpoint"` // https://s3.amazonaws.com or http://minio:9000
	Region          string `json:"region"`
	Bucket          string `json:"bucket"`
	AccessKeyID     string `json:"access_key_id"`
	SecretAccessKey string `json:"secret_access_key"`
	Prefix          string `json:"prefix"` // optional key prefix, e.g. "aetherix/"
	// PathStyle forces path-style URLs (needed for most MinIO installs).
	PathStyle bool `json:"path_style"`
}

// Valid reports whether the config has the required fields.
func (c S3Config) Valid() bool {
	return c.Endpoint != "" && c.Bucket != "" && c.AccessKeyID != "" && c.SecretAccessKey != ""
}

func (c S3Config) region() string {
	if c.Region == "" {
		return "us-east-1"
	}
	return c.Region
}

func (c S3Config) key(parts ...string) string {
	p := strings.Trim(c.Prefix, "/")
	rest := path.Join(parts...)
	if p == "" {
		return rest
	}
	return p + "/" + rest
}

// Client talks to S3 with SigV4.
type Client struct {
	cfg    S3Config
	http   *http.Client
}

// NewClient returns an S3 client.
func NewClient(cfg S3Config) *Client {
	return &Client{cfg: cfg, http: &http.Client{Timeout: 10 * time.Minute}}
}

// PutObject uploads body to key.
func (c *Client) PutObject(ctx context.Context, key string, body io.Reader, size int64, contentType string) error {
	u, err := c.objectURL(key)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, u, body)
	if err != nil {
		return err
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	req.Header.Set("Content-Type", contentType)
	if size >= 0 {
		req.ContentLength = size
	}
	if err := c.sign(req, nil); err != nil {
		return err
	}
	res, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(res.Body, 2048))
		return fmt.Errorf("s3 put %s: %s: %s", key, res.Status, string(b))
	}
	return nil
}

// GetObject downloads key.
func (c *Client) GetObject(ctx context.Context, key string) (io.ReadCloser, error) {
	u, err := c.objectURL(key)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	if err := c.sign(req, nil); err != nil {
		return nil, err
	}
	res, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if res.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(res.Body, 2048))
		res.Body.Close()
		return nil, fmt.Errorf("s3 get %s: %s: %s", key, res.Status, string(b))
	}
	return res.Body, nil
}

// ListObjects returns keys under prefix (non-recursive delimiter "/").
func (c *Client) ListObjects(ctx context.Context, prefix string) ([]string, error) {
	base, err := c.bucketURL()
	if err != nil {
		return nil, err
	}
	q := url.Values{}
	q.Set("list-type", "2")
	q.Set("prefix", prefix)
	u := base + "?" + q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	if err := c.sign(req, nil); err != nil {
		return nil, err
	}
	res, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	b, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if res.StatusCode/100 != 2 {
		return nil, fmt.Errorf("s3 list: %s: %s", res.Status, string(b))
	}
	return parseListKeys(string(b)), nil
}

func parseListKeys(xml string) []string {
	var keys []string
	const tag = "<Key>"
	for {
		i := strings.Index(xml, tag)
		if i < 0 {
			break
		}
		xml = xml[i+len(tag):]
		j := strings.Index(xml, "</Key>")
		if j < 0 {
			break
		}
		keys = append(keys, xml[:j])
		xml = xml[j+6:]
	}
	return keys
}

func (c *Client) objectURL(key string) (string, error) {
	base, err := url.Parse(strings.TrimRight(c.cfg.Endpoint, "/"))
	if err != nil {
		return "", err
	}
	if c.cfg.PathStyle || base.Hostname() == "localhost" || strings.Contains(base.Hostname(), "minio") {
		base.Path = path.Join(base.Path, c.cfg.Bucket, key)
		return base.String(), nil
	}
	// virtual-hosted-style
	base.Host = c.cfg.Bucket + "." + base.Host
	base.Path = path.Join(base.Path, key)
	return base.String(), nil
}

func (c *Client) bucketURL() (string, error) {
	base, err := url.Parse(strings.TrimRight(c.cfg.Endpoint, "/"))
	if err != nil {
		return "", err
	}
	if c.cfg.PathStyle || base.Hostname() == "localhost" || strings.Contains(base.Hostname(), "minio") {
		base.Path = path.Join(base.Path, c.cfg.Bucket)
		return base.String(), nil
	}
	base.Host = c.cfg.Bucket + "." + base.Host
	return base.String(), nil
}

func (c *Client) sign(req *http.Request, payloadHash []byte) error {
	now := time.Now().UTC()
	req.Header.Set("X-Amz-Date", now.Format("20060102T150405Z"))
	req.Header.Set("Host", req.URL.Host)
	if payloadHash == nil {
		req.Header.Set("X-Amz-Content-Sha256", "UNSIGNED-PAYLOAD")
	} else {
		req.Header.Set("X-Amz-Content-Sha256", hex.EncodeToString(payloadHash))
	}
	payload := "UNSIGNED-PAYLOAD"
	if payloadHash != nil {
		payload = hex.EncodeToString(payloadHash)
	}
	signedHeaders, canonicalHeaders := canonicalHeaderString(req)
	canonicalRequest := strings.Join([]string{
		req.Method,
		req.URL.EscapedPath(),
		req.URL.RawQuery,
		canonicalHeaders,
		signedHeaders,
		payload,
	}, "\n")
	credScope := now.Format("20060102") + "/" + c.cfg.region() + "/s3/aws4_request"
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		now.Format("20060102T150405Z"),
		credScope,
		sha256Hex([]byte(canonicalRequest)),
	}, "\n")
	signingKey := deriveKey(c.cfg.SecretAccessKey, now.Format("20060102"), c.cfg.region(), "s3")
	sig := hex.EncodeToString(hmacSHA256(signingKey, []byte(stringToSign)))
	req.Header.Set("Authorization", fmt.Sprintf(
		"AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		c.cfg.AccessKeyID, credScope, signedHeaders, sig))
	return nil
}

func canonicalHeaderString(req *http.Request) (signed, canonical string) {
	keys := make([]string, 0, len(req.Header))
	lower := map[string]string{}
	for k, vs := range req.Header {
		lk := strings.ToLower(k)
		keys = append(keys, lk)
		lower[lk] = strings.TrimSpace(strings.Join(vs, ","))
	}
	if _, ok := lower["host"]; !ok {
		keys = append(keys, "host")
		lower["host"] = req.URL.Host
	}
	sort.Strings(keys)
	var b bytes.Buffer
	for _, k := range keys {
		fmt.Fprintf(&b, "%s:%s\n", k, lower[k])
	}
	return strings.Join(keys, ";"), b.String()
}

func deriveKey(secret, date, region, service string) []byte {
	kDate := hmacSHA256([]byte("AWS4"+secret), []byte(date))
	kRegion := hmacSHA256(kDate, []byte(region))
	kService := hmacSHA256(kRegion, []byte(service))
	return hmacSHA256(kService, []byte("aws4_request"))
}

func hmacSHA256(key, data []byte) []byte {
	m := hmac.New(sha256.New, key)
	m.Write(data)
	return m.Sum(nil)
}

func sha256Hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
