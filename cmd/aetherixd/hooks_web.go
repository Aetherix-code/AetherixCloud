package main

import (
	"github.com/aetherix/aetherixcloud/internal/daemon"
	"github.com/aetherix/aetherixcloud/internal/web"
)

// hookWeb serves the embedded dashboard SPA on every node (mesh and public
// listeners). Called from registerHooks.
func hookWeb(d *daemon.Daemon) {
	d.Hooks.Fallback = web.Handler()
}
