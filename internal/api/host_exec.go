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
	"github.com/aetherix/aetherixcloud/internal/hostexec"
	"github.com/aetherix/aetherixcloud/internal/runtime"
	"github.com/coder/websocket"
)

// HostExecWSPath is the browser WebSocket for an interactive host shell.
const HostExecWSPath = "/api/host-exec/ws"

type hostExecWSIn struct {
	Start *struct {
		MachineID string `json:"machineId"`
		Rows      int32  `json:"rows"`
		Cols      int32  `json:"cols"`
	} `json:"start"`
	Stdin  string `json:"stdin"`
	Resize *struct {
		Rows int32 `json:"rows"`
		Cols int32 `json:"cols"`
	} `json:"resize"`
}

func hostExecWSHandler(s *server, authn *auth.Authenticator, public bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if public && authn != nil {
			tok := r.URL.Query().Get("token")
			if tok == "" {
				tok = auth.BearerFromHeader(r.Header.Get("Authorization"))
			}
			p, err := authn.Authenticate(r.Context(), tok)
			if err != nil || !p.Allows(auth.LevelAdmin) {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
		}
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: []string{"*"}})
		if err != nil {
			return
		}
		defer c.CloseNow()
		ctx := r.Context()
		svc := &machineSvc{s}
		if err := svc.serveHostExecWS(ctx, c); err != nil {
			_ = writeWSJSON(ctx, c, execWSOut{Error: err.Error()})
		}
		_ = c.Close(websocket.StatusNormalClosure, "")
	})
}

func (s *machineSvc) serveHostExecWS(ctx context.Context, c *websocket.Conn) error {
	_, data, err := c.Read(ctx)
	if err != nil {
		return err
	}
	var first hostExecWSIn
	if err := json.Unmarshal(data, &first); err != nil || first.Start == nil {
		return errors.New("first message must be start")
	}
	st := first.Start
	m, err := s.find(ctx, st.MachineID)
	if err != nil {
		return unwrapConnect(err)
	}
	start := &v1.HostExecStart{MachineId: m.Id, Rows: st.Rows, Cols: st.Cols}
	if s.State != nil && m.Id != s.State.MachineID {
		return s.proxyHostExecWS(ctx, c, m, start)
	}
	sess, err := hostexec.Start(ctx, int(start.Rows), int(start.Cols))
	if err != nil {
		return err
	}
	defer sess.Close()
	return bridgeLocalExec(ctx, c, sess)
}

func (s *machineSvc) proxyHostExecWS(ctx context.Context, c *websocket.Conn, m *v1.Machine, start *v1.HostExecStart) error {
	cli := aetherixv1connect.NewMachineServiceClient(s.HTTPClient, s.PeerURL(m.MeshIp), connect.WithGRPC())
	out := cli.HostExec(ctx)
	out.RequestHeader().Set(proxiedHeader, "1")
	if err := out.Send(&v1.HostExecRequest{Msg: &v1.HostExecRequest_Start{Start: start}}); err != nil {
		return err
	}
	go func() {
		for {
			_, data, err := c.Read(ctx)
			if err != nil {
				_ = out.CloseRequest()
				return
			}
			var msg hostExecWSIn
			if json.Unmarshal(data, &msg) != nil {
				continue
			}
			if msg.Stdin != "" {
				raw, err := base64.StdEncoding.DecodeString(msg.Stdin)
				if err != nil {
					raw = []byte(msg.Stdin)
				}
				if out.Send(&v1.HostExecRequest{Msg: &v1.HostExecRequest_Stdin{Stdin: raw}}) != nil {
					return
				}
			}
			if msg.Resize != nil {
				if out.Send(&v1.HostExecRequest{Msg: &v1.HostExecRequest_Resize{Resize: &v1.HostExecResize{Rows: msg.Resize.Rows, Cols: msg.Resize.Cols}}}) != nil {
					return
				}
			}
		}
	}()
	for {
		msg, err := out.Receive()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		switch x := msg.Msg.(type) {
		case *v1.HostExecResponse_Stdout:
			if err := writeWSJSON(ctx, c, execWSOut{Stdout: base64.StdEncoding.EncodeToString(x.Stdout)}); err != nil {
				return err
			}
		case *v1.HostExecResponse_Stderr:
			if err := writeWSJSON(ctx, c, execWSOut{Stderr: base64.StdEncoding.EncodeToString(x.Stderr)}); err != nil {
				return err
			}
		case *v1.HostExecResponse_ExitCode:
			code := x.ExitCode
			return writeWSJSON(ctx, c, execWSOut{ExitCode: &code})
		}
	}
}

// HostExec is the Connect bidi used by peers (and optional CLI) for host shells.
func (s *machineSvc) HostExec(ctx context.Context, stream *connect.BidiStream[v1.HostExecRequest, v1.HostExecResponse]) error {
	first, err := stream.Receive()
	if err != nil {
		return err
	}
	start := first.GetStart()
	if start == nil {
		return errInvalid("first message must be start")
	}
	m, err := s.find(ctx, start.MachineId)
	if err != nil {
		return err
	}
	start.MachineId = m.Id
	if s.State != nil && m.Id != s.State.MachineID {
		return s.proxyHostExec(ctx, m, first, stream)
	}
	sess, err := hostexec.Start(ctx, int(start.Rows), int(start.Cols))
	if err != nil {
		return internalErr(err)
	}
	defer sess.Close()
	return bridgeHostExecRPC(ctx, stream, sess)
}

func (s *machineSvc) proxyHostExec(ctx context.Context, m *v1.Machine, first *v1.HostExecRequest, in *connect.BidiStream[v1.HostExecRequest, v1.HostExecResponse]) error {
	if in.RequestHeader().Get(proxiedHeader) != "" {
		return connect.NewError(connect.CodeFailedPrecondition, errors.New("host exec is not on this machine"))
	}
	c := aetherixv1connect.NewMachineServiceClient(s.HTTPClient, s.PeerURL(m.MeshIp), connect.WithGRPC())
	out := c.HostExec(ctx)
	out.RequestHeader().Set(proxiedHeader, "1")
	if err := out.Send(first); err != nil {
		return internalErr(err)
	}
	go func() {
		for {
			msg, err := in.Receive()
			if err != nil {
				_ = out.CloseRequest()
				return
			}
			if out.Send(msg) != nil {
				return
			}
		}
	}()
	for {
		msg, err := out.Receive()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		if err := in.Send(msg); err != nil {
			return err
		}
	}
}

func bridgeHostExecRPC(ctx context.Context, stream *connect.BidiStream[v1.HostExecRequest, v1.HostExecResponse], sess runtime.ExecSession) error {
	var sendMu sync.Mutex
	send := func(m *v1.HostExecResponse) error {
		sendMu.Lock()
		defer sendMu.Unlock()
		return stream.Send(m)
	}
	var wg sync.WaitGroup
	pump := func(r io.Reader, stderr bool) {
		defer wg.Done()
		buf := make([]byte, 32<<10)
		for {
			n, err := r.Read(buf)
			if n > 0 {
				b := append([]byte(nil), buf[:n]...)
				m := &v1.HostExecResponse{Msg: &v1.HostExecResponse_Stdout{Stdout: b}}
				if stderr {
					m = &v1.HostExecResponse{Msg: &v1.HostExecResponse_Stderr{Stderr: b}}
				}
				if send(m) != nil {
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
			m, err := stream.Receive()
			if err != nil {
				if w := sess.Stdin(); w != nil {
					w.Close()
				}
				return
			}
			switch x := m.Msg.(type) {
			case *v1.HostExecRequest_Stdin:
				if w := sess.Stdin(); w != nil {
					_, _ = w.Write(x.Stdin)
				}
			case *v1.HostExecRequest_Resize:
				_ = sess.Resize(ctx, int(x.Resize.Rows), int(x.Resize.Cols))
			}
		}
	}()
	wg.Wait()
	code, err := sess.Wait(ctx)
	if err != nil {
		return internalErr(err)
	}
	return send(&v1.HostExecResponse{Msg: &v1.HostExecResponse_ExitCode{ExitCode: int32(code)}})
}
