// Package config loads, merges, and validates Recall configuration.
//
// Boundary: it owns the two-layer merge, the trust boundary between those
// layers, and every validation that can be decided without contacting a
// source. It knows nothing about retrieval, ranking, adapter internals, or how
// a subprocess is spawned. It resolves identity — [Config] maps a renameable
// source_id to its immutable source_uid — but it never resolves a secret.
//
// # The trust boundary
//
// A project configuration travels with a cloned repository, so loading one
// must never be able to execute attacker-chosen code. Two layers exist for
// exactly that reason:
//
//   - The user layer ($XDG_CONFIG_HOME/recall/config.toml and
//     adapters.d/*.toml) is trusted. It is the only place an adapter command,
//     argv, or environment may be declared.
//   - The project layer (recall.toml, discovered from a working directory) may
//     reference adapters by name and supply location, scope, priors, record
//     types, freshness mode, timeouts, sensitivity, and adapter settings.
//     Nothing executable.
//
// A project file that names an executable key anywhere in its document —
// command, args, argv, env, environment, exec, entrypoint, or shell — or that
// opens an [adapters] table at all, is rejected with an error naming the file
// and the offending key. It is never a warning and never a silent drop, and
// the check runs over the raw document before any typed decoding, so a key
// buried inside an adapter-owned settings table is caught with the same force
// as a top-level one.
//
// Three narrower rules protect identity and permissions across the same
// boundary. A project file may not reassign the source_uid of a source the
// user declared, may not repoint an existing source at a different adapter,
// and may only move sensitivity floors and profile ceilings in the restrictive
// direction. A cloned repository can therefore narrow what Recall reads, and
// can add sources of its own, but cannot redirect a persisted reference or
// widen access to an existing one.
//
// # Secrets
//
// Secrets are references, never values: an environment variable name or an OS
// keychain reference. This package reads neither. Materialization happens at
// the moment of use, in the package that spawns the adapter, so a resolved
// configuration and its [Config.Explain] view are always safe to print.
package config
