package db

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestVersatileSearch(t *testing.T) {
	t.Parallel()
	d := testDB(t)
	requireFTS(t, d)

	insertSession(t, d, "exact-name", "proj-search", func(s *Session) {
		s.Agent = "codex"
		s.StartedAt = new("2024-02-01T10:00:00Z")
		s.EndedAt = new("2024-02-01T11:00:00Z")
	})
	require.NoError(t, d.RenameSession("exact-name", new("Claude Code")))
	insertMessages(t, d, userMsg("exact-name", 0, "unrelated greeting"))

	insertSession(t, d, "content-hit", "proj-search", func(s *Session) {
		s.Agent = "claude"
		s.GitBranch = "feature/versatile-search"
		s.Cwd = "/tmp/src/components"
		s.StartedAt = new("2024-02-02T10:00:00Z")
		s.EndedAt = new("2024-02-02T11:00:00Z")
	})
	insertMessages(t, d,
		userMsg("content-hit", 0, "permission denied while running tests"),
		asstMsg("content-hit", 1, "retry after the timeout"),
	)

	insertSession(t, d, "cross-field", "agentsview", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = new("2024-02-03T10:00:00Z")
		s.EndedAt = new("2024-02-03T11:00:00Z")
	})
	insertMessages(t, d, userMsg("cross-field", 0, "MCP action failed"))

	insertSession(t, d, "tool-model", "proj-search", func(s *Session) {
		s.Agent = "cursor"
		s.StartedAt = new("2024-02-04T10:00:00Z")
		s.EndedAt = new("2024-02-04T11:00:00Z")
	})
	modelMsg := asstMsg("tool-model", 0, "I will call a tool")
	modelMsg.Model = "claude-sonnet-4"
	modelMsg.HasToolUse = true
	modelMsg.ToolCalls = []ToolCall{{
		SessionID: "tool-model",
		ToolName:  "Bash",
		Category:  "execution",
		FilePath:  "internal/db/search.go",
	}}
	insertMessages(t, d, modelMsg)

	insertSession(t, d, "id-hyphen", "proj-search", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = new("2024-02-05T10:00:00Z")
		s.EndedAt = new("2024-02-05T11:00:00Z")
	})
	insertMessages(t, d, userMsg("id-hyphen", 0, "plain body"))

	ids := func(page SearchPage) []string {
		out := make([]string, len(page.Results))
		for i, r := range page.Results {
			out[i] = r.SessionID
		}
		return out
	}
	contains := func(page SearchPage, id string) bool {
		for _, r := range page.Results {
			if r.SessionID == id {
				return true
			}
		}
		return false
	}

	t.Run("exact single word", func(t *testing.T) {
		page, err := d.Search(context.Background(), SearchFilter{Query: "timeout", Limit: 20})
		require.NoError(t, err)
		assert.True(t, contains(page, "content-hit"), ids(page))
	})

	t.Run("case insensitive", func(t *testing.T) {
		page, err := d.Search(context.Background(), SearchFilter{Query: "TIMEOUT", Limit: 20})
		require.NoError(t, err)
		assert.True(t, contains(page, "content-hit"), ids(page))
	})

	t.Run("exact full string name", func(t *testing.T) {
		page, err := d.Search(context.Background(), SearchFilter{Query: "Claude Code", Limit: 20})
		require.NoError(t, err)
		require.NotEmpty(t, page.Results)
		assert.Equal(t, "exact-name", page.Results[0].SessionID, ids(page))
	})

	t.Run("prefix match", func(t *testing.T) {
		page, err := d.Search(context.Background(), SearchFilter{Query: "permiss", Limit: 20})
		require.NoError(t, err)
		assert.True(t, contains(page, "content-hit"), ids(page))
	})

	t.Run("substring in name", func(t *testing.T) {
		page, err := d.Search(context.Background(), SearchFilter{Query: "laude", Limit: 20})
		require.NoError(t, err)
		assert.True(t, contains(page, "exact-name"), ids(page))
	})

	t.Run("multiple words same order", func(t *testing.T) {
		page, err := d.Search(context.Background(), SearchFilter{Query: "permission denied", Limit: 20})
		require.NoError(t, err)
		assert.True(t, contains(page, "content-hit"), ids(page))
	})

	t.Run("multiple words different order", func(t *testing.T) {
		page, err := d.Search(context.Background(), SearchFilter{Query: "denied permission", Limit: 20})
		require.NoError(t, err)
		assert.True(t, contains(page, "content-hit"), ids(page))
	})

	t.Run("words across fields", func(t *testing.T) {
		page, err := d.Search(context.Background(), SearchFilter{Query: "claude MCP", Limit: 20})
		require.NoError(t, err)
		assert.True(t, contains(page, "cross-field"), ids(page))
	})

	t.Run("quoted phrase", func(t *testing.T) {
		page, err := d.Search(context.Background(), SearchFilter{Query: `"permission denied"`, Limit: 20})
		require.NoError(t, err)
		assert.True(t, contains(page, "content-hit"), ids(page))

		miss, err := d.Search(context.Background(), SearchFilter{Query: `"denied permission"`, Limit: 20})
		require.NoError(t, err)
		assert.False(t, contains(miss, "content-hit"), ids(miss))
	})

	t.Run("unquoted words in different messages of one session", func(t *testing.T) {
		insertSession(t, d, "split-words", "proj-search", func(s *Session) {
			s.Agent = "claude"
			s.StartedAt = new("2024-02-06T10:00:00Z")
			s.EndedAt = new("2024-02-06T11:00:00Z")
		})
		insertMessages(t, d,
			userMsg("split-words", 0, "please confirm the rollout plan"),
			asstMsg("split-words", 1, "the previous behaviour stays unchanged"),
		)
		insertSession(t, d, "only-confirm", "proj-search", func(s *Session) {
			s.Agent = "claude"
			s.StartedAt = new("2024-02-07T10:00:00Z")
			s.EndedAt = new("2024-02-07T11:00:00Z")
		})
		insertMessages(t, d, userMsg("only-confirm", 0, "please confirm the rollout plan"))

		unquoted, err := d.Search(context.Background(), SearchFilter{
			Query: "confirm behaviour", Limit: 50,
		})
		require.NoError(t, err)
		assert.True(t, contains(unquoted, "split-words"), ids(unquoted))
		assert.False(t, contains(unquoted, "only-confirm"), ids(unquoted))

		quoted, err := d.Search(context.Background(), SearchFilter{
			Query: `"confirm behaviour"`, Limit: 50,
		})
		require.NoError(t, err)
		assert.False(t, contains(quoted, "split-words"),
			"quoted phrase must not match words in different places: %v", ids(quoted))
	})

	t.Run("quoted contiguous phrase in one message", func(t *testing.T) {
		insertSession(t, d, "phrase-hit", "proj-search", func(s *Session) {
			s.Agent = "claude"
			s.StartedAt = new("2024-02-08T10:00:00Z")
			s.EndedAt = new("2024-02-08T11:00:00Z")
		})
		insertMessages(t, d, userMsg("phrase-hit", 0,
			"we should confirm behaviour before shipping"))

		page, err := d.Search(context.Background(), SearchFilter{
			Query: `"confirm behaviour"`, Limit: 50,
		})
		require.NoError(t, err)
		require.True(t, contains(page, "phrase-hit"), ids(page))
		for _, r := range page.Results {
			if r.SessionID == "phrase-hit" {
				assert.Contains(t, strings.ToLower(r.Snippet), "confirm")
				assert.Contains(t, strings.ToLower(r.Snippet), "behaviour")
				assert.Contains(t, r.Snippet, "<mark>")
			}
		}
	})

	t.Run("leading trailing repeated whitespace", func(t *testing.T) {
		page, err := d.Search(context.Background(), SearchFilter{Query: "  permission   denied  ", Limit: 20})
		require.NoError(t, err)
		assert.True(t, contains(page, "content-hit"), ids(page))
	})

	t.Run("hyphen identifier", func(t *testing.T) {
		page, err := d.Search(context.Background(), SearchFilter{Query: "id-hyphen", Limit: 20})
		require.NoError(t, err)
		assert.True(t, contains(page, "id-hyphen"), ids(page))
	})

	t.Run("path and punctuation", func(t *testing.T) {
		page, err := d.Search(context.Background(), SearchFilter{Query: "src/components", Limit: 20})
		require.NoError(t, err)
		assert.True(t, contains(page, "content-hit"), ids(page))
	})

	t.Run("git branch", func(t *testing.T) {
		page, err := d.Search(context.Background(), SearchFilter{Query: "feature/versatile-search", Limit: 20})
		require.NoError(t, err)
		assert.True(t, contains(page, "content-hit"), ids(page))
	})

	t.Run("tool name", func(t *testing.T) {
		page, err := d.Search(context.Background(), SearchFilter{Query: "Bash", Limit: 20})
		require.NoError(t, err)
		assert.True(t, contains(page, "tool-model"), ids(page))
	})

	t.Run("model name", func(t *testing.T) {
		page, err := d.Search(context.Background(), SearchFilter{Query: "claude-sonnet-4", Limit: 20})
		require.NoError(t, err)
		assert.True(t, contains(page, "tool-model"), ids(page))
	})

	t.Run("empty input", func(t *testing.T) {
		page, err := d.Search(context.Background(), SearchFilter{Query: "", Limit: 20})
		require.NoError(t, err)
		assert.Empty(t, page.Results)
	})

	t.Run("whitespace only", func(t *testing.T) {
		page, err := d.Search(context.Background(), SearchFilter{Query: "   \t", Limit: 20})
		require.NoError(t, err)
		assert.Empty(t, page.Results)
	})

	t.Run("no results", func(t *testing.T) {
		page, err := d.Search(context.Background(), SearchFilter{Query: "definitely-not-a-hit-xyz", Limit: 20})
		require.NoError(t, err)
		assert.Empty(t, page.Results)
	})

	t.Run("exact name ranks above partial content", func(t *testing.T) {
		page, err := d.Search(context.Background(), SearchFilter{Query: "Claude Code", Limit: 20})
		require.NoError(t, err)
		require.NotEmpty(t, page.Results)
		assert.Equal(t, "exact-name", page.Results[0].SessionID)
	})

	t.Run("project filter still applies", func(t *testing.T) {
		page, err := d.Search(context.Background(), SearchFilter{
			Query: "claude", Project: "agentsview", Limit: 20,
		})
		require.NoError(t, err)
		assert.True(t, contains(page, "cross-field"), ids(page))
		assert.False(t, contains(page, "content-hit"), ids(page))
	})

	t.Run("regex metacharacters are literal", func(t *testing.T) {
		page, err := d.Search(context.Background(), SearchFilter{Query: ".*(", Limit: 20})
		require.NoError(t, err)
		assert.Empty(t, page.Results)
	})
}

func TestSearchSessionWhitespaceAndMultiWord(t *testing.T) {
	t.Parallel()
	d := testDB(t)
	insertSession(t, d, "s1", "proj")
	insertMessages(t, d,
		userMsg("s1", 0, "permission denied on write"),
		asstMsg("s1", 1, "unrelated"),
	)

	got, err := d.SearchSession(context.Background(), "s1", "  PERMISSION  ")
	require.NoError(t, err)
	assert.Equal(t, []int{0}, got)

	got, err = d.SearchSession(context.Background(), "s1", "denied permission")
	require.NoError(t, err)
	assert.Equal(t, []int{0}, got)

	got, err = d.SearchSession(context.Background(), "s1", `"permission denied"`)
	require.NoError(t, err)
	assert.Equal(t, []int{0}, got)

	got, err = d.SearchSession(context.Background(), "s1", "   ")
	require.NoError(t, err)
	assert.Empty(t, got)
}
