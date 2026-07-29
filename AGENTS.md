# Working with Recall

Recall searches user-controlled sources such as documents, task stores, and
project catalogs, then fuses their ranked results into one evidence list. It
reports incomplete coverage instead of turning a failed source into a false
“nothing found.”

## Recall or grep?

Use `grep` or `rg` when the target is one known file tree and the exact text is
what matters. Use Recall when the answer may live in more than one configured
source, when a source has its own search semantics, or when you need to know
whether every eligible source answered.

## The query loop

Start with pointer output:

```sh
recall query "what did we decide about ranking"
```

Each result gives a locator, title, and excerpt. Choose a locator and retrieve
the evidence you need:

```sh
recall expand 'docs:decisions/ranking.md#L18-L31' --detail full
```

Do not treat the pointer excerpt as the whole record. `recall expand` is the
step that reads the cited evidence.

Add `--explain` when you are debugging retrieval: it includes provenance,
lineage, score explanations, per-source outcomes, and the resolved plan.
Default pointer output is better when you only need to choose evidence.

For programs and agents:

```sh
recall query "ranking decision" --json
recall query "ranking decision" --json --explain
```

Ask for `--json` when you need compact, typed pointers to select and expand.
Ask for `--json --explain` when you need to audit why results ranked, which
sources answered, or what the retrieval plan did. `--json` chooses the
encoding; `--explain` chooses the diagnostic tier.

## Exit codes

- `0` — answered: results were returned and every eligible source answered.
- `1` — error: the command could not run because of usage, configuration, or
  another command-level failure.
- `2` — abstained: nothing matched and at least one source answered.
- `3` — degraded: at least one eligible source could not answer, so any result
  came from incomplete coverage.
- `4` — failed: every source asked failed, so Recall cannot support a claim
  that nothing matched.

Exit `2` is a supported statement about the searched corpus. Exits `3` and `4`
are coverage failures; do not report them as “no results.” Exit `1` means no
corpus claim was made at all.
