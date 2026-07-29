# Gmail adapter

`recall-gmail` is Recall's optional, first-party Gmail adapter. It reads one
Gmail account live through [`gog`](https://github.com/openclaw/gogcli), speaks
Recall's external adapter protocol over stdio, and is installed with the rest
of Recall's commands.

It is intentionally read-only. The adapter accepts only these `gog` commands:

```text
gog auth list
gog gmail search
gog gmail thread get
```

Every invocation also carries `--gmail-no-send` and `--no-input`. Queries are
argv operands rather than shell text, so Gmail syntax such as `-from:me` cannot
become a process flag or a shell command.

## Install and authorize gog

Install `gog` separately:

```sh
brew install openclaw/tap/gogcli
gog --version
```

Follow gog's setup to register a Google Desktop OAuth client, then authorize
only read access to Gmail:

```sh
gog auth credentials ~/Downloads/client_secret_....json
gog auth add you@gmail.com --services gmail --readonly
gog auth doctor --check
```

OAuth client JSON, refresh tokens, and keyring passwords are secrets. Do not
put them in Recall configuration or commit them to a repository. Recall passes
only the account address to `gog`; `gog` owns credential storage.

## Configure Recall

External adapter commands are trusted user configuration. Put the registration
in `$XDG_CONFIG_HOME/recall/config.toml`, normally
`~/.config/recall/config.toml`:

```toml
[adapters.gmail]
command = "recall-gmail"
freshness_modes = ["live"]
conformance = "/absolute/path/to/recall-gmail-conformance"

[[sources]]
# Replace this synthetic example with a unique value once, then never change it.
# `recall init` demonstrates the same immutable identity on its generated source.
source_uid = "01EXAMPLEGMAIL00"
source_id = "mail"
adapter = "gmail"
location = "you@gmail.com"
location_kind = "opaque"
freshness_mode = "live"
sensitivity = "confidential"
timeout_ms = 20000

[sources.settings]
max_candidates = 25
```

The conformance path is optional for ordinary use. When declared, it must be
absolute and lets Recall replay the adapter's committed protocol suite. In a
source checkout, use `cmd/recall-gmail/conformance`. Homebrew installs it at
`$(brew --prefix)/share/recall/gmail-conformance`; release archives carry the
same checkout-relative directory.

Every source also needs its own immutable `source_uid`. Generate it once, keep
it when renaming or editing the source, and never copy the documentation
placeholder into a real configuration.

```sh
recall doctor --conformance gmail
```

Add `mail` to a profile whose sensitivity ceiling is at least
`confidential`. Keeping personal mail out of a broad default profile makes
mail access an explicit caller decision:

```toml
[profiles.personal]
sources = ["docs", "tasks", "mail"]
sensitivity_ceiling = "confidential"
```

Check the resolved trust boundary and credential before querying:

```sh
recall config explain
recall sources --profile personal
recall query "dentist referral" --profile personal
```

## Default corpus and precision escape hatch

The default `scope_query` is:

```text
-in:spam -in:trash -in:chats -category:promotions -category:social -category:forums
```

Personal, Updates, sent, and uncategorized mail remain searchable. Promotions,
Social, and Forums stay out of general federated queries because body matches
inside bulk mail are a common source of unrelated names and terms.

This is the source's declared corpus, not a hidden per-query filter, and every
health and search diagnostic reports it. Override it when a separate Recall
source genuinely needs a different corpus:

```toml
[sources.settings]
scope_query = "-in:spam -in:trash -in:chats"
```

For a one-off search that deliberately needs bulk mail, use gog directly
instead of adding a permanently noisy second Recall source:

```sh
gog --account you@gmail.com gmail search \
  'bonnie category:promotions' --max 20
```

## Pointer and expansion safety

Search candidates contain only:

- Gmail thread ID
- sender
- subject
- selected labels and message count
- small typed metadata

They never contain Gmail's snippet or a message body. Snippets routinely hold
sign-in and unsubscribe URLs and are too easy to log or copy from a pointer.

Expansion invokes `gog gmail thread get --sanitize-content --full`. Recall then
sanitizes the returned text again: controls and bidirectional overrides are
removed, suspicious line structure is bounded, and URLs are replaced with
`[url removed]`. This deliberately trades clickable links for safer evidence.

Subjects shaped like sign-in links, verification codes, one-time passwords,
or password resets raise the candidate from `confidential` to `restricted`.
A profile capped at `confidential` suppresses those candidates and reports the
suppression without placing a live credential in context.

## Ranking and coverage

Gmail's result order is preserved. Recall promotes only an exact thread-ID
match. Relevance is measured over safe sender and subject fields:

- A sender or subject match uses Recall's shared relevance formula.
- An ordinary body-only match makes no relevance claim because the pointer
  cannot safely inspect enough body text to measure aboutness.
- A body-only result in Promotions, Social, or Forums receives relevance zero,
  unless the query explicitly selected that bulk category.

`scope_query` defines the source, so it does not make every search partial.
Boundaries inside that corpus do:

- an empty query answered through `browse_query`;
- a nonzero `newer_than_days`;
- Gmail returning a next-page token;
- a matching thread whose timestamp cannot be checked against requested time
  filters.

These return `outcome: partial` and name the reason. A failed `gog` invocation
returns unavailable, an unusable credential returns denied, and neither can
become an empty successful answer.

## Settings

| Setting | Default | Meaning |
|---|---:|---|
| `gog_binary` | `gog` | Executable path or name |
| `timeout_ms` | `15000` | Maximum duration of one gog call |
| `max_candidates` | `25` | Candidate cap before Recall fusion |
| `scope_query` | excludes Spam, Trash, Chats, Promotions, Social, Forums | Source corpus |
| `browse_query` | unread Primary inbox mail from the last 14 days, excluding sent mail | Empty-query behavior |
| `newer_than_days` | `0` | Optional recency boundary; zero is unbounded |

`replay` and `debug_stall_ms` exist for the recorded conformance suite and
should not be set in a live source.
