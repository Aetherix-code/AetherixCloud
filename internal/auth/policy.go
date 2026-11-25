package auth

import (
	"strings"
)

// Access classes of a procedure.
type Access int

const (
	// AccessLevel: authenticated, and min(scope, role) >= Level.
	AccessLevel Access = iota
	// AccessPublic: no credentials needed.
	AccessPublic
	// AccessSelf: any authenticated credential (>= read); the handler
	// restricts the data to the caller.
	AccessSelf
)

// Rule is the authorization rule of one RPC.
type Rule struct {
	Access Access
	Level  Level
}

// Procedure names ("/aetherix.v1.Service/Method").
const (
	procPrefix = "/aetherix.v1."
)

var public = map[string]bool{
	"AuthService/Login":          true,
	"AuthService/GetSetupStatus": true,
	"AuthService/Bootstrap":      true,
	"MachineService/JoinMachine": true,
}

var self = map[string]bool{
	"AuthService/Logout":           true,
	"AuthService/Me":               true,
	"AuthService/CreateApiToken":   true,
	"AuthService/ListApiTokens":    true,
	"AuthService/RevokeApiToken":   true,
	"AuthService/ListOAuthGrants":  true,
	"AuthService/RevokeOAuthGrant": true,
}

// explicit overrides; everything else follows the method-name rule below.
var explicit = map[string]Level{
	// cluster / machines: operator actions
	"ClusterService/UpdateCluster":     LevelAdmin,
	"ClusterService/Backup":            LevelAdmin, // dumps every table, including secrets
	"ClusterService/Restore":           LevelAdmin, // replaces cluster state
	"ClusterService/StartUpgrade":      LevelAdmin,
	"ClusterService/CancelUpgrade":     LevelAdmin,
	"ClusterService/CheckUpdates":      LevelAdmin,
	"MachineService/CreateJoinToken":   LevelAdmin,
	"MachineService/UpdateMachine":     LevelAdmin,
	"MachineService/CordonMachine":     LevelAdmin,
	"MachineService/DrainMachine":      LevelAdmin,
	"MachineService/RemoveMachine":     LevelAdmin,
	"MachineService/ReportHeartbeat":   LevelAdmin,
	"ProjectService/DeleteProject":     LevelAdmin,
	"ProjectService/DeleteEnvironment": LevelAdmin,
	"ServiceService/DeleteService":     LevelAdmin,
	// users & audit
	"AuthService/CreateUser":   LevelAdmin,
	"AuthService/ListUsers":    LevelAdmin,
	"AuthService/UpdateUser":   LevelAdmin,
	"AuthService/DeleteUser":   LevelAdmin,
	"AuthService/ListAuditLog": LevelAdmin,
	// GitHub App management
	"GitHubService/BeginManifestSetup":    LevelAdmin,
	"GitHubService/CompleteManifestSetup": LevelAdmin,
	"GitHubService/SetWebhookUrl":         LevelAdmin,
	"GitHubService/SimulatePush":          LevelDeploy,
	// reading variables is fine for viewers (secrets are redacted by the API)
	"ProjectService/ListVariables": LevelRead,
}

// RuleFor returns the rule of a procedure. Unknown read-looking methods
// (Get*, List*, Watch*, Stream*) need read; everything else needs deploy.
func RuleFor(procedure string) Rule {
	name := strings.TrimPrefix(procedure, procPrefix)
	if public[name] {
		return Rule{Access: AccessPublic}
	}
	if self[name] {
		return Rule{Access: AccessSelf, Level: LevelRead}
	}
	if l, ok := explicit[name]; ok {
		return Rule{Level: l}
	}
	method := name[strings.LastIndex(name, "/")+1:]
	for _, p := range []string{"Get", "List", "Watch", "Stream"} {
		if strings.HasPrefix(method, p) {
			return Rule{Level: LevelRead}
		}
	}
	if !strings.HasPrefix(name, "AuthService/") && !strings.HasPrefix(name, "GitHubService/") {
		return Rule{Level: LevelDeploy}
	}
	return Rule{Level: LevelAdmin} // deny-by-default for new auth/github RPCs
}
