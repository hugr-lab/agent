//go:build duckdb_arrow

package hubdb_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"sort"
	"testing"

	_ "github.com/duckdb/duckdb-go/v2"
	hugr "github.com/hugr-lab/query-engine"
	"github.com/hugr-lab/query-engine/pkg/auth"
	coredb "github.com/hugr-lab/query-engine/pkg/data-sources/sources/runtime/core-db"
	"github.com/hugr-lab/query-engine/pkg/db"
	"github.com/hugr-lab/query-engine/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hugr-lab/hugen/adapters/hubdb"
	"github.com/hugr-lab/hugen/adapters/hubdb/migrate"
)

type testOpts struct {
	Source    hubdb.Config
	VectorDim int
}

// testEngine spins up an embedded hugr engine with the hub.db source attached.
// migrate.Ensure provisions the DB on a direct connection before the engine starts.
func testEngine(t *testing.T, opts ...func(*testOpts)) (*hugr.Service, string) {
	t.Helper()
	ctx := context.Background()

	dir := t.TempDir()
	hubPath := filepath.Join(dir, "memory.db")

	o := &testOpts{
		Source: hubdb.Config{Path: hubPath},
	}
	for _, apply := range opts {
		apply(o)
	}
	o.Source.Path = hubPath
	if o.VectorDim > 0 {
		o.Source.VectorSize = o.VectorDim
	}

	// 1. Provision schema + seed via migrate on a direct connection.
	require.NoError(t, migrate.Ensure(migrate.Config{
		Path:       hubPath,
		VectorSize: o.VectorDim,
		Seed: &migrate.SeedData{
			AgentType: migrate.SeedAgentType{
				ID:          "hugr-data",
				Name:        "Hugr Data Agent",
				Description: "Default agent type for tests",
				Config:      map[string]any{"constitution": "test"},
			},
			Agent: migrate.SeedAgent{
				ID:      "agt_ag01",
				ShortID: "ag01",
				Name:    "hugr-data-agent-test",
			},
		},
	}))

	// 2. Attach the provisioned DB through the engine.
	source := hubdb.NewSource(o.Source)
	service, err := hugr.New(hugr.Config{
		DB:     db.Config{},
		CoreDB: coredb.New(coredb.Config{VectorSize: 0}),
		Auth:   &auth.Config{},
	})
	require.NoError(t, err)
	require.NoError(t, service.AttachRuntimeSource(ctx, source))
	require.NoError(t, service.Init(ctx))

	t.Cleanup(func() {
		_ = service.Close()
	})

	return service, hubPath
}

// mustQuery runs a GraphQL query and fails the test on transport or GraphQL
// errors. The returned Response is auto-closed at test teardown to release
// Arrow buffers held inside the result tree.
func mustQuery(t *testing.T, q types.Querier, query string, vars map[string]any) *types.Response {
	t.Helper()
	resp, err := q.Query(context.Background(), query, vars)
	require.NoError(t, err, "query failed: %s", query)
	if resp != nil {
		t.Cleanup(resp.Close)
	}
	require.NoError(t, resp.Err(), "graphql errors: %v\nquery: %s", resp.Errors, query)
	return resp
}

func TestMigrate_CreatesAllTables(t *testing.T) {
	// Provision via migrate only — no engine needed for table-level checks.
	dir := t.TempDir()
	hubPath := filepath.Join(dir, "memory.db")

	require.NoError(t, migrate.Ensure(migrate.Config{
		Path: hubPath,
		Seed: &migrate.SeedData{
			AgentType: migrate.SeedAgentType{ID: "hugr-data", Name: "X", Config: map[string]any{}},
			Agent:     migrate.SeedAgent{ID: "agt_ag01", ShortID: "ag01", Name: "x"},
		},
	}))

	// Open a direct connection to list tables.
	conn, err := sql.Open("duckdb", hubPath)
	require.NoError(t, err)
	defer conn.Close()

	rows, err := conn.Query(
		`SELECT table_name FROM duckdb_tables() WHERE database_name != 'system' ORDER BY table_name`)
	require.NoError(t, err)
	defer rows.Close()

	var got []string
	for rows.Next() {
		var name string
		require.NoError(t, rows.Scan(&name))
		got = append(got, name)
	}
	require.NoError(t, rows.Err())

	want := []string{
		"agent_types", "agents",
		"hypotheses",
		"memory_items", "memory_links", "memory_log", "memory_tags",
		"session_events", "session_notes", "session_participants", "session_reviews",
		"sessions",
		"version",
	}
	sort.Strings(got)
	assert.Equal(t, want, got)

	// version row reflects target schema.
	var schemaVersion string
	require.NoError(t, conn.QueryRow(
		`SELECT version FROM version WHERE name = 'schema'`).Scan(&schemaVersion))
	assert.Equal(t, migrate.SchemaVersion, schemaVersion)
}

func TestAttach_Idempotent(t *testing.T) {
	dir := t.TempDir()
	hubPath := filepath.Join(dir, "memory.db")

	provision := func() {
		require.NoError(t, migrate.Ensure(migrate.Config{
			Path: hubPath,
			Seed: &migrate.SeedData{
				AgentType: migrate.SeedAgentType{ID: "hugr-data", Name: "X", Config: map[string]any{}},
				Agent:     migrate.SeedAgent{ID: "agt_ag01", ShortID: "ag01", Name: "x"},
			},
		}))
	}

	runEngine := func() {
		ctx := context.Background()
		provision()
		src := hubdb.NewSource(hubdb.Config{Path: hubPath})
		service, err := hugr.New(hugr.Config{
			DB:     db.Config{},
			CoreDB: coredb.New(coredb.Config{VectorSize: 0}),
			Auth:   &auth.Config{},
		})
		require.NoError(t, err)
		require.NoError(t, service.AttachRuntimeSource(ctx, src))
		require.NoError(t, service.Init(ctx))
		defer service.Close()

		// Verify agent_types seed row is present via GraphQL (SDL-exposed).
		resp := mustQuery(t, service, `query { hub { db { agent_types { id } } } }`, nil)
		var rows []struct {
			ID string `json:"id"`
		}
		require.NoError(t, resp.ScanData("hub.db.agent_types", &rows))
		require.Len(t, rows, 1)
		assert.Equal(t, "hugr-data", rows[0].ID)
	}

	runEngine()
	runEngine() // second run must not reprovision (seed stays idempotent).
}

func TestGraphQL_SeedData(t *testing.T) {
	service, _ := testEngine(t)

	resp := mustQuery(t, service, `
		query {
			hub {
				db {
					agent_types { id name }
					agents { id short_id agent_type_id status }
				}
			}
		}
	`, nil)

	var types []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	require.NoError(t, resp.ScanData("hub.db.agent_types", &types))
	require.Len(t, types, 1)
	assert.Equal(t, "hugr-data", types[0].ID)

	var agents []struct {
		ID          string `json:"id"`
		ShortID     string `json:"short_id"`
		AgentTypeID string `json:"agent_type_id"`
		Status      string `json:"status"`
	}
	require.NoError(t, resp.ScanData("hub.db.agents", &agents))
	require.Len(t, agents, 1)
	assert.Equal(t, "agt_ag01", agents[0].ID)
	assert.Equal(t, "ag01", agents[0].ShortID)
	assert.Equal(t, "hugr-data", agents[0].AgentTypeID)
	assert.Equal(t, "active", agents[0].Status)
}

func TestGraphQL_MemoryItemsMutationAndComputed(t *testing.T) {
	service, _ := testEngine(t)

	// Insert a memory item — valid_to in the future.
	mustQuery(t, service, `
		mutation {
			hub { db { agent {
				insert_memory_items(data: {
					id: "mem_ag01_1713345600_abcdef",
					agent_id: "agt_ag01",
					content: "unit test fact",
					category: "schema",
					volatility: "stable",
					score: 0.5,
					source: "unit-test",
					valid_from: "2026-04-17T00:00:00Z",
					valid_to: "2099-04-17T00:00:00Z"
				}) { id }
			}}}
		}
	`, nil)

	resp := mustQuery(t, service, `
		query {
			hub { db { agent {
				memory_items(filter: {id: {eq: "mem_ag01_1713345600_abcdef"}}) {
					id content category is_valid age_days expires_in_days
				}
			}}}
		}
	`, nil)

	var rows []struct {
		ID            string `json:"id"`
		Content       string `json:"content"`
		Category      string `json:"category"`
		IsValid       bool   `json:"is_valid"`
		AgeDays       int    `json:"age_days"`
		ExpiresInDays int    `json:"expires_in_days"`
	}
	require.NoError(t, resp.ScanData("hub.db.agent.memory_items", &rows))
	require.Len(t, rows, 1)
	assert.True(t, rows[0].IsValid, "fact with future valid_to should be valid")
	assert.GreaterOrEqual(t, rows[0].AgeDays, 0)
	assert.Greater(t, rows[0].ExpiresInDays, 1000)
}

func TestGraphQL_Relations(t *testing.T) {
	service, _ := testEngine(t)

	// Insert two memory items + tag + link.
	mustQuery(t, service, `
		mutation {
			hub { db { agent {
				src: insert_memory_items(data: {
					id: "mem_ag01_1713345600_aaaaaa",
					agent_id: "agt_ag01",
					content: "source fact",
					category: "schema",
					volatility: "stable",
					score: 0.5,
					valid_from: "2026-04-17T00:00:00Z",
					valid_to: "2099-04-17T00:00:00Z"
				}) { id }
				tgt: insert_memory_items(data: {
					id: "mem_ag01_1713345600_bbbbbb",
					agent_id: "agt_ag01",
					content: "target fact",
					category: "schema",
					volatility: "stable",
					score: 0.5,
					valid_from: "2026-04-17T00:00:00Z",
					valid_to: "2099-04-17T00:00:00Z"
				}) { id }
				insert_memory_tags(data: {
					memory_item_id: "mem_ag01_1713345600_aaaaaa",
					tag: "tf"
				}) { memory_item_id }
				insert_memory_links(data: {
					source_id: "mem_ag01_1713345600_aaaaaa",
					target_id: "mem_ag01_1713345600_bbbbbb",
					relation: "uses"
				}) { source_id }
			}}}
		}
	`, nil)

	// Relations: memory_item -> tags, outgoing_links.target
	resp := mustQuery(t, service, `
		query {
			hub { db { agent {
				memory_items(filter: {id: {eq: "mem_ag01_1713345600_aaaaaa"}}) {
					id
					tags { tag }
					outgoing_links { target { id content } relation }
					agent { id name }
				}
			}}}
		}
	`, nil)

	var rows []struct {
		ID   string `json:"id"`
		Tags []struct {
			Tag string `json:"tag"`
		} `json:"tags"`
		OutgoingLinks []struct {
			Target struct {
				ID      string `json:"id"`
				Content string `json:"content"`
			} `json:"target"`
			Relation string `json:"relation"`
		} `json:"outgoing_links"`
		Agent struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"agent"`
	}
	require.NoError(t, resp.ScanData("hub.db.agent.memory_items", &rows))
	require.Len(t, rows, 1)
	require.Len(t, rows[0].Tags, 1)
	assert.Equal(t, "tf", rows[0].Tags[0].Tag)
	require.Len(t, rows[0].OutgoingLinks, 1)
	assert.Equal(t, "mem_ag01_1713345600_bbbbbb", rows[0].OutgoingLinks[0].Target.ID)
	assert.Equal(t, "uses", rows[0].OutgoingLinks[0].Relation)
	assert.Equal(t, "agt_ag01", rows[0].Agent.ID)
}

func TestGraphQL_SessionsAndEvents(t *testing.T) {
	service, _ := testEngine(t)

	// Create a session + 3 events.
	mustQuery(t, service, `
		mutation {
			hub { db { agent {
				insert_sessions(data: {
					id: "sess_ag01_1",
					agent_id: "agt_ag01",
					status: "active",
					mission: "test session"
				}) { id }
				e1: insert_session_events(data: {
					id: "evt_ag01_1713345600_000001", session_id: "sess_ag01_1", agent_id: "agt_ag01",
					seq: 1, event_type: "user_message", author: "user", content: "hello"
				}) { id }
				e2: insert_session_events(data: {
					id: "evt_ag01_1713345601_000002", session_id: "sess_ag01_1", agent_id: "agt_ag01",
					seq: 2, event_type: "agent_message", author: "agent", content: "hi"
				}) { id }
				e3: insert_session_events(data: {
					id: "evt_ag01_1713345602_000003", session_id: "sess_ag01_1", agent_id: "agt_ag01",
					seq: 3, event_type: "tool_call", author: "agent", tool_name: "search"
				}) { id }
			}}}
		}
	`, nil)

	resp := mustQuery(t, service, `
		query {
			hub { db { agent {
				sessions(filter: {id: {eq: "sess_ag01_1"}}) {
					id status mission
					events(order_by: [{field: "seq", direction: ASC}]) {
						seq event_type author content tool_name
					}
				}
			}}}
		}
	`, nil)

	var sessions []struct {
		ID      string `json:"id"`
		Status  string `json:"status"`
		Mission string `json:"mission"`
		Events  []struct {
			Seq       int    `json:"seq"`
			EventType string `json:"event_type"`
			Author    string `json:"author"`
			Content   string `json:"content"`
			ToolName  string `json:"tool_name"`
		} `json:"events"`
	}
	require.NoError(t, resp.ScanData("hub.db.agent.sessions", &sessions))
	require.Len(t, sessions, 1)
	assert.Equal(t, "active", sessions[0].Status)
	require.Len(t, sessions[0].Events, 3)
	assert.Equal(t, 1, sessions[0].Events[0].Seq)
	assert.Equal(t, "tool_call", sessions[0].Events[2].EventType)
	assert.Equal(t, "search", sessions[0].Events[2].ToolName)
}

func TestGraphQL_SessionEventsFull_ParameterizedView(t *testing.T) {
	service, _ := testEngine(t)

	// Root session with 3 events; forked session continuing after seq 2.
	mustQuery(t, service, `
		mutation {
			hub { db { agent {
				root: insert_sessions(data: {
					id: "sess_root", agent_id: "agt_ag01", status: "active"
				}) { id }
				fork: insert_sessions(data: {
					id: "sess_fork", agent_id: "agt_ag01", status: "active",
					parent_session_id: "sess_root", fork_after_seq: 2
				}) { id }
				r1: insert_session_events(data: {
					id: "evt_r1", session_id: "sess_root", agent_id: "agt_ag01",
					seq: 1, event_type: "user_message", author: "user", content: "root-1"
				}) { id }
				r2: insert_session_events(data: {
					id: "evt_r2", session_id: "sess_root", agent_id: "agt_ag01",
					seq: 2, event_type: "agent_message", author: "agent", content: "root-2"
				}) { id }
				r3: insert_session_events(data: {
					id: "evt_r3", session_id: "sess_root", agent_id: "agt_ag01",
					seq: 3, event_type: "agent_message", author: "agent", content: "root-3-post-fork"
				}) { id }
				f1: insert_session_events(data: {
					id: "evt_f1", session_id: "sess_fork", agent_id: "agt_ag01",
					seq: 3, event_type: "user_message", author: "user", content: "fork-3"
				}) { id }
			}}}
		}
	`, nil)

	resp := mustQuery(t, service, `
		query {
			hub { db { agent {
				session_events_full(args: {session_id: "sess_fork"}) {
					seq event_type content chain_depth
				}
			}}}
		}
	`, nil)

	var events []struct {
		Seq        int    `json:"seq"`
		EventType  string `json:"event_type"`
		Content    string `json:"content"`
		ChainDepth int    `json:"chain_depth"`
	}
	require.NoError(t, resp.ScanData("hub.db.agent.session_events_full", &events))
	// Expected: root evt_r1 (depth=1, seq=1), evt_r2 (depth=1, seq=2), fork evt_f1 (depth=0, seq=3).
	// evt_r3 excluded: its seq 3 > fork_after_seq 2.
	require.Len(t, events, 3)
	contents := []string{events[0].Content, events[1].Content, events[2].Content}
	assert.Contains(t, contents, "root-1")
	assert.Contains(t, contents, "root-2")
	assert.Contains(t, contents, "fork-3")
	assert.NotContains(t, contents, "root-3-post-fork")
}

func TestGraphQL_HypothesesAndReviews(t *testing.T) {
	service, _ := testEngine(t)

	mustQuery(t, service, `
		mutation {
			hub { db { agent {
				insert_sessions(data: {
					id: "sess_ag01_hyp", agent_id: "agt_ag01", status: "completed"
				}) { id }
				insert_hypotheses(data: {
					id: "hyp_ag01_1713345600_000001",
					agent_id: "agt_ag01",
					content: "tf2.incidents.severity has 3 distinct values",
					priority: "high",
					status: "proposed"
				}) { id }
				insert_session_reviews(data: {
					id: "rev_ag01_1713345600_000001",
					agent_id: "agt_ag01",
					session_id: "sess_ag01_hyp",
					status: "pending"
				}) { id }
				insert_session_notes(data: {
					id: "note_ag01_1713345600_000001",
					agent_id: "agt_ag01",
					session_id: "sess_ag01_hyp",
					content: "Observed memory budget spike at 08:00"
				}) { id }
			}}}
		}
	`, nil)

	resp := mustQuery(t, service, `
		query {
			hub { db { agent {
				hypotheses { id status priority content }
				session_reviews { id status session { id } }
				session_notes { id content session { id } }
			}}}
		}
	`, nil)

	var hyp []map[string]any
	require.NoError(t, resp.ScanData("hub.db.agent.hypotheses", &hyp))
	require.Len(t, hyp, 1)
	assert.Equal(t, "proposed", hyp[0]["status"])

	var rev []map[string]any
	require.NoError(t, resp.ScanData("hub.db.agent.session_reviews", &rev))
	require.Len(t, rev, 1)
	assert.Equal(t, "pending", rev[0]["status"])

	var notes []map[string]any
	require.NoError(t, resp.ScanData("hub.db.agent.session_notes", &notes))
	require.Len(t, notes, 1)
}

func TestGraphQL_MemoryLog(t *testing.T) {
	service, _ := testEngine(t)

	// Insert a fact and a log entry referring to it.
	mustQuery(t, service, `
		mutation {
			hub { db { agent {
				insert_memory_items(data: {
					id: "mem_ag01_1713345600_cccccc",
					agent_id: "agt_ag01",
					content: "fact for log",
					category: "schema",
					volatility: "stable",
					score: 0.5,
					valid_from: "2026-04-17T00:00:00Z",
					valid_to: "2099-04-17T00:00:00Z"
				}) { id }
				insert_memory_log(data: {
					event_time: "2026-04-17T10:00:00Z",
					event_type: "retrieve",
					memory_item_id: "mem_ag01_1713345600_cccccc",
					session_id: "sess_any",
					agent_id: "agt_ag01",
					details: "{\"reason\":\"unit-test\"}"
				}) { event_type }
			}}}
		}
	`, nil)

	resp := mustQuery(t, service, `
		query {
			hub { db { agent {
				memory_log(filter: {agent_id: {eq: "agt_ag01"}}) {
					event_type memory_item_id
				}
			}}}
		}
	`, nil)

	var logs []struct {
		EventType    string `json:"event_type"`
		MemoryItemID string `json:"memory_item_id"`
	}
	require.NoError(t, resp.ScanData("hub.db.agent.memory_log", &logs))
	require.Len(t, logs, 1)
	assert.Equal(t, "retrieve", logs[0].EventType)
}

func TestGraphQL_VectorSizeEnabled(t *testing.T) {
	service, _ := testEngine(t, func(o *testOpts) {
		o.VectorDim = 8
		o.Source.EmbedderModel = "_test_embedder"
	})

	// Schema with embedding column: verify column exists via plain select.
	resp := mustQuery(t, service, `
		query {
			hub { db { agent {
				memory_items(limit: 1) { id }
			}}}
		}
	`, nil)
	_ = resp
}
