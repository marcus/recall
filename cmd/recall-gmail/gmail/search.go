package gmail

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/marcus/recall/pkg/adapter"
	"github.com/marcus/recall/pkg/protocol"
	"github.com/marcus/recall/pkg/recall"
)

var (
	credentialPattern = regexp.MustCompile(`(?i)\b(` +
		`sign[- ]?in|log[- ]?in link|magic link|verification code|verify your|` +
		`one[- ]?time (code|password|passcode)|security code|password reset|` +
		`reset your password|confirm your (email|account)|two[- ]?factor|2fa|otp` +
		`)\b`)
	operatorPattern = regexp.MustCompile(`(?i)^-?(?:` +
		`from|to|cc|bcc|subject|label|category|is|in|has|list|filename|` +
		`deliveredto|after|before|older|newer|older_than|newer_than|` +
		`size|larger|smaller|rfc822msgid):`)
	explicitBulkPattern = regexp.MustCompile(`(?i)^category:(?:promotions|social|forums)\b`)
)

var noiseCategories = map[string]bool{
	"CATEGORY_PROMOTIONS": true,
	"CATEGORY_SOCIAL":     true,
	"CATEGORY_FORUMS":     true,
}

var automatedCategories = map[string]bool{
	"CATEGORY_PROMOTIONS": true,
	"CATEGORY_SOCIAL":     true,
	"CATEGORY_FORUMS":     true,
	"CATEGORY_UPDATES":    true,
}

func (a *Adapter) Search(ctx context.Context, req recall.SearchRequest) (recall.SearchResponse, error) {
	set, sourceID, _, runner, err := a.session()
	if err != nil {
		return adapter.FailedSearch(err), err
	}
	if req.AsOf != nil {
		err := protocol.Errorf(protocol.CodeAsOfUnsupported,
			"gmail: current mailbox state cannot answer an as_of query")
		return adapter.FailedSearch(err), err
	}
	if resp, skipped := adapter.UnsupportedFilters(req.Filters, "entities", "project"); skipped {
		return resp, nil
	}
	if !wantsMessages(req.Filters.RecordTypes) {
		return recall.SearchResponse{
			Candidates: []recall.Candidate{},
			Outcome:    recall.SearchSkipped,
			Reason:     recall.SkipRecordTypeMismatch,
			Diagnostics: map[string]any{
				"reason": "this source holds only mail",
			},
		}, nil
	}
	if err := stall(ctx, set.DebugStall); err != nil {
		return adapter.FailedSearch(err), err
	}

	started := a.now(runner)
	queryText := sanitizeLine(req.Query)
	pieces := shellPieces(queryText)
	terms := lexicalTerms(pieces)
	structured := hasOperator(pieces)
	explicitBulk := hasExplicitBulk(pieces)
	query, narrowings := buildQuery(queryText, req.Filters, set)

	limit := set.MaxCandidates
	if req.Limit > 0 && req.Limit < limit {
		limit = req.Limit
	}
	var payload searchPayload
	if err := runner.Run(ctx, "gmail-search",
		[]string{"gmail", "search", "--max", fmt.Sprint(limit), "-z", "UTC"},
		query, &payload); err != nil {
		return adapter.FailedSearch(err), err
	}
	now := a.now(runner)
	a.noteSuccess(now)

	type row struct {
		thread thread
		exact  bool
		order  int
	}
	rows := make([]row, 0, len(payload.Threads))
	anyExact := false
	unknownDates := 0
	impreciseDates := 0
	for i, t := range payload.Threads {
		if req.Filters.Since != nil || req.Filters.Until != nil {
			sent, precision, ok := threadTimePrecision(t.Date)
			if !ok {
				unknownDates++
				continue
			}
			uncertain := false
			if req.Filters.Since != nil {
				if precision > 0 && !sent.Add(precision).After(*req.Filters.Since) {
					continue
				}
				if sent.Before(*req.Filters.Since) {
					uncertain = true
				}
			}
			if req.Filters.Until != nil {
				if sent.After(*req.Filters.Until) {
					continue
				}
				if precision > 0 && sent.Add(precision).After(*req.Filters.Until) {
					uncertain = true
				}
			}
			if uncertain {
				impreciseDates++
			}
		}
		exact := exactHit(t.ID, terms)
		anyExact = anyExact || exact
		rows = append(rows, row{thread: t, exact: exact, order: i})
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].exact != rows[j].exact {
			return rows[i].exact
		}
		return rows[i].order < rows[j].order
	})

	if payload.NextPageToken != "" {
		narrowings = append(narrowings,
			fmt.Sprintf("Gmail had more matches than the %d this search asked for", limit))
	}
	if unknownDates > 0 {
		narrowings = append(narrowings,
			fmt.Sprintf("%d matching threads had no usable date for the requested time filter", unknownDates))
	}
	if impreciseDates > 0 {
		narrowings = append(narrowings,
			fmt.Sprintf("%d matching threads had only minute- or day-resolution dates at the requested time boundary and were retained conservatively", impreciseDates))
	}
	outcome := recall.SearchSuccess
	if len(narrowings) > 0 {
		outcome = recall.SearchPartial
	}

	candidates := make([]recall.Candidate, 0, len(rows))
	for i, row := range rows {
		candidate := makeCandidate(sourceID, row.thread, i+1, row.exact, terms, explicitBulk, now)
		if outcome != recall.SearchSuccess {
			candidate.ConfirmedAt = nil
		}
		candidates = append(candidates, candidate)
	}

	diag := map[string]any{
		"query_mode":  string(queryMode(terms, anyExact, req.Filters, structured)),
		"terms":       len(terms),
		"scope_query": sanitizeLine(set.ScopeQuery),
		"gmail_query": sanitizeLine(query),
		"threads":     len(payload.Threads),
		"truncated":   payload.NextPageToken != "",
		"transport":   runner.Kind(),
		"elapsed_ms":  a.now(runner).Sub(started).Milliseconds(),
	}
	if len(narrowings) > 0 {
		diag["coverage_reason"] = strings.Join(narrowings, "; ")
	}

	return recall.SearchResponse{
		Candidates:      candidates,
		Diagnostics:     diag,
		SourceWatermark: watermark(query, payload.Threads, payload.NextPageToken != ""),
		Outcome:         outcome,
	}, nil
}

func buildQuery(queryText string, filters recall.Filters, set Settings) (string, []string) {
	var parts, narrowings []string
	if scope := sanitizeLine(set.ScopeQuery); scope != "" {
		parts = append(parts, scope)
	}
	if queryText != "" {
		parts = append(parts, queryText)
	} else if browse := sanitizeLine(set.BrowseQuery); browse != "" {
		parts = append(parts, browse)
		narrowings = append(narrowings,
			fmt.Sprintf("an empty query means `%s` for this instance, not the whole mailbox", browse))
	}
	if set.NewerThanDays > 0 {
		parts = append(parts, fmt.Sprintf("newer_than:%dd", set.NewerThanDays))
		narrowings = append(narrowings,
			fmt.Sprintf("this source reads only the last %d days", set.NewerThanDays))
	}
	if filters.Since != nil {
		parts = append(parts, "after:"+filters.Since.AddDate(0, 0, -1).UTC().Format("2006/01/02"))
	}
	if filters.Until != nil {
		parts = append(parts, "before:"+filters.Until.AddDate(0, 0, 1).UTC().Format("2006/01/02"))
	}
	return strings.TrimSpace(strings.Join(parts, " ")), narrowings
}

func makeCandidate(sourceID string, t thread, rank int, exact bool, terms []string, explicitBulk bool, now time.Time) recall.Candidate {
	id := sanitizeLine(t.ID)
	rawSender := sanitizeLine(t.From)
	rawSubject := sanitizeLine(t.Subject)
	sender := stripURLs(rawSender)
	subject := stripURLs(rawSubject)
	if subject == "" {
		subject = "(no subject)"
	}
	labels := cleanLabels(t.Labels)
	count := t.MessageCount
	if count <= 0 {
		count = 1
	}

	signals := []recall.MatchSignal{recall.MatchLexical}
	switch {
	case exact:
		signals = []recall.MatchSignal{recall.MatchExactIdentifier}
	case len(terms) == 0:
		signals = []recall.MatchSignal{recall.MatchField}
	}
	local := headerCoverage(sender, subject, terms)
	candidate := recall.Candidate{
		CandidateID:        id,
		SourceRecordID:     id,
		Locator:            recall.Locator{SourceID: sourceID, Local: "thread/" + id},
		RecordType:         recall.RecordMessage,
		Title:              subject,
		Excerpt:            clipPreview(preview(sender, subject, labels, count)),
		LocalRank:          rank,
		LocalScore:         &local,
		MatchSignals:       signals,
		ObservedAt:         ptrTime(now),
		ConfirmedAt:        ptrTime(now),
		SourceRevision:     sourceRevision(count, valueOrDash(t.Date)),
		Sensitivity:        sensitivityOf(rawSubject, rawSender),
		Metadata:           metadata(sender, subject, labels, count, t.Date),
		ContentFingerprint: fingerprint(id, fmt.Sprint(count), t.Date),
	}
	if !exact {
		candidate.Relevance = headerRelevance(sender, subject, labels, terms, explicitBulk)
	}
	if sent, ok := threadTime(t.Date); ok {
		candidate.EventTime = &sent
	}
	return candidate
}

func wantsMessages(types []recall.RecordType) bool {
	if len(types) == 0 {
		return true
	}
	for _, kind := range types {
		if kind == recall.RecordMessage {
			return true
		}
	}
	return false
}

func cleanLabels(labels []string) []string {
	out := make([]string, 0, len(labels))
	for _, label := range labels {
		if label = sanitizeLine(label); label != "" {
			out = append(out, label)
		}
	}
	return out
}

func preview(sender, subject string, labels []string, count int) string {
	parts := make([]string, 0, 3)
	if sender != "" {
		parts = append(parts, sender)
	}
	if subject != "" {
		parts = append(parts, subject)
	}
	var state []string
	for _, label := range labels {
		switch label {
		case "UNREAD", "IMPORTANT", "STARRED", "INBOX":
			state = append(state, strings.ToLower(label))
		}
	}
	if count > 1 {
		state = append(state, fmt.Sprintf("%d messages", count))
	}
	if len(state) > 0 {
		parts = append(parts, strings.Join(state, ", "))
	}
	return strings.Join(parts, " · ")
}

func metadata(sender, subject string, labels []string, count int, sent string) map[string]any {
	md := map[string]any{
		"from":          sender,
		"subject":       subject,
		"labels":        labels,
		"message_count": count,
		"unread":        contains(labels, "UNREAD"),
		"important":     contains(labels, "IMPORTANT"),
	}
	for _, label := range labels {
		if strings.HasPrefix(label, "CATEGORY_") {
			md["category"] = label
			break
		}
	}
	if sent = sanitizeLine(sent); sent != "" {
		md["sent"] = sent
	}
	automated := false
	for _, label := range labels {
		automated = automated || automatedCategories[label]
	}
	md["needs_reply_hint"] = contains(labels, "INBOX") &&
		contains(labels, "UNREAD") && !automated
	return md
}

func sensitivityOf(subject, sender string) recall.Sensitivity {
	if credentialPattern.MatchString(subject) || credentialPattern.MatchString(sender) ||
		containsURL(subject) || containsURL(sender) {
		return recall.SensitivityRestricted
	}
	return recall.SensitivityConfidential
}

func headerCoverage(sender, subject string, terms []string) float64 {
	if len(terms) == 0 {
		return 0
	}
	haystack := make(map[string]bool)
	for _, term := range tokenize(sender + " " + subject) {
		haystack[term] = true
	}
	covered := 0
	for _, term := range terms {
		if haystack[term] {
			covered++
		}
	}
	return float64(covered) / float64(len(terms))
}

func headerRelevance(sender, subject string, labels, terms []string, explicitBulk bool) *float64 {
	if len(terms) == 0 {
		return nil
	}
	header := tokenize(sender + " " + subject)
	counts := make(map[string]int)
	for _, term := range header {
		counts[term]++
	}
	covered, hits := 0, 0
	for _, term := range terms {
		if counts[term] > 0 {
			covered++
			hits += counts[term]
		}
	}
	if covered == 0 {
		if !explicitBulk {
			for _, label := range labels {
				if noiseCategories[label] {
					zero := 0.0
					return &zero
				}
			}
		}
		return nil
	}
	value := recall.Relevance(covered, len(terms), hits, len(header))
	return &value
}

func shellPieces(query string) []string {
	var (
		out     []string
		current strings.Builder
		quote   rune
		escaped bool
	)
	flush := func() {
		if current.Len() > 0 {
			out = append(out, current.String())
			current.Reset()
		}
	}
	for _, r := range query {
		switch {
		case escaped:
			current.WriteRune(r)
			escaped = false
		case r == '\\':
			escaped = true
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				current.WriteRune(r)
			}
		case r == '\'' || r == '"':
			quote = r
		case r == ' ' || r == '\t' || r == '\n':
			flush()
		default:
			current.WriteRune(r)
		}
	}
	if escaped {
		current.WriteRune('\\')
	}
	flush()
	return out
}

func lexicalTerms(pieces []string) []string {
	var terms []string
	seen := make(map[string]bool)
	for _, piece := range pieces {
		operand := strings.Trim(piece, "{}()")
		if operand == "" || strings.EqualFold(operand, "AND") || strings.EqualFold(operand, "OR") {
			continue
		}
		if operatorPattern.MatchString(operand) {
			continue
		}
		if strings.HasPrefix(operand, "-") {
			continue
		}
		for _, term := range tokenize(operand) {
			if !seen[term] {
				seen[term] = true
				terms = append(terms, term)
			}
		}
	}
	return terms
}

func hasOperator(pieces []string) bool {
	for _, piece := range pieces {
		if operatorPattern.MatchString(strings.Trim(piece, "{}()")) {
			return true
		}
	}
	return false
}

func hasExplicitBulk(pieces []string) bool {
	for _, piece := range pieces {
		if explicitBulkPattern.MatchString(strings.Trim(piece, "{}()")) {
			return true
		}
	}
	return false
}

func exactHit(id string, terms []string) bool {
	id = strings.ToLower(id)
	for _, term := range terms {
		if term == id && id != "" {
			return true
		}
	}
	return false
}

func queryMode(terms []string, exact bool, filters recall.Filters, structured bool) recall.QueryMode {
	switch {
	case exact:
		return recall.QueryExact
	case structured:
		return recall.QueryStructured
	case len(terms) > 0:
		return recall.QueryLexical
	case filters.Since != nil || filters.Until != nil:
		return recall.QueryTemporal
	default:
		return recall.QueryStructured
	}
}

func threadTime(raw string) (time.Time, bool) {
	got, _, ok := threadTimePrecision(raw)
	return got, ok
}

func threadTimePrecision(raw string) (time.Time, time.Duration, bool) {
	raw = strings.TrimSpace(raw)
	for _, candidate := range []struct {
		layout    string
		precision time.Duration
	}{
		{"2006-01-02 15:04:05", 0},
		{"2006-01-02 15:04", time.Minute},
		{"2006-01-02", 24 * time.Hour},
	} {
		if got, err := time.ParseInLocation(candidate.layout, raw, time.UTC); err == nil {
			return got.UTC(), candidate.precision, true
		}
	}
	return time.Time{}, 0, false
}

func watermark(query string, threads []thread, truncated bool) string {
	dates := make([]string, 0, len(threads))
	for _, thread := range threads {
		if thread.Date != "" {
			dates = append(dates, thread.Date)
		}
	}
	sort.Strings(dates)
	newest, oldest := "-", "-"
	if len(dates) > 0 {
		oldest, newest = dates[0], dates[len(dates)-1]
	}
	digest := strings.TrimPrefix(fingerprint(query), "sha256:")
	if len(digest) > 12 {
		digest = digest[:12]
	}
	value := fmt.Sprintf("q=%s threads=%d newest=%s oldest=%s",
		digest, len(threads), newest, oldest)
	if truncated {
		value += " truncated=1"
	}
	return value
}

func contains(items []string, want string) bool {
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}

func ptrTime(value time.Time) *time.Time {
	value = value.UTC()
	return &value
}

func valueOrDash(value string) string {
	if value == "" {
		return "-"
	}
	return value
}
