// Package registry embeds a CNCF `distribution` registry in the daemon and
// provides the peer-to-peer image Puller.
//
// Model: images built (or pushed with `ax image push`) on a machine live in
// that machine's embedded registry, listening on 127.0.0.1:5000 (so refs
// "127.0.0.1:5000/<repo>:<tag>" are valid on EVERY machine and Docker treats
// 127.0.0.0/8 as insecure by default) and on <mesh IP>:5000 for peers.
// When another machine must run such an image, PeerPuller pulls it from the
// peers' registries over the mesh and tags it under the original ref. Docker
// must be configured with `insecure-registries: ["10.210.0.0/16"]` (done by
// deploy/dev and install.sh).
package registry

import "log/slog"

// Port is the registry port on every machine.
const Port = 5000

// LocalHost is the image host prefix valid on every machine.
const LocalHost = "127.0.0.1:5000"

// Server is the embedded registry.
type Server struct {
	// Dir stores blobs.
	Dir string
	// Addrs to listen on (e.g. "127.0.0.1:5000", "10.210.1.1:5000").
	Addrs  []string
	Logger *slog.Logger
}
