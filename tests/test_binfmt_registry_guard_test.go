package task

import (
	"bytes"
	"errors"
	"sync"
	"testing"

	impl "gsb/batch50-go-safety-d-01"
)

func guardLimits() impl.Limits {
	return impl.Limits{MaxEntries: 32, MaxMagicLen: 4, AllowedDirs: []string{"/opt/wrap", "/usr/local/libexec"}}
}

func guardEntry(magic, mask []byte, prio int) impl.Entry {
	return impl.Entry{
		Magic:       magic,
		Mask:        mask,
		Interpreter: "/opt/wrap/run-format",
		Priority:    prio,
	}
}

func fullMask(n int) []byte {
	m := make([]byte, n)
	for i := range m {
		m[i] = 0xFF
	}
	return m
}

func TestRegisterAssignsIDsAndDefaults(t *testing.T) {
	g := impl.NewBinfmtRegistryGuard(guardLimits())
	elf := []byte{0x7F, 'E', 'L', 'F'}
	id1, err := g.Register(guardEntry(elf, fullMask(4), 10))
	if err != nil {
		t.Fatalf("register the elf pattern: %v", err)
	}
	if id1 == "" {
		t.Fatalf("register must assign a non-empty id")
	}
	e1, ok := g.Entry(id1)
	if !ok {
		t.Fatalf("entry %q must be readable right after register", id1)
	}
	if !e1.Enabled {
		t.Fatalf("a fresh registration must be enabled")
	}
	if e1.Priority != 10 || e1.Seq <= 0 {
		t.Fatalf("entry priority=%d seq=%d, want priority 10 and a positive seq", e1.Priority, e1.Seq)
	}
	if !bytes.Equal(e1.Magic, elf) || !bytes.Equal(e1.Mask, fullMask(4)) {
		t.Fatalf("stored pattern = %v/%v, want %v/%v", e1.Magic, e1.Mask, elf, fullMask(4))
	}
	id2, err := g.Register(guardEntry([]byte{0xCA, 0xFE}, fullMask(2), 20))
	if err != nil {
		t.Fatalf("register the cafe pattern: %v", err)
	}
	if id2 == id1 {
		t.Fatalf("two registrations must get different ids, both were %q", id1)
	}
	all := g.Entries()
	if len(all) != 2 {
		t.Fatalf("host entries = %d, want 2", len(all))
	}
	if all[0].ID != id1 || all[1].ID != id2 {
		t.Fatalf("entries = %q/%q, want registration order %q/%q", all[0].ID, all[1].ID, id1, id2)
	}
}

func TestRegisterValidatesMagicAndMask(t *testing.T) {
	bad := []struct {
		name string
		e    impl.Entry
	}{
		{"empty magic", guardEntry(nil, nil, 1)},
		{"mask length", guardEntry([]byte{1, 2}, []byte{0xFF}, 1)},
		{"bits outside mask", guardEntry([]byte{0xFF}, []byte{0x0F}, 1)},
		{"longer than the cap", guardEntry([]byte{1, 2, 3, 4, 5}, fullMask(5), 1)},
	}
	for _, c := range bad {
		g := impl.NewBinfmtRegistryGuard(guardLimits())
		if _, err := g.Register(c.e); !errors.Is(err, impl.ErrBadEntry) {
			t.Fatalf("%s: got %v, want ErrBadEntry", c.name, err)
		}
		if got := len(g.Entries()); got != 0 {
			t.Fatalf("%s: a rejected entry left %d registrations behind", c.name, got)
		}
	}
}

func TestRegisterRejectsBadInterpreter(t *testing.T) {
	g := impl.NewBinfmtRegistryGuard(guardLimits())
	magic := []byte{0x4D, 0x5A}
	cases := []struct {
		name string
		path string
	}{
		{"relative path", "bin/run-format"},
		{"path with dot dot", "/opt/wrap/../../etc/run"},
		{"outside the allowed dirs", "/usr/bin/run"},
		{"empty path", ""},
	}
	for _, c := range cases {
		e := guardEntry(magic, fullMask(2), 1)
		e.Interpreter = c.path
		if _, err := g.Register(e); !errors.Is(err, impl.ErrBadInterpreter) {
			t.Fatalf("%s: got %v, want ErrBadInterpreter", c.name, err)
		}
	}
	ok := guardEntry(magic, fullMask(2), 1)
	ok.Interpreter = "/usr/local/libexec/run-mz"
	if _, err := g.Register(ok); err != nil {
		t.Fatalf("an interpreter inside an allowed dir must be accepted, got %v", err)
	}
}

func TestRegisterRejectsBadFlags(t *testing.T) {
	g := impl.NewBinfmtRegistryGuard(guardLimits())
	magic := []byte{0x1F, 0x8B}
	for _, flags := range [][]string{{"X"}, {"P", "P"}, {"O", "F"}} {
		e := guardEntry(magic, fullMask(2), 3)
		e.Flags = flags
		if _, err := g.Register(e); !errors.Is(err, impl.ErrBadFlags) {
			t.Fatalf("flags %v: got %v, want ErrBadFlags", flags, err)
		}
	}
	good := guardEntry(magic, fullMask(2), 3)
	good.Flags = []string{"P", "C"}
	id, err := g.Register(good)
	if err != nil {
		t.Fatalf("a known flag set must be accepted, got %v", err)
	}
	e, ok := g.Entry(id)
	if !ok || len(e.Flags) != 2 {
		t.Fatalf("stored flags = %v, want the two submitted flags", e.Flags)
	}
}

func TestRegisterRejectsSamePatternAtSamePriority(t *testing.T) {
	g := impl.NewBinfmtRegistryGuard(guardLimits())
	magic := []byte{0x7F, 'E'}
	if _, err := g.Register(guardEntry(magic, fullMask(2), 5)); err != nil {
		t.Fatalf("first registration: %v", err)
	}
	if _, err := g.Register(guardEntry(magic, fullMask(2), 5)); !errors.Is(err, impl.ErrMaskConflict) {
		t.Fatalf("an identical pattern at the same priority must conflict, got %v", err)
	}
	if got := len(g.Entries()); got != 1 {
		t.Fatalf("host entries = %d, want the conflicting registration to be dropped", got)
	}
	high, err := g.Register(guardEntry(magic, fullMask(2), 9))
	if err != nil {
		t.Fatalf("a different priority must not be reported as a conflict: %v", err)
	}
	got, ok, err := g.Resolve([]byte{0x7F, 'E', 0x01})
	if err != nil || !ok {
		t.Fatalf("resolve = %v ok=%v err=%v, want a match", got.ID, ok, err)
	}
	if got.ID != high {
		t.Fatalf("resolve returned %q, want the higher priority entry %q", got.ID, high)
	}
}

func TestRegisterEnforcesEntryLimit(t *testing.T) {
	limits := guardLimits()
	limits.MaxEntries = 2
	g := impl.NewBinfmtRegistryGuard(limits)
	for i := 0; i < 2; i++ {
		if _, err := g.Register(guardEntry([]byte{byte(0x10 + i)}, fullMask(1), 1)); err != nil {
			t.Fatalf("registration %d: %v", i, err)
		}
	}
	if _, err := g.Register(guardEntry([]byte{0x30}, fullMask(1), 1)); !errors.Is(err, impl.ErrEntryLimit) {
		t.Fatalf("the third registration must hit the cap, got %v", err)
	}
	if got := len(g.Entries()); got != 2 {
		t.Fatalf("host entries = %d, want the cap 2", got)
	}
	if _, ok, err := g.Resolve([]byte{0x30}); ok || err != nil {
		t.Fatalf("the rejected pattern must not resolve, ok=%v err=%v", ok, err)
	}
}

func TestResolvePrefersHigherPriority(t *testing.T) {
	g := impl.NewBinfmtRegistryGuard(guardLimits())
	if _, err := g.Register(guardEntry([]byte{0x50}, fullMask(1), 3)); err != nil {
		t.Fatalf("register the low priority entry: %v", err)
	}
	high, err := g.Register(guardEntry([]byte{0x50}, fullMask(1), 8))
	if err != nil {
		t.Fatalf("register the high priority entry: %v", err)
	}
	got, ok, err := g.Resolve([]byte{0x50, 0x01, 0x02})
	if err != nil || !ok {
		t.Fatalf("resolve = ok=%v err=%v, want a match", ok, err)
	}
	if got.ID != high || got.Priority != 8 {
		t.Fatalf("resolve returned %q priority %d, want %q priority 8", got.ID, got.Priority, high)
	}
}

func TestResolveRejectsBadInputAndMissesCleanly(t *testing.T) {
	g := impl.NewBinfmtRegistryGuard(guardLimits())
	if _, _, err := g.Resolve(nil); !errors.Is(err, impl.ErrBadEntry) {
		t.Fatalf("empty magic must be rejected with ErrBadEntry, got %v", err)
	}
	if _, _, err := g.Resolve([]byte{1, 2, 3, 4, 5}); !errors.Is(err, impl.ErrBadEntry) {
		t.Fatalf("magic longer than the cap must be rejected with ErrBadEntry, got %v", err)
	}
	if _, err := g.Register(guardEntry([]byte{0xAA, 0xBB}, fullMask(2), 1)); err != nil {
		t.Fatalf("register: %v", err)
	}
	got, ok, err := g.Resolve([]byte{0xAA, 0xBC})
	if ok || err != nil {
		t.Fatalf("a non matching prefix must miss cleanly, ok=%v err=%v entry=%+v", ok, err, got)
	}
	if got.ID != "" {
		t.Fatalf("a miss must return the zero entry, got id %q", got.ID)
	}
}

func TestSetEnabledSkipsDisabledEntries(t *testing.T) {
	g := impl.NewBinfmtRegistryGuard(guardLimits())
	if _, err := g.Register(guardEntry([]byte{0x60}, fullMask(1), 2)); err != nil {
		t.Fatalf("register the low priority entry: %v", err)
	}
	high, err := g.Register(guardEntry([]byte{0x60}, fullMask(1), 7))
	if err != nil {
		t.Fatalf("register the high priority entry: %v", err)
	}
	if err := g.SetEnabled(high, false); err != nil {
		t.Fatalf("disable the high priority entry: %v", err)
	}
	e, ok := g.Entry(high)
	if !ok || e.Enabled {
		t.Fatalf("entry %q enabled=%v ok=%v, want a disabled record", high, e.Enabled, ok)
	}
	got, ok, err := g.Resolve([]byte{0x60})
	if err != nil || !ok {
		t.Fatalf("resolve after disable = ok=%v err=%v, want the remaining entry", ok, err)
	}
	if got.Priority != 2 {
		t.Fatalf("resolve returned priority %d, want the enabled entry with priority 2", got.Priority)
	}
	if err := g.SetEnabled(high, true); err != nil {
		t.Fatalf("re-enable: %v", err)
	}
	got, ok, err = g.Resolve([]byte{0x60})
	if err != nil || !ok || got.ID != high {
		t.Fatalf("re-enabled entry must win again, got %q ok=%v err=%v", got.ID, ok, err)
	}
	if err := g.SetEnabled("bf-404", false); !errors.Is(err, impl.ErrUnknownEntry) {
		t.Fatalf("an unknown id must fail with ErrUnknownEntry, got %v", err)
	}
}

func TestSetPriorityReordersAndRejectsConflict(t *testing.T) {
	g := impl.NewBinfmtRegistryGuard(guardLimits())
	low, err := g.Register(guardEntry([]byte{0x70}, fullMask(1), 3))
	if err != nil {
		t.Fatalf("register the first entry: %v", err)
	}
	high, err := g.Register(guardEntry([]byte{0x70}, fullMask(1), 5))
	if err != nil {
		t.Fatalf("register the second entry: %v", err)
	}
	got, ok, err := g.Resolve([]byte{0x70})
	if err != nil || !ok || got.ID != high {
		t.Fatalf("resolve = %q ok=%v err=%v, want %q", got.ID, ok, err, high)
	}
	if err := g.SetPriority(low, 9); err != nil {
		t.Fatalf("raise the first entry: %v", err)
	}
	got, ok, err = g.Resolve([]byte{0x70})
	if err != nil || !ok || got.ID != low {
		t.Fatalf("resolve after the priority change = %q ok=%v err=%v, want %q", got.ID, ok, err, low)
	}
	third, err := g.Register(guardEntry([]byte{0x70}, fullMask(1), 1))
	if err != nil {
		t.Fatalf("register the third entry: %v", err)
	}
	if err := g.SetPriority(third, 9); !errors.Is(err, impl.ErrMaskConflict) {
		t.Fatalf("a priority change that duplicates a registration must fail, got %v", err)
	}
	e, ok := g.Entry(third)
	if !ok || e.Priority != 1 {
		t.Fatalf("entry priority after the rejected change = %d, want the original 1", e.Priority)
	}
}

func TestUnregisterRemovesEntryAndNeverReusesID(t *testing.T) {
	g := impl.NewBinfmtRegistryGuard(guardLimits())
	first, err := g.Register(guardEntry([]byte{0x80}, fullMask(1), 1))
	if err != nil {
		t.Fatalf("register the first entry: %v", err)
	}
	second, err := g.Register(guardEntry([]byte{0x81}, fullMask(1), 1))
	if err != nil {
		t.Fatalf("register the second entry: %v", err)
	}
	if err := g.Unregister(first); err != nil {
		t.Fatalf("unregister: %v", err)
	}
	if _, ok := g.Entry(first); ok {
		t.Fatalf("entry %q must be gone", first)
	}
	if _, ok, err := g.Resolve([]byte{0x80}); ok || err != nil {
		t.Fatalf("a removed pattern must not resolve, ok=%v err=%v", ok, err)
	}
	if err := g.Unregister(first); !errors.Is(err, impl.ErrUnknownEntry) {
		t.Fatalf("removing an unknown id must fail with ErrUnknownEntry, got %v", err)
	}
	third, err := g.Register(guardEntry([]byte{0x82}, fullMask(1), 1))
	if err != nil {
		t.Fatalf("register the third entry: %v", err)
	}
	if third == first {
		t.Fatalf("the id %q was reused after unregister", first)
	}
	all := g.Entries()
	if len(all) != 2 || all[0].ID != second || all[1].ID != third {
		t.Fatalf("host entries = %+v, want [%q %q] in registration order", all, second, third)
	}
}

func TestResolveInFallsBackToHost(t *testing.T) {
	g := impl.NewBinfmtRegistryGuard(guardLimits())
	hostID, err := g.Register(guardEntry([]byte{0xCA, 0xFE}, fullMask(2), 5))
	if err != nil {
		t.Fatalf("register the host entry: %v", err)
	}
	nsEntry := guardEntry([]byte{0xCA, 0xFE}, fullMask(2), 9)
	nsEntry.Namespace = "sandbox"
	nsID, err := g.Register(nsEntry)
	if err != nil {
		t.Fatalf("register the sandbox entry: %v", err)
	}
	inNS, ok, err := g.ResolveIn("sandbox", []byte{0xCA, 0xFE, 0x00})
	if err != nil || !ok || inNS.ID != nsID {
		t.Fatalf("sandbox resolve = %q ok=%v err=%v, want %q", inNS.ID, ok, err, nsID)
	}
	hostView, ok, err := g.Resolve([]byte{0xCA, 0xFE, 0x00})
	if err != nil || !ok || hostView.ID != hostID {
		t.Fatalf("host resolve = %q ok=%v err=%v, want %q", hostView.ID, ok, err, hostID)
	}
	other, ok, err := g.ResolveIn("other", []byte{0xCA, 0xFE, 0x00})
	if err != nil || !ok || other.ID != hostID {
		t.Fatalf("an unknown namespace must fall back to the host entry %q, got %q ok=%v err=%v", hostID, other.ID, ok, err)
	}
	if got := len(g.NamespaceEntries("sandbox")); got != 1 {
		t.Fatalf("sandbox entries = %d, want 1", got)
	}
	if got := len(g.Entries()); got != 1 {
		t.Fatalf("host entries = %d, want the overlay entry to stay out of the host view", got)
	}
}

func TestPurgeDropsNamespaceOverlays(t *testing.T) {
	g := impl.NewBinfmtRegistryGuard(guardLimits())
	hostID, err := g.Register(guardEntry([]byte{0x33}, fullMask(1), 2))
	if err != nil {
		t.Fatalf("register the host entry: %v", err)
	}
	for i, magic := range [][]byte{{0x34}, {0x35}} {
		e := guardEntry(magic, fullMask(1), 3+i)
		e.Namespace = "tenant"
		if _, err := g.Register(e); err != nil {
			t.Fatalf("register tenant entry %d: %v", i, err)
		}
	}
	if n := g.Purge("tenant"); n != 2 {
		t.Fatalf("purge removed %d entries, want 2", n)
	}
	if got := len(g.NamespaceEntries("tenant")); got != 0 {
		t.Fatalf("tenant entries after purge = %d, want 0", got)
	}
	if got, ok, err := g.ResolveIn("tenant", []byte{0x33}); err != nil || !ok || got.ID != hostID {
		t.Fatalf("tenant resolve after purge = %q ok=%v err=%v, want the host entry %q", got.ID, ok, err, hostID)
	}
	if n := g.Purge("tenant"); n != 0 {
		t.Fatalf("purging an empty namespace removed %d entries, want 0", n)
	}
	if got := len(g.Entries()); got != 1 {
		t.Fatalf("host entries = %d, want the host registration untouched", got)
	}
}

func TestConcurrentRegisterAssignsUniqueIDs(t *testing.T) {
	g := impl.NewBinfmtRegistryGuard(guardLimits())
	const workers = 16
	ids := make([]string, workers)
	errs := make([]error, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ids[i], errs[i] = g.Register(guardEntry([]byte{byte(i), 0xA5}, fullMask(2), 1))
		}(i)
	}
	wg.Wait()
	seen := make(map[string]bool, workers)
	for i := 0; i < workers; i++ {
		if errs[i] != nil {
			t.Fatalf("concurrent register %d: %v", i, errs[i])
		}
		if ids[i] == "" {
			t.Fatalf("concurrent register %d returned an empty id", i)
		}
		if seen[ids[i]] {
			t.Fatalf("id %q was handed out twice", ids[i])
		}
		seen[ids[i]] = true
	}
	if got := len(g.Entries()); got != workers {
		t.Fatalf("host entries = %d, want %d", got, workers)
	}
	for i := 0; i < workers; i++ {
		e, ok := g.Entry(ids[i])
		if !ok {
			t.Fatalf("entry %q is missing after the concurrent registers", ids[i])
		}
		if len(e.Magic) != 2 || e.Magic[0] != byte(i) {
			t.Fatalf("entry %q magic = %v, want the submitted %#x prefix", ids[i], e.Magic, i)
		}
	}
}

func TestSnapshotRestoreKeepsEntriesAndSequence(t *testing.T) {
	limits := guardLimits()
	g := impl.NewBinfmtRegistryGuard(limits)
	idA, err := g.Register(guardEntry([]byte{0x90}, fullMask(1), 4))
	if err != nil {
		t.Fatalf("register entry A: %v", err)
	}
	idB, err := g.Register(guardEntry([]byte{0x91}, fullMask(1), 6))
	if err != nil {
		t.Fatalf("register entry B: %v", err)
	}
	if err := g.SetEnabled(idB, false); err != nil {
		t.Fatalf("disable entry B: %v", err)
	}
	nsE := guardEntry([]byte{0x92}, fullMask(1), 2)
	nsE.Namespace = "tenant-a"
	if _, err := g.Register(nsE); err != nil {
		t.Fatalf("register the tenant entry: %v", err)
	}
	data, err := g.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if len(data) == 0 {
		t.Fatalf("snapshot must not be empty")
	}
	r := impl.NewBinfmtRegistryGuard(limits)
	if err := r.Restore(data); err != nil {
		t.Fatalf("restore: %v", err)
	}
	entries := r.Entries()
	if len(entries) != 2 || entries[0].ID != idA || entries[1].ID != idB {
		t.Fatalf("restored entries = %+v, want [%q %q]", entries, idA, idB)
	}
	if eB, ok := r.Entry(idB); !ok || eB.Enabled {
		t.Fatalf("restored entry B enabled=%v ok=%v, want the disabled flag kept", eB.Enabled, ok)
	}
	if got, ok, err := r.Resolve([]byte{0x90}); err != nil || !ok || got.ID != idA {
		t.Fatalf("restored resolve = %q ok=%v err=%v, want %q", got.ID, ok, err, idA)
	}
	if _, ok, err := r.Resolve([]byte{0x91}); ok || err != nil {
		t.Fatalf("the disabled entry must stay out of resolution, ok=%v err=%v", ok, err)
	}
	if got := len(r.NamespaceEntries("tenant-a")); got != 1 {
		t.Fatalf("restored tenant entries = %d, want 1", got)
	}
	idC, err := r.Register(guardEntry([]byte{0x93}, fullMask(1), 1))
	if err != nil {
		t.Fatalf("register after restore: %v", err)
	}
	if idC == idA || idC == idB {
		t.Fatalf("restore reused an old id: %q", idC)
	}
	if err := r.Restore([]byte("{not a registry snapshot")); !errors.Is(err, impl.ErrBadSnapshot) {
		t.Fatalf("a broken snapshot must fail with ErrBadSnapshot, got %v", err)
	}
	if got := len(r.Entries()); got != 3 {
		t.Fatalf("host entries after the rejected restore = %d, want the live 3", got)
	}
	if got, ok, err := r.Resolve([]byte{0x93}); err != nil || !ok || got.ID != idC {
		t.Fatalf("live state must survive a rejected restore, got %q ok=%v err=%v", got.ID, ok, err)
	}
}
