# Recall quickstart

This walkthrough builds Recall, indexes the small synthetic Markdown corpus in
the clone, and gets an answer. It does not need an account, network service, or
private data.

The captured output below comes from the same commands on a clean set of XDG
directories. Your source ID and result text will match; absolute paths,
generated IDs, Git revisions, and elapsed times will differ.

## Install

Recall requires Go 1.26.4 or newer.

```sh
git clone https://github.com/marcus/recall.git
cd recall
make install
export PATH="$HOME/.local/bin:$PATH"
```

`make install` builds and installs the core command and the reference external
adapter:

```text
installed recall -> /Users/you/.local/bin
installed recall-stream -> /Users/you/.local/bin
```

Confirm that the command on `PATH` is this project:

```sh
recall version
```

```text
recall <version> (<commit>)
```

## Create the first configuration

Point the first source at the committed synthetic corpus:

```sh
recall init --docs "$PWD/eval/packs/shapes/sources/corpus"
```

```text
created /Users/you/.config/recall/config.toml
documents source docs (8DTW05Y2RE0MSR7X)
documents directory /Users/you/src/recall/eval/packs/shapes/sources/corpus

Next:
  recall refresh --source docs
  recall query "what did we decide"
```

`recall init --json --docs <directory>` returns the same facts as structured
data and never prompts. Init refuses to replace an existing configuration; use
`--force` only when replacing that file is intentional. If
`XDG_CONFIG_HOME` is set, Recall writes
`$XDG_CONFIG_HOME/recall/config.toml` instead.

The generated file enables the documents source. It leaves Tasks and `td`
source examples commented out, with the requirement for each on the line above
it.

## Build the index

The documents adapter builds once during its first handshake. `refresh` makes
the maintenance step explicit and gives you a health report:

```sh
recall refresh --source docs
```

```text
outcome refreshed  elapsed 59ms  sources 1
source docs  status refreshed  elapsed 59ms  health healthy  coverage complete  generation gen-000002-a6915511bf58  source watermark git:ce483d42fc05+fs:99b49f5416f3  index watermark git:ce483d42fc05+fs:99b49f5416f3
```

## Query, then expand

Ask a question the synthetic corpus can answer:

```sh
recall query "calibrated spectrometer"
```

```text
outcome answered  coverage complete  results 1  elapsed 1ms

1. docs:guides/aurora-guide.md#L5-L8
   Aurora Field Guide > Observation zone > Instrument notes
 > Use a calibrated spectrometer when recording aurora emissions. Log the exposure interval beside each reading so later comparisons retain their context.
```

The result is a pointer, not a copied document. Pass its locator to `expand` to
read the evidence:

```sh
recall expand 'docs:guides/aurora-guide.md#L5-L8' --detail full
```

```text
provenance guides/aurora-guide.md:L5-L8  revision git:ce483d42fc05+fs:99b49f5416f3

### Instrument notes

Use a calibrated spectrometer when recording aurora emissions. Log the exposure
interval beside each reading so later comparisons retain their context.
```

## Check the installation

```sh
recall doctor
```

A healthy first configuration reports:

```text
status ok  profile default

configuration  pass
  /Users/you/.config/recall/config.toml (user)

trust_boundary pass
  no project configuration; every adapter command came from user configuration

identity       pass
  1 sources, 1 distinct identities

access         pass
  1 of 1 eligible sources name a local path

health         pass
  1 of 1 eligible sources answered a health probe

serving        pass
  1 of 1 probed sources are serving their whole corpus

store_isolation pass
  no probed source claims a store exclusively; nothing to compare

freshness      pass
  1 probed sources serve the freshness mode they were configured with

lineage        pass
  0 declared source-level derivation edges

abstention     skipped
  no evaluation.must_abstain queries are configured
```

`abstention` is skipped here because this first configuration has no
`evaluation.must_abstain` queries; it is not a failed health check.

Recall abstains when the sources answered successfully but nothing matched:

```sh
recall query "submarine ballast pressure"
echo $?
```

```text
outcome abstained  coverage complete  elapsed 1ms

results: none
2
```

Exit `2` is different from an error. It means Recall searched a complete,
answering source set and can support “nothing matched.” Exit `3` means coverage
was degraded, and exit `4` means every source failed; neither supports that
claim.

To index your own Markdown, replace the synthetic path deliberately:

```sh
recall init --force --docs "$HOME/Documents"
recall refresh --source docs
recall query "your first question"
```

See [the profile example](profile-example.md) when you are ready to add Tasks,
`td`, or an external adapter.
