package store

import (
	"fmt"
	"strings"
)

// Table names. Keep in sync with plan section 4.
const (
	TableCluster             = "cluster"
	TableMachines            = "machines"
	TableJoinTokens          = "join_tokens"
	TableProjects            = "projects"
	TableEnvironments        = "environments"
	TableServices            = "services"
	TableDeployments         = "deployments"
	TableReplicas            = "replicas"
	TableDomains             = "domains"
	TableVariables           = "variables"
	TableUsers               = "users"
	TableSessions            = "sessions"
	TableAPITokens           = "api_tokens"
	TableOAuthClients        = "oauth_clients"
	TableOAuthGrants         = "oauth_grants"
	TableGitHubInstallations = "github_installations"
	TableBuilds              = "builds"
	TableLeases              = "leases"
	TableEvents              = "events"
	TableAuditLog            = "audit_log"
	TableSettings            = "settings"
)

// TableDef describes a table: its denormalised index columns.
type TableDef struct {
	Name    string
	Indexes []string
}

// Tables is the full schema (migration 1).
var Tables = []TableDef{
	{TableCluster, nil},
	{TableMachines, []string{"name"}},
	{TableJoinTokens, []string{"token_hash"}},
	{TableProjects, []string{"name"}},
	{TableEnvironments, []string{"project_id", "name"}},
	{TableServices, []string{"project_id", "environment_id", "name"}},
	{TableDeployments, []string{"service_id"}},
	{TableReplicas, []string{"service_id", "deployment_id", "machine_id"}},
	{TableDomains, []string{"service_id", "hostname"}},
	{TableVariables, []string{"environment_id", "service_id", "key"}},
	{TableUsers, []string{"email"}},
	{TableSessions, []string{"user_id"}},
	{TableAPITokens, []string{"user_id", "token_hash"}},
	{TableOAuthClients, nil},
	{TableOAuthGrants, []string{"client_id", "user_id"}},
	{TableGitHubInstallations, nil},
	{TableBuilds, []string{"service_id", "status"}},
	// leases: one row per (lease name, claimant); see internal/lease.
	{TableLeases, []string{"name", "owner"}},
	{TableEvents, []string{"type"}},
	{TableAuditLog, []string{"actor"}},
	{TableSettings, nil},
}

// Migration is one versioned schema step.
type Migration struct {
	Version int
	Name    string
	SQL     []string
}

// Migrations returns the ordered migration list. Append new migrations; never
// edit existing ones (Corrosion schema files are generated from the same SQL).
func Migrations() []Migration {
	return []Migration{{Version: 1, Name: "initial", SQL: initialSQL()}}
}

func tableDef(name string) (TableDef, bool) {
	for _, t := range Tables {
		if t.Name == name {
			return t, true
		}
	}
	return TableDef{}, false
}

func initialSQL() []string {
	var out []string
	for _, t := range Tables {
		var b strings.Builder
		fmt.Fprintf(&b, "CREATE TABLE IF NOT EXISTS %s (\n  id TEXT NOT NULL PRIMARY KEY", t.Name)
		for _, c := range t.Indexes {
			fmt.Fprintf(&b, ",\n  %s TEXT NOT NULL DEFAULT ''", quoteIdent(c))
		}
		b.WriteString(",\n  data TEXT NOT NULL DEFAULT ''")
		b.WriteString(",\n  updated_at INTEGER NOT NULL DEFAULT 0\n);")
		out = append(out, b.String())
	}
	return out
}

func quoteIdent(s string) string { return `"` + s + `"` }

// SchemaSQL returns the complete schema as one script, suitable for
// Corrosion's schema directory (it accepts CREATE TABLE statements only, so
// secondary indexes are intentionally not included for Corrosion).
func SchemaSQL() string {
	var b strings.Builder
	for _, m := range Migrations() {
		for _, s := range m.SQL {
			b.WriteString(s)
			b.WriteString("\n\n")
		}
	}
	return b.String()
}
