package db

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

const (
	DefaultSearchLimit = 50
	MaxSearchLimit     = 500
)

// SystemMsgPrefixes lists non-goal content prefixes that identify
// system-injected user messages. These are excluded from search results even
// when the is_system column has not been backfilled (e.g. Claude sessions
// parsed before schema version 2). Keep in sync with the frontend list in
// frontend/src/lib/utils/messages.ts.
var SystemMsgPrefixes = []string{
	"This session is being continued",
	"[Request interrupted",
	"<task-notification>",
	"<command-message>",
	"<command-name>",
	"<local-command-",
	"Stop hook feedback:",
}

const (
	systemReminderOpenTag  = "<system-reminder>"
	systemReminderCloseTag = "</system-reminder>"
)

const (
	legacyGoalContextPrefix        = "<goal_context>"
	codexInternalContextTagPrefix  = "<codex_internal_context"
	goalContextSourceAttr          = `source="goal"`
	goalContextSourceAttrSQLPrefix = ` source="goal"`
)

var goalContextSourceAttrRe = regexp.MustCompile(`(?:^|\s)` +
	regexp.QuoteMeta(goalContextSourceAttr) + `(?:\s|/|$)`)

// IsGoalContextPrefixed reports whether a user-role message is a legacy
// Codex /goal continuation wrapper that may already be stored in older
// archives or read-only stores.
func IsGoalContextPrefixed(content, role string) bool {
	if role != "user" {
		return false
	}
	trimmed := strings.TrimLeft(content, systemPrefixTrimCutset)
	if strings.HasPrefix(trimmed, legacyGoalContextPrefix) {
		return true
	}
	if strings.HasPrefix(trimmed, codexInternalContextTagPrefix) {
		openTag, _, ok := strings.Cut(trimmed, ">")
		return ok && goalContextSourceAttrRe.MatchString(openTag)
	}
	return false
}

type systemPrefixSQLDialect int

const (
	systemPrefixSQLite systemPrefixSQLDialect = iota
	systemPrefixPostgres
	systemPrefixDuckDB
)

// SystemPrefixSQL returns a SQL clause that excludes user messages
// matching any system prefix. The column alias for content must be passed
// (e.g. "m.content" or "m2.content"). Uses case-sensitive substr and
// position checks instead of LIKE, which is case-insensitive on SQLite.
func SystemPrefixSQL(contentCol, roleCol string) string {
	return systemPrefixSQL(contentCol, roleCol, systemPrefixSQLite)
}

// PostgresSystemPrefixSQL is the PostgreSQL form of SystemPrefixSQL.
func PostgresSystemPrefixSQL(contentCol, roleCol string) string {
	return systemPrefixSQL(contentCol, roleCol, systemPrefixPostgres)
}

// DuckDBSystemPrefixSQL is the DuckDB form of SystemPrefixSQL.
func DuckDBSystemPrefixSQL(contentCol, roleCol string) string {
	return systemPrefixSQL(contentCol, roleCol, systemPrefixDuckDB)
}

func systemPrefixSQL(
	contentCol, roleCol string, dialect systemPrefixSQLDialect,
) string {
	// LTRIM strips the same whitespace as Go's strings.TrimSpace,
	// JS .trim(), and the parser's isSystem helpers: ASCII whitespace,
	// BOM (U+FEFF), and Unicode
	// spaces (U+0085, U+00A0, U+1680, U+2000–U+200A, U+2028,
	// U+2029, U+202F, U+205F, U+3000). SQLite, PostgreSQL, and DuckDB
	// handle multi-byte UTF-8 characters in the trim set correctly.
	trimmed := systemPrefixSQLTrimmed(contentCol)
	parts := make([]string, 0, len(SystemMsgPrefixes)+1)
	for _, p := range SystemMsgPrefixes {
		parts = append(parts, fmt.Sprintf(
			"substr(%s, 1, %d) = '%s'", trimmed, len(p), p,
		))
	}
	parts = append(parts, systemReminderTerminalSQL(trimmed, dialect))
	parts = append(parts, goalContextPrefixSQL(trimmed, dialect))
	guard := ""
	if dialect == systemPrefixSQLite {
		guard = systemPrefixFirstCPGuardSQL(contentCol) + " AND "
	}
	return "NOT (" + roleCol + " = 'user' AND " + guard + "(" +
		strings.Join(parts, " OR ") + "))"
}

// systemPrefixFirstCPGuardSQL builds a cheap prefilter implied by every
// prefix branch of systemPrefixSQL: for any branch to match, the raw
// content's first code point must be a trimmable whitespace character or the
// first character of one of the known prefixes. unicode() returns the first
// code point as an integer (NULL for empty content, COALESCEd to 0, which is
// never in the set), so rows with ordinary content skip the repeated
// LTRIM/prefix chain after one integer IN test. The guard is AND'ed inside
// the NOT(...), so a false guard reproduces exactly the all-branches-false
// result. SQLite-only for now: PG (ascii) and DuckDB (unicode) analogues
// need their own empty-string audits before the other dialects adopt it.
func systemPrefixFirstCPGuardSQL(contentCol string) string {
	seen := make(map[rune]bool)
	var cps []int
	add := func(r rune) {
		if !seen[r] {
			seen[r] = true
			cps = append(cps, int(r))
		}
	}
	for _, p := range SystemMsgPrefixes {
		add([]rune(p)[0])
	}
	add([]rune(legacyGoalContextPrefix)[0])
	add([]rune(codexInternalContextTagPrefix)[0])
	for _, r := range systemPrefixTrimCutset {
		add(r)
	}
	sort.Ints(cps)
	items := make([]string, len(cps))
	for i, cp := range cps {
		items[i] = strconv.Itoa(cp)
	}
	return "COALESCE(unicode(" + contentCol + "), 0) IN (" +
		strings.Join(items, ", ") + ")"
}

func systemPrefixSQLTrimmed(contentCol string) string {
	return "LTRIM(" + contentCol + ", ' \t\n\v\f\r" +
		"\u0085\u00A0\u1680" +
		"\u2000\u2001\u2002\u2003\u2004\u2005\u2006\u2007\u2008\u2009\u200A" +
		"\u2028\u2029\u202F\u205F\u3000\uFEFF')"
}

func systemReminderTerminalSQL(
	trimmed string, dialect systemPrefixSQLDialect,
) string {
	closePos := sqlPosition(dialect, systemReminderCloseTag, "rest")
	next := systemPrefixSQLTrimmed(fmt.Sprintf(
		"substr(rest, (%s) + %d)", closePos, len(systemReminderCloseTag),
	))
	terminal := fmt.Sprintf(
		"(substr(rest, 1, %d) <> '%s' OR %s = 0)",
		len(systemReminderOpenTag), systemReminderOpenTag, closePos,
	)
	classification := terminalRemainderSQL("rest", dialect)
	seedReminder := fmt.Sprintf(
		"substr(%s, 1, %d) = '%s'",
		trimmed, len(systemReminderOpenTag), systemReminderOpenTag,
	)
	return fmt.Sprintf(`(%s AND EXISTS (WITH RECURSIVE reminder_remainder(rest) AS (
SELECT %s
UNION ALL
SELECT %s FROM reminder_remainder
WHERE substr(rest, 1, %d) = '%s' AND %s > 0
)
SELECT 1 FROM reminder_remainder
WHERE %s AND (rest = '' OR %s)
LIMIT 1))`, seedReminder, trimmed, next,
		len(systemReminderOpenTag), systemReminderOpenTag, closePos,
		terminal, classification)
}

func terminalRemainderSQL(content string, dialect systemPrefixSQLDialect) string {
	parts := make([]string, 0, len(SystemMsgPrefixes)+1)
	for _, p := range SystemMsgPrefixes {
		parts = append(parts, fmt.Sprintf(
			"substr(%s, 1, %d) = '%s'", content, len(p), p,
		))
	}
	parts = append(parts, goalContextPrefixSQL(content, dialect))
	return strings.Join(parts, " OR ")
}

func goalContextPrefixSQL(trimmed string, dialect systemPrefixSQLDialect) string {
	legacy := fmt.Sprintf("substr(%s, 1, %d) = '%s'",
		trimmed, len(legacyGoalContextPrefix), legacyGoalContextPrefix)
	current := fmt.Sprintf(
		"(substr(%[1]s, 1, %[2]d) = '%[3]s' AND %[4]s)",
		trimmed, len(codexInternalContextTagPrefix),
		codexInternalContextTagPrefix,
		goalContextSourceAttrSQL(openingTagSQL(trimmed, dialect), dialect),
	)
	return "(" + legacy + " OR " + current + ")"
}

func openingTagSQL(trimmed string, dialect systemPrefixSQLDialect) string {
	return fmt.Sprintf("substr(%s, 1, %s)",
		trimmed, sqlPosition(dialect, ">", trimmed))
}

func goalContextSourceAttrSQL(
	openTag string, dialect systemPrefixSQLDialect,
) string {
	normalized := openTag
	for _, ws := range []string{"\t", "\n", "\v", "\f", "\r"} {
		normalized = fmt.Sprintf("replace(%s, '%s', ' ')", normalized, ws)
	}
	checks := []string{
		sqlContains(dialect, normalized, goalContextSourceAttrSQLPrefix+" "),
		sqlContains(dialect, normalized, goalContextSourceAttrSQLPrefix+">"),
		sqlContains(dialect, normalized, goalContextSourceAttrSQLPrefix+"/>"),
	}
	return "(" + strings.Join(checks, " OR ") + ")"
}

func sqlContains(
	dialect systemPrefixSQLDialect, haystack, needle string,
) string {
	return sqlPosition(dialect, needle, haystack) + " > 0"
}

func sqlPosition(
	dialect systemPrefixSQLDialect, needle, haystack string,
) string {
	quotedNeedle := "'" + needle + "'"
	if dialect == systemPrefixPostgres {
		return fmt.Sprintf("POSITION(%s IN %s)", quotedNeedle, haystack)
	}
	return fmt.Sprintf("instr(%s, %s)", haystack, quotedNeedle)
}

// systemPrefixTrimCutset is the leading-whitespace set SystemPrefixSQL's
// LTRIM strips: ASCII whitespace, BOM, and the Unicode spaces. Kept
// identical so the Go and SQL system-prefix checks agree.
const systemPrefixTrimCutset = " \t\n\v\f\r" +
	"\u0085\u00A0\u1680" +
	"\u2000\u2001\u2002\u2003\u2004\u2005\u2006\u2007\u2008\u2009\u200A" +
	"\u2028\u2029\u202F\u205F\u3000\uFEFF"

// IsSystemPrefixed reports whether a user-role message is a system-injected
// message identified by a SystemMsgPrefixes prefix. It is the Go equivalent
// of SystemPrefixSQL for callers that filter in Go rather than SQL: only
// user-role messages match, and leading whitespace is trimmed with the same
// cutset before the case-sensitive prefix comparison.
func IsSystemPrefixed(content, role string) bool {
	if role != "user" {
		return false
	}
	trimmed := strings.TrimLeft(content, systemPrefixTrimCutset)
	remainder, stripped := stripLeadingSystemReminderBlocks(trimmed)
	if stripped {
		if remainder == "" {
			return true
		}
		trimmed = remainder
	}
	if IsGoalContextPrefixed(trimmed, role) {
		return true
	}
	for _, p := range SystemMsgPrefixes {
		if strings.HasPrefix(trimmed, p) {
			return true
		}
	}
	return false
}

func stripLeadingSystemReminderBlocks(content string) (string, bool) {
	rest := strings.TrimLeft(content, systemPrefixTrimCutset)
	stripped := false
	for strings.HasPrefix(rest, systemReminderOpenTag) {
		closeIdx := strings.Index(rest, systemReminderCloseTag)
		if closeIdx < 0 {
			return "", false
		}
		rest = strings.TrimLeft(
			rest[closeIdx+len(systemReminderCloseTag):],
			systemPrefixTrimCutset,
		)
		stripped = true
	}
	return rest, stripped
}

// SearchResult holds a session-level match with the best-ranked snippet.
type SearchResult struct {
	SessionID      string  `json:"session_id"`
	Project        string  `json:"project"`
	Agent          string  `json:"agent"`
	Name           string  `json:"name"`
	Ordinal        int     `json:"ordinal"`
	SessionEndedAt string  `json:"session_ended_at"`
	Snippet        string  `json:"snippet"`
	Rank           float64 `json:"rank"`
}

// SearchFilter specifies search parameters.
type SearchFilter struct {
	Query   string
	Project string
	Sort    string // "relevance" (default) or "recency"
	Cursor  int    // offset for pagination
	Limit   int
}

// SearchPage holds paginated search results.
type SearchPage struct {
	Results    []SearchResult `json:"results"`
	NextCursor int            `json:"next_cursor,omitempty"`
}

// Search performs session search across message content (FTS5, with prefix
// matching) and session metadata (id, names, project, agent, paths, tools,
// models, statuses). Unquoted terms are AND'd in any order and may hit
// different fields. Quoted phrases must appear together in order.
//
// Each matching session appears once. Relevance ranking (see SearchRank*
// constants) is the default order; Sort "recency" orders by session time.
func (db *DB) Search(
	ctx context.Context, f SearchFilter,
) (SearchPage, error) {
	if f.Limit <= 0 || f.Limit > MaxSearchLimit {
		f.Limit = DefaultSearchLimit
	}
	terms := SearchTermsFromQuery(f.Query)
	if len(terms) == 0 {
		return SearchPage{}, nil
	}
	var ftsTerms []SearchTerm
	for _, t := range terms {
		if ftsSearchable(t.Value) {
			ftsTerms = append(ftsTerms, t)
		}
	}
	ftsExpr := FTSMatchExpression(ftsTerms)
	plainQuery := JoinedQuery(terms)
	firstTerm := terms[0].Value
	hasPhrase := 0
	if HasQuotedPhrase(terms) {
		hasPhrase = 1
	}

	orderBy := "relevance ASC, rank ASC, match_pos ASC, julianday(session_ended_at) DESC, s.id ASC"
	if f.Sort == "recency" {
		orderBy = "julianday(session_ended_at) DESC, s.id ASC"
	}

	blobSQL := SessionSearchBlobSQL("s")
	primarySQL := SessionPrimaryBlobSQL("s")
	sysPrefix := SystemPrefixSQL("m2.content", "m2.role")
	visiblePrefix := SystemPrefixSQL("mx.content", "mx.role")

	var b strings.Builder
	b.WriteString("WITH ")
	args := make([]any, 0, 16+len(terms)*8)

	for i, t := range terms {
		if i > 0 {
			b.WriteString(",\n")
		}
		like := SearchLikePattern(t.Value)
		ftsPart := `SELECT NULL AS session_id WHERE 0`
		if ftsSearchable(t.Value) {
			ftsPart = fmt.Sprintf(`SELECT m2.session_id AS session_id
			FROM messages_fts
			JOIN messages m2 ON messages_fts.rowid = m2.id
			JOIN sessions s2 ON m2.session_id = s2.id
			WHERE messages_fts MATCH ?
			  AND s2.deleted_at IS NULL
			  AND m2.is_system = 0
			  AND %s`, sysPrefix)
			args = append(args, FTSMatchExpression([]SearchTerm{t}))
		}
		fmt.Fprintf(&b, `term%d AS (
			%s
			UNION
			SELECT s.id FROM sessions s
			WHERE s.deleted_at IS NULL
			  AND LOWER(%s) LIKE LOWER(?) ESCAPE '\'
			UNION
			SELECT m.session_id FROM messages m
			JOIN sessions s ON s.id = m.session_id
			WHERE s.deleted_at IS NULL
			  AND m.is_system = 0
			  AND %s
			  AND (LOWER(m.content) LIKE LOWER(?) ESCAPE '\'
			    OR LOWER(m.model) LIKE LOWER(?) ESCAPE '\')
			UNION
			SELECT tc.session_id FROM tool_calls tc
			JOIN sessions s ON s.id = tc.session_id
			WHERE s.deleted_at IS NULL
			  AND (
			    LOWER(tc.tool_name) LIKE LOWER(?) ESCAPE '\'
			    OR LOWER(COALESCE(tc.skill_name, '')) LIKE LOWER(?) ESCAPE '\'
			    OR LOWER(COALESCE(tc.file_path, '')) LIKE LOWER(?) ESCAPE '\'
			    OR LOWER(COALESCE(tc.result_content, '')) LIKE LOWER(?) ESCAPE '\'
			  )
		)`, i, ftsPart, blobSQL, SystemPrefixSQL("m.content", "m.role"))
		args = append(args, like, like, like, like, like, like, like)
	}

	b.WriteString(",\nmatched AS (\n")
	for i := range terms {
		if i > 0 {
			b.WriteString(" INTERSECT ")
		}
		fmt.Fprintf(&b, "SELECT session_id FROM term%d", i)
	}
	b.WriteString("\n)")

	projectFTS := ""
	if f.Project != "" {
		projectFTS = "AND s2.project = ?"
	}
	if ftsExpr != "" {
		fmt.Fprintf(&b, `,
		fts_best AS (
			SELECT session_id, best_rowid, best_ordinal, best_rank, best_query
			FROM (
				SELECT m2.session_id,
					messages_fts.rowid AS best_rowid,
					m2.ordinal AS best_ordinal,
					rank AS best_rank,
					? AS best_query,
					ROW_NUMBER() OVER (
						PARTITION BY m2.session_id
						ORDER BY rank ASC, m2.ordinal ASC, messages_fts.rowid ASC
					) AS rn
				FROM messages_fts
				JOIN messages m2 ON messages_fts.rowid = m2.id
				JOIN sessions s2 ON m2.session_id = s2.id
				WHERE messages_fts MATCH ?
				  AND s2.deleted_at IS NULL
				  AND m2.is_system = 0
				  AND %s
				  %s
			)
			WHERE rn = 1
		)`, sysPrefix, projectFTS)
	} else {
		b.WriteString(`,
		fts_best AS (
			SELECT NULL AS session_id, NULL AS best_rowid,
				NULL AS best_ordinal, NULL AS best_rank, NULL AS best_query
			WHERE 0
		)`)
	}

	hitScoreParts := make([]string, len(terms))
	for i := range terms {
		hitScoreParts[i] = "CASE WHEN LOWER(m.content) LIKE LOWER(?) ESCAPE '\\' THEN 1 ELSE 0 END"
	}
	hitWhereParts := make([]string, len(terms))
	for i := range terms {
		hitWhereParts[i] = "LOWER(m.content) LIKE LOWER(?) ESCAPE '\\'"
	}
	fmt.Fprintf(&b, `,
		content_hit AS (
			SELECT session_id, ordinal, content, hit_query
			FROM (
				SELECT m.session_id, m.ordinal, m.content,
					? AS hit_query,
					(%s) AS term_hits,
					ROW_NUMBER() OVER (
						PARTITION BY m.session_id
						ORDER BY (%s) DESC, m.ordinal ASC
					) AS rn
				FROM messages m
				JOIN matched mt ON mt.session_id = m.session_id
				WHERE m.is_system = 0
				  AND %s
				  AND (%s)
			)
			WHERE rn = 1
		)`,
		strings.Join(hitScoreParts, " + "),
		strings.Join(hitScoreParts, " + "),
		SystemPrefixSQL("m.content", "m.role"),
		strings.Join(hitWhereParts, " OR "),
	)

	fmt.Fprintf(&b, `
		SELECT s.id, s.project, s.agent,
			COALESCE(s.display_name, s.session_name, s.first_message, '') AS name,
			COALESCE(s.ended_at, s.started_at, '') AS session_ended_at,
			COALESCE(best.best_ordinal, hit.ordinal, -1) AS ordinal,
			CASE
				WHEN COALESCE(m.content, hit.content) IS NOT NULL THEN CASE
					WHEN instr(LOWER(COALESCE(m.content, hit.content)), LOWER(COALESCE(best.best_query, hit.hit_query))) > 100
						THEN '...' || substr(COALESCE(m.content, hit.content), max(1, instr(LOWER(COALESCE(m.content, hit.content)), LOWER(COALESCE(best.best_query, hit.hit_query))) - 50), 200) || '...'
					ELSE substr(COALESCE(m.content, hit.content), 1, 200) || CASE WHEN length(COALESCE(m.content, hit.content)) > 200 THEN '...' ELSE '' END
				END
				WHEN LOWER(COALESCE(s.display_name, s.session_name, '')) LIKE LOWER(?) ESCAPE '\'
					THEN COALESCE(s.display_name, s.session_name, '')
				WHEN LOWER(COALESCE(s.first_message, '')) LIKE LOWER(?) ESCAPE '\'
					THEN COALESCE(s.first_message, '')
				WHEN LOWER(s.id) LIKE LOWER(?) ESCAPE '\' THEN s.id
				WHEN LOWER(s.project) LIKE LOWER(?) ESCAPE '\' THEN s.project
				WHEN LOWER(s.agent) LIKE LOWER(?) ESCAPE '\' THEN s.agent
				WHEN LOWER(COALESCE(s.git_branch, '')) LIKE LOWER(?) ESCAPE '\' THEN s.git_branch
				WHEN LOWER(COALESCE(s.cwd, '')) LIKE LOWER(?) ESCAPE '\' THEN s.cwd
				ELSE COALESCE(s.display_name, s.session_name, s.first_message, '')
			END AS snippet,
			COALESCE(best.best_rank, 0.0) AS rank,
			CASE
				WHEN best.best_query IS NOT NULL
					THEN instr(LOWER(m.content), LOWER(best.best_query))
				ELSE 0
			END AS match_pos,
			CASE
				WHEN LOWER(COALESCE(s.display_name, s.session_name, '')) = LOWER(?)
					OR LOWER(s.id) = LOWER(?) THEN %d
				WHEN LOWER(s.project) = LOWER(?)
					OR LOWER(s.agent) = LOWER(?)
					OR LOWER(COALESCE(s.agent_label, '')) = LOWER(?) THEN %d
				WHEN best.best_rowid IS NOT NULL AND ? = 1 THEN %d
				WHEN LOWER(COALESCE(s.display_name, s.session_name, '')) LIKE LOWER(?) ESCAPE '\'
					OR LOWER(s.id) LIKE LOWER(?) ESCAPE '\'
					OR LOWER(s.project) LIKE LOWER(?) ESCAPE '\'
					OR LOWER(s.agent) LIKE LOWER(?) ESCAPE '\' THEN %d
				WHEN %s THEN %d
				WHEN best.best_rowid IS NOT NULL THEN %d
				ELSE %d
			END AS relevance
		FROM matched mt
		JOIN sessions s ON s.id = mt.session_id
		LEFT JOIN fts_best best ON best.session_id = s.id
		LEFT JOIN content_hit hit ON hit.session_id = s.id
		LEFT JOIN messages m ON m.id = best.best_rowid
		WHERE s.deleted_at IS NULL
		  AND EXISTS (
			SELECT 1 FROM messages mx
			WHERE mx.session_id = s.id
			  AND mx.is_system = 0
			  AND %s
		  )`,
		SearchRankExactValue, SearchRankExactPrimary, SearchRankExactPhrase,
		SearchRankPrefixPrimary,
		sqliteAllTermsPrimarySQL(len(terms), primarySQL),
		SearchRankAllTermsPrimary, SearchRankAllTermsContent, SearchRankSubstring,
		visiblePrefix,
	)

	if ftsExpr != "" {
		args = append(args, firstTerm, ftsExpr)
		if f.Project != "" {
			args = append(args, f.Project)
		}
	}

	args = append(args, firstTerm)
	for range 3 {
		for _, t := range terms {
			args = append(args, SearchLikePattern(t.Value))
		}
	}

	firstLike := SearchLikePattern(firstTerm)
	for range 7 {
		args = append(args, firstLike)
	}

	args = append(args, plainQuery, plainQuery)
	args = append(args, plainQuery, plainQuery, plainQuery)
	args = append(args, hasPhrase)
	prefixPat := SearchPrefixPattern(plainQuery)
	for range 4 {
		args = append(args, prefixPat)
	}
	for _, t := range terms {
		args = append(args, SearchLikePattern(t.Value))
	}

	if f.Project != "" {
		b.WriteString(" AND s.project = ?")
		args = append(args, f.Project)
	}
	fmt.Fprintf(&b, `
		ORDER BY %s
		LIMIT ? OFFSET ?`, orderBy)
	args = append(args, f.Limit+1, f.Cursor)

	rows, err := db.getReader().QueryContext(ctx, b.String(), args...)
	if err != nil {
		return SearchPage{}, fmt.Errorf("searching: %w", err)
	}
	defer rows.Close()

	var results []SearchResult
	for rows.Next() {
		var r SearchResult
		var matchPos int
		var relevance int
		if err := rows.Scan(
			&r.SessionID, &r.Project, &r.Agent, &r.Name,
			&r.SessionEndedAt, &r.Ordinal,
			&r.Snippet, &r.Rank, &matchPos, &relevance,
		); err != nil {
			return SearchPage{},
				fmt.Errorf("scanning result: %w", err)
		}
		r.Snippet = highlightSearchSnippet(r.Snippet, terms)
		results = append(results, r)
	}
	if err := rows.Err(); err != nil {
		return SearchPage{}, err
	}

	page := SearchPage{Results: results}
	if len(results) > f.Limit {
		page.Results = results[:f.Limit]
		page.NextCursor = f.Cursor + f.Limit
	}
	return page, nil
}

func highlightSearchSnippet(snippet string, terms []SearchTerm) string {
	if snippet == "" || len(terms) == 0 {
		return snippet
	}
	type span struct{ start, end int }
	lower := strings.ToLower(snippet)
	needles := make([]string, 0, len(terms)+1)
	for _, t := range terms {
		if t.Value != "" {
			needles = append(needles, t.Value)
		}
	}
	if joined := JoinedQuery(terms); joined != "" {
		needles = append(needles, joined)
	}
	var spans []span
	for _, needle := range needles {
		n := strings.ToLower(needle)
		from := 0
		for {
			i := strings.Index(lower[from:], n)
			if i < 0 {
				break
			}
			i += from
			spans = append(spans, span{i, i + len(needle)})
			from = i + len(n)
		}
	}
	if len(spans) == 0 {
		return snippet
	}
	sort.Slice(spans, func(i, j int) bool {
		if spans[i].start == spans[j].start {
			return spans[i].end > spans[j].end
		}
		return spans[i].start < spans[j].start
	})
	merged := make([]span, 0, len(spans))
	for _, s := range spans {
		if n := len(merged); n > 0 && s.start <= merged[n-1].end {
			if s.end > merged[n-1].end {
				merged[n-1].end = s.end
			}
			continue
		}
		merged = append(merged, s)
	}
	var b strings.Builder
	prev := 0
	for _, s := range merged {
		if s.start < prev {
			continue
		}
		if s.end > len(snippet) {
			s.end = len(snippet)
		}
		b.WriteString(snippet[prev:s.start])
		b.WriteString("<mark>")
		b.WriteString(snippet[s.start:s.end])
		b.WriteString("</mark>")
		prev = s.end
	}
	b.WriteString(snippet[prev:])
	return b.String()
}

func sqliteAllTermsPrimarySQL(n int, primarySQL string) string {
	parts := make([]string, n)
	for i := range n {
		parts[i] = "LOWER(" + primarySQL + ") LIKE LOWER(?) ESCAPE '\\'"
	}
	return "(" + strings.Join(parts, " AND ") + ")"
}

// SearchSession performs a case-insensitive substring search within a single
// session's messages, returning matching ordinals in document order.
// This is used by the in-session find bar (analogous to browser Cmd+F).
// Both message content and tool-call result_content are searched so that
// matches inside tool output blocks are reachable. Only fields that the
// frontend renders and highlights are included to avoid phantom matches.
func (db *DB) SearchSession(
	ctx context.Context, sessionID, query string,
) ([]int, error) {
	terms := ParseUserQuery(query)
	if len(terms) == 0 {
		return nil, nil
	}
	// LIKE substring per term, AND'd. Quoted phrases stay contiguous.
	// SQLite LIKE is case-insensitive for ASCII by default.
	// LEFT JOIN tool_calls so that a hit in result_content also surfaces
	// the parent message ordinal; DISTINCT collapses multiple tool calls
	// on the same message into a single result.
	var pred strings.Builder
	args := []any{sessionID}
	for i, t := range terms {
		if i > 0 {
			pred.WriteString(" AND ")
		}
		pred.WriteString(`(m.content LIKE ? ESCAPE '\'
		        OR tc.result_content LIKE ? ESCAPE '\')`)
		like := SearchLikePattern(t.Value)
		args = append(args, like, like)
	}
	rows, err := db.getReader().QueryContext(ctx,
		`SELECT DISTINCT m.ordinal
		 FROM messages m
		 LEFT JOIN tool_calls tc ON tc.message_id = m.id
		 WHERE m.session_id = ?
		   AND m.is_system = 0
		   AND `+SystemPrefixSQL("m.content", "m.role")+`
		   AND `+pred.String()+`
		 ORDER BY m.ordinal ASC`,
		args...,
	)
	if err != nil {
		return nil, fmt.Errorf("session search: %w", err)
	}
	defer rows.Close()

	var ordinals []int
	for rows.Next() {
		var ord int
		if err := rows.Scan(&ord); err != nil {
			return nil, fmt.Errorf("scanning ordinal: %w", err)
		}
		ordinals = append(ordinals, ord)
	}
	return ordinals, rows.Err()
}

// PrepareFTSQuery turns a user's raw search input into a well-formed SQLite
// FTS5 MATCH expression. Terms are quoted (embedded quotes doubled) so
// punctuation such as '-' or ':' is literal and cannot 500 the MATCH parser.
// Unquoted words combine under FTS5's implicit AND. Quoted spans, including
// mixed `"exact phrase" other` input, stay phrases.
//
// Empty or whitespace-only input returns "". This is the shared quoting
// helper for HTTP, SQLite, PostgreSQL, DuckDB, and MCP search paths.
func PrepareFTSQuery(raw string) string {
	terms := ParseUserQuery(raw)
	if len(terms) == 0 {
		return ""
	}
	var b strings.Builder
	for i, t := range terms {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(QuoteFTS(t.Value))
	}
	return b.String()
}

// FTSTerms decomposes a PrepareFTSQuery output back into its individual terms,
// un-doubling escaped quotes inside quoted terms and collecting bare tokens. A
// multi-term AND query like `"error" "401"` yields ["error", "401"], a single
// quoted operator token `"error-401"` yields ["error-401"], and an explicit
// exact phrase `"fix bug"` yields a single ["fix bug"] term. This lets the
// substring backends (SQLite name-branch LIKE/instr, PostgreSQL ILIKE)
// reconstruct the same AND-of-terms vs. exact-phrase semantics the FTS engine
// applies, keeping behavior identical across backends.
func FTSTerms(v string) []string {
	if !strings.Contains(v, `"`) {
		if v = strings.TrimSpace(v); v == "" {
			return nil
		}
		return strings.Fields(v)
	}
	var terms []string
	var cur strings.Builder
	inQuote := false
	hasTerm := false
	flush := func() {
		if hasTerm {
			terms = append(terms, cur.String())
			cur.Reset()
			hasTerm = false
		}
	}
	for i := 0; i < len(v); i++ {
		c := v[i]
		switch {
		case c == '"':
			if inQuote && i+1 < len(v) && v[i+1] == '"' {
				// Doubled quote inside a quoted term is a literal quote.
				cur.WriteByte('"')
				hasTerm = true
				i++
				continue
			}
			inQuote = !inQuote
			hasTerm = true
		case !inQuote && (c == ' ' || c == '\t' || c == '\n' || c == '\r'):
			flush()
		default:
			cur.WriteByte(c)
			hasTerm = true
		}
	}
	flush()
	return terms
}

// StripFTSQuotes reverses PrepareFTSQuery into a plain substring suitable for
// LIKE and instr() operations (name-branch matching, snippet centering). It
// rejoins the parsed FTS terms with single spaces. So `"unique" "phrase"`
// becomes "unique phrase", a single quoted token like `"error-401"` becomes
// "error-401", and an explicit phrase `"fix bug"` becomes "fix bug". Input with
// no quotes is returned unchanged.
func StripFTSQuotes(v string) string {
	if !strings.Contains(v, `"`) {
		return v
	}
	return strings.Join(FTSTerms(v), " ")
}
