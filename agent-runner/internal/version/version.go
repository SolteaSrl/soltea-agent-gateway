// Package version espone la versione del runner, dichiarata al gateway nel frame
// hello (cosi' la list_agents mostra quali runner sono aggiornati) e allegata a
// ogni chat.result e ai log.
//
// E' una var (non const) per poterla sovrascrivere a build time via ldflags:
//
//	go build -ldflags "-X .../internal/version.Runner=0.4.0"
package version

// Runner e' la versione del runner. Tenere allineata alle release agent-runner-vX.Y.Z.
var Runner = "0.4.0"
