package task

import (
	"errors"
	"reflect"
	"testing"

	impl "gsb/batch50-go-safety-b-15"
)

func verifierPolicy() impl.VerifierPolicy {
	return impl.VerifierPolicy{
		MaxSctsPerCert:   2,
		MaxMergeDelayMs:  60000,
		SkewToleranceMs:  50,
		MaxRejectReasons: 4,
	}
}

func baseCert() impl.Certificate {
	return impl.Certificate{Serial: "0a1b2c3d", NotBefore: 1000000, NotAfter: 2000000}
}

func activeLog(logID string) impl.LogPolicy {
	return impl.LogPolicy{LogID: logID, State: impl.LogActive, MaxMergeDelayMs: 60000}
}

func goodSct(logID string, ts int64) impl.SctEntry {
	return impl.SctEntry{
		LogID:     logID,
		Timestamp: ts,
		Signature: "sct-sig:9f8e7d6c5b4a39281706",
		HashAlg:   "sha256",
	}
}

func mustApprove(t *testing.T, v *impl.SctLogVerifier, p impl.LogPolicy) {
	t.Helper()
	if err := v.ApproveLog(p); err != nil {
		t.Fatalf("approve log %q: %v", p.LogID, err)
	}
}

func TestApproveLogStoresActivePolicy(t *testing.T) {
	v := impl.NewSctLogVerifier(verifierPolicy())
	if err := v.ApproveLog(impl.LogPolicy{LogID: "log-b", State: impl.LogActive, MaxMergeDelayMs: 30000}); err != nil {
		t.Fatalf("approve log-b: %v", err)
	}
	mustApprove(t, v, activeLog("log-a"))
	got, ok := v.Log("log-a")
	if !ok {
		t.Fatalf("approved log must be readable")
	}
	if got.State != impl.LogActive || got.MaxMergeDelayMs != 60000 {
		t.Fatalf("stored policy = %+v, want active with 60000 ms budget", got)
	}
	logs := v.ApprovedLogs()
	if len(logs) != 2 || logs[0] != "log-a" || logs[1] != "log-b" {
		t.Fatalf("approved logs = %v, want sorted [log-a log-b]", logs)
	}
}

func TestApproveLogIdempotentAndConflict(t *testing.T) {
	v := impl.NewSctLogVerifier(verifierPolicy())
	mustApprove(t, v, activeLog("log-a"))
	if err := v.ApproveLog(activeLog("log-a")); err != nil {
		t.Fatalf("identical re-registration must be idempotent, got %v", err)
	}
	changed := activeLog("log-a")
	changed.MaxMergeDelayMs = 90000
	if err := v.ApproveLog(changed); !errors.Is(err, impl.ErrLogConflict) {
		t.Fatalf("conflicting registration error = %v, want ErrLogConflict", err)
	}
	got, ok := v.Log("log-a")
	if !ok || got.MaxMergeDelayMs != 60000 {
		t.Fatalf("conflicting registration must not overwrite, got %+v ok=%v", got, ok)
	}
}

func TestApproveLogRejectsBadInput(t *testing.T) {
	v := impl.NewSctLogVerifier(verifierPolicy())
	if err := v.ApproveLog(impl.LogPolicy{State: impl.LogActive}); !errors.Is(err, impl.ErrBlankLogID) {
		t.Fatalf("blank log id error = %v, want ErrBlankLogID", err)
	}
	if err := v.ApproveLog(impl.LogPolicy{LogID: "log-a"}); !errors.Is(err, impl.ErrBadLogState) {
		t.Fatalf("empty state error = %v, want ErrBadLogState", err)
	}
	if err := v.ApproveLog(impl.LogPolicy{LogID: "log-a", State: "shadow"}); !errors.Is(err, impl.ErrBadLogState) {
		t.Fatalf("unknown state error = %v, want ErrBadLogState", err)
	}
	if logs := v.ApprovedLogs(); len(logs) != 0 {
		t.Fatalf("rejected registrations must not be stored, got %v", logs)
	}
}

func TestRevokeLogWatermark(t *testing.T) {
	v := impl.NewSctLogVerifier(verifierPolicy())
	mustApprove(t, v, activeLog("log-a"))
	if err := v.RevokeLog("log-a", 5000); err != nil {
		t.Fatalf("revoke log-a: %v", err)
	}
	got, ok := v.Log("log-a")
	if !ok || got.State != impl.LogRevoked || got.RevokedAt != 5000 {
		t.Fatalf("revoked entry = %+v ok=%v, want revoked at 5000", got, ok)
	}
	if err := v.RevokeLog("log-a", 7000); err != nil {
		t.Fatalf("repeated revoke must succeed, got %v", err)
	}
	got, _ = v.Log("log-a")
	if got.RevokedAt != 5000 {
		t.Fatalf("revocation watermark = %d, want the first one (5000)", got.RevokedAt)
	}
	if err := v.RevokeLog("ghost", 1); !errors.Is(err, impl.ErrUnknownLog) {
		t.Fatalf("revoking an unknown log error = %v, want ErrUnknownLog", err)
	}
}

func TestVerifyAllowsSingleValidSct(t *testing.T) {
	v := impl.NewSctLogVerifier(verifierPolicy())
	mustApprove(t, v, activeLog("log-a"))
	cert := baseCert()
	cert.Scts = []impl.SctEntry{goodSct("log-a", cert.NotBefore+10000)}
	d, err := v.Verify(cert, 1500000)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !d.Allowed || d.Verdict != impl.VerdictAllowed || d.Reason != impl.ReasonSctValid {
		t.Fatalf("decision = %+v, want an allowed certificate", d)
	}
	if d.UsedLog != "log-a" || d.Valid != 1 || d.Checked != 1 {
		t.Fatalf("decision counters = %+v, want used log-a valid 1 checked 1", d)
	}
	if len(d.Rejected) != 0 {
		t.Fatalf("rejected map = %v, want empty for a clean certificate", d.Rejected)
	}
	st := v.Stats()
	if st.Verified != 1 || st.Allowed != 1 || st.Denied != 0 {
		t.Fatalf("stats = %+v, want verified 1 allowed 1 denied 0", st)
	}
}

func TestVerifyDeniesWhenNoSctValid(t *testing.T) {
	v := impl.NewSctLogVerifier(verifierPolicy())
	mustApprove(t, v, activeLog("log-a"))
	cert := baseCert()
	cert.Scts = []impl.SctEntry{
		goodSct("rogue-log", cert.NotBefore+10000),
		{LogID: "log-a", Timestamp: cert.NotBefore + 10000, Signature: "", HashAlg: "sha256"},
	}
	d, err := v.Verify(cert, 1500000)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if d.Allowed || d.Verdict != impl.VerdictDenied || d.Reason != impl.ReasonNoValidSct {
		t.Fatalf("decision = %+v, want denied with no_valid_sct", d)
	}
	if d.Checked != 2 || d.Valid != 0 || d.UsedLog != "" {
		t.Fatalf("decision counters = %+v, want checked 2 valid 0 no used log", d)
	}
	if len(d.Rejected) != 2 {
		t.Fatalf("rejected map = %v, want one reason per sct", d.Rejected)
	}
	if d.Rejected["rogue-log"] != impl.ReasonUnknownLog {
		t.Fatalf("rogue-log reason = %q, want %q", d.Rejected["rogue-log"], impl.ReasonUnknownLog)
	}
	if d.Rejected["log-a"] != impl.ReasonBadSignature {
		t.Fatalf("empty signature reason = %q, want %q", d.Rejected["log-a"], impl.ReasonBadSignature)
	}
	if st := v.Stats(); st.Denied != 1 || st.Allowed != 0 {
		t.Fatalf("stats = %+v, want denied 1 allowed 0", st)
	}
}

func TestVerifyRejectsBlankSerial(t *testing.T) {
	v := impl.NewSctLogVerifier(verifierPolicy())
	mustApprove(t, v, activeLog("log-a"))
	cert := baseCert()
	cert.Serial = "  "
	cert.Scts = []impl.SctEntry{goodSct("log-a", cert.NotBefore+10000)}
	if _, err := v.Verify(cert, 1500000); !errors.Is(err, impl.ErrBlankSerial) {
		t.Fatalf("blank serial error = %v, want ErrBlankSerial", err)
	}
	if st := v.Stats(); st.Verified != 0 {
		t.Fatalf("rejected envelope must not be counted, got %+v", st)
	}
}

func TestVerifyRejectsInvertedValidity(t *testing.T) {
	v := impl.NewSctLogVerifier(verifierPolicy())
	mustApprove(t, v, activeLog("log-a"))
	cert := baseCert()
	cert.NotAfter = cert.NotBefore
	cert.Scts = []impl.SctEntry{goodSct("log-a", cert.NotBefore)}
	if _, err := v.Verify(cert, 1500000); !errors.Is(err, impl.ErrInvalidValidity) {
		t.Fatalf("inverted validity error = %v, want ErrInvalidValidity", err)
	}
}

func TestVerifyRejectsTooManyScts(t *testing.T) {
	v := impl.NewSctLogVerifier(verifierPolicy())
	mustApprove(t, v, activeLog("log-a"))
	cert := baseCert()
	cert.Scts = []impl.SctEntry{
		goodSct("log-a", cert.NotBefore+10000),
		goodSct("log-a", cert.NotBefore+20000),
		goodSct("log-a", cert.NotBefore+30000),
	}
	if _, err := v.Verify(cert, 1500000); !errors.Is(err, impl.ErrTooManyScts) {
		t.Fatalf("oversized sct list error = %v, want ErrTooManyScts", err)
	}
}

func TestVerifySctRequiresSignature(t *testing.T) {
	v := impl.NewSctLogVerifier(verifierPolicy())
	mustApprove(t, v, activeLog("log-a"))
	cert := baseCert()
	empty := goodSct("log-a", cert.NotBefore+10000)
	empty.Signature = ""
	res, err := v.VerifySct(cert, empty, 1500000)
	if err != nil {
		t.Fatalf("verify sct: %v", err)
	}
	if res.Verdict != impl.VerdictDenied || res.Reason != impl.ReasonBadSignature {
		t.Fatalf("empty signature result = %+v, want denied bad_signature", res)
	}
	short := goodSct("log-a", cert.NotBefore+10000)
	short.Signature = "sct-sig:abc"
	res, err = v.VerifySct(cert, short, 1500000)
	if err != nil {
		t.Fatalf("verify short signature sct: %v", err)
	}
	if res.Reason != impl.ReasonBadSignature {
		t.Fatalf("short signature reason = %q, want %q", res.Reason, impl.ReasonBadSignature)
	}
}

func TestVerifySctRejectsUnsupportedHash(t *testing.T) {
	v := impl.NewSctLogVerifier(verifierPolicy())
	mustApprove(t, v, activeLog("log-a"))
	cert := baseCert()
	weak := goodSct("log-a", cert.NotBefore+10000)
	weak.HashAlg = "md5"
	res, err := v.VerifySct(cert, weak, 1500000)
	if err != nil {
		t.Fatalf("verify md5 sct: %v", err)
	}
	if res.Verdict != impl.VerdictDenied || res.Reason != impl.ReasonUnsupportedHash {
		t.Fatalf("md5 result = %+v, want denied unsupported_hash", res)
	}
	strong := goodSct("log-a", cert.NotBefore+10000)
	strong.HashAlg = "sha384"
	res, err = v.VerifySct(cert, strong, 1500000)
	if err != nil {
		t.Fatalf("verify sha384 sct: %v", err)
	}
	if res.Verdict != impl.VerdictAllowed || res.Reason != impl.ReasonSctValid {
		t.Fatalf("sha384 result = %+v, want allowed sct_valid", res)
	}
}

func TestVerifySctRejectsInactiveLog(t *testing.T) {
	v := impl.NewSctLogVerifier(verifierPolicy())
	cert := baseCert()
	ts := cert.NotBefore + 10000
	mustApprove(t, v, activeLog("log-a"))
	mustApprove(t, v, impl.LogPolicy{LogID: "log-r", State: impl.LogRetired, MaxMergeDelayMs: 60000})
	if err := v.RevokeLog("log-a", cert.NotBefore); err != nil {
		t.Fatalf("revoke log-a: %v", err)
	}
	unknown, err := v.VerifySct(cert, goodSct("log-x", ts), 1500000)
	if err != nil {
		t.Fatalf("verify unknown log sct: %v", err)
	}
	if unknown.Reason != impl.ReasonUnknownLog {
		t.Fatalf("unknown log reason = %q, want %q", unknown.Reason, impl.ReasonUnknownLog)
	}
	retired, err := v.VerifySct(cert, goodSct("log-r", ts), 1500000)
	if err != nil {
		t.Fatalf("verify retired log sct: %v", err)
	}
	if retired.Reason != impl.ReasonLogRetired {
		t.Fatalf("retired log reason = %q, want %q", retired.Reason, impl.ReasonLogRetired)
	}
	revoked, err := v.VerifySct(cert, goodSct("log-a", ts), 1500000)
	if err != nil {
		t.Fatalf("verify revoked log sct: %v", err)
	}
	if revoked.Reason != impl.ReasonLogRevoked {
		t.Fatalf("revoked log reason = %q, want %q", revoked.Reason, impl.ReasonLogRevoked)
	}
}

func TestVerifySctRejectsTimestampOutsideWindow(t *testing.T) {
	pol := verifierPolicy()
	pol.MaxMergeDelayMs = 100000000
	v := impl.NewSctLogVerifier(pol)
	mustApprove(t, v, impl.LogPolicy{LogID: "log-a", State: impl.LogActive, MaxMergeDelayMs: 100000000})
	cert := baseCert()
	at := cert.NotBefore + 10000

	early, err := v.VerifySct(cert, goodSct("log-a", cert.NotBefore-51), at)
	if err != nil {
		t.Fatalf("verify early sct: %v", err)
	}
	if early.Reason != impl.ReasonOutsideValidity {
		t.Fatalf("early timestamp reason = %q, want %q", early.Reason, impl.ReasonOutsideValidity)
	}
	expired, err := v.VerifySct(cert, goodSct("log-a", cert.NotAfter+1), at)
	if err != nil {
		t.Fatalf("verify expired sct: %v", err)
	}
	if expired.Reason != impl.ReasonOutsideValidity {
		t.Fatalf("post expiry reason = %q, want %q", expired.Reason, impl.ReasonOutsideValidity)
	}
	future, err := v.VerifySct(cert, goodSct("log-a", at+51), at)
	if err != nil {
		t.Fatalf("verify future sct: %v", err)
	}
	if future.Reason != impl.ReasonTimestampSkew {
		t.Fatalf("future timestamp reason = %q, want %q", future.Reason, impl.ReasonTimestampSkew)
	}
	edge, err := v.VerifySct(cert, goodSct("log-a", at+50), at)
	if err != nil {
		t.Fatalf("verify tolerance edge sct: %v", err)
	}
	if edge.Verdict != impl.VerdictAllowed {
		t.Fatalf("timestamp exactly at the tolerance edge verdict = %q, want allowed", edge.Verdict)
	}
}

func TestMergeDelayComputesAndEnforcesLimit(t *testing.T) {
	v := impl.NewSctLogVerifier(verifierPolicy())
	log := activeLog("log-a")
	cert := baseCert()

	delay, err := v.MergeDelay(cert, goodSct("log-a", cert.NotBefore+1000), log)
	if err != nil {
		t.Fatalf("merge delay for an on time sct: %v", err)
	}
	if delay != 1000 {
		t.Fatalf("merge delay = %d, want 1000", delay)
	}
	late, err := v.MergeDelay(cert, goodSct("log-a", cert.NotBefore+60001), log)
	if !errors.Is(err, impl.ErrMergeDelay) {
		t.Fatalf("late sct error = %v, want ErrMergeDelay", err)
	}
	if late != 60001 {
		t.Fatalf("reported delay = %d, want the measured 60001", late)
	}
	skewed, err := v.MergeDelay(cert, goodSct("log-a", cert.NotBefore-51), log)
	if !errors.Is(err, impl.ErrTimestampSkew) {
		t.Fatalf("negative delay error = %v, want ErrTimestampSkew", err)
	}
	if skewed != -51 {
		t.Fatalf("negative delay = %d, want -51", skewed)
	}
	fallback, err := v.MergeDelay(cert, goodSct("log-a", cert.NotBefore+59000), impl.LogPolicy{LogID: "log-a", State: impl.LogActive})
	if err != nil || fallback != 59000 {
		t.Fatalf("unset log budget must fall back to the policy default, delay=%d err=%v", fallback, err)
	}
}

func TestSnapshotRestoreRoundTrip(t *testing.T) {
	v := impl.NewSctLogVerifier(verifierPolicy())
	mustApprove(t, v, activeLog("log-a"))
	mustApprove(t, v, impl.LogPolicy{LogID: "log-b", State: impl.LogActive, MaxMergeDelayMs: 30000})
	if err := v.RevokeLog("log-b", 4000); err != nil {
		t.Fatalf("revoke log-b: %v", err)
	}
	good := baseCert()
	good.Scts = []impl.SctEntry{goodSct("log-a", good.NotBefore+10000)}
	if _, err := v.Verify(good, 1500000); err != nil {
		t.Fatalf("verify good certificate: %v", err)
	}
	bad := baseCert()
	bad.Scts = []impl.SctEntry{goodSct("ghost-log", bad.NotBefore+10000)}
	if _, err := v.Verify(bad, 1500000); err != nil {
		t.Fatalf("verify rejected certificate: %v", err)
	}

	data, err := v.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	restored := impl.NewSctLogVerifier(verifierPolicy())
	if err := restored.Restore(data); err != nil {
		t.Fatalf("restore: %v", err)
	}
	got, ok := restored.Log("log-b")
	if !ok || got.State != impl.LogRevoked || got.RevokedAt != 4000 {
		t.Fatalf("restored log-b = %+v ok=%v, want revoked at 4000", got, ok)
	}
	if before, after := v.Stats(), restored.Stats(); before != after {
		t.Fatalf("restored stats = %+v, want %+v", after, before)
	}
	if !reflect.DeepEqual(v.ApprovedLogs(), restored.ApprovedLogs()) {
		t.Fatalf("restored log list = %v, want %v", restored.ApprovedLogs(), v.ApprovedLogs())
	}

	if err := restored.Restore([]byte("not a snapshot payload")); !errors.Is(err, impl.ErrBadSnapshot) {
		t.Fatalf("garbage snapshot error = %v, want ErrBadSnapshot", err)
	}
	if _, ok := restored.Log("log-b"); !ok {
		t.Fatalf("failed restore must leave the live log list untouched")
	}
	if after := restored.Stats(); after != v.Stats() {
		t.Fatalf("failed restore must leave the counters untouched, got %+v", after)
	}
}
