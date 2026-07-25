`signals.jsonl` is a DIRECTORY in this fixture, deliberately.

The case has to record a store that exists and cannot be read, and the two
alternatives do not say that. A missing signals.jsonl means Clara has not
written the store yet, which is an empty store and complete coverage; a
permission bit is not preserved by a checkout. A path of the right name and the
wrong type opens and then fails to read, on every platform, from a checkout, for
ever.
