// Package scheduler owns the agent's background work queue:
// post-session reviews (priority 10), hypothesis verification
// (priority 20), and periodic consolidation (priority 30).
//
// The scheduler is a single goroutine in standalone mode; Hub-mode
// deployments will swap in a priority-queue client without changing
// the public contract.
//
// Dependencies flow inward: the SessionManager publishes work via
// QueueReview, and the scheduler reads from HubDB + invokes functions
// in pkg/memory. Scheduler itself does not depend on pkg/session.
package scheduler
