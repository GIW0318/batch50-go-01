package task

import (
	"errors"
	"testing"

	impl "gsb/batch50-go-safety-b-14"
)

func newGuard() *impl.SmtpDataFramingGuard {
	return impl.NewSmtpDataFramingGuard(impl.Limits{MaxLineLength: 80, MaxBytes: 4096})
}

func TestBeginOpensTransaction(t *testing.T) {
	g := newGuard()
	tx, err := g.Begin("sess-1", 20)
	if err != nil {
		t.Fatalf("begin must succeed, got %v", err)
	}
	if tx.ID == "" {
		t.Fatalf("transaction id must not be empty")
	}
	if tx.SessionID != "sess-1" || tx.Declared != 20 {
		t.Fatalf("transaction = %+v, want sess-1 with 20 declared bytes", tx)
	}
	state, ok := g.State("sess-1")
	if !ok {
		t.Fatalf("session must be recorded")
	}
	if state.State != impl.SessionRunning {
		t.Fatalf("state = %q, want %q", state.State, impl.SessionRunning)
	}
	if state.Received != 0 || state.Lines != 0 || state.Chunks != 0 {
		t.Fatalf("fresh session accounting = %+v, want zeros", state)
	}
	if got := len(g.Sessions()); got != 1 {
		t.Fatalf("sessions = %d, want 1", got)
	}
}

func TestBeginRejectsBadArgs(t *testing.T) {
	g := newGuard()
	if _, err := g.Begin("   ", 10); !errors.Is(err, impl.ErrBadSession) {
		t.Fatalf("blank session id must yield ErrBadSession, got %v", err)
	}
	if _, err := g.Begin("sess-bad", 0); !errors.Is(err, impl.ErrBadDeclared) {
		t.Fatalf("zero declared size must yield ErrBadDeclared, got %v", err)
	}
	if _, err := g.Begin("sess-bad", -3); !errors.Is(err, impl.ErrBadDeclared) {
		t.Fatalf("negative declared size must yield ErrBadDeclared, got %v", err)
	}
	if got := len(g.Sessions()); got != 0 {
		t.Fatalf("rejected begins must not create sessions, got %d", got)
	}
}

func TestBeginRejectsConcurrentSession(t *testing.T) {
	g := newGuard()
	if _, err := g.Begin("sess-a", 10); err != nil {
		t.Fatalf("first begin: %v", err)
	}
	if _, err := g.Begin("sess-b", 10); !errors.Is(err, impl.ErrSessionBusy) {
		t.Fatalf("second begin must yield ErrSessionBusy, got %v", err)
	}
	if _, err := g.Feed([]byte("abc\r\n")); err != nil {
		t.Fatalf("the first session must still accept data, got %v", err)
	}
	state, ok := g.State("sess-a")
	if !ok || state.Received != 5 {
		t.Fatalf("first session accounting = %+v ok=%v, want 5 bytes", state, ok)
	}
}

func TestFeedWithoutSessionRejected(t *testing.T) {
	g := newGuard()
	if _, err := g.Feed([]byte("abc\r\n")); !errors.Is(err, impl.ErrNoSession) {
		t.Fatalf("feed without a session must yield ErrNoSession, got %v", err)
	}
}

func TestFeedRejectsBareLF(t *testing.T) {
	g := newGuard()
	if _, err := g.Begin("sess-lf", 20); err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := g.Feed([]byte("hel\nlo\r\n")); !errors.Is(err, impl.ErrBareLF) {
		t.Fatalf("a bare line feed must yield ErrBareLF, got %v", err)
	}
	if _, err := g.Feed([]byte("more\r\n")); !errors.Is(err, impl.ErrNoSession) {
		t.Fatalf("the connection must be back in command state, got %v", err)
	}
	state, _ := g.State("sess-lf")
	if state.State != impl.SessionAborted {
		t.Fatalf("state = %q, want %q", state.State, impl.SessionAborted)
	}
}

func TestFeedRejectsBareCR(t *testing.T) {
	g := newGuard()
	if _, err := g.Begin("sess-cr", 20); err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := g.Feed([]byte("abc\rxyz\r\n")); !errors.Is(err, impl.ErrBareCR) {
		t.Fatalf("a carriage return without line feed must yield ErrBareCR, got %v", err)
	}
	if _, err := g.Feed([]byte("more\r\n")); !errors.Is(err, impl.ErrNoSession) {
		t.Fatalf("the connection must be back in command state, got %v", err)
	}
}

func TestFeedAccountsAcrossChunks(t *testing.T) {
	g := newGuard()
	if _, err := g.Begin("sess-acc", 16); err != nil {
		t.Fatalf("begin: %v", err)
	}
	first, err := g.Feed([]byte("abc\r\n"))
	if err != nil {
		t.Fatalf("first chunk: %v", err)
	}
	if first.Received != 5 || first.Remaining != 11 {
		t.Fatalf("first progress = %+v, want 5 received and 11 remaining", first)
	}
	if first.Lines != 1 || first.Chunks != 1 || !first.InData {
		t.Fatalf("first progress = %+v, want one line, one chunk and an open data phase", first)
	}
	second, err := g.Feed([]byte("def\r\n"))
	if err != nil {
		t.Fatalf("second chunk: %v", err)
	}
	if second.Received != 10 || second.Remaining != 6 || second.Lines != 2 || second.Chunks != 2 {
		t.Fatalf("second progress = %+v, want 10 received, 6 remaining, 2 lines, 2 chunks", second)
	}
}

func TestFeedRejectsOverflow(t *testing.T) {
	g := newGuard()
	if _, err := g.Begin("sess-over", 4); err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := g.Feed([]byte("hello")); !errors.Is(err, impl.ErrSizeOverflow) {
		t.Fatalf("more bytes than declared must yield ErrSizeOverflow, got %v", err)
	}
	if _, err := g.Feed([]byte("x")); !errors.Is(err, impl.ErrNoSession) {
		t.Fatalf("overflow must close the transaction, got %v", err)
	}
}

func TestFeedUnstuffsLeadingDots(t *testing.T) {
	g := newGuard()
	if _, err := g.Begin("sess-dot", 14); err != nil {
		t.Fatalf("begin: %v", err)
	}
	progress, err := g.Feed([]byte("..hello\r\n...\r\n"))
	if err != nil {
		t.Fatalf("feed must succeed, got %v", err)
	}
	if progress.DotStuffed != 2 {
		t.Fatalf("dot stuffed lines = %d, want 2", progress.DotStuffed)
	}
	if progress.Lines != 2 {
		t.Fatalf("lines = %d, want 2", progress.Lines)
	}
	if got := string(g.Body()); got != ".hello\r\n..\r\n" {
		t.Fatalf("body = %q, want one leading dot removed per line", got)
	}
}

func TestFeedKeepsInteriorDots(t *testing.T) {
	g := newGuard()
	if _, err := g.Begin("sess-mid", 6); err != nil {
		t.Fatalf("begin: %v", err)
	}
	progress, err := g.Feed([]byte("a..b\r\n"))
	if err != nil {
		t.Fatalf("feed must succeed, got %v", err)
	}
	if progress.DotStuffed != 0 {
		t.Fatalf("dot stuffed lines = %d, want 0 for a dot run that is not at line start", progress.DotStuffed)
	}
	if got := string(g.Body()); got != "a..b\r\n" {
		t.Fatalf("body = %q, want the interior dots untouched", got)
	}
}

func TestFinishClosesAndResets(t *testing.T) {
	g := newGuard()
	if _, err := g.Begin("sess-fin", 14); err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := g.Feed([]byte("hello\r\nworld\r\n")); err != nil {
		t.Fatalf("feed: %v", err)
	}
	if err := g.Finish("sess-fin"); err != nil {
		t.Fatalf("finish must succeed, got %v", err)
	}
	state, ok := g.State("sess-fin")
	if !ok {
		t.Fatalf("finished session must stay readable")
	}
	if state.State != impl.SessionClosed {
		t.Fatalf("state = %q, want %q", state.State, impl.SessionClosed)
	}
	if got := string(g.Body()); got != "hello\r\nworld\r\n" {
		t.Fatalf("body = %q, want the reconciled payload", got)
	}
	if lines := g.BodyLines(); len(lines) != 2 || lines[0] != "hello" || lines[1] != "world" {
		t.Fatalf("body lines = %v, want hello and world", lines)
	}
	if _, err := g.Feed([]byte("late\r\n")); !errors.Is(err, impl.ErrNoSession) {
		t.Fatalf("feed after finish must yield ErrNoSession, got %v", err)
	}
	if err := g.Finish("sess-fin"); !errors.Is(err, impl.ErrSessionNotFound) {
		t.Fatalf("second finish must yield ErrSessionNotFound, got %v", err)
	}
}

func TestFinishUnknownSessionRejected(t *testing.T) {
	g := newGuard()
	if err := g.Finish("never-opened"); !errors.Is(err, impl.ErrSessionNotFound) {
		t.Fatalf("unknown session must yield ErrSessionNotFound, got %v", err)
	}
}

func TestAbortDropsTransaction(t *testing.T) {
	g := newGuard()
	if _, err := g.Begin("sess-abort", 20); err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := g.Feed([]byte("part")); err != nil {
		t.Fatalf("feed: %v", err)
	}
	if err := g.Abort("sess-abort"); err != nil {
		t.Fatalf("abort must succeed, got %v", err)
	}
	if got := len(g.Body()); got != 0 {
		t.Fatalf("body length = %d after abort, want 0", got)
	}
	if _, err := g.Feed([]byte("x")); !errors.Is(err, impl.ErrNoSession) {
		t.Fatalf("feed after abort must yield ErrNoSession, got %v", err)
	}
	state, _ := g.State("sess-abort")
	if state.State != impl.SessionAborted {
		t.Fatalf("state = %q, want %q", state.State, impl.SessionAborted)
	}
}

func TestAbortTwiceRejected(t *testing.T) {
	g := newGuard()
	if _, err := g.Begin("sess-twice", 10); err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := g.Abort("sess-twice"); err != nil {
		t.Fatalf("first abort must succeed, got %v", err)
	}
	if err := g.Abort("sess-twice"); !errors.Is(err, impl.ErrSessionNotFound) {
		t.Fatalf("second abort must yield ErrSessionNotFound, got %v", err)
	}
}

func TestSnapshotRestoreMidTransaction(t *testing.T) {
	g := newGuard()
	if _, err := g.Begin("sess-snap", 14); err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := g.Feed([]byte("hello\r\n")); err != nil {
		t.Fatalf("first chunk: %v", err)
	}
	data, err := g.Snapshot()
	if err != nil {
		t.Fatalf("snapshot must succeed, got %v", err)
	}
	if len(data) == 0 {
		t.Fatalf("snapshot payload must not be empty")
	}
	fresh := newGuard()
	if err := fresh.Restore(data); err != nil {
		t.Fatalf("restore must succeed, got %v", err)
	}
	state, ok := fresh.State("sess-snap")
	if !ok {
		t.Fatalf("session must survive the restore")
	}
	if state.Received != 7 || state.Lines != 1 || state.Chunks != 1 {
		t.Fatalf("restored accounting = %+v, want 7 bytes, 1 line, 1 chunk", state)
	}
	if _, err := fresh.Feed([]byte("world\r\n")); err != nil {
		t.Fatalf("feed after restore: %v", err)
	}
	if err := fresh.Finish("sess-snap"); err != nil {
		t.Fatalf("finish after restore: %v", err)
	}
	if got := string(fresh.Body()); got != "hello\r\nworld\r\n" {
		t.Fatalf("body = %q, want the payload split across the restart", got)
	}
	if err := fresh.Restore([]byte("garbage")); !errors.Is(err, impl.ErrBadSnapshot) {
		t.Fatalf("corrupt payload must yield ErrBadSnapshot, got %v", err)
	}
	if _, ok := fresh.State("sess-snap"); !ok {
		t.Fatalf("failed restore must not wipe the live state")
	}
}
