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

// noa is the "model-driven context compaction" mechanism introduced in norma v0.4.0, exposed as a platform
// experimental feature that the user toggles in system settings. It is mutually exclusive with built-in compaction: noaadapter.Enable is the only entry point, installing in one step
// the context takeover handler (Compactor), the Compress tool, and three resident prompt segments; not calling Enable means it's off
// (built-in compaction works as usual). The toggle is resolved by the noaEnabledFn each agent injects, read once per run,
// so switching only affects runs started afterward and needs no agent rebuild.

// enableNoa wires noa into opts when the resolver reports it's on. archiveRoot is the persistence base directory for the compacted originals
// (taken from the global workDir; every agent lands uniformly under <workDir>/noa, not scattered across task/intent directories), and sessionID
// names the archive subdirectory beneath it (globally unique, so there's no collision within the same base directory).
//
// noa is an experimental feature: a wiring failure must not interrupt a real task. On error it reports via onWarn and falls back to built-in compaction.
// On a successful enable it clears opts.Compaction to avoid agentcore warning about "two context managers set at once".
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
			onWarn("Failed to enable noa compaction, falling back to built-in compaction: " + err.Error())
		}
		return
	}
	// Compactor overrides Compaction, but agentcore warns every time both are present; clear it explicitly.
	opts.Compaction = nil
}
