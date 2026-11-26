package task

import (
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"

	impl "gsb/batch50-go-safety-c-37"
)

func newSim() *impl.IamPolicySimulator {
	return impl.NewIamPolicySimulator(impl.Limits{MaxPolicies: 32, MaxBatch: 8})
}

func allowPolicy(id string, actions, resources []string) impl.Policy {
	return impl.Policy{
		ID:        id,
		Layer:     impl.LayerIdentity,
		Effect:    impl.EffectAllow,
		Actions:   actions,
		Resources: resources,
	}
}

func s3Request(action, resource string) impl.Request {
	return impl.Request{
		Principal: "alice",
		Account:   "111122223333",
		Action:    action,
		Resource:  resource,
		Region:    "eu-west-1",
		Time:      time.Date(2025, time.June, 1, 12, 0, 0, 0, time.UTC),
		Tags:      map[string]string{"team": "core"},
	}
}

func mustGrant(t *testing.T, s *impl.IamPolicySimulator, policy impl.Policy) {
	t.Helper()
	if err := s.Grant(policy); err != nil {
		t.Fatalf("grant %s: %v", policy.ID, err)
	}
}

func mustEvaluate(t *testing.T, s *impl.IamPolicySimulator, req impl.Request) impl.Decision {
	t.Helper()
	d, err := s.Evaluate(req)
	if err != nil {
		t.Fatalf("evaluate %s on %s: %v", req.Action, req.Resource, err)
	}
	return d
}

func TestGrantRejectsBadPolicies(t *testing.T) {
	s := newSim()
	cases := []struct {
		name string
		p    impl.Policy
	}{
		{"blank id", impl.Policy{Layer: impl.LayerIdentity, Effect: impl.EffectAllow, Actions: []string{"s3:GetObject"}, Resources: []string{"*"}}},
		{"unknown layer", impl.Policy{ID: "p-x", Layer: "root", Effect: impl.EffectAllow, Actions: []string{"s3:GetObject"}, Resources: []string{"*"}}},
		{"bad effect", impl.Policy{ID: "p-x", Layer: impl.LayerIdentity, Effect: "Maybe", Actions: []string{"s3:GetObject"}, Resources: []string{"*"}}},
		{"no actions", impl.Policy{ID: "p-x", Layer: impl.LayerIdentity, Effect: impl.EffectAllow, Resources: []string{"*"}}},
		{"unsupported service", impl.Policy{ID: "p-x", Layer: impl.LayerIdentity, Effect: impl.EffectAllow, Actions: []string{"quantum:Teleport"}, Resources: []string{"*"}}},
	}
	for _, c := range cases {
		if err := s.Grant(c.p); !errors.Is(err, impl.ErrBadPolicy) {
			t.Fatalf("%s must fail with ErrBadPolicy, got %v", c.name, err)
		}
	}
	if got := s.Policies(); len(got) != 0 {
		t.Fatalf("a rejected policy must not be granted, got %+v", got)
	}
	mustGrant(t, s, allowPolicy("p-read", []string{"s3:GetObject"}, []string{"*"}))
	if err := s.Grant(allowPolicy("p-read", []string{"s3:PutObject"}, []string{"*"})); !errors.Is(err, impl.ErrDuplicatePolicy) {
		t.Fatalf("a repeated policy id must fail with ErrDuplicatePolicy, got %v", err)
	}
	if err := s.Revoke("p-read"); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if got := s.Policies(); len(got) != 0 {
		t.Fatalf("a revoked policy must be gone, got %+v", got)
	}
	if err := s.Revoke("p-read"); !errors.Is(err, impl.ErrNoPolicy) {
		t.Fatalf("revoking a policy twice must fail with ErrNoPolicy, got %v", err)
	}
}

func TestGrantRejectsUnknownConditionOperator(t *testing.T) {
	s := newSim()
	cases := []map[string]string{
		{"StringGreaterThan:aws:CurrentTime": "2025-01-01T00:00:00Z"},
		{"DateLessThan:aws:CurrentTime": "yesterday"},
		{"Bool:aws:PrincipalTag/admin": "yes"},
	}
	for _, cond := range cases {
		p := allowPolicy("p-cond", []string{"s3:GetObject"}, []string{"*"})
		p.Conditions = cond
		if err := s.Grant(p); !errors.Is(err, impl.ErrBadCondition) {
			t.Fatalf("condition %v must fail with ErrBadCondition, got %v", cond, err)
		}
	}
}

func TestEvaluateAllowsByIdentityPolicy(t *testing.T) {
	s := newSim()
	mustGrant(t, s, allowPolicy("p-read", []string{"s3:GetObject"}, []string{"arn:aws:s3:::logs/*"}))
	d := mustEvaluate(t, s, s3Request("s3:GetObject", "arn:aws:s3:::logs/a"))
	if d.Result != impl.ResultAllow || d.Reason != impl.ReasonIdentity {
		t.Fatalf("decision = %s/%s, want %s/%s", d.Result, d.Reason, impl.ResultAllow, impl.ReasonIdentity)
	}
	if d.PolicyID != "p-read" || d.Layer != impl.LayerIdentity {
		t.Fatalf("decision source = %q/%q, want p-read/identity", d.PolicyID, d.Layer)
	}
	if d.ID != "d-1" {
		t.Fatalf("first decision id = %q, want d-1", d.ID)
	}
	second := mustEvaluate(t, s, s3Request("s3:GetObject", "arn:aws:s3:::logs/b"))
	if second.ID != "d-2" {
		t.Fatalf("second decision id = %q, want d-2", second.ID)
	}
	stored, ok := s.DecisionFor("d-1")
	if !ok || stored.Result != impl.ResultAllow {
		t.Fatalf("stored decision = %+v, want the allow decision", stored)
	}
	stats := s.Stats()
	if stats.Evaluated != 2 || stats.Allowed != 2 || stats.Denied != 0 {
		t.Fatalf("stats = %+v, want two evaluated and two allowed", stats)
	}
}

func TestExplicitDenyBeatsAllow(t *testing.T) {
	s := newSim()
	mustGrant(t, s, allowPolicy("p-read", []string{"s3:Get*"}, []string{"*"}))
	mustGrant(t, s, impl.Policy{
		ID:        "p-deny-secret",
		Layer:     impl.LayerIdentity,
		Effect:    impl.EffectDeny,
		Actions:   []string{"s3:GetObject"},
		Resources: []string{"arn:aws:s3:::secret/*"},
	})
	d := mustEvaluate(t, s, s3Request("s3:GetObject", "arn:aws:s3:::secret/key"))
	if d.Result != impl.ResultDeny || d.Reason != impl.ReasonExplicitDeny {
		t.Fatalf("decision = %s/%s, want %s/%s", d.Result, d.Reason, impl.ResultDeny, impl.ReasonExplicitDeny)
	}
	if d.PolicyID != "p-deny-secret" {
		t.Fatalf("deny came from %q, want p-deny-secret", d.PolicyID)
	}
	stats := s.Stats()
	if stats.Denied != 1 || stats.Allowed != 0 {
		t.Fatalf("stats = %+v, want one denied and none allowed", stats)
	}
}

func TestActionWildcardMatchesOnlyListedActions(t *testing.T) {
	s := newSim()
	mustGrant(t, s, allowPolicy("p-get", []string{"s3:Get*"}, []string{"*"}))
	if d := mustEvaluate(t, s, s3Request("s3:GetObject", "arn:aws:s3:::logs/a")); d.Result != impl.ResultAllow {
		t.Fatalf("s3:GetObject must be allowed, got %s/%s", d.Result, d.Reason)
	}
	d := mustEvaluate(t, s, s3Request("s3:PutObject", "arn:aws:s3:::logs/a"))
	if d.Result != impl.ResultImplicit || d.Reason != impl.ReasonNoAllow {
		t.Fatalf("s3:PutObject must be an implicit deny, got %s/%s", d.Result, d.Reason)
	}
	if d.PolicyID != "" {
		t.Fatalf("an implicit deny must not name a policy, got %q", d.PolicyID)
	}
}

func TestResourceWildcardStopsAtSeparator(t *testing.T) {
	s := newSim()
	mustGrant(t, s, allowPolicy("p-logs", []string{"s3:GetObject"}, []string{"arn:aws:s3:::logs/*"}))
	if d := mustEvaluate(t, s, s3Request("s3:GetObject", "arn:aws:s3:::logs/2024")); d.Result != impl.ResultAllow {
		t.Fatalf("one segment after the bucket must match, got %s/%s", d.Result, d.Reason)
	}
	if d := mustEvaluate(t, s, s3Request("s3:GetObject", "arn:aws:s3:::logs/2024/01")); d.Result != impl.ResultImplicit {
		t.Fatalf("a wildcard spanning two segments must not match, got %s/%s", d.Result, d.Reason)
	}
	if d := mustEvaluate(t, s, s3Request("s3:GetObject", "arn:aws:s3:::logs")); d.Result != impl.ResultImplicit {
		t.Fatalf("the bare bucket must not match a key pattern, got %s/%s", d.Result, d.Reason)
	}
}

func TestConditionStringOperators(t *testing.T) {
	s := newSim()
	team := allowPolicy("p-team", []string{"s3:*"}, []string{"*"})
	team.Conditions = map[string]string{"StringEquals:aws:PrincipalTag/team": "core"}
	mustGrant(t, s, team)
	req := s3Request("s3:GetObject", "arn:aws:s3:::logs/a")
	if d := mustEvaluate(t, s, req); d.Result != impl.ResultAllow {
		t.Fatalf("the matching tag must allow, got %s/%s", d.Result, d.Reason)
	}
	other := s3Request("s3:GetObject", "arn:aws:s3:::logs/a")
	other.Tags = map[string]string{"team": "edge"}
	if d := mustEvaluate(t, s, other); d.Result != impl.ResultImplicit {
		t.Fatalf("a mismatched tag must not allow, got %s/%s", d.Result, d.Reason)
	}

	region := newSim()
	p := allowPolicy("p-eu", []string{"s3:*"}, []string{"*"})
	p.Conditions = map[string]string{"StringLike:aws:RequestedRegion": "eu-*"}
	mustGrant(t, region, p)
	if d := mustEvaluate(t, region, s3Request("s3:GetObject", "arn:aws:s3:::logs/a")); d.Result != impl.ResultAllow {
		t.Fatalf("eu-west-1 must match eu-*, got %s/%s", d.Result, d.Reason)
	}
	us := s3Request("s3:GetObject", "arn:aws:s3:::logs/a")
	us.Region = "us-east-1"
	if d := mustEvaluate(t, region, us); d.Result != impl.ResultImplicit {
		t.Fatalf("us-east-1 must not match eu-*, got %s/%s", d.Result, d.Reason)
	}
}

func TestConditionNullChecksExistence(t *testing.T) {
	s := newSim()
	p := allowPolicy("p-breakglass", []string{"iam:*"}, []string{"*"})
	p.Conditions = map[string]string{"Null:aws:PrincipalTag/breakglass": "true"}
	mustGrant(t, s, p)
	req := impl.Request{
		Principal: "alice", Account: "111122223333", Action: "iam:GetUser",
		Resource: "arn:aws:iam::111122223333:user/alice", Region: "eu-west-1",
		Time: time.Date(2025, time.June, 1, 12, 0, 0, 0, time.UTC), Tags: map[string]string{},
	}
	if d := mustEvaluate(t, s, req); d.Result != impl.ResultImplicit {
		t.Fatalf("a missing key under Null=true must not allow, got %s/%s", d.Result, d.Reason)
	}
	req.Tags = map[string]string{"breakglass": "1"}
	if d := mustEvaluate(t, s, req); d.Result != impl.ResultAllow {
		t.Fatalf("a present key under Null=true must allow, got %s/%s", d.Result, d.Reason)
	}

	absent := newSim()
	q := allowPolicy("p-normal", []string{"iam:*"}, []string{"*"})
	q.Conditions = map[string]string{"Null:aws:PrincipalTag/breakglass": "false"}
	mustGrant(t, absent, q)
	normal := req
	normal.Tags = map[string]string{}
	if d := mustEvaluate(t, absent, normal); d.Result != impl.ResultAllow {
		t.Fatalf("an absent key under Null=false must allow, got %s/%s", d.Result, d.Reason)
	}
}

func TestConditionDateLessThanUsesRequestTime(t *testing.T) {
	s := newSim()
	p := allowPolicy("p-window", []string{"s3:*"}, []string{"*"})
	p.Conditions = map[string]string{"DateLessThan:aws:CurrentTime": "2026-01-01T00:00:00Z"}
	mustGrant(t, s, p)
	if d := mustEvaluate(t, s, s3Request("s3:GetObject", "arn:aws:s3:::logs/a")); d.Result != impl.ResultAllow {
		t.Fatalf("a request before the deadline must allow, got %s/%s", d.Result, d.Reason)
	}
	late := s3Request("s3:GetObject", "arn:aws:s3:::logs/a")
	late.Time = time.Date(2026, time.June, 1, 0, 0, 0, 0, time.UTC)
	if d := mustEvaluate(t, s, late); d.Result != impl.ResultImplicit {
		t.Fatalf("a request after the deadline must not allow, got %s/%s", d.Result, d.Reason)
	}
}

func TestConditionForAllValuesSemantics(t *testing.T) {
	s := newSim()
	p := allowPolicy("p-roles", []string{"s3:*"}, []string{"*"})
	p.Conditions = map[string]string{"ForAllValues:StringEquals:aws:PrincipalTag/roles": "core"}
	mustGrant(t, s, p)
	good := s3Request("s3:GetObject", "arn:aws:s3:::logs/a")
	good.Tags = map[string]string{"roles": "core,core"}
	if d := mustEvaluate(t, s, good); d.Result != impl.ResultAllow {
		t.Fatalf("every value equal to core must allow, got %s/%s", d.Result, d.Reason)
	}
	bad := s3Request("s3:GetObject", "arn:aws:s3:::logs/a")
	bad.Tags = map[string]string{"roles": "core,ops"}
	if d := mustEvaluate(t, s, bad); d.Result != impl.ResultImplicit {
		t.Fatalf("a foreign value in the set must not allow, got %s/%s", d.Result, d.Reason)
	}
}

func TestUnknownConditionKeyFailsClosed(t *testing.T) {
	s := newSim()
	p := allowPolicy("p-dept", []string{"s3:*"}, []string{"*"})
	p.Conditions = map[string]string{"StringEquals:aws:Department": "security"}
	mustGrant(t, s, p)
	d := mustEvaluate(t, s, s3Request("s3:GetObject", "arn:aws:s3:::logs/a"))
	if d.Result != impl.ResultImplicit {
		t.Fatalf("an unmatchable condition key must fail closed, got %s/%s", d.Result, d.Reason)
	}
}

func TestUnsupportedRequestActionIsDenied(t *testing.T) {
	s := newSim()
	mustGrant(t, s, allowPolicy("p-all", []string{"*"}, []string{"*"}))
	req := s3Request("quantum:Teleport", "arn:aws:s3:::logs/a")
	d := mustEvaluate(t, s, req)
	if d.Result != impl.ResultImplicit || d.Reason != impl.ReasonUnsupportedActor {
		t.Fatalf("an unsupported action must be an implicit deny, got %s/%s", d.Result, d.Reason)
	}
	if d.PolicyID != "" {
		t.Fatalf("an unsupported action must not name a policy, got %q", d.PolicyID)
	}
	blank := s3Request("", "arn:aws:s3:::logs/a")
	if _, err := s.Evaluate(blank); !errors.Is(err, impl.ErrBadRequest) {
		t.Fatalf("a blank action must fail with ErrBadRequest, got %v", err)
	}
}

func TestSimulateMatchesSingleEvaluate(t *testing.T) {
	s := newSim()
	mustGrant(t, s, allowPolicy("p-read", []string{"s3:Get*"}, []string{"arn:aws:s3:::logs/*"}))
	mustGrant(t, s, impl.Policy{
		ID: "p-deny", Layer: impl.LayerIdentity, Effect: impl.EffectDeny,
		Actions: []string{"s3:GetObject"}, Resources: []string{"arn:aws:s3:::secret/*"},
	})
	batch := []impl.Request{
		s3Request("s3:GetObject", "arn:aws:s3:::logs/a"),
		s3Request("s3:PutObject", "arn:aws:s3:::logs/a"),
		s3Request("s3:GetObject", "arn:aws:s3:::secret/key"),
	}
	decisions, err := s.Simulate(batch)
	if err != nil {
		t.Fatalf("simulate: %v", err)
	}
	if len(decisions) != len(batch) {
		t.Fatalf("decisions = %d, want %d", len(decisions), len(batch))
	}
	wantResult := []string{impl.ResultAllow, impl.ResultImplicit, impl.ResultDeny}
	for i, want := range wantResult {
		if decisions[i].Result != want {
			t.Fatalf("decision %d result = %q, want %q", i, decisions[i].Result, want)
		}
		if decisions[i].ID != "d-"+strconv.Itoa(i+1) {
			t.Fatalf("decision %d id = %q, want d-%d", i, decisions[i].ID, i+1)
		}
	}
	single := mustEvaluate(t, s, batch[0])
	if single.Result != decisions[0].Result || single.Reason != decisions[0].Reason || single.PolicyID != decisions[0].PolicyID {
		t.Fatalf("single decision = %+v, batch decision = %+v", single, decisions[0])
	}
	small := impl.NewIamPolicySimulator(impl.Limits{MaxPolicies: 8, MaxBatch: 2})
	if _, err := small.Simulate(batch); !errors.Is(err, impl.ErrTooManyRequests) {
		t.Fatalf("an oversized batch must fail with ErrTooManyRequests, got %v", err)
	}
}

func TestExplainDescribesDecision(t *testing.T) {
	s := newSim()
	mustGrant(t, s, allowPolicy("p-read", []string{"s3:Get*"}, []string{"*"}))
	d := mustEvaluate(t, s, s3Request("s3:GetObject", "arn:aws:s3:::logs/a"))
	tr, err := s.Explain(d.ID)
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	if tr.DecisionID != d.ID || tr.Result != d.Result || tr.Reason != d.Reason {
		t.Fatalf("trace = %+v, want the recorded decision", tr)
	}
	if len(tr.Policies) != 1 || tr.Policies[0] != "p-read" {
		t.Fatalf("trace policies = %v, want [p-read]", tr.Policies)
	}
	if len(tr.Steps) < 2 {
		t.Fatalf("trace steps = %v, want at least two steps", tr.Steps)
	}
	if _, err := s.Explain("d-404"); !errors.Is(err, impl.ErrNoDecision) {
		t.Fatalf("explaining an unknown decision must fail with ErrNoDecision, got %v", err)
	}
}

func TestSnapshotRestoreAndConcurrentIDs(t *testing.T) {
	s := newSim()
	mustGrant(t, s, allowPolicy("p-read", []string{"s3:Get*"}, []string{"*"}))
	mustGrant(t, s, allowPolicy("p-write", []string{"s3:Put*"}, []string{"arn:aws:s3:::logs/*"}))
	mustEvaluate(t, s, s3Request("s3:GetObject", "arn:aws:s3:::logs/a"))
	data, err := s.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	restored := newSim()
	if err := restored.Restore(data); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if got := restored.Policies(); len(got) != 2 || got[0].ID != "p-read" || got[1].ID != "p-write" {
		t.Fatalf("restored policies = %+v, want p-read and p-write", got)
	}
	if stats := restored.Stats(); stats.Evaluated != 1 || stats.Allowed != 1 {
		t.Fatalf("restored stats = %+v, want one allowed decision", stats)
	}
	if d := mustEvaluate(t, restored, s3Request("s3:GetObject", "arn:aws:s3:::logs/a")); d.Result != impl.ResultAllow {
		t.Fatalf("a restored simulator must still allow, got %s/%s", d.Result, d.Reason)
	}
	if err := restored.Restore([]byte("###broken###")); !errors.Is(err, impl.ErrBadSnapshot) {
		t.Fatalf("corrupt snapshot must fail with ErrBadSnapshot, got %v", err)
	}
	if got := restored.Policies(); len(got) != 2 {
		t.Fatalf("a rejected restore must not touch the policies, got %+v", got)
	}

	const workers = 8
	var wg sync.WaitGroup
	ids := make([]string, workers)
	failures := make([]error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			d, err := restored.Evaluate(s3Request("s3:GetObject", "arn:aws:s3:::logs/z"))
			if err != nil {
				failures[idx] = err
				return
			}
			ids[idx] = d.ID
		}(i)
	}
	wg.Wait()
	seen := map[string]bool{}
	for i := 0; i < workers; i++ {
		if failures[i] != nil {
			t.Fatalf("worker %d failed: %v", i, failures[i])
		}
		if ids[i] == "" || seen[ids[i]] {
			t.Fatalf("worker %d produced a duplicate or empty id %q", i, ids[i])
		}
		seen[ids[i]] = true
	}
	if stats := restored.Stats(); stats.Evaluated != 1+1+workers {
		t.Fatalf("stats = %+v, want %d evaluated decisions", stats, 1+1+workers)
	}
}
