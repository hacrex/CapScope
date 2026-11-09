package store

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// ErrInvalidQuery rejects a statement before it runs (not a single read query).
var ErrInvalidQuery = errors.New("invalid query")

const (
	sqlDefaultLimit = 1000
	sqlMaxLimit     = 5000
)

// SQLResult is one page of ad-hoc SQL output.
type SQLResult struct {
	Columns   []string `json:"columns"`
	Rows      [][]any  `json:"rows"`
	Truncated bool     `json:"truncated"`
	MS        int64    `json:"ms"`
}

var (
	sqlStart = regexp.MustCompile(`(?is)^(select|with)\b`)
	// sqlStringLit strips SQLite string literals; sqlDQString strips
	// double-quoted identifiers the same way ('' / "" are escaped interior
	// quotes). Real pairings and regex pairings agree because both start a
	// token at the first unpaired quote.
	sqlComments  = regexp.MustCompile(`(?s)/\*.*?\*/|--[^\n]*`)
	sqlStringLit = regexp.MustCompile(`'(?:[^']|'')*'`)
	sqlDQString  = regexp.MustCompile(`"(?:[^"]|"")*"`)
	// replace() is a legitimate scalar function; REPLACE INTO / INSERT OR
	// REPLACE are caught by the start check instead.
	sqlForbidden = regexp.MustCompile(`(?i)\b(insert|update|delete|create|drop|alter|attach|detach|reindex|vacuum|pragma|analyze|begin|commit|rollback|savepoint|release|load_extension)\b`)
)

// validateSQL admits exactly one read statement: it must start with SELECT or
// WITH (WITH can prefix DELETE, so write keywords are rejected separately),
// contain no stacked statements, and carry no write/DDL keywords outside
// string literals and comments. The read pool is also opened with
// _query_only=1, so writes fail even if this misses something.
func validateSQL(query string) (string, error) {
	q := strings.TrimSpace(query)
	if q == "" {
		return "", fmt.Errorf("%w: empty query", ErrInvalidQuery)
	}
	// Strip string literals BEFORE comments, mirroring SQLite's own parsing
	// order. If comments were stripped first, a `--` inside a string literal
	// (e.g. line='%--') would swallow the remainder of the query — including
	// stacked statements like "; DELETE FROM users" — so the bare text would
	// look harmless while SQLite still executes the full query.
	bare := sqlComments.ReplaceAllString(sqlDQString.ReplaceAllString(
		sqlStringLit.ReplaceAllString(q, " "), " "), " ")
	// Any quote that survives both passes is unbalanced/mispaired; treat the
	// query as invalid rather than guessing what SQLite will pair it with.
	if strings.ContainsAny(bare, `'"`) {
		return "", fmt.Errorf("%w: unterminated string literal", ErrInvalidQuery)
	}
	// Allow one trailing semicolon on the bare text (covers "SELECT 1; -- note");
	// any other semicolon means a stacked statement.
	bare = strings.TrimSpace(strings.TrimSuffix(strings.TrimRight(bare, " \t\r\n"), ";"))
	if !sqlStart.MatchString(q) {
		return "", fmt.Errorf("%w: only SELECT statements are allowed", ErrInvalidQuery)
	}
	if strings.Contains(bare, ";") {
		return "", fmt.Errorf("%w: multiple statements are not allowed", ErrInvalidQuery)
	}
	if m := sqlForbidden.FindString(bare); m != "" {
		return "", fmt.Errorf("%w: %s is not allowed", ErrInvalidQuery, strings.ToLower(m))
	}
	return q, nil
}

// QuerySQL runs one read-only statement over the stored tables and returns at
// most limit rows (default 1000, max 5000; truncated reports more matched).
func (s *Store) QuerySQL(ctx context.Context, query string, limit int) (SQLResult, error) {
	q, err := validateSQL(query)
	if err != nil {
		return SQLResult{}, err
	}
	if limit <= 0 {
		limit = sqlDefaultLimit
	}
	if limit > sqlMaxLimit {
		limit = sqlMaxLimit
	}
	start := time.Now()
	rs, err := s.r.QueryContext(ctx, q)
	if err != nil {
		return SQLResult{}, fmt.Errorf("%w: sql: %v", ErrInvalidQuery, err)
	}
	defer rs.Close()
	cols, err := rs.Columns()
	if err != nil {
		return SQLResult{}, fmt.Errorf("sql: %w", err)
	}
	res := SQLResult{Columns: cols, Rows: make([][]any, 0, 16)}
	vals := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	for rs.Next() {
		if len(res.Rows) >= limit {
			res.Truncated = true
			break
		}
		if err := rs.Scan(ptrs...); err != nil {
			return SQLResult{}, fmt.Errorf("sql: %w", err)
		}
		row := make([]any, len(cols))
		for i, v := range vals {
			if b, ok := v.([]byte); ok {
				v = string(b)
			}
			row[i] = v
		}
		res.Rows = append(res.Rows, row)
	}
	if err := rs.Err(); err != nil {
		return SQLResult{}, fmt.Errorf("sql: %w", err)
	}
	res.MS = time.Since(start).Milliseconds()
	return res, nil
}
