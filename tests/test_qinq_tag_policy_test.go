package task

import (
	"bytes"
	"errors"
	"strconv"
	"strings"
	"testing"

	impl "gsb/batch50-go-safety-b-05"
)

const dstMAC = "02:00:00:00:00:aa"

type tagSpec struct {
	tpid uint16
	tci  uint16
}

func parseMAC(t *testing.T, s string) []byte {
	t.Helper()
	parts := strings.Split(s, ":")
	if len(parts) != 6 {
		t.Fatalf("bad mac %q", s)
	}
	out := make([]byte, 0, 6)
	for _, p := range parts {
		v, err := strconv.ParseUint(p, 16, 8)
		if err != nil {
			t.Fatalf("bad mac %q: %v", s, err)
		}
		out = append(out, byte(v))
	}
	return out
}

func buildFrame(t *testing.T, src string, tags []tagSpec, etherType uint16, payload string) []byte {
	t.Helper()
	f := append([]byte{}, parseMAC(t, dstMAC)...)
	f = append(f, parseMAC(t, src)...)
	for _, tag := range tags {
		f = append(f, byte(tag.tpid>>8), byte(tag.tpid), byte(tag.tci>>8), byte(tag.tci))
	}
	f = append(f, byte(etherType>>8), byte(etherType))
	f = append(f, []byte(payload)...)
	return f
}

func qinqPort(name string) impl.PortConfig {
	return impl.PortConfig{
		Name:          name,
		Mode:          impl.ModeQinQ,
		OuterTPID:     impl.TPID88A8,
		AllowedSVLANs: []uint16{100, 200},
		NativeVLAN:    10,
		EgressAction:  impl.EgressKeep,
	}
}

func accessPort(name string) impl.PortConfig {
	return impl.PortConfig{
		Name:          name,
		Mode:          impl.ModeAccess,
		OuterTPID:     impl.TPID8100,
		AllowedSVLANs: []uint16{10},
		NativeVLAN:    10,
		EgressAction:  impl.EgressKeep,
	}
}

func policyConfig() impl.PolicyConfig {
	return impl.PolicyConfig{MaxSVLANsPerPort: 4, MaxTableEntries: 4}
}

func doubleTagged(t *testing.T, src string, svlan, cvlan uint16) []byte {
	t.Helper()
	return buildFrame(t, src, []tagSpec{{impl.TPID88A8, svlan}, {impl.TPID8100, cvlan}}, 0x0800, "payload")
}

func TestClassifyAdmitsDoubleTaggedFrame(t *testing.T) {
	p := impl.NewQinQTagPolicy(policyConfig())
	port := qinqPort("ae1")
	frame := doubleTagged(t, "02:00:00:00:00:01", 100, 20)
	v, err := p.Classify(frame, port)
	if err != nil {
		t.Fatalf("classify: %v", err)
	}
	if v.Action != impl.ActionAdmit {
		t.Fatalf("action = %q, want %q (reason %q)", v.Action, impl.ActionAdmit, v.Reason)
	}
	if v.SVLAN != 100 || v.CVLAN != 20 {
		t.Fatalf("svlan/cvlan = %d/%d, want 100/20", v.SVLAN, v.CVLAN)
	}
	if v.Native {
		t.Fatalf("tagged frame must not be marked native")
	}
	if v.Reason != impl.ReasonAdmit {
		t.Fatalf("reason = %q, want %q", v.Reason, impl.ReasonAdmit)
	}
	if got := p.PortStats("ae1").Admitted; got != 1 {
		t.Fatalf("admitted frames = %d, want 1", got)
	}
}

func TestClassifyRejectsSVLANOutsideAllowedSet(t *testing.T) {
	p := impl.NewQinQTagPolicy(policyConfig())
	port := qinqPort("ae1")
	v, err := p.Classify(doubleTagged(t, "02:00:00:00:00:01", 300, 20), port)
	if err != nil {
		t.Fatalf("classify: %v", err)
	}
	if v.Action != impl.ActionDrop || v.Reason != impl.ReasonDropSVLAN {
		t.Fatalf("verdict = %+v, want drop with %q", v, impl.ReasonDropSVLAN)
	}
	if got := p.PortStats("ae1").Dropped; got != 1 {
		t.Fatalf("dropped frames = %d, want 1", got)
	}
}

func TestClassifyRejectsInnerEqualsNative(t *testing.T) {
	p := impl.NewQinQTagPolicy(policyConfig())
	port := qinqPort("ae1")
	v, err := p.Classify(doubleTagged(t, "02:00:00:00:00:01", 100, 10), port)
	if err != nil {
		t.Fatalf("classify: %v", err)
	}
	if v.Action != impl.ActionDrop || v.Reason != impl.ReasonDropNative {
		t.Fatalf("verdict = %+v, want drop with %q", v, impl.ReasonDropNative)
	}
}

func TestClassifyUntaggedFallsToNativeVLAN(t *testing.T) {
	p := impl.NewQinQTagPolicy(policyConfig())
	port := accessPort("ge1")
	frame := buildFrame(t, "02:00:00:00:00:02", nil, 0x0800, "plain")
	v, err := p.Classify(frame, port)
	if err != nil {
		t.Fatalf("classify: %v", err)
	}
	if v.Action != impl.ActionAdmit {
		t.Fatalf("action = %q, want admit (reason %q)", v.Action, v.Reason)
	}
	if !v.Native {
		t.Fatalf("untagged frame must be marked native")
	}
	if v.SVLAN != 10 || v.CVLAN != 0 {
		t.Fatalf("svlan/cvlan = %d/%d, want 10/0", v.SVLAN, v.CVLAN)
	}
	if got := p.PortStats("ge1").Admitted; got != 1 {
		t.Fatalf("admitted frames = %d, want 1", got)
	}
}

func TestClassifyRejectsWrongTagDepth(t *testing.T) {
	p := impl.NewQinQTagPolicy(policyConfig())
	single := buildFrame(t, "02:00:00:00:00:01", []tagSpec{{impl.TPID88A8, 100}}, 0x0800, "payload")
	v, err := p.Classify(single, qinqPort("ae1"))
	if err != nil {
		t.Fatalf("classify single tag: %v", err)
	}
	if v.Action != impl.ActionDrop || v.Reason != impl.ReasonDropShape {
		t.Fatalf("single tag on a qinq port = %+v, want shape drop", v)
	}
	tagged := doubleTagged(t, "02:00:00:00:00:02", 100, 20)
	v, err = p.Classify(tagged, accessPort("ge1"))
	if err != nil {
		t.Fatalf("classify tagged access: %v", err)
	}
	if v.Action != impl.ActionDrop || v.Reason != impl.ReasonDropShape {
		t.Fatalf("tagged frame on an access port = %+v, want shape drop", v)
	}
}

func TestClassifyRejectsUnsupportedTPID(t *testing.T) {
	p := impl.NewQinQTagPolicy(policyConfig())
	frame := buildFrame(t, "02:00:00:00:00:01", []tagSpec{{impl.TPID9100, 100}, {impl.TPID8100, 20}}, 0x0800, "payload")
	if _, err := p.Classify(frame, qinqPort("ae1")); !errors.Is(err, impl.ErrUnknownTPID) {
		t.Fatalf("unsupported tpid: err = %v, want ErrUnknownTPID", err)
	}
}

func TestParseExtractsTagStack(t *testing.T) {
	p := impl.NewQinQTagPolicy(policyConfig())
	stack, err := p.Parse(doubleTagged(t, "02:00:00:00:00:01", 100, 20))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !stack.HasOuter || stack.OuterTPID != impl.TPID88A8 || stack.OuterVLAN != 100 {
		t.Fatalf("outer tag = %+v", stack)
	}
	if !stack.HasInner || stack.InnerTPID != impl.TPID8100 || stack.InnerVLAN != 20 {
		t.Fatalf("inner tag = %+v", stack)
	}
	if stack.EtherType != 0x0800 {
		t.Fatalf("ethertype = %#x, want 0x0800", stack.EtherType)
	}
}

func TestParseRejectsShortFrame(t *testing.T) {
	p := impl.NewQinQTagPolicy(policyConfig())
	if _, err := p.Parse([]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}); !errors.Is(err, impl.ErrShortFrame) {
		t.Fatalf("short frame: err = %v, want ErrShortFrame", err)
	}
}

func TestEgressKeepReturnsIdenticalFrame(t *testing.T) {
	p := impl.NewQinQTagPolicy(policyConfig())
	frame := doubleTagged(t, "02:00:00:00:00:01", 100, 20)
	out, err := p.Egress(frame, qinqPort("ae1"))
	if err != nil {
		t.Fatalf("egress: %v", err)
	}
	if !bytes.Equal(out, frame) {
		t.Fatalf("keep action altered the frame")
	}
}

func TestEgressDropRejectsFrame(t *testing.T) {
	p := impl.NewQinQTagPolicy(policyConfig())
	port := qinqPort("ae1")
	port.EgressAction = impl.EgressDrop
	if _, err := p.Egress(doubleTagged(t, "02:00:00:00:00:01", 100, 20), port); !errors.Is(err, impl.ErrEgressDropped) {
		t.Fatalf("drop action: err = %v, want ErrEgressDropped", err)
	}
}

func TestEgressStripRemovesOuterTag(t *testing.T) {
	p := impl.NewQinQTagPolicy(policyConfig())
	frame := doubleTagged(t, "02:00:00:00:00:01", 100, 20)
	port := qinqPort("ae1")
	port.EgressAction = impl.EgressStrip
	out, err := p.Egress(frame, port)
	if err != nil {
		t.Fatalf("egress: %v", err)
	}
	if len(out) != len(frame)-4 {
		t.Fatalf("stripped length = %d, want %d", len(out), len(frame)-4)
	}
	if out[12] != 0x81 || out[13] != 0x00 {
		t.Fatalf("inner tpid after strip = %#x%#x, want 0x8100", out[12], out[13])
	}
	vid := uint16(out[14])<<8 | uint16(out[15])
	if vid&0x0fff != 20 {
		t.Fatalf("inner vlan after strip = %d, want 20", vid&0x0fff)
	}
	if out[16] != 0x08 || out[17] != 0x00 {
		t.Fatalf("ethertype after strip = %#x%#x, want 0x0800", out[16], out[17])
	}
}

func TestLearnRecordsMacBySVLAN(t *testing.T) {
	p := impl.NewQinQTagPolicy(policyConfig())
	port := qinqPort("ae1")
	if err := p.SetPortConfig(port); err != nil {
		t.Fatalf("set port config: %v", err)
	}
	if err := p.Learn("ae1", doubleTagged(t, "02:00:00:00:00:01", 100, 20)); err != nil {
		t.Fatalf("learn: %v", err)
	}
	owner, ok := p.Lookup(100, "02:00:00:00:00:01")
	if !ok || owner != "ae1" {
		t.Fatalf("lookup = %q, %v, want ae1", owner, ok)
	}
	if got := p.EntryCount(); got != 1 {
		t.Fatalf("entry count = %d, want 1", got)
	}
	if got := p.PortStats("ae1").Learned; got != 1 {
		t.Fatalf("learned count = %d, want 1", got)
	}
	if _, ok := p.Lookup(200, "02:00:00:00:00:01"); ok {
		t.Fatalf("the same MAC must not appear under another service vlan")
	}
}

func TestLearnMovesMacWithoutDoubleWrite(t *testing.T) {
	p := impl.NewQinQTagPolicy(policyConfig())
	if err := p.SetPortConfig(qinqPort("ae1")); err != nil {
		t.Fatalf("set port config: %v", err)
	}
	if err := p.SetPortConfig(qinqPort("ae2")); err != nil {
		t.Fatalf("set port config: %v", err)
	}
	if err := p.Learn("ae1", doubleTagged(t, "02:00:00:00:00:03", 100, 20)); err != nil {
		t.Fatalf("learn on ae1: %v", err)
	}
	if err := p.Learn("ae2", doubleTagged(t, "02:00:00:00:00:03", 100, 30)); err != nil {
		t.Fatalf("learn on ae2: %v", err)
	}
	owner, ok := p.Lookup(100, "02:00:00:00:00:03")
	if !ok || owner != "ae2" {
		t.Fatalf("lookup after move = %q, %v, want ae2", owner, ok)
	}
	if got := p.EntryCount(); got != 1 {
		t.Fatalf("entry count after move = %d, want 1", got)
	}
	if got := p.PortStats("ae2").Moved; got != 1 {
		t.Fatalf("moved count = %d, want 1", got)
	}
}

func TestLearnTableCapacityRejectsNewKey(t *testing.T) {
	cfg := policyConfig()
	cfg.MaxTableEntries = 2
	p := impl.NewQinQTagPolicy(cfg)
	if err := p.SetPortConfig(qinqPort("ae1")); err != nil {
		t.Fatalf("set port config: %v", err)
	}
	if err := p.Learn("ae1", doubleTagged(t, "02:00:00:00:00:11", 100, 20)); err != nil {
		t.Fatalf("learn first: %v", err)
	}
	if err := p.Learn("ae1", doubleTagged(t, "02:00:00:00:00:12", 100, 20)); err != nil {
		t.Fatalf("learn second: %v", err)
	}
	err := p.Learn("ae1", doubleTagged(t, "02:00:00:00:00:13", 100, 20))
	if !errors.Is(err, impl.ErrTableFull) {
		t.Fatalf("third mac: err = %v, want ErrTableFull", err)
	}
	if got := p.EntryCount(); got != 2 {
		t.Fatalf("entry count = %d, want 2", got)
	}
	if _, ok := p.Lookup(100, "02:00:00:00:00:13"); ok {
		t.Fatalf("rejected MAC must not be installed")
	}
}

func TestSnapshotRestoreKeepsLearningTable(t *testing.T) {
	p := impl.NewQinQTagPolicy(policyConfig())
	if err := p.SetPortConfig(qinqPort("ae1")); err != nil {
		t.Fatalf("set port config: %v", err)
	}
	if err := p.Learn("ae1", doubleTagged(t, "02:00:00:00:00:21", 100, 20)); err != nil {
		t.Fatalf("learn first: %v", err)
	}
	if err := p.Learn("ae1", doubleTagged(t, "02:00:00:00:00:22", 200, 20)); err != nil {
		t.Fatalf("learn second: %v", err)
	}
	data, err := p.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	restored := impl.NewQinQTagPolicy(policyConfig())
	if err := restored.Restore(data); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if got := restored.EntryCount(); got != 2 {
		t.Fatalf("entry count after restore = %d, want 2", got)
	}
	if owner, ok := restored.Lookup(200, "02:00:00:00:00:22"); !ok || owner != "ae1" {
		t.Fatalf("lookup after restore = %q, %v, want ae1", owner, ok)
	}
	if got := restored.PortStats("ae1").Learned; got != 2 {
		t.Fatalf("learned count after restore = %d, want 2", got)
	}
}
