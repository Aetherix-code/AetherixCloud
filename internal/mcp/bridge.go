package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"

	"connectrpc.com/connect"
	"github.com/aetherix/aetherixcloud/gen/aetherix/v1/aetherixv1connect"
	"github.com/aetherix/aetherixcloud/internal/auth"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// MCP tools never re-implement behaviour: each call goes through the same
// Connect handlers as the dashboard, in process, carrying the caller's own
// bearer token. The API's auth interceptor therefore enforces scopes and RBAC
// exactly as for any other client.

// handlerTransport is an http.RoundTripper that serves requests with an
// http.Handler in-process, streaming the response.
type handlerTransport struct{ h func() http.Handler }

type pipeWriter struct {
	hdr    http.Header
	pw     *io.PipeWriter
	once   sync.Once
	status int
	snap   http.Header
	ready  chan struct{}
}

func (w *pipeWriter) Header() http.Header { return w.hdr }
func (w *pipeWriter) WriteHeader(code int) {
	w.once.Do(func() { w.status, w.snap = code, w.hdr.Clone(); close(w.ready) })
}
func (w *pipeWriter) Write(b []byte) (int, error) { w.WriteHeader(200); return w.pw.Write(b) }
func (w *pipeWriter) Flush()                      { w.WriteHeader(200) }

func (t handlerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	h := t.h()
	if h == nil {
		return nil, connect.NewError(connect.CodeUnavailable, errNotReady)
	}
	pr, pw := io.Pipe()
	w := &pipeWriter{hdr: http.Header{}, pw: pw, ready: make(chan struct{})}
	sreq := req.Clone(req.Context())
	sreq.RemoteAddr = "127.0.0.1:0"
	sreq.RequestURI = req.URL.RequestURI()
	go func() {
		defer func() {
			if r := recover(); r != nil {
				w.WriteHeader(500)
				pw.CloseWithError(io.ErrUnexpectedEOF)
				return
			}
		}()
		h.ServeHTTP(w, sreq)
		w.WriteHeader(200)
		_ = pw.Close()
	}()
	select {
	case <-w.ready:
	case <-req.Context().Done():
		_ = pr.CloseWithError(req.Context().Err())
		return nil, req.Context().Err()
	}
	return &http.Response{Status: http.StatusText(w.status), StatusCode: w.status, Header: w.snap, Body: pr,
		Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1, Request: req, ContentLength: -1}, nil
}

type headerInterceptor struct{ bearer, client string }

func (h headerInterceptor) set(hd http.Header) {
	hd.Set("Authorization", "Bearer "+h.bearer)
	hd.Set(auth.InternalHeader, auth.InternalSecret())
	hd.Set(auth.ClientHeader, h.client)
}

func (h headerInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		h.set(req.Header())
		return next(ctx, req)
	}
}
func (h headerInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return func(ctx context.Context, spec connect.Spec) connect.StreamingClientConn {
		c := next(ctx, spec)
		h.set(c.RequestHeader())
		return c
	}
}
func (h headerInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return next
}

// apiClients are the service clients used by tools.
type apiClients struct {
	Cluster aetherixv1connect.ClusterServiceClient
	Machine aetherixv1connect.MachineServiceClient
	Project aetherixv1connect.ProjectServiceClient
	Service aetherixv1connect.ServiceServiceClient
	Deploy  aetherixv1connect.DeployServiceClient
	Log     aetherixv1connect.LogServiceClient
	Auth    aetherixv1connect.AuthServiceClient
	GitHub  aetherixv1connect.GitHubServiceClient
	http    *http.Client
	bearer  string
	client  string
}

const internalBase = "http://aetherix.internal"

func (s *Server) newAPI(bearer, client string) *apiClients {
	hc := &http.Client{Transport: handlerTransport{h: s.cfg.API}}
	o := connect.WithInterceptors(headerInterceptor{bearer, client})
	return &apiClients{
		Cluster: aetherixv1connect.NewClusterServiceClient(hc, internalBase, o),
		Machine: aetherixv1connect.NewMachineServiceClient(hc, internalBase, o),
		Project: aetherixv1connect.NewProjectServiceClient(hc, internalBase, o),
		Service: aetherixv1connect.NewServiceServiceClient(hc, internalBase, o),
		Deploy:  aetherixv1connect.NewDeployServiceClient(hc, internalBase, o),
		Log:     aetherixv1connect.NewLogServiceClient(hc, internalBase, o),
		Auth:    aetherixv1connect.NewAuthServiceClient(hc, internalBase, o),
		GitHub:  aetherixv1connect.NewGitHubServiceClient(hc, internalBase, o),
		http:    hc,
		bearer:  bearer,
		client:  client,
	}
}

// httpJSON calls a JSON HTTP API path on the same public handler (S3, backups, build settings).
func (c *call) httpJSON(ctx context.Context, method, path string, body any) (any, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, internalBase+path, rdr)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.api.bearer)
	req.Header.Set(auth.InternalHeader, auth.InternalSecret())
	req.Header.Set(auth.ClientHeader, c.api.client)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := c.api.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 4<<20))
	if res.StatusCode >= 400 {
		msg := strings.TrimSpace(string(raw))
		if msg == "" {
			msg = res.Status
		}
		return nil, fmt.Errorf("%s %s: %s", method, path, msg)
	}
	if len(raw) == 0 || res.StatusCode == http.StatusNoContent {
		return map[string]any{"ok": true}, nil
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		return map[string]any{"raw": string(raw)}, nil
	}
	return out, nil
}

// toAny converts a protobuf message to a plain JSON value (snake_case names).
func toAny(m proto.Message) any {
	b, err := protojson.MarshalOptions{UseProtoNames: true}.Marshal(m)
	if err != nil {
		return nil
	}
	var out any
	_ = json.Unmarshal(b, &out)
	return out
}

func listAny[T proto.Message](items []T) []any {
	out := make([]any, 0, len(items))
	for _, it := range items {
		out = append(out, toAny(it))
	}
	return out
}
