package ingress

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sort"
	"strconv"
	"sync"
	"time"

	v1 "github.com/aetherix/aetherixcloud/gen/aetherix/v1"
	"github.com/aetherix/aetherixcloud/internal/store"
)

// TCPProxy listens on published host ports for ServicePort mode "tcp" and
// forwards connections to healthy replicas over the mesh. Only ingress nodes
// should run it (same machines that terminate public HTTP).
type TCPProxy struct {
	Store  store.Store
	Self   string
	Logger *slog.Logger

	mu        sync.Mutex
	listeners map[string]*tcpListener // key: "proto/port"
}

type tcpListener struct {
	key      string
	port     int
	ln       net.Listener
	cancel   context.CancelFunc
	backends func() []string
}

func (p *TCPProxy) log() *slog.Logger {
	if p.Logger != nil {
		return p.Logger
	}
	return slog.Default()
}

// Run reconciles TCP listeners until ctx is cancelled.
func (p *TCPProxy) Run(ctx context.Context) {
	if p.listeners == nil {
		p.listeners = map[string]*tcpListener{}
	}
	t := time.NewTicker(3 * time.Second)
	defer t.Stop()
	p.reconcile(ctx)
	for {
		select {
		case <-ctx.Done():
			p.closeAll()
			return
		case <-t.C:
			p.reconcile(ctx)
		}
	}
}

func (p *TCPProxy) closeAll() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for k, l := range p.listeners {
		l.cancel()
		_ = l.ln.Close()
		delete(p.listeners, k)
	}
}

func (p *TCPProxy) reconcile(ctx context.Context) {
	desired, err := p.desired(ctx)
	if err != nil {
		p.log().Warn("tcp proxy reconcile", "err", err)
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for k, l := range p.listeners {
		if _, ok := desired[k]; !ok {
			l.cancel()
			_ = l.ln.Close()
			delete(p.listeners, k)
			p.log().Info("tcp proxy closed", "port", l.port)
		}
	}
	for k, d := range desired {
		if cur, ok := p.listeners[k]; ok {
			cur.backends = d.backends
			continue
		}
		ln, err := net.Listen("tcp", ":"+strconv.Itoa(d.port))
		if err != nil {
			p.log().Warn("tcp proxy listen", "port", d.port, "err", err)
			continue
		}
		lctx, cancel := context.WithCancel(ctx)
		l := &tcpListener{key: k, port: d.port, ln: ln, cancel: cancel, backends: d.backends}
		p.listeners[k] = l
		go p.serve(lctx, l)
		p.log().Info("tcp proxy listening", "port", d.port)
	}
}

type desiredTCP struct {
	port     int
	backends func() []string
}

func (p *TCPProxy) desired(ctx context.Context) (map[string]desiredTCP, error) {
	services, err := store.ListServices(ctx, p.Store, nil)
	if err != nil {
		return nil, err
	}
	out := map[string]desiredTCP{}
	for _, svc := range services {
		for _, port := range tcpPorts(svc.Spec) {
			// PublishedPort is assigned once by the API (random cluster-wide
			// port). Skip until allocated — do not fall back to container port.
			pub := int(port.PublishedPort)
			if pub <= 0 || pub > 65535 {
				continue
			}
			key := fmt.Sprintf("tcp/%d", pub)
			svcID := svc.Id
			cport := port.ContainerPort
			st := p.Store
			out[key] = desiredTCP{
				port: pub,
				backends: func() []string {
					reps, err := store.ListReplicas(context.Background(), st, nil)
					if err != nil {
						return nil
					}
					ms, err := store.ListMachines(context.Background(), st)
					if err != nil {
						return nil
					}
					m := map[string]*v1.Machine{}
					for _, x := range ms {
						m[x.Id] = x
					}
					return tcpUpstreams(svcID, cport, reps, m)
				},
			}
		}
	}
	return out, nil
}

func tcpPorts(spec *v1.ServiceSpec) []*v1.ServicePort {
	var out []*v1.ServicePort
	for _, p := range spec.GetPorts() {
		if p.GetMode() == "tcp" {
			out = append(out, p)
		}
	}
	return out
}

func tcpUpstreams(serviceID string, containerPort int32, replicas []*v1.Replica, machines map[string]*v1.Machine) []string {
	var dials []string
	for _, r := range replicas {
		if r.ServiceId != serviceID || !r.DesiredRunning || r.State != v1.ReplicaState_REPLICA_STATE_RUNNING || r.ContainerIp == "" {
			continue
		}
		if m := machines[r.MachineId]; m != nil && m.State == v1.MachineState_MACHINE_STATE_DOWN {
			continue
		}
		dials = append(dials, net.JoinHostPort(r.ContainerIp, strconv.Itoa(int(containerPort))))
	}
	sort.Strings(dials)
	return dials
}

func (p *TCPProxy) serve(ctx context.Context, l *tcpListener) {
	for {
		conn, err := l.ln.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				return
			default:
				p.log().Debug("tcp accept", "port", l.port, "err", err)
				return
			}
		}
		go p.handle(ctx, l, conn)
	}
}

func (p *TCPProxy) handle(ctx context.Context, l *tcpListener, client net.Conn) {
	defer client.Close()
	backends := l.backends()
	if len(backends) == 0 {
		return
	}
	// Round-robin by hashing remote addr into the list.
	idx := 0
	if ra, ok := client.RemoteAddr().(*net.TCPAddr); ok {
		idx = int(ra.IP[len(ra.IP)-1]) % len(backends)
	}
	var upstream net.Conn
	var err error
	for i := 0; i < len(backends); i++ {
		d := backends[(idx+i)%len(backends)]
		dctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		upstream, err = (&net.Dialer{}).DialContext(dctx, "tcp", d)
		cancel()
		if err == nil {
			break
		}
	}
	if upstream == nil {
		return
	}
	defer upstream.Close()
	errc := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(upstream, client); errc <- struct{}{} }()
	go func() { _, _ = io.Copy(client, upstream); errc <- struct{}{} }()
	select {
	case <-errc:
	case <-ctx.Done():
	}
}
