package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sync"

	"connectrpc.com/connect"
	v1 "github.com/aetherix/aetherixcloud/gen/aetherix/v1"
	"github.com/aetherix/aetherixcloud/gen/aetherix/v1/aetherixv1connect"
	"github.com/aetherix/aetherixcloud/internal/auth"
	"github.com/aetherix/aetherixcloud/internal/runtime"
	"github.com/aetherix/aetherixcloud/internal/store"
	"github.com/coder/websocket"
)

// ExecWSPath is the browser-facing WebSocket for interactive exec. Browsers
// cannot upload a ReadableStream body over HTTP/1.1 (Vite proxy, Safari), so
// the SPA uses this instead of the Connect bidi Exec RPC.
const ExecWSPath = "/api/exec/ws"

type execWSIn struct {
	Start *struct {
		ServiceID string            `json:"serviceId"`
		ReplicaID string            `json:"replicaId"`
		Command   []string          `json:"command"`
		TTY       bool              `json:"tty"`
		Stdin     bool              `json:"stdin"`
		Rows      int32             `json:"rows"`
		Cols      int32             `json:"cols"`
		Env       map[string]string `json:"env"`
	} `json:"start"`
	Stdin  string `json:"stdin"` // base64
	Resize *struct {
		Rows int32 `json:"rows"`
		Cols int32 `json:"cols"`
	} `json:"resize"`
}

type execWSOut struct {
	Stdout   string `json:"stdout,omitempty"` // base64
	Stderr   string `json:"stderr,omitempty"` // base64
	ExitCode *int32 `json:"exitCode,omitempty"`
	Error    string `json:"error,omitempty"`
}

func execWSHandler(s *server, authn *auth.Authenticator, public bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if public && authn != nil {
			tok := r.URL.Query().Get("token")
			if tok == "" {
				tok = auth.BearerFromHeader(r.Header.Get("Authorization"))
			}
			p, err := authn.Authenticate(r.Context(), tok)
			if err != nil || !p.Allows(auth.LevelDeploy) {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
		}
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{
			OriginPatterns: []string{"*"},
		})
		if err != nil {
			return
		}
		defer c.CloseNow()
		ctx := r.Context()
		svc := &serviceSvc{s}
		if err := svc.serveExecWS(ctx, c); err != nil {
			_ = writeWSJSON(ctx, c, execWSOut{Error: err.Error()})
		}
		_ = c.Close(websocket.StatusNormalClosure, "")
	})
}

func (s *serviceSvc) serveExecWS(ctx context.Context, c *websocket.Conn) error {
	_, data, err := c.Read(ctx)
	if err != nil {
		return err
	}
	var first execWSIn
	if err := json.Unmarshal(data, &first); err != nil || first.Start == nil {
		return errors.New("first message must be start")
	}
	st := first.Start
	if len(st.Command) == 0 {
		st.Command = []string{"/bin/sh"}
	}
	start := &v1.ExecStart{
		ServiceId: st.ServiceID, ReplicaId: st.ReplicaID, Command: st.Command,
		Tty: st.TTY, Stdin: st.Stdin, Rows: st.Rows, Cols: st.Cols, Env: st.Env,
	}
	rep, err := s.pickReplica(ctx, start.ServiceId, start.ReplicaId)
	if err != nil {
		return unwrapConnect(err)
	}
	start.ReplicaId = rep.Id

	if s.State != nil && rep.MachineId != s.State.MachineID {
		return s.proxyExecWS(ctx, c, rep, start)
	}
	if s.Runtime == nil {
		return errors.New("no container runtime on this node")
	}
	var env []string
	for k, v := range start.Env {
		env = append(env, k+"="+v)
	}
	sess, err := s.Runtime.Exec(ctx, rep.ContainerId, runtime.ExecOptions{
		Command: start.Command, Env: env, TTY: start.Tty, Stdin: start.Stdin,
		Rows: int(start.Rows), Cols: int(start.Cols),
	})
	if err != nil {
		return err
	}
	defer sess.Close()
	return bridgeLocalExec(ctx, c, sess)
}

func bridgeLocalExec(ctx context.Context, c *websocket.Conn, sess runtime.ExecSession) error {
	var sendMu sync.Mutex
	send := func(o execWSOut) error {
		sendMu.Lock()
		defer sendMu.Unlock()
		return writeWSJSON(ctx, c, o)
	}
	var wg sync.WaitGroup
	pump := func(r io.Reader, stderr bool) {
		defer wg.Done()
		buf := make([]byte, 32<<10)
		for {
			n, err := r.Read(buf)
			if n > 0 {
				b64 := base64.StdEncoding.EncodeToString(buf[:n])
				o := execWSOut{Stdout: b64}
				if stderr {
					o = execWSOut{Stderr: b64}
				}
				if send(o) != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}
	wg.Add(1)
	go pump(sess.Stdout(), false)
	if e := sess.Stderr(); e != nil {
		wg.Add(1)
		go pump(e, true)
	}
	go func() {
		for {
			_, data, err := c.Read(ctx)
			if err != nil {
				if w := sess.Stdin(); w != nil {
					w.Close()
				}
				return
			}
			var msg execWSIn
			if json.Unmarshal(data, &msg) != nil {
				continue
			}
			if msg.Stdin != "" {
				raw, err := base64.StdEncoding.DecodeString(msg.Stdin)
				if err != nil {
					raw = []byte(msg.Stdin)
				}
				if w := sess.Stdin(); w != nil {
					_, _ = w.Write(raw)
				}
			}
			if msg.Resize != nil {
				_ = sess.Resize(ctx, int(msg.Resize.Rows), int(msg.Resize.Cols))
			}
		}
	}()
	wg.Wait()
	code, err := sess.Wait(ctx)
	if err != nil {
		return err
	}
	ec := int32(code)
	return send(execWSOut{ExitCode: &ec})
}

func (s *serviceSvc) proxyExecWS(ctx context.Context, c *websocket.Conn, rep *v1.Replica, start *v1.ExecStart) error {
	m, err := store.GetMachine(ctx, s.Store, rep.MachineId)
	if err != nil {
		return errors.New("machine not found")
	}
	cli := aetherixv1connect.NewServiceServiceClient(s.HTTPClient, s.PeerURL(m.MeshIp), connect.WithGRPC())
	out := cli.Exec(ctx)
	out.RequestHeader().Set(proxiedHeader, "1")
	if err := out.Send(&v1.ExecRequest{Msg: &v1.ExecRequest_Start{Start: start}}); err != nil {
		return err
	}
	go func() {
		for {
			_, data, err := c.Read(ctx)
			if err != nil {
				_ = out.CloseRequest()
				return
			}
			var msg execWSIn
			if json.Unmarshal(data, &msg) != nil {
				continue
			}
			if msg.Stdin != "" {
				raw, err := base64.StdEncoding.DecodeString(msg.Stdin)
				if err != nil {
					raw = []byte(msg.Stdin)
				}
				if out.Send(&v1.ExecRequest{Msg: &v1.ExecRequest_Stdin{Stdin: raw}}) != nil {
					return
				}
			}
			if msg.Resize != nil {
				if out.Send(&v1.ExecRequest{Msg: &v1.ExecRequest_Resize{Resize: &v1.ExecResize{Rows: msg.Resize.Rows, Cols: msg.Resize.Cols}}}) != nil {
					return
				}
			}
		}
	}()
	for {
		m, err := out.Receive()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		switch x := m.Msg.(type) {
		case *v1.ExecResponse_Stdout:
			if err := writeWSJSON(ctx, c, execWSOut{Stdout: base64.StdEncoding.EncodeToString(x.Stdout)}); err != nil {
				return err
			}
		case *v1.ExecResponse_Stderr:
			if err := writeWSJSON(ctx, c, execWSOut{Stderr: base64.StdEncoding.EncodeToString(x.Stderr)}); err != nil {
				return err
			}
		case *v1.ExecResponse_ExitCode:
			code := x.ExitCode
			return writeWSJSON(ctx, c, execWSOut{ExitCode: &code})
		}
	}
}

func writeWSJSON(ctx context.Context, c *websocket.Conn, o execWSOut) error {
	b, err := json.Marshal(o)
	if err != nil {
		return err
	}
	return c.Write(ctx, websocket.MessageText, b)
}

func unwrapConnect(err error) error {
	var ce *connect.Error
	if errors.As(err, &ce) {
		return errors.New(ce.Message())
	}
	return err
}
