package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestParseUserQuery(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		raw  string
		want []SearchTerm
	}{
		{name: "empty", raw: "", want: nil},
		{name: "whitespace only", raw: " \t\n  ", want: nil},
		{name: "single word", raw: "claude", want: []SearchTerm{{Value: "claude"}}},
		{name: "trimmed", raw: "  claude  ", want: []SearchTerm{{Value: "claude"}}},
		{name: "repeated whitespace", raw: "fix   the\tbug", want: []SearchTerm{
			{Value: "fix"}, {Value: "the"}, {Value: "bug"},
		}},
		{name: "quoted phrase", raw: `"Claude Code"`, want: []SearchTerm{
			{Value: "Claude Code", Phrase: true},
		}},
		{name: "mixed phrase and term", raw: `"Claude Code" permission`, want: []SearchTerm{
			{Value: "Claude Code", Phrase: true}, {Value: "permission"},
		}},
		{name: "term then phrase", raw: `timeout "permission denied"`, want: []SearchTerm{
			{Value: "timeout"}, {Value: "permission denied", Phrase: true},
		}},
		{name: "hyphen token", raw: "error-401", want: []SearchTerm{{Value: "error-401"}}},
		{name: "path", raw: "src/components", want: []SearchTerm{{Value: "src/components"}}},
		{name: "embedded doubled quote", raw: `"say""hi"`, want: []SearchTerm{
			{Value: `say"hi`, Phrase: true},
		}},
		{name: "unclosed quote is phrase", raw: `"still open`, want: []SearchTerm{
			{Value: "still open", Phrase: true},
		}},
		{name: "empty quotes skipped", raw: `""`, want: nil},
		{name: "mid-token quote is literal", raw: `say"hi`, want: []SearchTerm{{Value: `say"hi`}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, ParseUserQuery(tt.raw))
		})
	}
}

func TestFTSMatchExpression(t *testing.T) {
	t.Parallel()
	assert.Equal(t, `"claude"*`, FTSMatchExpression([]SearchTerm{{Value: "claude"}}))
	assert.Equal(t, `"fix"* "bug"*`, FTSMatchExpression([]SearchTerm{
		{Value: "fix"}, {Value: "bug"},
	}))
	assert.Equal(t, `"confirm behaviour"`, FTSMatchExpression([]SearchTerm{
		{Value: "confirm behaviour", Phrase: true},
	}))
	assert.Equal(t, `"confirm"`, FTSMatchExpression([]SearchTerm{
		{Value: "confirm", Phrase: true},
	}))
	assert.Equal(t, `"Claude Code"`, FTSMatchExpression([]SearchTerm{
		{Value: "Claude Code", Phrase: true},
	}))
	assert.Equal(t, `"error-401"`, FTSMatchExpression([]SearchTerm{{Value: "error-401"}}))
	assert.Equal(t, `"src/components"`, FTSMatchExpression([]SearchTerm{{Value: "src/components"}}))
	assert.Equal(t, `"say""hi"`, FTSMatchExpression([]SearchTerm{{Value: `say"hi`}}))
}

func TestPrepareFTSQueryMixedQuotes(t *testing.T) {
	t.Parallel()
	assert.Equal(t, `"Claude Code" "permission"`, PrepareFTSQuery(`"Claude Code" permission`))
	assert.Equal(t, `"fix bug"`, PrepareFTSQuery(`"fix bug"`))
	assert.Equal(t, `"login"`, PrepareFTSQuery("login"))
}
