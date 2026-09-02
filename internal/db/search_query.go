package db

import (
	"strings"
	"unicode"
)

// Relevance ranks for session search. Lower values are stronger matches.
// ORDER BY uses these buckets, then FTS score, match position, and recency.
const (
	SearchRankExactValue      = 0 // session id or name equals the full query
	SearchRankExactPrimary    = 1 // project, agent, or label equals the full query
	SearchRankExactPhrase     = 2 // quoted phrase matched in message content
	SearchRankPrefixPrimary   = 3 // prefix of id, name, project, or agent
	SearchRankAllTermsPrimary = 4 // every term appears in primary metadata
	SearchRankAllTermsContent = 5 // every term appears in message content
	SearchRankSubstring       = 6 // remaining substring / cross-field matches
)

// SearchTerm is one unit of a user query: an unquoted word or a quoted phrase.
type SearchTerm struct {
	Value  string
	Phrase bool
}

// SearchTermsFromQuery parses raw or PrepareFTSQuery-quoted input into terms.
func SearchTermsFromQuery(raw string) []SearchTerm {
	terms := ParseUserQuery(raw)
	if len(terms) > 0 {
		return terms
	}
	for _, t := range FTSTerms(raw) {
		if t == "" {
			continue
		}
		terms = append(terms, SearchTerm{Value: t})
	}
	return terms
}

// ParseUserQuery splits raw search input into terms.
//
// Unquoted text is split on Unicode whitespace. Double-quoted spans are a
// single phrase and keep internal spacing. Embedded quotes inside a phrase
// are escaped by doubling them, matching FTS5. An unclosed quote takes the
// remainder of the input as a phrase. Empty and whitespace-only input yield
// no terms.
func ParseUserQuery(raw string) []SearchTerm {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	var terms []SearchTerm
	var cur strings.Builder
	inQuote := false
	has := false
	flush := func(phrase bool) {
		if !has {
			return
		}
		v := cur.String()
		cur.Reset()
		has = false
		if !phrase {
			v = strings.TrimSpace(v)
		}
		if v == "" {
			return
		}
		terms = append(terms, SearchTerm{Value: v, Phrase: phrase})
	}
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		switch {
		case c == '"':
			if inQuote && i+1 < len(raw) && raw[i+1] == '"' {
				cur.WriteByte('"')
				has = true
				i++
				continue
			}
			if inQuote {
				flush(true)
				inQuote = false
				continue
			}
			if !has {
				inQuote = true
				continue
			}
			// Quote in the middle of an unquoted token is literal, matching
			// the previous "quote the whole whitespace token" behavior.
			cur.WriteByte('"')
			has = true
		case !inQuote && unicode.IsSpace(rune(c)):
			flush(false)
		default:
			cur.WriteByte(c)
			has = true
		}
	}
	if inQuote {
		flush(true)
	} else {
		flush(false)
	}
	return terms
}

// QuoteFTS wraps s as an FTS5 quoted token, doubling embedded quotes.
func QuoteFTS(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

// FTSMatchExpression builds an FTS5 MATCH string for terms.
// Unquoted alphanumeric terms get a prefix star so "clau" matches "claude".
// Quoted phrases and punctuation-bearing tokens stay exact quoted literals.
func FTSMatchExpression(terms []SearchTerm) string {
	if len(terms) == 0 {
		return ""
	}
	var b strings.Builder
	for i, t := range terms {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(QuoteFTS(t.Value))
		if ftsPrefixable(t.Value) {
			b.WriteByte('*')
		}
	}
	return b.String()
}

// ftsPrefixable reports whether a term is a single FTS token that can take a
// trailing '*'. Punctuation-bearing identifiers stay quoted phrases so FTS5
// does not treat '-' or ':' as operators.
func ftsPrefixable(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !unicode.IsLetter(r) && !unicode.IsNumber(r) && r != '_' {
			return false
		}
	}
	return true
}

// ftsSearchable reports whether a term contains a letter or digit so FTS5
// MATCH will see at least one token. Punctuation-only input is matched via
// LIKE instead, which avoids malformed MATCH errors.
func ftsSearchable(s string) bool {
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsNumber(r) {
			return true
		}
	}
	return false
}

// JoinedQuery reconstructs a normalized query string from parsed terms.
func JoinedQuery(terms []SearchTerm) string {
	if len(terms) == 0 {
		return ""
	}
	parts := make([]string, len(terms))
	for i, t := range terms {
		parts[i] = t.Value
	}
	return strings.Join(parts, " ")
}

// HasQuotedPhrase reports whether any term is a user-quoted multi-word phrase.
func HasQuotedPhrase(terms []SearchTerm) bool {
	for _, t := range terms {
		if t.Phrase && strings.ContainsFunc(t.Value, unicode.IsSpace) {
			return true
		}
	}
	return false
}

// SearchLikePattern is a case-preserving LIKE/ILIKE substring pattern.
func SearchLikePattern(term string) string {
	return "%" + EscapeLikePattern(term) + "%"
}

// SearchPrefixPattern is a LIKE/ILIKE prefix pattern.
func SearchPrefixPattern(term string) string {
	return EscapeLikePattern(term) + "%"
}

// SessionSearchBlobSQL concatenates the session fields users reasonably
// search by name, identifier, path, status, or metadata. COALESCE keeps
// NULL columns from collapsing the whole blob to NULL.
func SessionSearchBlobSQL(alias string) string {
	cols := []string{
		"id",
		"source_session_id",
		"project",
		"agent",
		"agent_label",
		"display_name",
		"session_name",
		"first_message",
		"git_branch",
		"cwd",
		"file_path",
		"outcome",
		"termination_status",
		"machine",
		"session_kind",
		"entrypoint",
	}
	parts := make([]string, len(cols))
	for i, c := range cols {
		parts[i] = "COALESCE(" + alias + "." + c + ", '')"
	}
	return "(" + strings.Join(parts, " || ' ' || ") + ")"
}

// SessionPrimaryBlobSQL concatenates id, names, project, and agent for
// primary-field ranking checks.
func SessionPrimaryBlobSQL(alias string) string {
	return "(" +
		"COALESCE(" + alias + ".display_name, '') || ' ' || " +
		"COALESCE(" + alias + ".session_name, '') || ' ' || " +
		"COALESCE(" + alias + ".id, '') || ' ' || " +
		"COALESCE(" + alias + ".project, '') || ' ' || " +
		"COALESCE(" + alias + ".agent, '') || ' ' || " +
		"COALESCE(" + alias + ".agent_label, '')" +
		")"
}
