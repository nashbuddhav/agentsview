package postgres

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/db"
)

const attachToolCallBatchSize = 500

// GetMessages returns paginated messages for a session.
func (s *Store) GetMessages(
	ctx context.Context,
	sessionID string, from, limit int, asc bool,
) ([]db.Message, error) {
	if limit <= 0 || limit > db.MaxMessageLimit {
		limit = db.DefaultMessageLimit
	}

	dir := "ASC"
	op := ">="
	if !asc {
		dir = "DESC"
		op = "<="
	}

	query := fmt.Sprintf(`
		SELECT %s
		FROM messages
		WHERE session_id = $1 AND ordinal %s $2
		ORDER BY ordinal %s
		LIMIT $3`, pgMessageCols, op, dir)

	rows, err := s.pg.QueryContext(
		ctx, query, sessionID, from, limit,
	)
	if err != nil {
		return nil, fmt.Errorf(
			"querying messages: %w", err,
		)
	}
	defer rows.Close()

	msgs, err := scanPGMessages(rows)
	if err != nil {
		return nil, err
	}
	if err := s.attachToolCalls(ctx, msgs); err != nil {
		return nil, err
	}
	return msgs, nil
}

const pgMessageCols = `session_id, ordinal, role, content, thinking_text,
	timestamp, has_thinking, has_tool_use,
	content_length, is_system, model, token_usage,
	context_tokens, output_tokens, provider_id,
	has_context_tokens, has_output_tokens,
	claude_message_id, claude_request_id,
	source_type, source_subtype, prompt_source, source_uuid,
	source_parent_uuid, is_sidechain,
	is_compact_boundary`

// GetMessagesWindow mirrors internal/db's GetMessagesWindow: linear mode
// (optionally role-filtered) delegates to GetMessages when Roles is empty;
// Around mode merges three queries (before/anchor/after) into one ascending
// slice. The anchor query has no role predicate so the anchor row is always
// present regardless of Roles; before/after apply the role filter first, so
// Before/After count role-matching messages, not raw ordinal distance.
func (s *Store) GetMessagesWindow(
	ctx context.Context, sessionID string, w db.MessageWindow,
) ([]db.Message, error) {
	if w.Around != nil {
		return s.getMessagesAroundAnchor(ctx, sessionID, w)
	}
	from := 0
	if w.From != nil {
		from = *w.From
	}
	if len(w.Roles) == 0 {
		return s.GetMessages(ctx, sessionID, from, w.Limit, w.Asc)
	}
	return s.getMessagesLinearRoleFiltered(ctx, sessionID, from, w.Limit, w.Asc, w.Roles)
}

func (s *Store) getMessagesLinearRoleFiltered(
	ctx context.Context,
	sessionID string, from, limit int, asc bool, roles []string,
) ([]db.Message, error) {
	if limit <= 0 || limit > db.MaxMessageLimit {
		limit = db.DefaultMessageLimit
	}
	dir := "ASC"
	op := ">="
	if !asc {
		dir = "DESC"
		op = "<="
	}
	roleClause, roleArgs := pgRoleFilterClause(roles, 3)
	query := fmt.Sprintf(`
		SELECT %s
		FROM messages
		WHERE session_id = $1 AND ordinal %s $2%s
		ORDER BY ordinal %s
		LIMIT $%d`, pgMessageCols, op, roleClause, dir, len(roleArgs)+3)
	args := append([]any{sessionID, from}, roleArgs...)
	args = append(args, limit)

	rows, err := s.pg.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("querying role-filtered messages: %w", err)
	}
	defer rows.Close()
	msgs, err := scanPGMessages(rows)
	if err != nil {
		return nil, err
	}
	if err := s.attachToolCalls(ctx, msgs); err != nil {
		return nil, err
	}
	return msgs, nil
}

func (s *Store) getMessagesAroundAnchor(
	ctx context.Context, sessionID string, w db.MessageWindow,
) ([]db.Message, error) {
	anchor := *w.Around
	beforeLimit := max(w.Before, 0)
	afterLimit := max(w.After, 0)
	roleClause, roleArgs := pgRoleFilterClause(w.Roles, 3)

	beforeQuery := fmt.Sprintf(`
		SELECT %s FROM messages
		WHERE session_id = $1 AND ordinal < $2%s
		ORDER BY ordinal DESC LIMIT $%d`,
		pgMessageCols, roleClause, len(roleArgs)+3)
	beforeArgs := append([]any{sessionID, anchor}, roleArgs...)
	beforeArgs = append(beforeArgs, beforeLimit)
	before, err := s.queryMessageRows(ctx, beforeQuery, beforeArgs...)
	if err != nil {
		return nil, fmt.Errorf("querying before-window messages: %w", err)
	}
	slices.Reverse(before)

	anchorQuery := fmt.Sprintf(`
		SELECT %s FROM messages WHERE session_id = $1 AND ordinal = $2`,
		pgMessageCols)
	anchorMsgs, err := s.queryMessageRows(ctx, anchorQuery, sessionID, anchor)
	if err != nil {
		return nil, fmt.Errorf("querying anchor message: %w", err)
	}

	afterQuery := fmt.Sprintf(`
		SELECT %s FROM messages
		WHERE session_id = $1 AND ordinal > $2%s
		ORDER BY ordinal ASC LIMIT $%d`,
		pgMessageCols, roleClause, len(roleArgs)+3)
	afterArgs := append([]any{sessionID, anchor}, roleArgs...)
	afterArgs = append(afterArgs, afterLimit)
	after, err := s.queryMessageRows(ctx, afterQuery, afterArgs...)
	if err != nil {
		return nil, fmt.Errorf("querying after-window messages: %w", err)
	}

	msgs := make([]db.Message, 0, len(before)+len(anchorMsgs)+len(after))
	msgs = append(msgs, before...)
	msgs = append(msgs, anchorMsgs...)
	msgs = append(msgs, after...)
	if err := s.attachToolCalls(ctx, msgs); err != nil {
		return nil, err
	}
	return msgs, nil
}

// queryMessageRows runs query and scans the resulting message rows without
// attaching tool calls; callers batch that across the merged window set.
func (s *Store) queryMessageRows(
	ctx context.Context, query string, args ...any,
) ([]db.Message, error) {
	rows, err := s.pg.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanPGMessages(rows)
}

// pgRoleFilterClause returns an "AND role IN ($n, ...)" clause and its bind
// args for the given roles, or ("", nil) when roles is empty. startAt is the
// first placeholder ordinal to use (the caller's query already consumes
// $1..$(startAt-1)).
func pgRoleFilterClause(roles []string, startAt int) (string, []any) {
	if len(roles) == 0 {
		return "", nil
	}
	placeholders := make([]string, len(roles))
	args := make([]any, len(roles))
	for i, r := range roles {
		placeholders[i] = fmt.Sprintf("$%d", startAt+i)
		args[i] = r
	}
	return " AND role IN (" + strings.Join(placeholders, ",") + ")", args
}

// GetAllMessages returns all messages for a session ordered
// by ordinal.
func (s *Store) GetAllMessages(
	ctx context.Context, sessionID string,
) ([]db.Message, error) {
	rows, err := s.pg.QueryContext(ctx, fmt.Sprintf(`
		SELECT %s
		FROM messages
		WHERE session_id = $1
		ORDER BY ordinal ASC`, pgMessageCols), sessionID)
	if err != nil {
		return nil, fmt.Errorf(
			"querying all messages: %w", err,
		)
	}
	defer rows.Close()

	msgs, err := scanPGMessages(rows)
	if err != nil {
		return nil, err
	}
	if err := s.attachToolCalls(ctx, msgs); err != nil {
		return nil, err
	}
	return msgs, nil
}

func (s *Store) GetResumeModelCounts(
	ctx context.Context, sessionID string,
) ([]db.ModelCount, error) {
	rows, err := s.pg.QueryContext(ctx, `
		SELECT model, COUNT(*)
		FROM messages
		WHERE session_id = $1
			AND role = 'assistant'
			AND model != ''
			AND model != '<synthetic>'
		GROUP BY model`,
		sessionID,
	)
	if err != nil {
		return nil, fmt.Errorf("querying postgres resume model counts: %w", err)
	}
	defer rows.Close()
	var counts []db.ModelCount
	for rows.Next() {
		var count db.ModelCount
		if err := rows.Scan(&count.Model, &count.Count); err != nil {
			return nil, fmt.Errorf("scanning postgres resume model count: %w", err)
		}
		counts = append(counts, count)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating postgres resume model counts: %w", err)
	}
	return counts, nil
}

// SearchSession performs ILIKE substring search within a single
// session's messages, returning matching ordinals.
func (s *Store) SearchSession(
	ctx context.Context, sessionID, query string,
) ([]int, error) {
	terms := db.SearchTermsFromQuery(query)
	if len(terms) == 0 {
		return nil, nil
	}
	args := []any{sessionID}
	var pred strings.Builder
	for i, t := range terms {
		if i > 0 {
			pred.WriteString(" AND ")
		}
		idx := i + 2
		fmt.Fprintf(&pred, "(m.content ILIKE $%d OR tc.result_content ILIKE $%d)", idx, idx)
		args = append(args, db.SearchLikePattern(t.Value))
	}
	rows, err := s.pg.QueryContext(ctx, `
		SELECT DISTINCT m.ordinal
		FROM messages m
		LEFT JOIN tool_calls tc
			ON tc.session_id = m.session_id
			AND tc.message_ordinal = m.ordinal
		WHERE m.session_id = $1
			AND m.is_system = FALSE
			AND `+db.PostgresSystemPrefixSQL("m.content", "m.role")+`
			AND `+pred.String()+`
		ORDER BY m.ordinal ASC`,
		args...,
	)
	if err != nil {
		return nil, fmt.Errorf(
			"searching session: %w", err,
		)
	}
	defer rows.Close()

	var ordinals []int
	for rows.Next() {
		var ord int
		if err := rows.Scan(&ord); err != nil {
			return nil, fmt.Errorf(
				"scanning ordinal: %w", err,
			)
		}
		ordinals = append(ordinals, ord)
	}
	return ordinals, rows.Err()
}

// HasFTS returns true because ILIKE search is available.
func (s *Store) HasFTS() bool { return true }

// HasSemantic reports whether a PG vector searcher was wired at startup
// (pg serve found a generation matching its embeddings fingerprint). When
// false, SearchContent rejects "semantic"/"hybrid" modes up front with
// db.ErrSemanticUnavailable.
func (s *Store) HasSemantic() bool { return s.getVectorSearcher() != nil }

// escapeLike escapes SQL LIKE metacharacters so the bind
// parameter is treated as a literal substring.
func escapeLike(v string) string {
	return db.EscapeLikePattern(v)
}

// Search performs ILIKE session search across message content and session
// metadata. Unquoted terms are AND'd in any order and may hit different
// fields. Quoted phrases must appear together. Ranking matches SQLite.
func (s *Store) Search(
	ctx context.Context, f db.SearchFilter,
) (db.SearchPage, error) {
	if f.Limit <= 0 || f.Limit > db.MaxSearchLimit {
		f.Limit = db.DefaultSearchLimit
	}
	terms := db.SearchTermsFromQuery(f.Query)
	if len(terms) == 0 {
		return db.SearchPage{}, nil
	}
	plainQuery := db.JoinedQuery(terms)
	firstTerm := terms[0].Value
	hasPhrase := 0
	if db.HasQuotedPhrase(terms) {
		hasPhrase = 1
	}

	outerOrderBy := "relevance ASC, match_pos ASC, session_ended_at DESC NULLS LAST, session_id ASC"
	if f.Sort == "recency" {
		outerOrderBy = "session_ended_at DESC NULLS LAST, session_id ASC"
	}

	blobSQL := db.SessionSearchBlobSQL("s")
	primarySQL := db.SessionPrimaryBlobSQL("s")
	sysMsg := db.PostgresSystemPrefixSQL("m.content", "m.role")
	sysVis := db.PostgresSystemPrefixSQL("mx.content", "mx.role")

	var args []any
	ph := func(v any) string {
		args = append(args, v)
		return fmt.Sprintf("$%d", len(args))
	}

	var b strings.Builder
	b.WriteString("WITH ")
	for i, t := range terms {
		if i > 0 {
			b.WriteString(",\n")
		}
		like := ph(db.SearchLikePattern(t.Value))
		fmt.Fprintf(&b, `term%d AS (
			SELECT m.session_id
			FROM messages m
			JOIN sessions s ON m.session_id = s.id
			WHERE s.deleted_at IS NULL
			  AND m.is_system = FALSE
			  AND %s
			  AND m.content ILIKE %s ESCAPE E'\\'
			UNION
			SELECT s.id FROM sessions s
			WHERE s.deleted_at IS NULL
			  AND %s ILIKE %s ESCAPE E'\\'
			UNION
			SELECT m.session_id FROM messages m
			JOIN sessions s ON s.id = m.session_id
			WHERE s.deleted_at IS NULL
			  AND m.model ILIKE %s ESCAPE E'\\'
			UNION
			SELECT tc.session_id FROM tool_calls tc
			JOIN sessions s ON s.id = tc.session_id
			WHERE s.deleted_at IS NULL
			  AND (
			    tc.tool_name ILIKE %s ESCAPE E'\\'
			    OR COALESCE(tc.skill_name, '') ILIKE %s ESCAPE E'\\'
			    OR COALESCE(tc.file_path, '') ILIKE %s ESCAPE E'\\'
			  )
		)`, i, sysMsg, like, blobSQL, like, like, like, like, like)
	}

	b.WriteString(",\nmatched AS (\n")
	for i := range terms {
		if i > 0 {
			b.WriteString(" INTERSECT ")
		}
		fmt.Fprintf(&b, "SELECT session_id FROM term%d", i)
	}

	firstLike := ph(db.SearchLikePattern(firstTerm))
	firstRaw := ph(firstTerm)
	plainPh := ph(plainQuery)
	prefixPh := ph(db.SearchPrefixPattern(plainQuery))
	hasPhrasePh := ph(hasPhrase)

	primaryPredParts := make([]string, len(terms))
	for i, t := range terms {
		primaryPredParts[i] = primarySQL + " ILIKE " + ph(db.SearchLikePattern(t.Value)) + ` ESCAPE E'\\'`
	}
	primaryPred := "(" + strings.Join(primaryPredParts, " AND ") + ")"

	projectClause := ""
	if f.Project != "" {
		projectClause = "AND s.project = " + ph(f.Project)
	}
	limitPh := ph(f.Limit + 1)
	offsetPh := ph(f.Cursor)

	fmt.Fprintf(&b, `
		),
		msg_best AS (
			SELECT DISTINCT ON (m.session_id)
				m.session_id,
				m.ordinal,
				POSITION(LOWER(%s) IN LOWER(m.content)) AS match_pos,
				CASE
					WHEN POSITION(LOWER(%s) IN LOWER(m.content)) > 100
						THEN '...' || SUBSTRING(m.content
							FROM GREATEST(1, POSITION(
								LOWER(%s) IN LOWER(m.content)
							) - 50) FOR 200) || '...'
					ELSE SUBSTRING(m.content FROM 1 FOR 200)
						|| CASE WHEN LENGTH(m.content) > 200
							THEN '...' ELSE '' END
				END AS snippet
			FROM messages m
			JOIN matched mt ON mt.session_id = m.session_id
			JOIN sessions s ON m.session_id = s.id
			WHERE m.is_system = FALSE
			  AND %s
			  AND m.content ILIKE %s ESCAPE E'\\'
			ORDER BY m.session_id,
				POSITION(LOWER(%s) IN LOWER(m.content)) ASC,
				m.ordinal ASC
		)
		SELECT s.id, s.project, s.agent,
			COALESCE(s.display_name, s.session_name, s.first_message, '') AS name,
			COALESCE(s.ended_at, s.started_at) AS session_ended_at,
			COALESCE(mb.ordinal, -1) AS ordinal,
			CASE
				WHEN mb.snippet IS NOT NULL THEN mb.snippet
				WHEN COALESCE(s.display_name, s.session_name, '') ILIKE %s ESCAPE E'\\'
					THEN COALESCE(s.display_name, s.session_name, '')
				WHEN COALESCE(s.first_message, '') ILIKE %s ESCAPE E'\\'
					THEN COALESCE(s.first_message, '')
				WHEN s.id ILIKE %s ESCAPE E'\\' THEN s.id
				WHEN s.project ILIKE %s ESCAPE E'\\' THEN s.project
				WHEN s.agent ILIKE %s ESCAPE E'\\' THEN s.agent
				WHEN COALESCE(s.git_branch, '') ILIKE %s ESCAPE E'\\' THEN s.git_branch
				WHEN COALESCE(s.cwd, '') ILIKE %s ESCAPE E'\\' THEN s.cwd
				ELSE COALESCE(s.display_name, s.session_name, s.first_message, '')
			END AS snippet,
			1.0 AS rank,
			COALESCE(mb.match_pos, 0) AS match_pos,
			CASE
				WHEN LOWER(COALESCE(s.display_name, s.session_name, '')) = LOWER(%s)
					OR LOWER(s.id) = LOWER(%s) THEN %d
				WHEN LOWER(s.project) = LOWER(%s)
					OR LOWER(s.agent) = LOWER(%s)
					OR LOWER(COALESCE(s.agent_label, '')) = LOWER(%s) THEN %d
				WHEN mb.session_id IS NOT NULL AND %s = 1 THEN %d
				WHEN COALESCE(s.display_name, s.session_name, '') ILIKE %s ESCAPE E'\\'
					OR s.id ILIKE %s ESCAPE E'\\'
					OR s.project ILIKE %s ESCAPE E'\\'
					OR s.agent ILIKE %s ESCAPE E'\\' THEN %d
				WHEN %s THEN %d
				WHEN mb.session_id IS NOT NULL THEN %d
				ELSE %d
			END AS relevance
		FROM matched mt
		JOIN sessions s ON s.id = mt.session_id
		LEFT JOIN msg_best mb ON mb.session_id = s.id
		WHERE s.deleted_at IS NULL
		  AND EXISTS (
			SELECT 1 FROM messages mx
			WHERE mx.session_id = s.id
			  AND mx.is_system = FALSE
			  AND %s
		  )
		  %s
		ORDER BY %s
		LIMIT %s OFFSET %s`,
		firstRaw, firstRaw, firstRaw, sysMsg, firstLike, firstRaw,
		firstLike, firstLike, firstLike, firstLike, firstLike, firstLike, firstLike,
		plainPh, plainPh, db.SearchRankExactValue,
		plainPh, plainPh, plainPh, db.SearchRankExactPrimary,
		hasPhrasePh, db.SearchRankExactPhrase,
		prefixPh, prefixPh, prefixPh, prefixPh, db.SearchRankPrefixPrimary,
		primaryPred, db.SearchRankAllTermsPrimary,
		db.SearchRankAllTermsContent, db.SearchRankSubstring,
		sysVis, projectClause, outerOrderBy, limitPh, offsetPh,
	)

	query := b.String()

	rows, err := s.pg.QueryContext(ctx, query, args...)
	if err != nil {
		return db.SearchPage{},
			fmt.Errorf("searching: %w", err)
	}
	defer rows.Close()

	results := []db.SearchResult{}
	for rows.Next() {
		var r db.SearchResult
		var endedAt *time.Time
		var matchPos int
		var relevance int
		if err := rows.Scan(
			&r.SessionID, &r.Project, &r.Agent, &r.Name,
			&endedAt, &r.Ordinal,
			&r.Snippet, &r.Rank, &matchPos, &relevance,
		); err != nil {
			return db.SearchPage{},
				fmt.Errorf(
					"scanning search result: %w", err,
				)
		}
		if endedAt != nil {
			r.SessionEndedAt = FormatISO8601(*endedAt)
		}
		results = append(results, r)
	}
	if err := rows.Err(); err != nil {
		return db.SearchPage{}, err
	}

	page := db.SearchPage{Results: results}
	if len(results) > f.Limit {
		page.Results = results[:f.Limit]
		page.NextCursor = f.Cursor + f.Limit
	}
	return page, nil
}

// attachToolCalls loads tool_calls for the given messages and
// attaches them to each message's ToolCalls field.
func (s *Store) attachToolCalls(
	ctx context.Context, msgs []db.Message,
) error {
	if len(msgs) == 0 {
		return nil
	}

	ordToIdx := make(map[int]int, len(msgs))
	sessionID := msgs[0].SessionID
	ordinals := make([]int, 0, len(msgs))
	for i, m := range msgs {
		ordToIdx[m.Ordinal] = i
		ordinals = append(ordinals, m.Ordinal)
	}

	for i := 0; i < len(ordinals); i += attachToolCallBatchSize {
		end := min(i+attachToolCallBatchSize, len(ordinals))
		if err := s.attachToolCallsBatch(
			ctx, msgs, ordToIdx, sessionID,
			ordinals[i:end],
		); err != nil {
			return err
		}
	}
	if err := s.attachToolResultEvents(
		ctx, msgs, ordToIdx, sessionID, ordinals,
	); err != nil {
		return err
	}
	return nil
}

func (s *Store) attachToolCallsBatch(
	ctx context.Context,
	msgs []db.Message,
	ordToIdx map[int]int,
	sessionID string,
	batch []int,
) error {
	if len(batch) == 0 {
		return nil
	}

	args := []any{sessionID}
	phs := make([]string, len(batch))
	for i, ord := range batch {
		args = append(args, ord)
		phs[i] = fmt.Sprintf("$%d", i+2)
	}

	query := fmt.Sprintf(`
		SELECT message_ordinal, session_id, tool_name,
			category,
			COALESCE(tool_use_id, ''),
			COALESCE(input_json, ''),
			COALESCE(skill_name, ''),
			COALESCE(result_content_length, 0),
			COALESCE(result_content, ''),
			COALESCE(subagent_session_id, ''),
			COALESCE(file_path, ''),
			COALESCE(call_index, 0)
		FROM tool_calls
		WHERE session_id = $1
			AND message_ordinal IN (%s)
		ORDER BY message_ordinal, call_index`,
		strings.Join(phs, ","))

	rows, err := s.pg.QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf(
			"querying tool_calls: %w", err,
		)
	}
	defer rows.Close()

	for rows.Next() {
		var tc db.ToolCall
		var msgOrdinal int
		if err := rows.Scan(
			&msgOrdinal, &tc.SessionID,
			&tc.ToolName, &tc.Category,
			&tc.ToolUseID, &tc.InputJSON, &tc.SkillName,
			&tc.ResultContentLength, &tc.ResultContent,
			&tc.SubagentSessionID,
			&tc.FilePath, &tc.CallIndex,
		); err != nil {
			return fmt.Errorf(
				"scanning tool_call: %w", err,
			)
		}
		if idx, ok := ordToIdx[msgOrdinal]; ok {
			msgs[idx].ToolCalls = append(
				msgs[idx].ToolCalls, tc,
			)
		}
	}
	return rows.Err()
}

func (s *Store) attachToolResultEvents(
	ctx context.Context,
	msgs []db.Message,
	ordToIdx map[int]int,
	sessionID string,
	ordinals []int,
) error {
	for i := 0; i < len(ordinals); i += attachToolCallBatchSize {
		end := min(i+attachToolCallBatchSize, len(ordinals))
		if err := s.attachToolResultEventsBatch(
			ctx, msgs, ordToIdx, sessionID, ordinals[i:end],
		); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) attachToolResultEventsBatch(
	ctx context.Context,
	msgs []db.Message,
	ordToIdx map[int]int,
	sessionID string,
	ordinals []int,
) error {
	if len(ordinals) == 0 {
		return nil
	}

	args := []any{sessionID}
	phs := make([]string, len(ordinals))
	for i, ord := range ordinals {
		args = append(args, ord)
		phs[i] = fmt.Sprintf("$%d", i+2)
	}

	query := fmt.Sprintf(`
		SELECT tool_call_message_ordinal, call_index,
			COALESCE(tool_use_id, ''),
			COALESCE(agent_id, ''),
			COALESCE(subagent_session_id, ''),
			source, status, content, content_length,
			timestamp, event_index
		FROM tool_result_events
		WHERE session_id = $1
			AND tool_call_message_ordinal IN (%s)
		ORDER BY tool_call_message_ordinal, call_index, event_index`,
		strings.Join(phs, ","))

	rows, err := s.pg.QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("querying tool_result_events: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			msgOrdinal int
			callIndex  int
			ev         db.ToolResultEvent
			ts         *time.Time
		)
		if err := rows.Scan(
			&msgOrdinal, &callIndex,
			&ev.ToolUseID, &ev.AgentID,
			&ev.SubagentSessionID,
			&ev.Source, &ev.Status,
			&ev.Content, &ev.ContentLength,
			&ts, &ev.EventIndex,
		); err != nil {
			return fmt.Errorf("scanning tool_result_event: %w", err)
		}
		if ts != nil {
			ev.Timestamp = FormatISO8601(*ts)
		}
		idx, ok := ordToIdx[msgOrdinal]
		if !ok {
			continue
		}
		if callIndex < 0 || callIndex >= len(msgs[idx].ToolCalls) {
			continue
		}
		msgs[idx].ToolCalls[callIndex].ResultEvents = append(
			msgs[idx].ToolCalls[callIndex].ResultEvents,
			ev,
		)
	}
	return rows.Err()
}

// scanPGMessages scans message rows from PostgreSQL,
// converting TIMESTAMPTZ to string.
//
// The PG messages table has no id column (composite PK on
// session_id, ordinal), so we synthesize Message.ID = int64(ordinal)
// to match the convention used by TurnRow.MessageID and
// CallRow.MessageID in session_timing.go. The frontend keys
// {#each messages (message.id)} and looks up turns via
// turnByMessage.get(message.id); both depend on Message.ID being
// non-zero, unique within a session, and equal to int64(ordinal)
// so it joins with TurnRow.MessageID.
func scanPGMessages(rows interface {
	Next() bool
	Scan(dest ...any) error
	Err() error
},
) ([]db.Message, error) {
	msgs := []db.Message{}
	for rows.Next() {
		var m db.Message
		var ts *time.Time
		var tokenUsage string
		if err := rows.Scan(
			&m.SessionID, &m.Ordinal, &m.Role,
			&m.Content, &m.ThinkingText, &ts, &m.HasThinking,
			&m.HasToolUse, &m.ContentLength, &m.IsSystem,
			&m.Model, &tokenUsage,
			&m.ContextTokens, &m.OutputTokens,
			&m.ProviderID,
			&m.HasContextTokens, &m.HasOutputTokens,
			&m.ClaudeMessageID, &m.ClaudeRequestID,
			&m.SourceType, &m.SourceSubtype, &m.PromptSource, &m.SourceUUID,
			&m.SourceParentUUID, &m.IsSidechain,
			&m.IsCompactBoundary,
		); err != nil {
			return nil, fmt.Errorf(
				"scanning message: %w", err,
			)
		}
		m.ID = int64(m.Ordinal)
		if ts != nil {
			m.Timestamp = FormatISO8601(*ts)
		}
		if tokenUsage != "" {
			m.TokenUsage = []byte(tokenUsage)
		}
		msgs = append(msgs, m)
	}
	return msgs, rows.Err()
}
