package store

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/dylin/llmchat/internal/domain"
)

// Integration tests for the Postgres store. They need a real database because
// they exercise the one-active-leaf invariant under true concurrency, which no
// fake can reproduce.
//
// Run them with a live Postgres (the docker-compose one works):
//
//	DATABASE_URL="postgres://llmchat:llmchat@127.0.0.1:5432/llmchat?sslmode=disable" \
//	  go test -race ./internal/store/
//
// Without DATABASE_URL they skip, so the default unit-test run stays hermetic.

func newLiveStore(t *testing.T) *PG {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set; skipping Postgres integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool config: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		t.Fatalf("ping database: %v", err)
	}
	t.Cleanup(pool.Close)
	return NewPG(pool)
}

// countActiveLeaf reports how many messages in a conversation are flagged
// is_active_leaf. The whole point of the partial unique index is that this is
// either 0 or 1; a concurrency bug that lets two inserts commit makes it 2.
func (p *PG) countActiveLeaf(ctx context.Context, convID uuid.UUID) (int, error) {
	var n int
	err := p.pool.QueryRow(ctx,
		`SELECT count(*) FROM messages WHERE conversation_id = $1 AND is_active_leaf`, convID).
		Scan(&n)
	return n, err
}

// newScratchConversation creates a throwaway conversation with one root message
// so tests have a leaf to append onto. Registered for cleanup.
func newScratchConversation(t *testing.T, s *PG) domain.Conversation {
	t.Helper()
	ctx := context.Background()
	conv, err := s.Create(ctx, domain.Conversation{
		ID: uuid.New(), Title: "it", OwnerID: "it", Model: "m",
	})
	if err != nil {
		t.Fatalf("create conversation: %v", err)
	}
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.Delete(c, conv.ID.String())
	})
	root := domain.Message{
		ID: uuid.New(), ConversationID: conv.ID,
		Role: domain.RoleSystem, Content: "root", Depth: 0,
	}
	if _, err := s.AppendMessage(ctx, root); err != nil {
		t.Fatalf("append root: %v", err)
	}
	return conv
}

// TestConcurrentAppendKeepsOneActiveLeaf is the regression test for the bug a
// load test found: with the clear-then-insert write path and no serialisation,
// concurrent appends to one conversation raced the partial unique index and
// roughly three-quarters of them failed with a 23505 unique violation, returned
// to the client as HTTP 500.
//
// The fix takes SELECT ... FOR UPDATE on the conversation row at the start of
// the append transaction, so writers on one conversation queue. This test fires
// N concurrent appends and asserts ALL of them succeed AND exactly one row ends
// flagged active — the invariant the index guards.
func TestConcurrentAppendKeepsOneActiveLeaf(t *testing.T) {
	s := newLiveStore(t)
	conv := newScratchConversation(t, s)

	const n = 25
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		fails int
	)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			msg := domain.Message{
				ID: uuid.New(), ConversationID: conv.ID,
				Role: domain.RoleUser, Content: "concurrent", Depth: 1,
			}
			if _, err := s.AppendMessage(ctx, msg); err != nil {
				mu.Lock()
				fails++
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()

	if fails != 0 {
		t.Fatalf("%d of %d concurrent appends failed; the one-active-leaf race is back (need SELECT ... FOR UPDATE in AppendMessage)", fails, n)
	}

	// Exactly one active leaf must remain, and it must be one of the inserted
	// messages (a leaf count of 0 would also be a bug: losing the pointer).
	ctx := context.Background()
	active, err := s.countActiveLeaf(ctx, conv.ID)
	if err != nil {
		t.Fatalf("count active leaf: %v", err)
	}
	if active != 1 {
		t.Fatalf("active leaf count = %d, want exactly 1 (race left zero or multiple)", active)
	}
}

// TestConcurrentSwitchBranchKeepsOneActiveLeaf is the same race on SwitchBranch:
// switching to two different leaves at once must not yield two active leaves.
func TestConcurrentSwitchBranchKeepsOneActiveLeaf(t *testing.T) {
	s := newLiveStore(t)
	conv := newScratchConversation(t, s)
	ctx := context.Background()

	// Build two sibling leaves under the root so we have two targets.
	leafA := domain.Message{ID: uuid.New(), ConversationID: conv.ID,
		Role: domain.RoleUser, Content: "A", Depth: 1}
	leafB := domain.Message{ID: uuid.New(), ConversationID: conv.ID,
		Role: domain.RoleUser, Content: "B", Depth: 1}
	if _, err := s.AppendMessage(ctx, leafA); err != nil {
		t.Fatal(err)
	}
	// Append leafB parented to the root directly (sibling of A).
	root, err := s.Branch(ctx, leafA.ID.String())
	if err != nil || len(root) == 0 {
		t.Fatalf("branch to find root: %v", err)
	}
	rootID := root[0].ID
	leafB.ParentID = &rootID
	if _, err := s.AppendMessage(ctx, leafB); err != nil {
		t.Fatal(err)
	}

	// Hammer switches between the two leaves concurrently.
	const n = 20
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		fails int
	)
	for i := 0; i < n; i++ {
		target := leafA.ID
		if i%2 == 0 {
			target = leafB.ID
		}
		wg.Add(1)
		go func(target uuid.UUID) {
			defer wg.Done()
			c, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			if err := s.SwitchBranch(c, conv.ID.String(), target.String()); err != nil {
				mu.Lock()
				fails++
				mu.Unlock()
			}
		}(target)
	}
	wg.Wait()

	if fails != 0 {
		t.Fatalf("%d of %d concurrent switches failed; SwitchBranch needs SELECT ... FOR UPDATE", fails, n)
	}
	active, err := s.countActiveLeaf(ctx, conv.ID)
	if err != nil {
		t.Fatalf("count active leaf: %v", err)
	}
	if active != 1 {
		t.Fatalf("active leaf count = %d, want exactly 1 after concurrent switches", active)
	}
}
