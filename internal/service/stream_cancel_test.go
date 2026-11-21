package service

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/dylin/llmchat/internal/contextpack"
	"github.com/dylin/llmchat/internal/domain"
	"github.com/dylin/llmchat/internal/provider"
)

// The tests in this file exist to lock one specific regression, which is the
// most serious bug this project ever shipped:
//
//	persistStream forwarded chunks with
//		for c := range in { select { case out <- c: ... case <-ctx.Done(): ... } }
//	which looks like it honours cancellation but does not. Ranging over the
//	upstream channel drains it to closure regardless of ctx, and once the client
//	has disconnected the buffered forward channel still accepts writes, so the
//	send case always wins and the ctx case is never selected. Cancellation only
//	appeared to work in tests that happened to overrun the 32-slot buffer; in
//	production it did nothing, so a closed browser tab kept the model generating
//	and kept paying for tokens nobody would read.
//
// The fix selects on <-ctx.Done() around the RECEIVE, not just the send. These
// tests would fail against the old loop.

// fakeProvider streams a fixed number of word-shaped chunks on a timer, then
// blocks until either it has produced everything (normal) or ctx is cancelled
// (disconnect). It records how many chunks it actually emitted so a test can
// assert generation truly stopped, and a channel to learn whether the consumer
// kept draining after we tried to cancel.
// fakeProvider streams a fixed number of word-shaped chunks on a timer.
//
// Detecting the real bug requires matching its mechanism exactly, so this fake is
// subtle and the subtlety is load-bearing.
//
// The bug was NOT an absent ctx check: the old loop did have a ctx.Done case in
// its *send* select. The bug was that cancellation was checked only AFTER a
// chunk was received, because `for c := range in` blocks in the receive itself.
// When the client disconnects, the producer (a real HTTP body read) may go quiet
// for a while — nothing arrives on `in`, so the loop sits in the range's implicit
// receive and never re-evaluates any ctx check. It resumes only when the next
// chunk arrives. The fixed loop selects on ctx around the receive, so it exits
// immediately even if the producer has gone completely silent.
//
// Therefore this fake, on goSilent(), stops handing off further chunks WITHOUT
// closing the channel. That is the only condition that separates the two loops:
//   - fixed loop: exits on ctx.Done in the receive select, closes out; test sees
//     the stream end promptly.
//   - buggy loop: stuck forever in `range in` (no chunk, channel still open), so
//     out never closes; the test's deadline trips and it FAILS, not hangs.
//
// produced reports how many chunks were handed off, for the control test.
type fakeProvider struct {
	chunks   []string
	interval time.Duration
	produced chan int
	// goSilent is closed to make the provider stop producing *without* closing
	// the stream channel, simulating an upstream that has not yet noticed the
	// disconnect. This is what forces cancellation to be detected at the receive.
	goSilent chan struct{}
	// release is closed by test cleanup so a provider parked in goSilent can
	// exit instead of leaking a goroutine past the test.
	release chan struct{}
}

func newFakeProvider(words int, interval time.Duration) *fakeProvider {
	p := &fakeProvider{
		interval: interval,
		produced: make(chan int, 1),
		goSilent: make(chan struct{}),
		release:  make(chan struct{}),
	}
	p.chunks = make([]string, words)
	for i := range p.chunks {
		p.chunks[i] = "word "
	}
	return p
}

func (p *fakeProvider) Name() string { return "fake" }

func (p *fakeProvider) Stream(_ context.Context, _ provider.Request) (<-chan provider.Chunk, error) {
	out := make(chan provider.Chunk) // unbuffered
	quiet := func(n int) {
		// Report the count, then hold the channel OPEN (do not close, do not
		// return) until the test releases us: a stalled read has not closed.
		select {
		case p.produced <- n:
		default:
		}
		<-p.release
	}
	go func() {
		defer close(out)
		n := 0
		for _, c := range p.chunks {
			select {
			case <-p.goSilent:
				quiet(n)
				return
			default:
			}
			timer := time.NewTimer(p.interval)
			select {
			case <-timer.C:
			case <-p.goSilent:
				timer.Stop()
				quiet(n)
				return
			}
			select {
			case out <- provider.Chunk{Content: c}:
				n++
			case <-p.goSilent:
				quiet(n)
				return
			}
		}
		select {
		case p.produced <- n:
		default:
		}
	}()
	return out, nil
}

// goQuiet tells the provider to stop producing without closing the stream.
// Safe to call multiple times.
func (p *fakeProvider) goQuiet() {
	select {
	case <-p.goSilent:
	default:
		close(p.goSilent)
	}
}

// releaseParking frees a provider parked after goQuiet, so cleanup leaves no
// goroutine behind.
func (p *fakeProvider) releaseParking() { close(p.release) }

func (p *fakeProvider) Complete(_ context.Context, _ provider.Request) (string, provider.Usage, error) {
	return "summary", provider.Usage{}, nil
}

// fakeStore records appended messages so tests can inspect the persisted tree
// without a database. The saved channel is the coordination point: it is
// signalled once per AppendMessage with the saved message and the context it was
// saved under, so a test waits on the actual save rather than guessing at
// goroutine timing. (An earlier version polled a flag and read the reply from
// the *provider's* completion signal, which fires before the consumer's save
// runs — a race that made assertions flaky.)
type fakeStore struct {
	mu       sync.Mutex
	conv     domain.Conversation
	byId     map[uuid.UUID]domain.Message
	appended []domain.Message

	saved chan savedMsg

	// saveBlock, when non-nil, makes AppendMessage wait for it to close — so a
	// test can prove a disconnect-time save survives the request context being
	// cancelled mid-save.
	saveBlock chan struct{}
}

// savedMsg carries what a test needs after a save completes.
type savedMsg struct {
	msg domain.Message
	// errAtSave is ctx.Err() sampled synchronously inside AppendMessage, i.e.
	// the liveness of the save context at the moment persistence ran. It must be
	// read at save time, not later: finish() runs `defer cancel()` on the
	// detached timeout context when it returns, so a ctx handle stored and
	// inspected afterwards would read "canceled" purely because the save had
	// already completed — an artifact of the test, not a real detachment failure.
	errAtSave error
}

func newFakeStore(conv domain.Conversation) *fakeStore {
	return &fakeStore{
		conv:  conv,
		byId:  map[uuid.UUID]domain.Message{},
		saved: make(chan savedMsg, 64),
	}
}

func (f *fakeStore) Create(_ context.Context, c domain.Conversation) (domain.Conversation, error) {
	return c, nil
}

func (f *fakeStore) Get(_ context.Context, id string) (domain.Conversation, error) {
	if uuid.MustParse(id) != f.conv.ID {
		return domain.Conversation{}, domain.ErrNotFound
	}
	return f.conv, nil
}

func (f *fakeStore) List(context.Context, string, int, int) ([]domain.Conversation, error) {
	return nil, nil
}
func (f *fakeStore) Touch(context.Context, string, string) error { return nil }
func (f *fakeStore) SetCurrentLeaf(context.Context, string, string) error {
	return nil
}
func (f *fakeStore) Delete(context.Context, string) error { return nil }

func (f *fakeStore) AppendMessage(ctx context.Context, m domain.Message) (domain.Message, error) {
	f.mu.Lock()
	// Block only the assistant save. Send writes the user message synchronously
	// before returning the stream; blocking that too would deadlock Send itself
	// before the test ever gets a chance to cancel.
	if f.saveBlock != nil && m.Role == domain.RoleAssistant {
		f.mu.Unlock()
		<-f.saveBlock
		f.mu.Lock()
	}
	f.appended = append(f.appended, m)
	f.byId[m.ID] = m
	f.conv.CurrentLeaf = m.ID
	errAtSave := ctx.Err()
	f.mu.Unlock()
	f.saved <- savedMsg{msg: m, errAtSave: errAtSave}
	return m, nil
}

func (f *fakeStore) Branch(_ context.Context, leafID string) (domain.Branch, error) {
	// Walk parents from leafID to root, oldest first.
	var out domain.Branch
	id := uuid.MustParse(leafID)
	for {
		m, ok := f.byId[id]
		if !ok {
			break
		}
		out = append([]domain.Message{m}, out...)
		if m.ParentID == nil {
			break
		}
		id = *m.ParentID
	}
	return out, nil
}

func (f *fakeStore) Tree(context.Context, string) ([]domain.Message, error) { return nil, nil }
func (f *fakeStore) Children(context.Context, string) ([]domain.Message, error) {
	return nil, nil
}
func (f *fakeStore) SwitchBranch(context.Context, string, string) error { return nil }

func (f *fakeStore) NextDepth(_ context.Context, _, parentID string) (int, error) {
	if parentID == "" {
		return 0, nil
	}
	if p, ok := f.byId[uuid.MustParse(parentID)]; ok {
		return p.Depth + 1, nil
	}
	return 0, nil
}

func (f *fakeStore) UpdateMessageContent(_ context.Context, id, content string, tokens int) (domain.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m := f.byId[uuid.MustParse(id)]
	m.Content = content
	m.TokenCount = tokens
	return m, nil
}

// awaitAssistant waits for the persist goroutine to save an assistant message
// and returns it with the context it was saved under. It is the single
// coordination point every test uses, replacing sleep-and-poll.
//
// It must skip the user-message save that Send performs before streaming: both
// go through the same AppendMessage, so the first signal is the user turn and
// the assistant turn is the one carrying the reply.
func (f *fakeStore) awaitAssistant(t *testing.T, within time.Duration) savedMsg {
	t.Helper()
	deadline := time.After(within)
	for {
		select {
		case s := <-f.saved:
			if s.msg.Role == domain.RoleAssistant {
				return s
			}
			// A user turn saved first; keep waiting for the reply.
		case <-deadline:
			t.Fatal("timed out waiting for the assistant reply to be saved")
			return savedMsg{}
		}
	}
}

func baseCfg() Config {
	return Config{
		Strategy:     contextpack.StrategyTruncate,
		MaxContext:   8000,
		ReserveReply: 500,
		ReserveInput: 200,
	}
}

func newTestService(conv domain.Conversation, prov provider.Provider) (*Service, *fakeStore) {
	fs := newFakeStore(conv)
	svc := New(fs, fs, prov, baseCfg(), nil)
	return svc, fs
}

// TestStreamCancelStopsGenerationEarly is the core regression test and the
// reason this file exists.
//
// The disconnect is simulated exactly as it happens in production: the request
// context is cancelled AND the upstream goes quiet (stops handing off chunks
// without closing its channel, because a real HTTP read has not yet noticed the
// peer vanished). We trigger both together.
//
//   - Fixed loop selects on ctx.Done in the receive, so it returns the instant
//     the context is cancelled, closes the stream, and `closed` fires.
//   - Buggy `for c := range in` is parked in the receive. No chunk arrives (the
//     provider went quiet) and the channel is never closed, so it cannot reach
//     its send-side ctx check. `out` never closes and this test FAILS on the
//     deadline — deterministically, not by hanging, because the deadline is
//     explicit.
//
// The earlier version of this test closed the provider channel on cancel, which
// let the buggy range-loop exit on channel-close and pass. That is the trap to
// remember: a fake that cooperates with cancellation cannot detect a loop that
// fails to observe cancellation.
func TestStreamCancelStopsGenerationEarly(t *testing.T) {
	conv := domain.Conversation{ID: uuid.New(), Model: "test"}
	fp := newFakeProvider(200, time.Millisecond)
	t.Cleanup(fp.releaseParking)
	svc, _ := newTestService(conv, fp)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	ch, err := svc.Send(ctx, SendMessageInput{
		ConversationID: conv.ID.String(),
		Content:        "hello",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Read a couple of chunks to prove the stream is live, then disconnect:
	// cancel the request AND make the upstream go silent, as a real peer
	// disconnect looks from inside persistStream.
	closed := make(chan struct{})
	go func() {
		got := 0
		for range ch {
			got++
			if got == 2 {
				fp.goQuiet()
				cancel()
			}
		}
		close(closed)
	}()

	select {
	case <-closed:
		// Correct: persistStream exited on ctx.Done in the receive select.
	case <-time.After(2 * time.Second):
		t.Fatal("stream never closed after disconnect while upstream went quiet: persistStream does not detect cancellation at the receive (old range-loop bug)")
	}
}

// TestPartialReplyPersistedOnCancel proves that whatever was generated before a
// disconnect is still saved. The user who reopens the tab must see the partial
// answer, not a blank gap.
func TestPartialReplyPersistedOnCancel(t *testing.T) {
	conv := domain.Conversation{ID: uuid.New(), Model: "test"}
	fp := newFakeProvider(200, 2*time.Millisecond)
	t.Cleanup(fp.releaseParking)
	svc, fs := newTestService(conv, fp)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	ch, err := svc.Send(ctx, SendMessageInput{ConversationID: conv.ID.String(), Content: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	// Read a few so the reply buffer is non-empty, then disconnect (upstream
	// goes quiet and the request context is cancelled) and drain.
	for i := 0; i < 4; i++ {
		<-ch
	}
	fp.goQuiet()
	cancel()
	go func() {
		for range ch {
		}
	}()

	saved := fs.awaitAssistant(t, 2*time.Second)
	if saved.msg.Content == "" {
		t.Fatal("partial reply is empty; the disconnect lost the generated text")
	}
}

// TestSaveOnCancelSurvivesCancelledContext proves the persistence call uses a
// DETACHED context (context.WithoutCancel). This is the subtle half of the fix:
// the forwarding ctx is already cancelled when we cancel, so a naive AppendMessage
// that honoured ctx would fail the save and lose the partial reply.
func TestSaveOnCancelSurvivesCancelledContext(t *testing.T) {
	conv := domain.Conversation{ID: uuid.New(), Model: "test"}
	fp := newFakeProvider(200, time.Millisecond)
	t.Cleanup(fp.releaseParking)
	svc, fs := newTestService(conv, fp)

	// Block the assistant save so we can cancel the request context while the
	// save is in flight, then prove the save itself ran under a live context.
	fs.saveBlock = make(chan struct{})

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	ch, err := svc.Send(ctx, SendMessageInput{ConversationID: conv.ID.String(), Content: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	// Read one chunk to start the pipeline, then disconnect. persistStream will
	// reach finish() and block inside AppendMessage.
	<-ch
	fp.goQuiet()
	cancel()
	go func() {
		for range ch {
		}
	}()

	// Give persistStream a moment to enter the blocked save, then release it.
	time.Sleep(30 * time.Millisecond)
	close(fs.saveBlock)

	saved := fs.awaitAssistant(t, 2*time.Second)
	// The save context must have been live at the moment persistence ran. If it
	// had inherited the cancelled request context, AppendMessage's DB work would
	// abort the instant the tab closed and the partial reply would be lost.
	if saved.errAtSave != nil {
		t.Fatalf("save ran under a cancelled context (err=%v); needs context.WithoutCancel", saved.errAtSave)
	}
	if saved.msg.Content == "" {
		t.Fatal("partial reply empty after detached save")
	}
}

// TestStreamCompletesWithoutCancel is the control: with no disconnect, all
// chunks forward and the full reply is saved. Guards against a fix that fixes
// cancellation by breaking the normal path.
func TestStreamCompletesWithoutCancel(t *testing.T) {
	conv := domain.Conversation{ID: uuid.New(), Model: "test"}
	fp := newFakeProvider(20, time.Millisecond)
	t.Cleanup(fp.releaseParking)
	svc, fs := newTestService(conv, fp)

	ch, err := svc.Send(context.Background(), SendMessageInput{ConversationID: conv.ID.String(), Content: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	got := 0
	for range ch {
		got++
	}
	if got != 20 {
		t.Errorf("forwarded %d chunks, want 20", got)
	}

	saved := fs.awaitAssistant(t, 2*time.Second)
	if want := 20 * len("word "); len(saved.msg.Content) != want {
		t.Errorf("saved reply length = %d, want %d", len(saved.msg.Content), want)
	}
}
