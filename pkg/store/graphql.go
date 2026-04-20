package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/hugr-lab/query-engine/types"
)

// dbTime unmarshals both RFC3339 and DuckDB's bare-timestamp format
// ("2026-04-17 15:45:48.900887") from the GraphQL Timestamp scalar.
type dbTime struct{ time.Time }

var dbTimeLayouts = []string{
	time.RFC3339Nano,
	time.RFC3339,
	"2006-01-02 15:04:05.999999999",
	"2006-01-02 15:04:05",
}

func (t *dbTime) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "" || s == "null" {
		return nil
	}
	for _, layout := range dbTimeLayouts {
		if parsed, err := time.Parse(layout, s); err == nil {
			t.Time = parsed
			return nil
		}
	}
	return fmt.Errorf("hubdb: unparseable timestamp %q", s)
}

// runQuery executes a GraphQL query, auto-closes the response (releases Arrow
// buffers), checks for GraphQL errors, and scans the leaf at `path` into T.
// Returns a zero T and types.ErrWrongDataPath when the path does not exist.
func runQuery[T any](ctx context.Context, q types.Querier, query string, vars map[string]any, path string) (T, error) {
	var zero T
	resp, err := q.Query(ctx, query, vars)
	if err != nil {
		return zero, fmt.Errorf("hubdb query: %w", err)
	}
	defer resp.Close()
	if err := resp.Err(); err != nil {
		return zero, fmt.Errorf("hubdb graphql: %w", err)
	}
	var out T
	if err := resp.ScanData(path, &out); err != nil {
		if errors.Is(err, types.ErrWrongDataPath) || errors.Is(err, types.ErrNoData) {
			return zero, err
		}
		return zero, fmt.Errorf("hubdb scan %s: %w", path, err)
	}
	return out, nil
}

// runMutation executes a GraphQL mutation and discards the payload — callers
// use it for writes whose return value is OperationResult.affected_rows.
func runMutation(ctx context.Context, q types.Querier, mutation string, vars map[string]any) error {
	resp, err := q.Query(ctx, mutation, vars)
	if err != nil {
		return fmt.Errorf("hubdb mutation: %w", err)
	}
	defer resp.Close()
	return resp.Err()
}
