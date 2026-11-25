// Package ingress runs Caddy on machines with the ingress role.
//
// Pieces:
//
//   - Generate (config.go): pure function turning domains + healthy replicas
//     (reached directly over the mesh) into a Caddy JSON config.
//   - Controller (controller.go): per-node loop that pushes the generated
//     config to the local Caddy admin API under the lease "ingress:<machine>",
//     and (under the lease "ingress-domains") maintains default hostnames and
//     certificate state.
//   - Supervisor (supervisor.go): starts/restarts the local caddy binary.
//   - Storage + StorageHandler (storage.go): certmagic-compatible storage
//     backed by the replicated store (certificates are sealed with the
//     cluster key; ACME locks are leases). The Caddy side of the protocol is
//     the module in deploy/caddy.
//   - Drain helpers (drain.go): the rolling-deploy controller marks a replica
//     as draining; ingress removes it from upstreams and acks by recording the
//     time of the last applied config.
//   - Default hostnames (hostnames.go) and DNS verification (dns.go).
package ingress
