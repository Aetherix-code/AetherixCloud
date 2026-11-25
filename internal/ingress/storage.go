package ingress

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aetherix/aetherixcloud/internal/lease"
	"github.com/aetherix/aetherixcloud/internal/secrets"
	"github.com/aetherix/aetherixcloud/internal/store"
)

// StoragePath is where StorageHandler is mounted.
const StoragePath = "/internal/ingress/storage/"

// ErrKeyNotFound is returned for unknown storage keys.
var ErrKeyNotFound = errors.New("ingress storage: key not found")

type storageRow struct {
	Key      string `json:"key"`
	Value    string `json:"value"` // sealed with the cluster key
	Modified int64  `json:"modified"`
	Size     int64  `json:"size"`
}

// KeyInfo mirrors certmagic.KeyInfo.
type KeyInfo struct {
	Key        string    `json:"key"`
	Modified   time.Time `json:"modified"`
	Size       int64     `json:"size"`
	IsTerminal bool      `json:"is_terminal"`
}

// Storage is the server side of the Caddy storage module: certmagic's
// Storage semantics on top of the replicated store. Values (private keys!)
// are sealed with the cluster key before they reach the store; ACME locks are
// leases, so only one node in the cluster works on an order at a time.
type Storage struct {
	Store   store.Store
	Key     secrets.Key
	Leases  *lease.Manager
	LockTTL time.Duration // default 5m (clients renew while held)

	lockMu sync.Mutex
	locks  map[string]*heldLock
}

type heldLock struct {
	owner string
	l     *lease.Lease
}

func (s *Storage) id(key string) string { return prefixCaddy + strings.Trim(key, "/") }

func (s *Storage) Put(ctx context.Context, key string, value []byte) error {
	sealed, err := s.Key.Seal(value)
	if err != nil {
		return err
	}
	return putSetting(ctx, s.Store, s.id(key), storageRow{Key: strings.Trim(key, "/"), Value: sealed,
		Modified: time.Now().UnixNano(), Size: int64(len(value))})
}

func (s *Storage) Get(ctx context.Context, key string) ([]byte, error) {
	row, err := store.GetJSON[storageRow](ctx, s.Store, store.TableSettings, s.id(key))
	if store.IsNotFound(err) {
		return nil, ErrKeyNotFound
	}
	if err != nil {
		return nil, err
	}
	return s.Key.Open(row.Value)
}

func (s *Storage) Exists(ctx context.Context, key string) (bool, error) {
	_, err := s.Store.Get(ctx, store.TableSettings, s.id(key))
	if store.IsNotFound(err) {
		return false, nil
	}
	return err == nil, err
}

// Delete removes key and, like certmagic's FileStorage, everything below it.
func (s *Storage) Delete(ctx context.Context, key string) error {
	key = strings.Trim(key, "/")
	recs, err := listPrefix(ctx, s.Store, prefixCaddy+key)
	if err != nil {
		return err
	}
	found := false
	for _, r := range recs {
		k := strings.TrimPrefix(r.ID, prefixCaddy)
		if k == key || strings.HasPrefix(k, key+"/") {
			found = true
			if err := s.Store.Delete(ctx, store.TableSettings, r.ID); err != nil {
				return err
			}
		}
	}
	if !found {
		return ErrKeyNotFound
	}
	return nil
}

// List returns keys below prefix; non-recursive lists only direct children
// (files and "directories").
func (s *Storage) List(ctx context.Context, prefix string, recursive bool) ([]string, error) {
	prefix = strings.Trim(prefix, "/")
	recs, err := listPrefix(ctx, s.Store, prefixCaddy+prefix)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, r := range recs {
		k := strings.TrimPrefix(r.ID, prefixCaddy)
		var rest string
		switch {
		case prefix == "":
			rest = k
		case strings.HasPrefix(k, prefix+"/"):
			rest = strings.TrimPrefix(k, prefix+"/")
		default:
			continue
		}
		if recursive {
			seen[k] = true
			continue
		}
		child, _, _ := strings.Cut(rest, "/")
		if prefix != "" {
			child = prefix + "/" + child
		}
		seen[child] = true
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out, nil
}

func (s *Storage) Stat(ctx context.Context, key string) (KeyInfo, error) {
	key = strings.Trim(key, "/")
	row, err := store.GetJSON[storageRow](ctx, s.Store, store.TableSettings, s.id(key))
	if err == nil {
		return KeyInfo{Key: key, Modified: time.Unix(0, row.Modified), Size: row.Size, IsTerminal: true}, nil
	}
	if !store.IsNotFound(err) {
		return KeyInfo{}, err
	}
	kids, err := s.List(ctx, key, false)
	if err != nil {
		return KeyInfo{}, err
	}
	if len(kids) == 0 {
		return KeyInfo{}, ErrKeyNotFound
	}
	return KeyInfo{Key: key, Modified: time.Now()}, nil
}

// Lock tries to acquire the named cluster-wide lock for owner (an opaque ID
// of the lock holder, e.g. a random string per Caddy lock call). It returns
// acquired=false when someone else holds it; callers poll. Re-locking with
// the same owner renews the lock.
func (s *Storage) Lock(ctx context.Context, name, owner string) (bool, error) {
	s.lockMu.Lock()
	defer s.lockMu.Unlock()
	if s.locks == nil {
		s.locks = map[string]*heldLock{}
	}
	ttl := s.LockTTL
	if ttl == 0 {
		ttl = 5 * time.Minute
	}
	if h, ok := s.locks[name]; ok {
		if h.owner == owner {
			if err := h.l.Renew(ctx); err != nil {
				delete(s.locks, name)
				return false, nil
			}
			return true, nil
		}
		if h.l.Valid() {
			return false, nil
		}
		delete(s.locks, name)
	}
	l, err := s.Leases.Acquire(ctx, "acme/"+name, ttl)
	if err != nil {
		if errors.Is(err, lease.ErrHeld) || errors.Is(err, lease.ErrLost) {
			return false, nil
		}
		return false, err
	}
	s.locks[name] = &heldLock{owner: owner, l: l}
	return true, nil
}

// Unlock releases a lock held by owner.
func (s *Storage) Unlock(ctx context.Context, name, owner string) error {
	s.lockMu.Lock()
	defer s.lockMu.Unlock()
	h, ok := s.locks[name]
	if !ok || h.owner != owner {
		return nil
	}
	delete(s.locks, name)
	return h.l.Release(ctx)
}

// Certificate returns the leaf certificate expiry for host, if a certificate
// is stored for it (any issuer).
func (s *Storage) CertExpiry(ctx context.Context, host string) (time.Time, bool) {
	name := strings.ReplaceAll(strings.ToLower(host), "*", "wildcard_")
	recs, err := listPrefix(ctx, s.Store, prefixCaddy+"certificates/")
	if err != nil {
		return time.Time{}, false
	}
	suffix := "/" + name + "/" + name + ".crt"
	for _, r := range recs {
		if !strings.HasSuffix(r.ID, suffix) {
			continue
		}
		pemBytes, err := s.Get(ctx, strings.TrimPrefix(r.ID, prefixCaddy))
		if err != nil {
			continue
		}
		if t, ok := leafNotAfter(pemBytes); ok {
			return t, true
		}
	}
	return time.Time{}, false
}

// StorageToken derives the bearer token shared by the daemon and the Caddy
// storage module (it is delivered inside the generated Caddy config).
func StorageToken(key secrets.Key) string {
	m := hmac.New(sha256.New, key[:])
	m.Write([]byte("aetherix-ingress-storage"))
	return hex.EncodeToString(m.Sum(nil))
}

// StorageHandler serves the storage protocol over HTTP. It is mounted before
// the node has its store, so Bind supplies the backend later (503 until then).
type StorageHandler struct {
	mu    sync.RWMutex
	s     *Storage
	token string
}

// Bind activates the handler.
func (h *StorageHandler) Bind(s *Storage) {
	h.mu.Lock()
	h.s, h.token = s, StorageToken(s.Key)
	h.mu.Unlock()
}

type storageReq struct {
	Key       string `json:"key"`
	Value     []byte `json:"value"`
	Prefix    string `json:"prefix"`
	Recursive bool   `json:"recursive"`
	Name      string `json:"name"`
	Owner     string `json:"owner"`
}

// ServeHTTP implements http.Handler: POST StoragePath+op.
func (h *StorageHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mu.RLock()
	s, token := h.s, h.token
	h.mu.RUnlock()
	if s == nil {
		http.Error(w, "storage not ready", http.StatusServiceUnavailable)
		return
	}
	got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var req storageReq
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<20)).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	reply := func(v any, err error) {
		switch {
		case errors.Is(err, ErrKeyNotFound):
			http.Error(w, "not found", http.StatusNotFound)
		case err != nil:
			http.Error(w, err.Error(), http.StatusInternalServerError)
		default:
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(v)
		}
	}
	type empty struct{}
	switch strings.TrimPrefix(r.URL.Path, StoragePath) {
	case "store":
		reply(empty{}, s.Put(ctx, req.Key, req.Value))
	case "load":
		v, err := s.Get(ctx, req.Key)
		reply(map[string][]byte{"value": v}, err)
	case "delete":
		reply(empty{}, s.Delete(ctx, req.Key))
	case "exists":
		ok, err := s.Exists(ctx, req.Key)
		reply(map[string]bool{"exists": ok}, err)
	case "list":
		keys, err := s.List(ctx, req.Prefix, req.Recursive)
		reply(map[string][]string{"keys": keys}, err)
	case "stat":
		ki, err := s.Stat(ctx, req.Key)
		reply(ki, err)
	case "lock":
		ok, err := s.Lock(ctx, req.Name, req.Owner)
		if err == nil && !ok {
			http.Error(w, "locked", http.StatusConflict)
			return
		}
		reply(empty{}, err)
	case "unlock":
		reply(empty{}, s.Unlock(ctx, req.Name, req.Owner))
	default:
		http.NotFound(w, r)
	}
}
