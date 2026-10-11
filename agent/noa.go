package agent

import (
	"log"
	"path/filepath"

	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/noaadapter"
)

// noaWarn returns a diagnostics sink tagging non-fatal noa messages with the
// session, routed through the package logger (agents have no per-instance one).
func noaWarn(session string) func(string) {
	return func(msg string) { log.Printf("[noa] %s: %s", session, msg) }
}

// noa is the "model-driven context compression" mechanism introduced in norma
// v0.4.0. As an experimental platform feature it is toggled by the user in
// Settings. It is mutually exclusive with built-in compaction: noaadapter.Enable
// is the only entry point, attaching a context takeover handler (Compactor), the
// Compress tool, and three persistent prompt segments in one shot; not calling
// Enable means it is OFF (built-in compaction keeps running). The toggle is
// resolved by the noaEnabledFn injected per agent, and is read once per run, so
// flipping the switch only affects subsequent runs and does not require
// rebuilding the agent.

// enableNoa wires noa into opts when the resolver reports enabled. archiveRoot
// is the persistent base directory for compressed originals (taken from the
// global workDir, with every agent landing under <workDir>/noa, not scattered
// across task/intent directories); sessionID names the archive subdirectory
// underneath (globally unique, so no conflicts within the same base directory).
//
// noa is experimental: a failure to wire it must NOT interrupt real tasks. On
// error, report via onWarn and fall back to the built-in compaction. On success,
// clear opts.Compaction to avoid agentcore warning about "two context managers
// set at the same time".
func enableNoa(opts *agentcore.Options, enabled func() bool, archiveRoot, sessionID string, onWarn func(string)) {
	if enabled == nil || !enabled() {
		return
	}
	if opts.OnWarn == nil {
		opts.OnWarn = onWarn
	}
	if err := noaadapter.Enable(opts, noaadapter.Options{
		ArchiveBaseDir: filepath.Join(archiveRoot, "noa"),
		SessionID:      sessionID,
		OnWarn:         onWarn,
	}); err != nil {
		if onWarn != nil {
			onWarn("noa compression enable failed, falling back to built-in compaction: " + err.Error())
		}
		return
	}
	// Compactor overrides Compaction, but when both coexist agentcore warns every
	// time; clear it explicitly.
	opts.Compaction = nil
}
