package task

import (
	"errors"
	"fmt"
	"net/netip"
	"sync"
	"testing"
	"time"

	impl "gsb/batch50-go-safety-b-06"
)

func natConfig() impl.Config {
	return impl.Config{
		Prefix:             "64:ff9b::/96",
		Pool:               []string{"198.51.100.10", "198.51.100.11", "198.51.100.12", "198.51.100.13"},
		MaxBlocksPerTenant: 2,
		BlockSize:          4,
		MaxSessions:        64,
		ReclaimDelay:       10 * time.Second,
		QueueTimeout:       0,
		TCPIdle:            30 * time.Second,
		UDPIdle:            20 * time.Second,
		ICMPIdle:           10 * time.Second,
	}
}

func mustRegister(t *testing.T, tbl *impl.Nat64SessionTable, id, prefix string) {
	t.Helper()
	if err := tbl.RegisterTenant(id, prefix); err != nil {
		t.Fatalf("register tenant %s: %v", id, err)
	}
}

func mustReserve(t *testing.T, tbl *impl.Nat64SessionTable, id string, n int) impl.Block {
	t.Helper()
	blk, err := tbl.Reserve(id, n)
	if err != nil {
		t.Fatalf("reserve %d ports for %s: %v", n, id, err)
	}
	return blk
}

func mustTranslate(t *testing.T, tbl *impl.Nat64SessionTable, src, dst impl.Endpoint, proto uint8) impl.Mapped {
	t.Helper()
	m, err := tbl.Translate(src, dst, proto)
	if err != nil {
		t.Fatalf("translate %+v -> %+v proto %d: %v", src, dst, proto, err)
	}
	return m
}

func TestTranslateAllocatesMappingInsideReservedBlock(t *testing.T) {
	tbl := impl.NewNat64SessionTable(natConfig())
	mustRegister(t, tbl, "tenant-a", "2001:db8:a::/64")
	blk := mustReserve(t, tbl, "tenant-a", 4)
	if blk.TenantID != "tenant-a" {
		t.Fatalf("block tenant = %q, want tenant-a", blk.TenantID)
	}
	if blk.IPv4 != "198.51.100.10" {
		t.Fatalf("block address = %q, want the first pool address", blk.IPv4)
	}
	if blk.PortStart != impl.FirstBlockPort || blk.PortEnd != impl.FirstBlockPort+3 {
		t.Fatalf("block range = %d..%d, want %d..%d", blk.PortStart, blk.PortEnd, impl.FirstBlockPort, impl.FirstBlockPort+3)
	}
	src := impl.Endpoint{Addr: "2001:db8:a::5", Port: 40000}
	dst := impl.Endpoint{Addr: "64:ff9b::c000:201", Port: 53}
	m := mustTranslate(t, tbl, src, dst, impl.ProtoUDP)
	if m.SessionID == "" {
		t.Fatalf("mapping session id must not be empty")
	}
	if m.TenantID != "tenant-a" || m.Proto != impl.ProtoUDP {
		t.Fatalf("mapping tenant=%q proto=%d, want tenant-a/udp", m.TenantID, m.Proto)
	}
	if m.Internal != src {
		t.Fatalf("mapping internal = %+v, want %+v", m.Internal, src)
	}
	if m.Destination.Addr != "192.0.2.1" || m.Destination.Port != 53 {
		t.Fatalf("mapping destination = %+v, want 192.0.2.1:53", m.Destination)
	}
	if m.External.Addr != blk.IPv4 {
		t.Fatalf("external address = %q, want the block address %q", m.External.Addr, blk.IPv4)
	}
	if m.External.Port < blk.PortStart || m.External.Port > blk.PortEnd {
		t.Fatalf("external port = %d, want inside %d..%d", m.External.Port, blk.PortStart, blk.PortEnd)
	}
	if m.Refs != 1 {
		t.Fatalf("mapping refs = %d, want 1", m.Refs)
	}
	if got := tbl.TenantUsage("tenant-a").Sessions; got != 1 {
		t.Fatalf("tenant session count = %d, want 1", got)
	}
}

func TestReserveRejectsBadSizeAndBlockCap(t *testing.T) {
	tbl := impl.NewNat64SessionTable(natConfig())
	mustRegister(t, tbl, "tenant-a", "2001:db8:a::/64")
	if _, err := tbl.Reserve("tenant-a", 0); !errors.Is(err, impl.ErrBadBlockSize) {
		t.Fatalf("reserving zero ports must fail with ErrBadBlockSize, got %v", err)
	}
	if _, err := tbl.Reserve("tenant-a", 5); !errors.Is(err, impl.ErrBadBlockSize) {
		t.Fatalf("reserving more ports than the block size must fail with ErrBadBlockSize, got %v", err)
	}
	mustReserve(t, tbl, "tenant-a", 2)
	second := mustReserve(t, tbl, "tenant-a", 2)
	if second.IPv4 != "198.51.100.11" {
		t.Fatalf("second block address = %q, want the next pool address", second.IPv4)
	}
	if _, err := tbl.Reserve("tenant-a", 1); !errors.Is(err, impl.ErrBlockLimit) {
		t.Fatalf("third block must hit the per tenant cap, got %v", err)
	}
	if _, err := tbl.Reserve("tenant-x", 1); !errors.Is(err, impl.ErrUnknownTenant) {
		t.Fatalf("reserving for an unknown tenant must fail with ErrUnknownTenant, got %v", err)
	}
	if got := tbl.TenantUsage("tenant-a").Blocks; got != 2 {
		t.Fatalf("tenant block count = %d, want 2", got)
	}
}

func TestTranslateRejectsDestinationOutsideNat64Prefix(t *testing.T) {
	tbl := impl.NewNat64SessionTable(natConfig())
	mustRegister(t, tbl, "tenant-a", "2001:db8:a::/64")
	src := impl.Endpoint{Addr: "2001:db8:a::9", Port: 1234}
	if _, err := tbl.Translate(src, impl.Endpoint{Addr: "192.0.2.1", Port: 443}, impl.ProtoTCP); !errors.Is(err, impl.ErrOutsidePrefix) {
		t.Fatalf("plain ipv4 destination must fail with ErrOutsidePrefix, got %v", err)
	}
	if _, err := tbl.Translate(src, impl.Endpoint{Addr: "2001:db8:ffff::1", Port: 443}, impl.ProtoTCP); !errors.Is(err, impl.ErrOutsidePrefix) {
		t.Fatalf("ipv6 destination outside the prefix must fail with ErrOutsidePrefix, got %v", err)
	}
	if _, err := tbl.Translate(src, impl.Endpoint{Addr: "not-an-address", Port: 443}, impl.ProtoTCP); !errors.Is(err, impl.ErrBadEndpoint) {
		t.Fatalf("malformed destination must fail with ErrBadEndpoint, got %v", err)
	}
	if _, err := tbl.Translate(impl.Endpoint{Addr: "nonsense", Port: 1}, impl.Endpoint{Addr: "64:ff9b::c000:201", Port: 80}, impl.ProtoTCP); !errors.Is(err, impl.ErrBadEndpoint) {
		t.Fatalf("malformed source must fail with ErrBadEndpoint, got %v", err)
	}
	if got := tbl.TenantUsage("tenant-a").Sessions; got != 0 {
		t.Fatalf("rejected translations must not create sessions, got %d", got)
	}
}

func TestTranslateRejectsUnregisteredTenant(t *testing.T) {
	tbl := impl.NewNat64SessionTable(natConfig())
	mustRegister(t, tbl, "tenant-a", "2001:db8:a::/64")
	_, err := tbl.Translate(impl.Endpoint{Addr: "2001:db8:z::1", Port: 1000}, impl.Endpoint{Addr: "64:ff9b::c000:201", Port: 80}, impl.ProtoTCP)
	if !errors.Is(err, impl.ErrUnknownTenant) {
		t.Fatalf("source outside every tenant prefix must fail with ErrUnknownTenant, got %v", err)
	}
	if err := tbl.RegisterTenant("tenant-a", "2001:db8:b::/64"); !errors.Is(err, impl.ErrTenantExists) {
		t.Fatalf("re-registering a tenant with another prefix must fail with ErrTenantExists, got %v", err)
	}
	if err := tbl.RegisterTenant("", "2001:db8:c::/64"); !errors.Is(err, impl.ErrBadTenant) {
		t.Fatalf("blank tenant id must fail with ErrBadTenant, got %v", err)
	}
	if err := tbl.RegisterTenant("tenant-d", "198.51.100.0/24"); !errors.Is(err, impl.ErrBadTenant) {
		t.Fatalf("ipv4 tenant prefix must fail with ErrBadTenant, got %v", err)
	}
}

func TestTranslateSharesExternalForRepeatedEndpoint(t *testing.T) {
	tbl := impl.NewNat64SessionTable(natConfig())
	mustRegister(t, tbl, "tenant-a", "2001:db8:a::/64")
	blk := mustReserve(t, tbl, "tenant-a", 4)
	src := impl.Endpoint{Addr: "2001:db8:a::5", Port: 40000}
	dst := impl.Endpoint{Addr: "64:ff9b::c000:201", Port: 443}
	first := mustTranslate(t, tbl, src, dst, impl.ProtoTCP)
	second := mustTranslate(t, tbl, src, dst, impl.ProtoTCP)
	if second.SessionID != first.SessionID {
		t.Fatalf("repeated translation must reuse the session, got %q then %q", first.SessionID, second.SessionID)
	}
	if second.External != first.External {
		t.Fatalf("repeated translation changed the external endpoint: %+v then %+v", first.External, second.External)
	}
	if second.Refs != 2 {
		t.Fatalf("shared mapping refs = %d, want 2", second.Refs)
	}
	if got := tbl.TenantUsage("tenant-a").Sessions; got != 1 {
		t.Fatalf("shared mapping must count once, got %d sessions", got)
	}
	other := mustTranslate(t, tbl, src, impl.Endpoint{Addr: "64:ff9b::c000:201", Port: 8443}, impl.ProtoTCP)
	if other.SessionID == first.SessionID {
		t.Fatalf("a different destination port must get its own session")
	}
	if other.External.Port == first.External.Port {
		t.Fatalf("two sessions must not share one external port: %d", other.External.Port)
	}
	if got := tbl.TenantUsage("tenant-a").Sessions; got != 2 {
		t.Fatalf("tenant session count = %d, want 2", got)
	}
	if other.External.Addr != blk.IPv4 {
		t.Fatalf("external address = %q, want the block address %q", other.External.Addr, blk.IPv4)
	}
}

func TestReleaseClosesSessionAndRejectsUnknownMapping(t *testing.T) {
	tbl := impl.NewNat64SessionTable(natConfig())
	mustRegister(t, tbl, "tenant-a", "2001:db8:a::/64")
	mustReserve(t, tbl, "tenant-a", 4)
	src := impl.Endpoint{Addr: "2001:db8:a::7", Port: 5555}
	dst := impl.Endpoint{Addr: "64:ff9b::c000:201", Port: 443}
	m1 := mustTranslate(t, tbl, src, dst, impl.ProtoTCP)
	m2 := mustTranslate(t, tbl, src, dst, impl.ProtoTCP)
	if m2.Refs != 2 {
		t.Fatalf("refs before release = %d, want 2", m2.Refs)
	}
	if err := tbl.Release(m1); err != nil {
		t.Fatalf("first release must succeed, got %v", err)
	}
	live, ok := tbl.Lookup(m1.External, impl.ProtoTCP)
	if !ok {
		t.Fatalf("mapping must stay alive with one reference left")
	}
	if live.Refs != 1 {
		t.Fatalf("refs after one release = %d, want 1", live.Refs)
	}
	if got := tbl.TenantUsage("tenant-a").Sessions; got != 1 {
		t.Fatalf("tenant session count = %d, want 1", got)
	}
	if err := tbl.Release(m1); err != nil {
		t.Fatalf("second release must succeed, got %v", err)
	}
	if _, ok := tbl.Lookup(m1.External, impl.ProtoTCP); ok {
		t.Fatalf("mapping must be gone after the last reference is released")
	}
	if err := tbl.Release(m1); !errors.Is(err, impl.ErrUnknownMapping) {
		t.Fatalf("releasing a closed mapping must fail with ErrUnknownMapping, got %v", err)
	}
	if err := tbl.Release(impl.Mapped{}); !errors.Is(err, impl.ErrUnknownMapping) {
		t.Fatalf("releasing an empty mapping must fail with ErrUnknownMapping, got %v", err)
	}
}

func TestSweepExpiresPerProtocolIdleTimeout(t *testing.T) {
	tbl := impl.NewNat64SessionTable(natConfig())
	mustRegister(t, tbl, "tenant-a", "2001:db8:a::/64")
	mustReserve(t, tbl, "tenant-a", 4)
	t0 := time.Now()
	dst := impl.Endpoint{Addr: "64:ff9b::c000:201", Port: 443}
	mustTranslate(t, tbl, impl.Endpoint{Addr: "2001:db8:a::1", Port: 1000}, dst, impl.ProtoTCP)
	mustTranslate(t, tbl, impl.Endpoint{Addr: "2001:db8:a::2", Port: 1000}, dst, impl.ProtoUDP)
	mustTranslate(t, tbl, impl.Endpoint{Addr: "2001:db8:a::3", Port: 1000}, dst, impl.ProtoICMP)
	if n := tbl.Sweep(t0.Add(15 * time.Second)); n != 1 {
		t.Fatalf("sweep at +15s expired %d sessions, want only the icmp one", n)
	}
	if n := tbl.Sweep(t0.Add(25 * time.Second)); n != 1 {
		t.Fatalf("sweep at +25s expired %d sessions, want only the udp one", n)
	}
	if n := tbl.Sweep(t0.Add(35 * time.Second)); n != 1 {
		t.Fatalf("sweep at +35s expired %d sessions, want only the tcp one", n)
	}
	if n := tbl.Sweep(t0.Add(36 * time.Second)); n != 0 {
		t.Fatalf("second sweep with no live sessions expired %d, want 0", n)
	}
	if got := tbl.TenantUsage("tenant-a").Sessions; got != 0 {
		t.Fatalf("session count after ageing = %d, want 0", got)
	}
}

func TestSweepHoldsPortThroughReclaimDelay(t *testing.T) {
	cfg := natConfig()
	cfg.BlockSize = 1
	cfg.MaxBlocksPerTenant = 1
	cfg.TCPIdle = 5 * time.Second
	cfg.ReclaimDelay = 10 * time.Second
	tbl := impl.NewNat64SessionTable(cfg)
	mustRegister(t, tbl, "tenant-a", "2001:db8:a::/64")
	mustReserve(t, tbl, "tenant-a", 1)
	t0 := time.Now()
	dst := impl.Endpoint{Addr: "64:ff9b::c000:201", Port: 443}
	first := mustTranslate(t, tbl, impl.Endpoint{Addr: "2001:db8:a::1", Port: 1111}, dst, impl.ProtoTCP)
	if n := tbl.Sweep(t0.Add(6 * time.Second)); n != 1 {
		t.Fatalf("sweep at +6s expired %d sessions, want 1", n)
	}
	if _, err := tbl.Translate(impl.Endpoint{Addr: "2001:db8:a::2", Port: 2222}, dst, impl.ProtoTCP); !errors.Is(err, impl.ErrPortExhausted) {
		t.Fatalf("port in the reclaim window must not be handed out, got %v", err)
	}
	if n := tbl.Sweep(t0.Add(20 * time.Second)); n != 0 {
		t.Fatalf("sweep after the reclaim delay expired %d sessions, want 0", n)
	}
	second := mustTranslate(t, tbl, impl.Endpoint{Addr: "2001:db8:a::2", Port: 2222}, dst, impl.ProtoTCP)
	if second.External.Port != first.External.Port {
		t.Fatalf("reclaimed port = %d, want the only block port %d", second.External.Port, first.External.Port)
	}
	if second.SessionID == first.SessionID {
		t.Fatalf("a new flow must not inherit the closed session id")
	}
}

func TestTranslateRejectsExhaustedBlock(t *testing.T) {
	cfg := natConfig()
	cfg.BlockSize = 2
	cfg.MaxBlocksPerTenant = 1
	tbl := impl.NewNat64SessionTable(cfg)
	mustRegister(t, tbl, "tenant-a", "2001:db8:a::/64")
	mustRegister(t, tbl, "tenant-b", "2001:db8:b::/64")
	mustReserve(t, tbl, "tenant-a", 2)
	mustReserve(t, tbl, "tenant-b", 2)
	dst := impl.Endpoint{Addr: "64:ff9b::c000:201", Port: 443}
	mustTranslate(t, tbl, impl.Endpoint{Addr: "2001:db8:a::1", Port: 1}, dst, impl.ProtoUDP)
	mustTranslate(t, tbl, impl.Endpoint{Addr: "2001:db8:a::2", Port: 2}, dst, impl.ProtoUDP)
	if _, err := tbl.Translate(impl.Endpoint{Addr: "2001:db8:a::3", Port: 3}, dst, impl.ProtoUDP); !errors.Is(err, impl.ErrPortExhausted) {
		t.Fatalf("translation on a full block must fail with ErrPortExhausted, got %v", err)
	}
	if got := tbl.TenantUsage("tenant-a").Sessions; got != 2 {
		t.Fatalf("failed translation must not add a session, got %d", got)
	}
	unaffected := mustTranslate(t, tbl, impl.Endpoint{Addr: "2001:db8:b::1", Port: 1}, dst, impl.ProtoUDP)
	if unaffected.TenantID != "tenant-b" {
		t.Fatalf("tenant-b translation returned tenant %q", unaffected.TenantID)
	}
}

func TestSynthAAAAUsesRealRecordsWhenPresent(t *testing.T) {
	tbl := impl.NewNat64SessionTable(natConfig())
	ans, err := tbl.SynthAAAA("app.example", []string{"192.0.2.7"}, []string{"2001:db8::7", "2001:db8::8"})
	if err != nil {
		t.Fatalf("synth with a real aaaa answer must succeed, got %v", err)
	}
	if ans.Synth {
		t.Fatalf("answer with real aaaa records must not be marked synthesized")
	}
	if len(ans.Records) != 2 || ans.Records[0] != "2001:db8::7" || ans.Records[1] != "2001:db8::8" {
		t.Fatalf("records = %v, want the real aaaa records untouched", ans.Records)
	}
	if ans.Name != "app.example" {
		t.Fatalf("answer name = %q, want app.example", ans.Name)
	}
}

func TestSynthAAAAEmbedsIPv4UnderPrefix(t *testing.T) {
	tbl := impl.NewNat64SessionTable(natConfig())
	ans, err := tbl.SynthAAAA("legacy.example", []string{"192.0.2.7", "192.0.2.8"}, nil)
	if err != nil {
		t.Fatalf("synth from a records: %v", err)
	}
	if !ans.Synth {
		t.Fatalf("answer built from a records must be marked synthesized")
	}
	if len(ans.Records) != 2 {
		t.Fatalf("synthesized records = %v, want one per a record", ans.Records)
	}
	wantFirst := netip.MustParseAddr("64:ff9b::c000:207")
	wantSecond := netip.MustParseAddr("64:ff9b::c000:208")
	gotFirst, err := netip.ParseAddr(ans.Records[0])
	if err != nil {
		t.Fatalf("synthesized record %q is not an ipv6 address: %v", ans.Records[0], err)
	}
	gotSecond, err := netip.ParseAddr(ans.Records[1])
	if err != nil {
		t.Fatalf("synthesized record %q is not an ipv6 address: %v", ans.Records[1], err)
	}
	if gotFirst != wantFirst || gotSecond != wantSecond {
		t.Fatalf("synthesized records = %v, want %v and %v", ans.Records, wantFirst, wantSecond)
	}
}

func TestSynthAAAAReturnsErrNoDataWithoutARecords(t *testing.T) {
	tbl := impl.NewNat64SessionTable(natConfig())
	if _, err := tbl.SynthAAAA("empty.example", nil, nil); !errors.Is(err, impl.ErrNoData) {
		t.Fatalf("name without any record must fail with ErrNoData, got %v", err)
	}
	if _, err := tbl.SynthAAAA("bad.example", []string{"definitely-not-an-ip"}, nil); !errors.Is(err, impl.ErrBadEndpoint) {
		t.Fatalf("malformed a record must fail with ErrBadEndpoint, got %v", err)
	}
}

func TestSnapshotRestoreKeepsSessionsAndBlocks(t *testing.T) {
	cfg := natConfig()
	tbl := impl.NewNat64SessionTable(cfg)
	mustRegister(t, tbl, "tenant-a", "2001:db8:a::/64")
	blk := mustReserve(t, tbl, "tenant-a", 4)
	dst := impl.Endpoint{Addr: "64:ff9b::c000:201", Port: 443}
	first := mustTranslate(t, tbl, impl.Endpoint{Addr: "2001:db8:a::1", Port: 1000}, dst, impl.ProtoTCP)
	mustTranslate(t, tbl, impl.Endpoint{Addr: "2001:db8:a::2", Port: 1000}, dst, impl.ProtoTCP)
	data, err := tbl.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if len(data) == 0 {
		t.Fatalf("snapshot payload must not be empty")
	}
	restored := impl.NewNat64SessionTable(cfg)
	if err := restored.Restore(data); err != nil {
		t.Fatalf("restore: %v", err)
	}
	usage := restored.TenantUsage("tenant-a")
	if usage.Blocks != 1 || usage.Sessions != 2 {
		t.Fatalf("restored usage = %+v, want 1 block and 2 sessions", usage)
	}
	back, ok := restored.Lookup(first.External, impl.ProtoTCP)
	if !ok {
		t.Fatalf("restored table lost the external mapping")
	}
	if back.SessionID != first.SessionID || back.Internal != first.Internal {
		t.Fatalf("restored mapping = %+v, want session %q internal %+v", back, first.SessionID, first.Internal)
	}
	third := mustTranslate(t, restored, impl.Endpoint{Addr: "2001:db8:a::3", Port: 1000}, dst, impl.ProtoTCP)
	if third.External == first.External {
		t.Fatalf("fresh translation after restore reused a live external endpoint")
	}
	if third.External.Addr != blk.IPv4 {
		t.Fatalf("fresh translation address = %q, want the restored block address %q", third.External.Addr, blk.IPv4)
	}
}

func TestRestoreRejectsCorruptSnapshotAtomically(t *testing.T) {
	cfg := natConfig()
	tbl := impl.NewNat64SessionTable(cfg)
	mustRegister(t, tbl, "tenant-a", "2001:db8:a::/64")
	dst := impl.Endpoint{Addr: "64:ff9b::c000:201", Port: 443}
	live := mustTranslate(t, tbl, impl.Endpoint{Addr: "2001:db8:a::1", Port: 1000}, dst, impl.ProtoTCP)
	if err := tbl.Restore([]byte("{this-is-not-a-snapshot")); !errors.Is(err, impl.ErrBadSnapshot) {
		t.Fatalf("corrupt snapshot must fail with ErrBadSnapshot, got %v", err)
	}
	back, ok := tbl.Lookup(live.External, impl.ProtoTCP)
	if !ok {
		t.Fatalf("rejected restore must leave the live table untouched")
	}
	if back.SessionID != live.SessionID {
		t.Fatalf("live mapping changed after a rejected restore: %q then %q", live.SessionID, back.SessionID)
	}
	if got := tbl.TenantUsage("tenant-a").Sessions; got != 1 {
		t.Fatalf("live session count after rejected restore = %d, want 1", got)
	}
}

func TestConcurrentTranslateKeepsTenantsIsolated(t *testing.T) {
	cfg := natConfig()
	cfg.BlockSize = 32
	cfg.MaxBlocksPerTenant = 1
	cfg.MaxSessions = 256
	tbl := impl.NewNat64SessionTable(cfg)
	mustRegister(t, tbl, "tenant-a", "2001:db8:a::/64")
	mustRegister(t, tbl, "tenant-b", "2001:db8:b::/64")
	blockA := mustReserve(t, tbl, "tenant-a", 32)
	blockB := mustReserve(t, tbl, "tenant-b", 32)
	dst := impl.Endpoint{Addr: "64:ff9b::c000:201", Port: 443}

	var wg sync.WaitGroup
	var mu sync.Mutex
	externals := make(map[string]string)
	failures := make([]string, 0)
	for i := 0; i < 32; i++ {
		for _, tenant := range []string{"tenant-a", "tenant-b"} {
			wg.Add(1)
			go func(i int, tenant string) {
				defer wg.Done()
				prefix := "2001:db8:a::"
				wantAddr := blockA.IPv4
				if tenant == "tenant-b" {
					prefix = "2001:db8:b::"
					wantAddr = blockB.IPv4
				}
				src := impl.Endpoint{Addr: fmt.Sprintf("%s%d", prefix, i+1), Port: uint16(2000 + i)}
				m, err := tbl.Translate(src, dst, impl.ProtoTCP)
				mu.Lock()
				defer mu.Unlock()
				if err != nil {
					failures = append(failures, fmt.Sprintf("%s: %v", tenant, err))
					return
				}
				if m.TenantID != tenant {
					failures = append(failures, fmt.Sprintf("flow of %s was mapped to %s", tenant, m.TenantID))
					return
				}
				if m.External.Addr != wantAddr {
					failures = append(failures, fmt.Sprintf("%s got external address %s, want %s", tenant, m.External.Addr, wantAddr))
					return
				}
				key := fmt.Sprintf("%s:%d", m.External.Addr, m.External.Port)
				if prev, dup := externals[key]; dup {
					failures = append(failures, fmt.Sprintf("external %s handed to both %s and %s", key, prev, tenant))
					return
				}
				externals[key] = tenant
			}(i, tenant)
		}
	}
	wg.Wait()
	if len(failures) != 0 {
		t.Fatalf("concurrent translation failures: %v", failures)
	}
	if len(externals) != 64 {
		t.Fatalf("distinct external endpoints = %d, want 64", len(externals))
	}
	if got := tbl.TenantUsage("tenant-a").Sessions; got != 32 {
		t.Fatalf("tenant-a sessions = %d, want 32", got)
	}
	if got := tbl.TenantUsage("tenant-b").Sessions; got != 32 {
		t.Fatalf("tenant-b sessions = %d, want 32", got)
	}
}
