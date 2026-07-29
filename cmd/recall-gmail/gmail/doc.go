// Package gmail implements Recall's first-party Gmail adapter.
//
// The adapter reads Gmail through gog rather than owning OAuth credentials or
// importing a Google API client. That boundary keeps authentication in one
// user-controlled tool and gives this package one deliberately small surface:
// search threads, expand one thread, and probe the configured account.
//
// Search is intentionally narrower than a mailbox mirror. Its default corpus
// excludes Spam, Trash, Chats, Promotions, Social, and Forums while retaining
// Personal, Updates, sent, and uncategorized mail. The scope is configurable
// and is reported in every search and health diagnostic. Boundaries inside the
// configured corpus, such as pagination, browsing, and an optional recency
// limit, report partial coverage.
//
// Candidate previews contain sender, subject, and selected labels only. They
// never contain Gmail snippets or bodies. Expansion asks gog to sanitize the
// message, then independently strips controls, bidirectional overrides, forged
// line structure, and URLs before returning evidence. Credential-shaped mail
// raises from confidential to restricted sensitivity.
package gmail
