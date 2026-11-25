// Command aetherixd is the AetherixCloud node daemon. Every machine runs the
// same binary: it serves the API, runs the reconciler and joins the mesh.
//
//	aetherixd init  [flags]   create a new cluster on this machine
//	aetherixd join  --token T join an existing cluster
//	aetherixd run   [flags]   run the daemon
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	v1 "github.com/aetherix/aetherixcloud/gen/aetherix/v1"
	"github.com/aetherix/aetherixcloud/internal/cluster"
	"github.com/aetherix/aetherixcloud/internal/daemon"
	"github.com/spf13/cobra"
)

var version = "dev"

func main() {
	root := &cobra.Command{Use: "aetherixd", Short: "AetherixCloud node daemon", SilenceUsage: true, Version: version}
	root.AddCommand(initCmd(), joinCmd(), runCmd())
	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func parseRoles(rs []string) ([]v1.MachineRole, error) {
	var out []v1.MachineRole
	for _, r := range rs {
		v, ok := v1.MachineRole_value["MACHINE_ROLE_"+strings.ToUpper(r)]
		if !ok {
			return nil, fmt.Errorf("unknown role %q (worker, ingress, builder)", r)
		}
		out = append(out, v1.MachineRole(v))
	}
	return out, nil
}

func parseLabels(ls []string) map[string]string {
	m := map[string]string{}
	for _, l := range ls {
		k, v, _ := strings.Cut(l, "=")
		m[k] = v
	}
	return m
}

func initCmd() *cobra.Command {
	var dataDir, clusterName, name, domain, fromBackup string
	var advertise, roles, labels []string
	var wgPort, apiPort int
	var asJSON bool
	c := &cobra.Command{
		Use:   "init",
		Short: "Create a new cluster on this machine",
		Long: `Create a new cluster on this machine.

Disaster recovery: pass --from-backup path/to/file.snapshot (version ≥ 2, from
ax cluster backup / Settings → Backups) to recover store state and cluster
identity. Peer machines are cleared on first start and must rejoin; Docker
volume data is not in the snapshot — restore from S3 if needed.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if cluster.Exists(dataDir) {
				return fmt.Errorf("%s already contains a node; remove it to re-initialise", dataDir)
			}
			rs, err := parseRoles(roles)
			if err != nil {
				return err
			}
			opts := cluster.InitOptions{ClusterName: clusterName, MachineName: name, Advertise: advertise,
				WGPort: wgPort, APIPort: apiPort, Roles: rs, Labels: parseLabels(labels), Domain: domain}
			var st *cluster.NodeState
			var token string
			if fromBackup != "" {
				st, token, err = cluster.InitFromBackup(dataDir, fromBackup, opts)
			} else {
				st, token, err = cluster.Init(opts)
			}
			if err != nil {
				return err
			}
			if err := st.Save(dataDir); err != nil {
				return err
			}
			host := "localhost"
			if len(advertise) > 0 {
				host = advertise[0]
			}
			out := map[string]any{"cluster_id": st.ClusterID, "machine_id": st.MachineID, "machine_name": st.MachineName,
				"admin_token": token, "ca_pem": st.CACertPEM, "api_url": fmt.Sprintf("https://%s:%d", host, st.APIPort)}
			if fromBackup != "" {
				out["from_backup"] = fromBackup
			}
			if asJSON {
				return json.NewEncoder(os.Stdout).Encode(out)
			}
			extra := ""
			if fromBackup != "" {
				extra = "\nSnapshot will be applied on first daemon start; rejoin other machines afterwards.\n"
			}
			fmt.Printf("Cluster %q initialised on %s.%s\nAdmin API token (shown once):\n  %s\n\nStart the daemon: aetherixd run --data-dir %s\nThen: ax context add default --url %s --token %s --insecure\n",
				st.ClusterName, st.MachineName, extra, token, dataDir, out["api_url"], token)
			return nil
		},
	}
	f := c.Flags()
	f.StringVar(&dataDir, "data-dir", "/var/lib/aetherix", "data directory")
	f.StringVar(&clusterName, "cluster-name", "aetherix", "cluster name")
	f.StringVar(&name, "name", "node-1", "machine name")
	f.StringVar(&domain, "domain", "", "default domain")
	f.StringSliceVar(&advertise, "advertise", nil, "public IPs/hostnames of this machine (repeatable)")
	f.StringSliceVar(&roles, "roles", nil, "roles: worker,ingress,builder (default all)")
	f.StringSliceVar(&labels, "label", nil, "machine labels k=v")
	f.IntVar(&wgPort, "wg-port", 51820, "WireGuard UDP port")
	f.IntVar(&apiPort, "api-port", cluster.DefaultAPIPort, "public API TLS port")
	f.StringVar(&fromBackup, "from-backup", "", "restore cluster state from a .snapshot on first start")
	f.BoolVar(&asJSON, "json", false, "machine-readable output")
	return c
}

func joinCmd() *cobra.Command {
	var dataDir, token, name string
	var advertise, labels []string
	var wgPort, apiPort int
	c := &cobra.Command{
		Use:   "join",
		Short: "Join an existing cluster with a token",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if cluster.Exists(dataDir) {
				return fmt.Errorf("%s already contains a node", dataDir)
			}
			st, err := cluster.Join(cmd.Context(), cluster.JoinOptions{Token: token, Name: name, Advertise: advertise,
				WGPort: wgPort, APIPort: apiPort, Labels: parseLabels(labels)})
			if err != nil {
				return err
			}
			if err := st.Save(dataDir); err != nil {
				return err
			}
			fmt.Printf("Joined cluster %q as %s (%s). Start the daemon: aetherixd run --data-dir %s\n", st.ClusterName, st.MachineName, st.Subnet, dataDir)
			return nil
		},
	}
	f := c.Flags()
	f.StringVar(&dataDir, "data-dir", "/var/lib/aetherix", "data directory")
	f.StringVar(&token, "token", "", "join token")
	f.StringVar(&name, "name", "", "machine name (default: assigned)")
	f.StringSliceVar(&advertise, "advertise", nil, "public IPs/hostnames of this machine")
	f.StringSliceVar(&labels, "label", nil, "machine labels k=v")
	f.IntVar(&wgPort, "wg-port", 51820, "WireGuard UDP port")
	f.IntVar(&apiPort, "api-port", cluster.DefaultAPIPort, "public API TLS port")
	_ = c.MarkFlagRequired("token")
	return c
}

func runCmd() *cobra.Command {
	var cfg daemon.Config
	var verbose bool
	c := &cobra.Command{
		Use:   "run",
		Short: "Run the daemon",
		RunE: func(cmd *cobra.Command, _ []string) error {
			lvl := slog.LevelInfo
			if verbose {
				lvl = slog.LevelDebug
			}
			cfg.Logger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))
			cfg.Version = version
			ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()
			d := daemon.New(cfg)
			registerHooks(d) // later workers: ingress, mcp, github, web ...
			return d.Run(ctx)
		},
	}
	f := c.Flags()
	f.StringVar(&cfg.DataDir, "data-dir", "/var/lib/aetherix", "data directory")
	f.StringVar(&cfg.Store, "store", "", "store backend: corrosion | sqlite (default: corrosion if installed)")
	f.StringVar(&cfg.CorrosionBin, "corrosion-bin", "", "path to the corrosion binary")
	f.StringVar(&cfg.ListenPublic, "listen", ":8443", "public TLS API address")
	f.BoolVar(&cfg.NoMesh, "no-mesh", false, "do not manage WireGuard (single-node dev)")
	f.BoolVar(&cfg.NoDocker, "no-docker", false, "do not use Docker")
	f.BoolVar(&cfg.FastMembership, "fast-membership", false, "fast failure detection (dev clusters)")
	f.BoolVar(&verbose, "v", false, "verbose logging")
	return c
}
