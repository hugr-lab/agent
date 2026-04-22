// Package queries is the shared GraphQL runner used by every store
// subpackage. Thin wrappers over types.Querier.Query that auto-close
// the response and surface GraphQL errors.
package queries

import (
	"context"
	"fmt"

	"github.com/hugr-lab/query-engine/types"
)

// RunQuery executes a GraphQL query against q, closes the response,
// and returns the payload at path scanned as T. Delegates to
// types.Scan[T] which picks between slice (ScanTable) and object
// (ScanObject) destinations by reflect.
func RunQuery[T any](ctx context.Context, q types.Querier, query string, vars map[string]any, path string) (T, error) {
	var zero T
	resp, err := q.Query(ctx, query, vars)
	if err != nil {
		return zero, fmt.Errorf("hubdb query: %w", err)
	}
	defer resp.Close()
	if err := resp.Err(); err != nil {
		return zero, fmt.Errorf("hubdb graphql: %w", err)
	}
	return types.Scan[T](resp, path)
}

// RunQueryJSON is the JSON-shaped variant of RunQuery: the response is
// first marshalled to JSON and then unmarshalled into dest. Needed for
// row types that contain `json.RawMessage` fields — ScanTable/Arrow
// path cannot decode Arrow utf8 into RawMessage, while ScanData goes
// through JSON and handles it naturally.
func RunQueryJSON(ctx context.Context, q types.Querier, query string, vars map[string]any, path string, dest any) error {
	resp, err := q.Query(ctx, query, vars)
	if err != nil {
		return fmt.Errorf("hubdb query: %w", err)
	}
	defer resp.Close()
	if err := resp.Err(); err != nil {
		return fmt.Errorf("hubdb graphql: %w", err)
	}
	return resp.ScanData(path, dest)
}

// RunMutation executes a GraphQL mutation and discards the payload —
// callers use it for writes whose return value is just
// OperationResult.affected_rows.
func RunMutation(ctx context.Context, q types.Querier, mutation string, vars map[string]any) error {
	resp, err := q.Query(ctx, mutation, vars)
	if err != nil {
		return fmt.Errorf("hubdb mutation: %w", err)
	}
	defer resp.Close()
	return resp.Err()
}
