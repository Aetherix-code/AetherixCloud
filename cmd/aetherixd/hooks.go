package main

import "github.com/aetherix/aetherixcloud/internal/daemon"

// registerHooks is the single integration point for the later workers. Each
// adds one line here, e.g.
//
//	d.Hooks.Handlers["/mcp"] = mcp.Handler(...)            // mcp worker
//	d.Hooks.Handlers["/api/github/webhook"] = ...          // github worker
//	d.Hooks.Handlers["/"] = web.Handler()                  // dashboard-ha worker
//	d.Hooks.Services = append(d.Hooks.Services, ingress.Run) // ingress worker
func registerHooks(d *daemon.Daemon) {
	hookWeb(d)           // dashboard-ha: embedded SPA (hooks_web.go)
	hookGitHub(d)        // github/build: webhooks, GitHubService, builds (hooks_github.go)
	registerAuthHooks(d) // auth-api/mcp: /mcp, OAuth 2.1, first-owner seed (hooks_auth.go)
	hookIngress(d)       // ingress/rolling/reschedule: Caddy, rollouts, failover (hooks_ingress.go)
	hookExtras(d)        // templates, metrics, backups, cron, notifications (hooks_extras.go)
}
