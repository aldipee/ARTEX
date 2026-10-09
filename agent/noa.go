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

// noa is the model-driven context compression mechanism introduced in norma v0.4.0 and
// exposed as an experimental setting in the UI. It is mutually exclusive with built-in
// compaction: noaadapter.Enable is the only entry point and attaches the context manager
// (Compactor), Compress tool, and three persistent prompt blocks at once. Without Enable,
// noa is off and built-in compaction continues to work. Each agent reads noaEnabledFn once
// per run, so changing the setting affects future runs without rebuilding the agent.

// enableNoa wires noa into opts when the setting is enabled. archiveRoot is the persistent
// base directory for compressed originals (the global workDir, shared by all agents under
// <workDir>/noa rather than split by task/intent), and sessionID names a unique archive
// subdirectory beneath it.
//
// Because noa is experimental, setup failures must not interrupt a live task. Report errors
// through onWarn and fall back to built-in compression. After successful setup, clear
// opts.Compaction to avoid agentcore warning that two context managers are configured.
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
			onWarn("noa compression setup failed; falling back to built-in compression: " + err.Error())
		}
		return
	}
	// Compactor supersedes Compaction; clear the latter to avoid an agentcore warning about both being set.
	opts.Compaction = nil
}
