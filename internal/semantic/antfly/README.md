# Antfly Lite engine

`Build`, `Open`, `Index.Search`, and `Index.Close` wrap the local Antfly Lite
engine. There is no network service, model provider, Python environment, or Go
module dependency. Native loading is optional: ordinary `go test ./...` works
without Antfly. Unsupported platforms and builds with `CGO_ENABLED=0` return an
error wrapping `ErrUnavailable` when the engine is opened or built.

## Pinned dependency and environment

The runtime is the **v0.3.1 darwin-arm64 embedded release**, C ABI 3, already
unpacked on aerie at:

```
/Volumes/Swift/scratch/recall/antfly-engine/embedded/
```

Keep the embedded archive layout, assets and license notices together. The
runtime loader requires this exact `lib/libantfly.dylib` and verifies its SHA-256
before `dlopen`:

```
ecfacc69ccece59b38e7ad8d5920edb57d77141fa3b42bcb96d0c3aebd92e0ce  lib/libantfly.dylib
ce8e4fb830c51924923271653413b69625f43f276b1b1ae8adaa805d25d7f2c8  embedded.tgz
```

Configure the library before the first engine operation in the process:

```sh
export ANTFLY_LIBRARY=/Volumes/Swift/scratch/recall/antfly-engine/embedded/lib/libantfly.dylib
```

The runtime requires macOS 15.0 or later on arm64. The normal Go toolchain
with cgo and the macOS C compiler builds the small C ABI
bridge. No Antfly header, `pkg-config`, Antfly Go SDK, linker search path,
`DYLD_LIBRARY_PATH`, or native build tag is required. The dylib is loaded by
absolute path and retained for the process lifetime; database handles and their
reader leases are released by `Close`. Library initialization, including an
initial failure, is cached for the process lifetime, so fix the environment
before restarting a process that reported `ErrUnavailable`.

The bridge currently supports macOS arm64 only. Adding another runtime artifact
requires an explicit platform port and checksum pin. Loading a different build
with the same version label is deliberately rejected. This package does not
download, install, or update native dependencies.

## Build and query contract

The destination parent directory must exist and the destination file must not
exist. Build creates a hosted file, registers an external cosine index, submits
**one batch** with `sync_level="write"`, runs maintenance until idle, then closes
the writer before returning. The document retains `id` as a regular field as
well as `_embeddings`; an embeddings-only document does not produce searchable
hits in this release. Empty input creates an empty searchable index. The caller
owns staging, cleanup after a failed build, replacement and publication.

IDs must be unique, nonempty, valid UTF-8 strings. Vectors must match the positive dimension
count, contain only finite values and have nonzero length. Native cosine handles
unnormalized vectors, so this package does not change caller vectors. Open reads
and validates index metadata from the native file, with no companion manifest.

Every search explicitly sends `search_effort=1`. Native order is preserved and
at most the requested limit is returned. Zero limit returns an empty slice;
negative limits are errors. **Scores are signed cosine in [-1,1]**, returned
directly by the pinned Lite API. They are not the 1000-scaled positive scores of
Antfly's other search paths. Small floating-point endpoint excursions are
clamped; malformed, missing or out-of-range scores fail the search. A failed
native response is an error, never an empty successful result.

Cancellation is checked before and after native calls and between build stages.
The native ABI has no cancellation hook, so cancellation cannot interrupt an
in-progress C call. Search and Close on one Index are serialized, and Close is
idempotent. Callers must close every successfully opened Index.

## Checks

Portable contract tests use an in-process fake of the native seam. The optional
integration tag runs the real pinned library, including fresh build/reopen,
empty input, signed score semantics and exact top-12 retrieval against a cosine
oracle over 256 vectors:

```sh
go test -race ./internal/semantic/antfly
CGO_ENABLED=0 go test ./internal/semantic/antfly
ANTFLY_LIBRARY=/Volumes/Swift/scratch/recall/antfly-engine/embedded/lib/libantfly.dylib \
  go test -tags antflyintegration ./internal/semantic/antfly
```

The integration suite requires the pinned runtime and does not silently skip a
missing dependency on its supported platform. No paid fallback exists.
