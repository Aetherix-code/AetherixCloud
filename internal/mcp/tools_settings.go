package mcp

import (
	"context"
	"errors"

	"connectrpc.com/connect"
	v1 "github.com/aetherix/aetherixcloud/gen/aetherix/v1"
	"github.com/aetherix/aetherixcloud/internal/auth"
)

// Settings / extras HTTP APIs (S3 targets, backups, build concurrency & images).
func (s *Server) registerSettingsTools() {
	register(s, "get_build_concurrency", "Max parallel builds per builder node (excess stay queued).", auth.LevelRead, ro("Build concurrency"),
		func(ctx context.Context, c *call, _ empty) (any, error) {
			return c.httpJSON(ctx, "GET", "/api/build-concurrency", nil)
		})

	type concIn struct {
		MaxParallel int `json:"max_parallel" jsonschema:"1–64 builds per builder at once (default 2)"`
	}
	register(s, "set_build_concurrency", "Set how many builds each builder may run in parallel; the rest queue.", auth.LevelAdmin, write("Set build concurrency", true),
		func(ctx context.Context, c *call, in concIn) (any, error) {
			if in.MaxParallel <= 0 {
				return nil, errors.New("max_parallel must be >= 1")
			}
			return c.httpJSON(ctx, "PUT", "/api/build-concurrency", map[string]any{"max_parallel": in.MaxParallel})
		})

	register(s, "get_build_images", "Cluster-wide build-image S3 archive settings.", auth.LevelRead, ro("Build images"),
		func(ctx context.Context, c *call, _ empty) (any, error) {
			return c.httpJSON(ctx, "GET", "/api/build-images", nil)
		})

	type imagesIn struct {
		Enabled  bool   `json:"enabled"`
		TargetID string `json:"target_id,omitempty" jsonschema:"S3 target id (empty = default)"`
		Retain   int    `json:"retain,omitempty" jsonschema:"archives and local tags kept per app"`
	}
	register(s, "set_build_images", "Enable/configure S3 archive of build images and local tag pruning.", auth.LevelAdmin, write("Set build images", true),
		func(ctx context.Context, c *call, in imagesIn) (any, error) {
			body := map[string]any{"enabled": in.Enabled, "target_id": in.TargetID}
			if in.Retain > 0 {
				body["retain"] = in.Retain
			}
			return c.httpJSON(ctx, "PUT", "/api/build-images", body)
		})

	register(s, "list_s3_targets", "Named S3-compatible destinations for backups and image archives.", auth.LevelRead, ro("List S3 targets"),
		func(ctx context.Context, c *call, _ empty) (any, error) {
			return c.httpJSON(ctx, "GET", "/api/s3-targets", nil)
		})

	type s3In struct {
		Name            string `json:"name"`
		Endpoint        string `json:"endpoint"`
		Region          string `json:"region,omitempty"`
		Bucket          string `json:"bucket"`
		AccessKeyID     string `json:"access_key_id"`
		SecretAccessKey string `json:"secret_access_key"`
		Prefix          string `json:"prefix,omitempty"`
		PathStyle       *bool  `json:"path_style,omitempty"`
	}
	register(s, "create_s3_target", "Add an S3-compatible target (MinIO, AWS, R2, …).", auth.LevelAdmin, write("Create S3 target", false),
		func(ctx context.Context, c *call, in s3In) (any, error) {
			return c.httpJSON(ctx, "POST", "/api/s3-targets", in)
		})

	type updateS3In struct {
		ID string `json:"id" jsonschema:"target id"`
		s3In
	}
	register(s, "update_s3_target", "Update an S3 target. Omit secret_access_key to keep the existing secret.", auth.LevelAdmin, write("Update S3 target", true),
		func(ctx context.Context, c *call, in updateS3In) (any, error) {
			if in.ID == "" {
				return nil, errors.New("id is required")
			}
			return c.httpJSON(ctx, "PUT", "/api/s3-targets/"+in.ID, in.s3In)
		})

	type deleteS3In struct {
		ID string `json:"id" jsonschema:"target id"`
		Confirmation
	}
	register(s, "delete_s3_target", "Remove an S3 target. Destructive: needs confirm=true.", auth.LevelAdmin, destructive("Delete S3 target"),
		func(ctx context.Context, c *call, in deleteS3In) (any, error) {
			if dry, err := c.gate("delete_s3_target", in.ID, in.Confirmation, func() (map[string]any, error) {
				return map[string]any{"action": "delete s3 target", "id": in.ID}, nil
			}); err != nil || dry != nil {
				return dry, err
			}
			return c.httpJSON(ctx, "DELETE", "/api/s3-targets/"+in.ID, nil)
		})

	register(s, "list_backups", "Cluster backup schedule, recent runs, and S3 targets.", auth.LevelRead, ro("List backups"),
		func(ctx context.Context, c *call, _ empty) (any, error) {
			return c.httpJSON(ctx, "GET", "/api/backups", nil)
		})

	type clusterBackupIn struct {
		Enabled  bool   `json:"enabled"`
		TargetID string `json:"target_id,omitempty"`
		Cron     string `json:"cron,omitempty" jsonschema:"cron expression for cluster settings backups"`
		Retain   int    `json:"retain,omitempty"`
	}
	register(s, "set_cluster_backup_schedule", "Enable/configure scheduled cluster settings backups to S3.", auth.LevelAdmin, write("Cluster backup schedule", true),
		func(ctx context.Context, c *call, in clusterBackupIn) (any, error) {
			return c.httpJSON(ctx, "PUT", "/api/backups", map[string]any{"cluster": in})
		})

	type runBackupIn struct {
		ServiceID string `json:"service_id,omitempty" jsonschema:"empty = all scheduled volume backups"`
		Volume    string `json:"volume,omitempty"`
	}
	register(s, "run_backup", "Run volume backup(s) now.", auth.LevelAdmin, write("Run backup", false),
		func(ctx context.Context, c *call, in runBackupIn) (any, error) {
			return c.httpJSON(ctx, "POST", "/api/backups/run", in)
		})

	register(s, "run_cluster_backup", "Snapshot cluster settings to S3 now.", auth.LevelAdmin, write("Run cluster backup", false),
		func(ctx context.Context, c *call, _ empty) (any, error) {
			return c.httpJSON(ctx, "POST", "/api/backups/cluster/run", map[string]any{})
		})

	type restoreIn struct {
		RunID     string `json:"run_id,omitempty"`
		ServiceID string `json:"service_id,omitempty"`
		Volume    string `json:"volume,omitempty"`
		Key       string `json:"key,omitempty" jsonschema:"S3 object key if not using run_id"`
		Confirmation
	}
	register(s, "restore_volume_backup", "Restore a volume from S3 (stops the app first). Destructive: needs confirm=true.",
		auth.LevelAdmin, destructive("Restore volume backup"),
		func(ctx context.Context, c *call, in restoreIn) (any, error) {
			if dry, err := c.gate("restore_volume_backup", in.RunID+"/"+in.ServiceID+"/"+in.Volume, in.Confirmation, func() (map[string]any, error) {
				return map[string]any{"action": "restore volume", "run_id": in.RunID, "service_id": in.ServiceID,
					"volume": in.Volume, "key": in.Key, "effect": "the service is stopped, the volume is restored from S3, then you can start it again"}, nil
			}); err != nil || dry != nil {
				return dry, err
			}
			body := map[string]any{}
			if in.RunID != "" {
				body["run_id"] = in.RunID
			}
			if in.ServiceID != "" {
				body["service_id"] = in.ServiceID
			}
			if in.Volume != "" {
				body["volume"] = in.Volume
			}
			if in.Key != "" {
				body["key"] = in.Key
			}
			return c.httpJSON(ctx, "POST", "/api/backups/restore", body)
		})

	type tokenIn struct {
		Name       string   `json:"name" jsonschema:"token label"`
		Scopes     []string `json:"scopes,omitempty" jsonschema:"read, deploy, admin (default read)"`
		TTLSeconds int      `json:"ttl_seconds,omitempty" jsonschema:"0 = no expiry"`
	}
	register(s, "create_api_token", "Create an API / MCP token. The plaintext is returned once.", auth.LevelDeploy, write("Create API token", false),
		func(ctx context.Context, c *call, in tokenIn) (any, error) {
			scopes := in.Scopes
			if len(scopes) == 0 {
				scopes = []string{"read"}
			}
			r, err := c.api.Auth.CreateApiToken(ctx, connect.NewRequest(&v1.CreateApiTokenRequest{
				Name: in.Name, Scopes: scopes, TtlSeconds: int32(in.TTLSeconds)}))
			if err != nil {
				return nil, err
			}
			return map[string]any{"token": toAny(r.Msg.Token), "plaintext": r.Msg.Plaintext,
				"note": "store plaintext now; it is never shown again"}, nil
		})

	register(s, "list_api_tokens", "List the caller's API tokens (no plaintext).", auth.LevelRead, ro("List API tokens"),
		func(ctx context.Context, c *call, _ empty) (any, error) {
			r, err := c.api.Auth.ListApiTokens(ctx, connect.NewRequest(&v1.ListApiTokensRequest{}))
			if err != nil {
				return nil, err
			}
			return map[string]any{"tokens": listAny(r.Msg.Tokens)}, nil
		})

	type revokeTokenIn struct {
		TokenID string `json:"token_id"`
	}
	register(s, "revoke_api_token", "Revoke an API token.", auth.LevelDeploy, write("Revoke API token", true),
		func(ctx context.Context, c *call, in revokeTokenIn) (any, error) {
			if in.TokenID == "" {
				return nil, errors.New("token_id is required")
			}
			if _, err := c.api.Auth.RevokeApiToken(ctx, connect.NewRequest(&v1.RevokeApiTokenRequest{TokenId: in.TokenID})); err != nil {
				return nil, err
			}
			return map[string]any{"revoked": in.TokenID}, nil
		})
}
