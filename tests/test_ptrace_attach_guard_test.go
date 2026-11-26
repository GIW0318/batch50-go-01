package task

import (
	"errors"
	"testing"

	impl "gsb/batch50-go-safety-d-05"
)

func scopePolicy() impl.ScopePolicy {
	return impl.ScopePolicy{Scope: 0, AllowSameUID: true, MaxTickets: 4, TicketTTLMs: 1000, MaxTracked: 8}
}

func trackProcess(t *testing.T, g *impl.PtraceAttachGuard, p impl.ProcessInfo) {
	t.Helper()
	if err := g.Track(p); err != nil {
		t.Fatalf("track pid %d: %v", p.PID, err)
	}
}

func TestSetPolicyRejectsInvalidScope(t *testing.T) {
	g := impl.NewPtraceAttachGuard(scopePolicy())
	if err := g.SetPolicy(impl.ScopePolicy{Scope: -1, AllowSameUID: true}); !errors.Is(err, impl.ErrBadScope) {
		t.Fatalf("scope -1 error = %v, want ErrBadScope", err)
	}
	if err := g.SetPolicy(impl.ScopePolicy{Scope: 4, AllowSameUID: true}); !errors.Is(err, impl.ErrBadScope) {
		t.Fatalf("scope 4 error = %v, want ErrBadScope", err)
	}
	if err := g.SetPolicy(impl.ScopePolicy{Scope: 2, AllowSameUID: true, MaxTickets: 4, TicketTTLMs: 1000, MaxTracked: 8}); err != nil {
		t.Fatalf("scope 2 must be accepted: %v", err)
	}
}

func TestTrackValidatesAndStoresProcess(t *testing.T) {
	g := impl.NewPtraceAttachGuard(scopePolicy())
	if err := g.Track(impl.ProcessInfo{PID: 0, Image: "/bin/sh"}); !errors.Is(err, impl.ErrBadProcess) {
		t.Fatalf("zero pid error = %v, want ErrBadProcess", err)
	}
	if err := g.Track(impl.ProcessInfo{PID: 100, Image: "  "}); !errors.Is(err, impl.ErrBadProcess) {
		t.Fatalf("blank image error = %v, want ErrBadProcess", err)
	}
	trackProcess(t, g, impl.ProcessInfo{PID: 100, PPID: 1, UID: 1000, Image: "/usr/sbin/agent"})
	p, ok := g.Process(100)
	if !ok {
		t.Fatalf("tracked process must be readable")
	}
	if p.Image != "/usr/sbin/agent" || p.UID != 1000 || p.PPID != 1 {
		t.Fatalf("stored process = %+v, want the tracked fields", p)
	}
	if _, ok := g.Process(999); ok {
		t.Fatalf("unknown process must not be readable")
	}

	tight := scopePolicy()
	tight.MaxTracked = 1
	g2 := impl.NewPtraceAttachGuard(tight)
	trackProcess(t, g2, impl.ProcessInfo{PID: 100, PPID: 1, UID: 1000, Image: "/srv/a"})
	if err := g2.Track(impl.ProcessInfo{PID: 200, PPID: 1, UID: 1000, Image: "/srv/b"}); !errors.Is(err, impl.ErrTrackedLimit) {
		t.Fatalf("tracked table overflow error = %v, want ErrTrackedLimit", err)
	}
}

func TestScopeZeroAllowsSameUID(t *testing.T) {
	g := impl.NewPtraceAttachGuard(scopePolicy())
	trackProcess(t, g, impl.ProcessInfo{PID: 100, PPID: 1, UID: 1000, Image: "/usr/bin/tracer"})
	trackProcess(t, g, impl.ProcessInfo{PID: 200, PPID: 100, UID: 1000, Image: "/srv/app"})

	reason, err := g.ScopeVerdict(100, 200)
	if err != nil {
		t.Fatalf("scope verdict: %v", err)
	}
	if reason != impl.ReasonAllowed {
		t.Fatalf("scope 0 same uid reason = %q, want %q", reason, impl.ReasonAllowed)
	}
	tk, err := g.RequestTicket(100, 200, 5000)
	if err != nil {
		t.Fatalf("request ticket: %v", err)
	}
	if tk.ID == "" || tk.State != impl.TicketActive {
		t.Fatalf("ticket = %+v, want an active ticket with an id", tk)
	}
	if tk.TracerPID != 100 || tk.TraceePID != 200 || tk.Scope != 0 {
		t.Fatalf("ticket pair = %+v, want 100 -> 200 under scope 0", tk)
	}
	if tk.IssuedAt != 5000 || tk.ExpiresAt != 6000 {
		t.Fatalf("ticket window = %d..%d, want 5000..6000", tk.IssuedAt, tk.ExpiresAt)
	}
}

func TestScopeOneRequiresDirectParent(t *testing.T) {
	pol := scopePolicy()
	pol.Scope = 1
	g := impl.NewPtraceAttachGuard(pol)
	trackProcess(t, g, impl.ProcessInfo{PID: 100, PPID: 1, UID: 1000, Image: "/usr/bin/tracer"})
	trackProcess(t, g, impl.ProcessInfo{PID: 200, PPID: 900, UID: 1000, Image: "/srv/app"})

	reason, err := g.ScopeVerdict(100, 200)
	if err != nil {
		t.Fatalf("scope verdict: %v", err)
	}
	if reason != impl.ReasonNotParent {
		t.Fatalf("scope 1 stranger reason = %q, want %q", reason, impl.ReasonNotParent)
	}
	if _, err := g.RequestTicket(100, 200, 5000); !errors.Is(err, impl.ErrScopeDenied) {
		t.Fatalf("non parent ticket error = %v, want ErrScopeDenied", err)
	}
	if st := g.Stats(); st.Issued != 0 {
		t.Fatalf("a refused request must not be counted as issued, got %+v", st)
	}
}

func TestScopeOneAllowsParent(t *testing.T) {
	pol := scopePolicy()
	pol.Scope = 1
	g := impl.NewPtraceAttachGuard(pol)
	trackProcess(t, g, impl.ProcessInfo{PID: 100, PPID: 1, UID: 1000, Image: "/usr/bin/tracer"})
	trackProcess(t, g, impl.ProcessInfo{PID: 200, PPID: 100, UID: 1000, Image: "/srv/app"})

	reason, err := g.ScopeVerdict(100, 200)
	if err != nil {
		t.Fatalf("scope verdict: %v", err)
	}
	if reason != impl.ReasonAllowed {
		t.Fatalf("parent reason = %q, want %q", reason, impl.ReasonAllowed)
	}
	tk, err := g.RequestTicket(100, 200, 5000)
	if err != nil {
		t.Fatalf("parent ticket: %v", err)
	}
	if tk.ID == "" {
		t.Fatalf("parent ticket must carry an id")
	}
}

func TestScopeTwoRequiresRoot(t *testing.T) {
	pol := scopePolicy()
	pol.Scope = 2
	g := impl.NewPtraceAttachGuard(pol)
	trackProcess(t, g, impl.ProcessInfo{PID: 100, PPID: 1, UID: 1000, Image: "/usr/bin/tracer"})
	trackProcess(t, g, impl.ProcessInfo{PID: 200, PPID: 100, UID: 1000, Image: "/srv/app"})

	reason, err := g.ScopeVerdict(100, 200)
	if err != nil {
		t.Fatalf("scope verdict: %v", err)
	}
	if reason != impl.ReasonNotRoot {
		t.Fatalf("non root reason = %q, want %q", reason, impl.ReasonNotRoot)
	}
	if _, err := g.RequestTicket(100, 200, 5000); !errors.Is(err, impl.ErrScopeDenied) {
		t.Fatalf("non root ticket error = %v, want ErrScopeDenied", err)
	}

	root := impl.NewPtraceAttachGuard(pol)
	trackProcess(t, root, impl.ProcessInfo{PID: 300, PPID: 1, UID: 0, Image: "/usr/bin/supervisor"})
	trackProcess(t, root, impl.ProcessInfo{PID: 400, PPID: 300, UID: 1000, Image: "/srv/app"})
	reason, err = root.ScopeVerdict(300, 400)
	if err != nil {
		t.Fatalf("root scope verdict: %v", err)
	}
	if reason != impl.ReasonAllowed {
		t.Fatalf("root reason = %q, want %q", reason, impl.ReasonAllowed)
	}
	var tk impl.Ticket
	tk, err = root.RequestTicket(300, 400, 5000)
	if err != nil {
		t.Fatalf("root ticket: %v", err)
	}
	if tk.Scope != 2 {
		t.Fatalf("ticket scope = %d, want 2", tk.Scope)
	}
}

func TestScopeThreeDeniesAll(t *testing.T) {
	pol := scopePolicy()
	pol.Scope = 3
	g := impl.NewPtraceAttachGuard(pol)
	trackProcess(t, g, impl.ProcessInfo{PID: 100, PPID: 1, UID: 0, Image: "/usr/bin/tracer"})
	trackProcess(t, g, impl.ProcessInfo{PID: 200, PPID: 100, UID: 0, Image: "/srv/app"})

	reason, err := g.ScopeVerdict(100, 200)
	if err != nil {
		t.Fatalf("scope verdict: %v", err)
	}
	if reason != impl.ReasonScopeDenied {
		t.Fatalf("scope 3 reason = %q, want %q", reason, impl.ReasonScopeDenied)
	}
	if _, err := g.RequestTicket(100, 200, 5000); !errors.Is(err, impl.ErrScopeDenied) {
		t.Fatalf("scope 3 ticket error = %v, want ErrScopeDenied", err)
	}
}

func TestSelfAttachRejected(t *testing.T) {
	g := impl.NewPtraceAttachGuard(scopePolicy())
	trackProcess(t, g, impl.ProcessInfo{PID: 300, PPID: 1, UID: 0, Image: "/usr/bin/debugger"})
	reason, err := g.ScopeVerdict(300, 300)
	if err != nil {
		t.Fatalf("scope verdict: %v", err)
	}
	if reason != impl.ReasonSelfAttach {
		t.Fatalf("self attach reason = %q, want %q", reason, impl.ReasonSelfAttach)
	}
	if _, err := g.RequestTicket(300, 300, 5000); !errors.Is(err, impl.ErrScopeDenied) {
		t.Fatalf("self attach ticket error = %v, want ErrScopeDenied", err)
	}
}

func TestUIDMismatchUnderClassicScope(t *testing.T) {
	pol := scopePolicy()
	pol.Scope = 0
	pol.AllowSameUID = false
	g := impl.NewPtraceAttachGuard(pol)
	trackProcess(t, g, impl.ProcessInfo{PID: 100, PPID: 1, UID: 1000, Image: "/usr/bin/tracer"})
	trackProcess(t, g, impl.ProcessInfo{PID: 200, PPID: 1, UID: 2000, Image: "/srv/app"})

	reason, err := g.ScopeVerdict(100, 200)
	if err != nil {
		t.Fatalf("scope verdict: %v", err)
	}
	if reason != impl.ReasonUIDMismatch {
		t.Fatalf("uid mismatch reason = %q, want %q", reason, impl.ReasonUIDMismatch)
	}
	if _, err := g.RequestTicket(100, 200, 5000); !errors.Is(err, impl.ErrScopeDenied) {
		t.Fatalf("uid mismatch ticket error = %v, want ErrScopeDenied", err)
	}
}

func TestUnknownProcessRejected(t *testing.T) {
	g := impl.NewPtraceAttachGuard(scopePolicy())
	if _, err := g.ScopeVerdict(1, 2); !errors.Is(err, impl.ErrUnknownProcess) {
		t.Fatalf("unknown pair verdict error = %v, want ErrUnknownProcess", err)
	}
	trackProcess(t, g, impl.ProcessInfo{PID: 100, PPID: 1, UID: 1000, Image: "/usr/bin/tracer"})
	if _, err := g.RequestTicket(100, 2, 5000); !errors.Is(err, impl.ErrUnknownProcess) {
		t.Fatalf("unknown tracee ticket error = %v, want ErrUnknownProcess", err)
	}
	if err := g.MarkExec(2, "/srv/app", 5000); !errors.Is(err, impl.ErrUnknownProcess) {
		t.Fatalf("exec on unknown process error = %v, want ErrUnknownProcess", err)
	}
}

func TestAlreadyTracedRejected(t *testing.T) {
	g := impl.NewPtraceAttachGuard(scopePolicy())
	trackProcess(t, g, impl.ProcessInfo{PID: 100, PPID: 1, UID: 1000, Image: "/usr/bin/tracer"})
	trackProcess(t, g, impl.ProcessInfo{PID: 200, PPID: 100, UID: 1000, Image: "/srv/app"})
	trackProcess(t, g, impl.ProcessInfo{PID: 300, PPID: 1, UID: 1000, Image: "/usr/bin/profiler"})

	if _, err := g.RequestTicket(100, 200, 5000); err != nil {
		t.Fatalf("first ticket: %v", err)
	}
	if _, err := g.RequestTicket(300, 200, 5100); !errors.Is(err, impl.ErrAlreadyTraced) {
		t.Fatalf("second tracer error = %v, want ErrAlreadyTraced", err)
	}
	if st := g.Stats(); st.Issued != 1 {
		t.Fatalf("issued tickets = %d, want 1", st.Issued)
	}
}

func TestTicketLimitEnforced(t *testing.T) {
	pol := scopePolicy()
	pol.MaxTickets = 1
	g := impl.NewPtraceAttachGuard(pol)
	trackProcess(t, g, impl.ProcessInfo{PID: 100, PPID: 1, UID: 1000, Image: "/usr/bin/tracer"})
	trackProcess(t, g, impl.ProcessInfo{PID: 200, PPID: 100, UID: 1000, Image: "/srv/app"})
	trackProcess(t, g, impl.ProcessInfo{PID: 300, PPID: 100, UID: 1000, Image: "/srv/worker"})

	if _, err := g.RequestTicket(100, 200, 5000); err != nil {
		t.Fatalf("first ticket: %v", err)
	}
	if _, err := g.RequestTicket(100, 300, 5100); !errors.Is(err, impl.ErrTicketLimit) {
		t.Fatalf("second ticket error = %v, want ErrTicketLimit", err)
	}
	if st := g.Stats(); st.Issued != 1 {
		t.Fatalf("issued tickets = %d, want the budget bound 1", st.Issued)
	}
}

func TestAttachConsumesTicketAndTracks(t *testing.T) {
	g := impl.NewPtraceAttachGuard(scopePolicy())
	trackProcess(t, g, impl.ProcessInfo{PID: 100, PPID: 1, UID: 1000, Image: "/usr/bin/tracer"})
	trackProcess(t, g, impl.ProcessInfo{PID: 200, PPID: 100, UID: 1000, Image: "/srv/app"})
	tk, err := g.RequestTicket(100, 200, 5000)
	if err != nil {
		t.Fatalf("request ticket: %v", err)
	}
	res, err := g.Attach(impl.AttachRequest{TicketID: tk.ID, TracerPID: 100, TraceePID: 200, At: 5100, Mode: impl.ModeAttach})
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	if !res.Allowed || !res.Traced || res.Reason != impl.ReasonAllowed {
		t.Fatalf("attach result = %+v, want allowed and traced", res)
	}
	tracer, ok := g.TracerOf(200)
	if !ok || tracer != 100 {
		t.Fatalf("tracer of 200 = %d ok=%v, want 100", tracer, ok)
	}
	used, ok := g.Ticket(tk.ID)
	if !ok || used.State != impl.TicketUsed {
		t.Fatalf("attached ticket = %+v ok=%v, want state %q", used, ok, impl.TicketUsed)
	}
	if st := g.Stats(); st.Attached != 1 {
		t.Fatalf("attached counter = %d, want 1", st.Attached)
	}

	if err := g.Detach(100, 200); err != nil {
		t.Fatalf("detach: %v", err)
	}
	if _, ok := g.TracerOf(200); ok {
		t.Fatalf("detached process must not report a tracer")
	}
	if err := g.Detach(100, 200); !errors.Is(err, impl.ErrNotTraced) {
		t.Fatalf("repeated detach error = %v, want ErrNotTraced", err)
	}
	if err := g.MarkExec(200, "/srv/app-v2", 5200); err != nil {
		t.Fatalf("mark exec: %v", err)
	}
	p, ok := g.Process(200)
	if !ok || p.Image != "/srv/app-v2" {
		t.Fatalf("process image after exec = %q ok=%v, want /srv/app-v2", p.Image, ok)
	}
}

func TestAttachRejectsExpiredTicketAndBadMode(t *testing.T) {
	pol := scopePolicy()
	pol.TicketTTLMs = 100
	g := impl.NewPtraceAttachGuard(pol)
	trackProcess(t, g, impl.ProcessInfo{PID: 100, PPID: 1, UID: 1000, Image: "/usr/bin/tracer"})
	trackProcess(t, g, impl.ProcessInfo{PID: 200, PPID: 100, UID: 1000, Image: "/srv/app"})
	tk, err := g.RequestTicket(100, 200, 5000)
	if err != nil {
		t.Fatalf("request ticket: %v", err)
	}
	if _, err := g.Attach(impl.AttachRequest{TicketID: tk.ID, TracerPID: 100, TraceePID: 200, At: 5050, Mode: "poke"}); !errors.Is(err, impl.ErrBadMode) {
		t.Fatalf("bad mode error = %v, want ErrBadMode", err)
	}
	res, err := g.Attach(impl.AttachRequest{TicketID: tk.ID, TracerPID: 100, TraceePID: 200, At: 5200, Mode: impl.ModeAttach})
	if err != nil {
		t.Fatalf("expired attach: %v", err)
	}
	if res.Allowed || res.Traced || res.Reason != impl.ReasonTicketExpired {
		t.Fatalf("expired attach result = %+v, want denied ticket_expired", res)
	}
	if st := g.Stats(); st.Denied != 1 || st.Attached != 0 {
		t.Fatalf("stats = %+v, want denied 1 attached 0", st)
	}
	if _, ok := g.TracerOf(200); ok {
		t.Fatalf("expired ticket must not start tracing")
	}
	if _, err := g.Attach(impl.AttachRequest{TicketID: "t-ghost", TracerPID: 100, TraceePID: 200, At: 5200, Mode: impl.ModeAttach}); !errors.Is(err, impl.ErrUnknownTicket) {
		t.Fatalf("unknown ticket error = %v, want ErrUnknownTicket", err)
	}
}

func TestSnapshotRestoreKeepsActiveTickets(t *testing.T) {
	g := impl.NewPtraceAttachGuard(scopePolicy())
	trackProcess(t, g, impl.ProcessInfo{PID: 100, PPID: 1, UID: 1000, Image: "/usr/bin/tracer"})
	trackProcess(t, g, impl.ProcessInfo{PID: 200, PPID: 100, UID: 1000, Image: "/srv/app"})
	tk, err := g.RequestTicket(100, 200, 5000)
	if err != nil {
		t.Fatalf("request ticket: %v", err)
	}
	data, err := g.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	restored := impl.NewPtraceAttachGuard(scopePolicy())
	if err := restored.Restore(data); err != nil {
		t.Fatalf("restore: %v", err)
	}
	got, ok := restored.Ticket(tk.ID)
	if !ok || got.State != impl.TicketActive || got.ExpiresAt != 6000 || got.TracerPID != 100 || got.TraceePID != 200 {
		t.Fatalf("restored ticket = %+v ok=%v, want the active 100 -> 200 window", got, ok)
	}
	p, ok := restored.Process(200)
	if !ok || p.Image != "/srv/app" || p.UID != 1000 {
		t.Fatalf("restored process = %+v ok=%v, want /srv/app uid 1000", p, ok)
	}
	if all := restored.Tickets(); len(all) != 1 {
		t.Fatalf("restored ticket count = %d, want 1", len(all))
	}
	if st := restored.Stats(); st.Issued != 1 {
		t.Fatalf("restored issued counter = %d, want 1", st.Issued)
	}
	if err := restored.Restore([]byte("not a snapshot payload")); !errors.Is(err, impl.ErrBadSnapshot) {
		t.Fatalf("garbage snapshot error = %v, want ErrBadSnapshot", err)
	}
	if _, ok := restored.Ticket(tk.ID); !ok {
		t.Fatalf("failed restore must leave the ticket table untouched")
	}
}
