package executor

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeGranter records every InfoExists / AddGrant call. Tests
// configure visibility maps to drive the preflight result.
type fakeGranter struct {
	mu        sync.Mutex
	visible   map[string]bool
	grants    []grantCall
	addErr    error
}

type grantCall struct {
	artifactID string
	agentID    string
	sessionID  string
	grantedBy  string
}

func (f *fakeGranter) InfoExists(_ context.Context, _, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.visible[id] {
		return nil
	}
	return errors.New("artifacts: unknown artifact: " + id)
}

func (f *fakeGranter) AddGrant(_ context.Context, artifactID, agentID, sessionID, grantedBy string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.addErr != nil {
		return f.addErr
	}
	f.grants = append(f.grants, grantCall{
		artifactID: artifactID,
		agentID:    agentID,
		sessionID:  sessionID,
		grantedBy:  grantedBy,
	})
	return nil
}

func TestPreflightInputArtifacts_AllVisible(t *testing.T) {
	g := &fakeGranter{visible: map[string]bool{"art_a": true, "art_b": true}}
	e := &Executor{artifacts: g, agentID: "agt_ag01"}
	node := &missionNode{
		id:             "sub_1",
		inputArtifacts: []string{"art_a", "art_b"},
	}
	reason, ok := e.preflightInputArtifacts(context.Background(), "coord_x", node)
	require.True(t, ok, "reason=%q", reason)
	require.Len(t, g.grants, 2)
	assert.Equal(t, grantCall{
		artifactID: "art_a", agentID: "agt_ag01",
		sessionID: "sub_1", grantedBy: "coord_x",
	}, g.grants[0])
	assert.Equal(t, "art_b", g.grants[1].artifactID)
}

func TestPreflightInputArtifacts_VisibilityMiss(t *testing.T) {
	g := &fakeGranter{visible: map[string]bool{"art_a": true}}
	e := &Executor{artifacts: g, agentID: "agt_ag01"}
	node := &missionNode{
		id:             "sub_1",
		inputArtifacts: []string{"art_a", "art_missing"},
	}
	reason, ok := e.preflightInputArtifacts(context.Background(), "coord_x", node)
	require.False(t, ok)
	assert.Contains(t, reason, "input_artifact_unknown_or_invisible")
	assert.Contains(t, reason, "art_missing")
	// First artifact's grant was issued before the second blew up
	// — we don't roll back (idempotent).
	assert.Len(t, g.grants, 1)
}

func TestPreflightInputArtifacts_NoGranter(t *testing.T) {
	e := &Executor{artifacts: nil}
	node := &missionNode{id: "sub_1", inputArtifacts: []string{"art_x"}}
	reason, ok := e.preflightInputArtifacts(context.Background(), "coord_x", node)
	require.False(t, ok)
	assert.Contains(t, reason, "manager not wired")
}

func TestPreflightInputArtifacts_GrantError(t *testing.T) {
	g := &fakeGranter{visible: map[string]bool{"art_a": true}, addErr: errors.New("db down")}
	e := &Executor{artifacts: g, agentID: "agt_ag01"}
	node := &missionNode{id: "sub_1", inputArtifacts: []string{"art_a"}}
	reason, ok := e.preflightInputArtifacts(context.Background(), "coord_x", node)
	require.False(t, ok)
	assert.Contains(t, reason, "input_artifact_grant_failed")
	assert.Contains(t, reason, "art_a")
}
