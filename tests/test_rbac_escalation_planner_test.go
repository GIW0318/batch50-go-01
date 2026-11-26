package task

import (
	"errors"
	"sync"
	"testing"

	impl "gsb/batch50-go-safety-c-41"
)

func baseLimits() impl.Limits {
	return impl.Limits{MaxObjects: 32, MaxHops: 4, MaxPaths: 8, MaxAggregateDepth: 4}
}

func aliceSubject() impl.Subject {
	return impl.Subject{Kind: impl.SubjectUser, Name: "alice"}
}

func podTarget() impl.Target {
	return impl.Target{Namespace: "prod", Resource: "pods", Verb: "get"}
}

func readerGraph() []impl.Object {
	return []impl.Object{
		{
			Kind:  impl.KindClusterRole,
			Name:  "pod-reader",
			Rules: []impl.Rule{{Verbs: []string{"get", "list"}, Resources: []string{"pods"}}},
		},
		{
			Kind:      impl.KindRoleBinding,
			Namespace: "prod",
			Name:      "rb-reader",
			RoleRef:   "pod-reader",
			Subjects:  []impl.SubjectRef{{Kind: impl.SubjectUser, Name: "alice"}},
		},
	}
}

func escalateGraph() []impl.Object {
	return []impl.Object{
		{
			Kind:  impl.KindClusterRole,
			Name:  "exec-admin",
			Rules: []impl.Rule{{Verbs: []string{"create"}, Resources: []string{"pods/exec"}}},
		},
		{
			Kind:  impl.KindClusterRole,
			Name:  "binding-authority",
			Rules: []impl.Rule{{Verbs: []string{"bind"}, Resources: []string{"clusterrolebindings"}}},
		},
		{
			Kind:     impl.KindClusterRoleBinding,
			Name:     "crb-bob-binder",
			RoleRef:  "binding-authority",
			Subjects: []impl.SubjectRef{{Kind: impl.SubjectUser, Name: "bob"}},
		},
	}
}

func TestLoadIndexesGraphAndDirectGrant(t *testing.T) {
	p := impl.NewRbacEscalationPlanner(baseLimits())
	if err := p.Load(readerGraph()); err != nil {
		t.Fatalf("load graph: %v", err)
	}
	g := p.Graph()
	if g.ObjectCount != 2 {
		t.Fatalf("graph object count = %d, want 2", g.ObjectCount)
	}
	if g.Version != 1 {
		t.Fatalf("graph version = %d, want 1 after the first load", g.Version)
	}
	if !p.Holds(aliceSubject(), podTarget()) {
		t.Fatalf("alice must hold get on pods through rb-reader")
	}
	other := impl.Target{Namespace: "staging", Resource: "pods", Verb: "get"}
	if p.Holds(aliceSubject(), other) {
		t.Fatalf("a RoleBinding in prod must not grant the same verb in staging")
	}
}

func TestLoadRejectsBadKindAndKeepsPreviousGraph(t *testing.T) {
	p := impl.NewRbacEscalationPlanner(baseLimits())
	if err := p.Load(readerGraph()); err != nil {
		t.Fatalf("load baseline graph: %v", err)
	}
	broken := readerGraph()
	broken[0].Kind = "Policy"
	if err := p.Load(broken); !errors.Is(err, impl.ErrBadObject) {
		t.Fatalf("unknown kind must fail with ErrBadObject, got %v", err)
	}
	g := p.Graph()
	if g.ObjectCount != 2 || g.Version != 1 {
		t.Fatalf("a rejected load must not touch the graph, got count=%d version=%d", g.ObjectCount, g.Version)
	}
	if !p.Holds(aliceSubject(), podTarget()) {
		t.Fatalf("the previous graph must stay usable after a rejected load")
	}
}

func TestLoadRejectsDuplicateObject(t *testing.T) {
	p := impl.NewRbacEscalationPlanner(baseLimits())
	dupes := []impl.Object{
		{Kind: impl.KindClusterRole, Name: "ops-reader"},
		{Kind: impl.KindClusterRole, Name: "ops-reader"},
	}
	if err := p.Load(dupes); !errors.Is(err, impl.ErrDuplicateObject) {
		t.Fatalf("a repeated role must fail with ErrDuplicateObject, got %v", err)
	}
	if got := p.Graph().ObjectCount; got != 0 {
		t.Fatalf("rejected load left %d objects behind", got)
	}
}

func TestPathsReportsDirectBindingStep(t *testing.T) {
	p := impl.NewRbacEscalationPlanner(baseLimits())
	if err := p.Load(readerGraph()); err != nil {
		t.Fatalf("load graph: %v", err)
	}
	paths, err := p.Paths(aliceSubject(), podTarget())
	if err != nil {
		t.Fatalf("paths: %v", err)
	}
	if len(paths) != 1 {
		t.Fatalf("path count = %d, want exactly 1", len(paths))
	}
	got := paths[0]
	if got.PathID == "" {
		t.Fatalf("path id must not be empty")
	}
	if got.Hops != 1 || len(got.Steps) != 1 {
		t.Fatalf("direct path hops=%d steps=%d, want 1 and 1", got.Hops, len(got.Steps))
	}
	step := got.Steps[0]
	if step.Ordinal != 1 {
		t.Fatalf("first step ordinal = %d, want 1", step.Ordinal)
	}
	if step.Via != impl.ViaDirect {
		t.Fatalf("first step via = %q, want %q", step.Via, impl.ViaDirect)
	}
	if step.Binding != "rb-reader" || step.Role != "pod-reader" {
		t.Fatalf("step binding=%q role=%q, want rb-reader/pod-reader", step.Binding, step.Role)
	}
	if step.Grant != "get pods" {
		t.Fatalf("step grant = %q, want %q", step.Grant, "get pods")
	}
}

func TestPathsSortedByHopsThenPathID(t *testing.T) {
	p := impl.NewRbacEscalationPlanner(baseLimits())
	objects := []impl.Object{
		{
			Kind:  impl.KindClusterRole,
			Name:  "pod-reader",
			Rules: []impl.Rule{{Verbs: []string{"get"}, Resources: []string{"pods"}}},
		},
		{
			Kind:      impl.KindRoleBinding,
			Namespace: "prod",
			Name:      "rb-zulu",
			RoleRef:   "pod-reader",
			Subjects:  []impl.SubjectRef{{Kind: impl.SubjectUser, Name: "alice"}},
		},
		{
			Kind:      impl.KindRoleBinding,
			Namespace: "prod",
			Name:      "rb-alpha",
			RoleRef:   "pod-reader",
			Subjects:  []impl.SubjectRef{{Kind: impl.SubjectUser, Name: "alice"}},
		},
	}
	if err := p.Load(objects); err != nil {
		t.Fatalf("load graph: %v", err)
	}
	paths, err := p.Paths(aliceSubject(), podTarget())
	if err != nil {
		t.Fatalf("paths: %v", err)
	}
	if len(paths) != 2 {
		t.Fatalf("path count = %d, want 2 distinct bindings", len(paths))
	}
	if paths[0].Hops > paths[1].Hops {
		t.Fatalf("paths are not ordered by hops: %d then %d", paths[0].Hops, paths[1].Hops)
	}
	if paths[0].Hops == paths[1].Hops && paths[0].PathID > paths[1].PathID {
		t.Fatalf("paths of equal hops are not ordered by path id: %q then %q", paths[0].PathID, paths[1].PathID)
	}
	if paths[0].PathID == paths[1].PathID {
		t.Fatalf("two different bindings must not share the path id %q", paths[0].PathID)
	}
}

func TestExplainReturnsStepsAndRejectsUnknownID(t *testing.T) {
	p := impl.NewRbacEscalationPlanner(baseLimits())
	if err := p.Load(escalateGraph()); err != nil {
		t.Fatalf("load graph: %v", err)
	}
	target := impl.Target{Namespace: "prod", Resource: "pods/exec", Verb: "create"}
	paths, err := p.Paths(impl.Subject{Kind: impl.SubjectUser, Name: "bob"}, target)
	if err != nil {
		t.Fatalf("paths: %v", err)
	}
	if len(paths) == 0 {
		t.Fatalf("expected an escalation path for bob")
	}
	steps, err := p.Explain(paths[0].PathID)
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	if len(steps) != len(paths[0].Steps) {
		t.Fatalf("explained %d steps, path has %d", len(steps), len(paths[0].Steps))
	}
	for i, step := range steps {
		if step.Ordinal != i+1 {
			t.Fatalf("step %d ordinal = %d, want %d", i, step.Ordinal, i+1)
		}
	}
	if _, err := p.Explain("path-404"); !errors.Is(err, impl.ErrPathNotFound) {
		t.Fatalf("unknown path id must fail with ErrPathNotFound, got %v", err)
	}
}

func TestBindAuthorityAddsSecondHopOnlyWhenPrivilegeMissing(t *testing.T) {
	p := impl.NewRbacEscalationPlanner(baseLimits())
	if err := p.Load(escalateGraph()); err != nil {
		t.Fatalf("load graph: %v", err)
	}
	target := impl.Target{Namespace: "prod", Resource: "pods/exec", Verb: "create"}
	paths, err := p.Paths(impl.Subject{Kind: impl.SubjectUser, Name: "bob"}, target)
	if err != nil {
		t.Fatalf("paths: %v", err)
	}
	if len(paths) != 1 {
		t.Fatalf("path count = %d, want one bind based chain", len(paths))
	}
	if paths[0].Hops != 2 || len(paths[0].Steps) != 2 {
		t.Fatalf("bind chain hops=%d steps=%d, want 2 and 2", paths[0].Hops, len(paths[0].Steps))
	}
	second := paths[0].Steps[1]
	if second.Via != impl.ViaBind || second.Role != "exec-admin" {
		t.Fatalf("second step via=%q role=%q, want bind/exec-admin", second.Via, second.Role)
	}
	if second.Grant != "create pods/exec" {
		t.Fatalf("second step grant = %q, want %q", second.Grant, "create pods/exec")
	}

	direct := append(escalateGraph(), impl.Object{
		Kind:      impl.KindClusterRoleBinding,
		Name:      "crb-bob-exec",
		RoleRef:   "exec-admin",
		Subjects:  []impl.SubjectRef{{Kind: impl.SubjectUser, Name: "bob"}},
	})
	p2 := impl.NewRbacEscalationPlanner(baseLimits())
	if err := p2.Load(direct); err != nil {
		t.Fatalf("load graph with direct grant: %v", err)
	}
	paths2, err := p2.Paths(impl.Subject{Kind: impl.SubjectUser, Name: "bob"}, target)
	if err != nil {
		t.Fatalf("paths: %v", err)
	}
	if len(paths2) != 1 {
		t.Fatalf("a subject that already holds the privilege must get exactly one path, got %d", len(paths2))
	}
	if paths2[0].Hops != 1 {
		t.Fatalf("the surviving path has %d hops, want the direct 1 hop grant", paths2[0].Hops)
	}
}

func TestWildcardAndSubresourceJudgedSeparately(t *testing.T) {
	objects := []impl.Object{
		{
			Kind:  impl.KindClusterRole,
			Name:  "plain-pods",
			Rules: []impl.Rule{{Verbs: []string{"get"}, Resources: []string{"pods"}}},
		},
		{
			Kind:  impl.KindClusterRole,
			Name:  "pods-subresource",
			Rules: []impl.Rule{{Verbs: []string{"get"}, Resources: []string{"pods/*"}}},
		},
		{
			Kind:  impl.KindClusterRole,
			Name:  "everything",
			Rules: []impl.Rule{{Verbs: []string{"get"}, Resources: []string{"*"}}},
		},
		{
			Kind:      impl.KindRoleBinding,
			Namespace: "prod",
			Name:      "rb-plain",
			RoleRef:   "plain-pods",
			Subjects:  []impl.SubjectRef{{Kind: impl.SubjectUser, Name: "plain"}},
		},
		{
			Kind:      impl.KindRoleBinding,
			Namespace: "prod",
			Name:      "rb-sub",
			RoleRef:   "pods-subresource",
			Subjects:  []impl.SubjectRef{{Kind: impl.SubjectUser, Name: "sub"}},
		},
		{
			Kind:      impl.KindRoleBinding,
			Namespace: "prod",
			Name:      "rb-all",
			RoleRef:   "everything",
			Subjects:  []impl.SubjectRef{{Kind: impl.SubjectUser, Name: "all"}},
		},
	}
	p := impl.NewRbacEscalationPlanner(baseLimits())
	if err := p.Load(objects); err != nil {
		t.Fatalf("load graph: %v", err)
	}
	plain := impl.Subject{Kind: impl.SubjectUser, Name: "plain"}
	sub := impl.Subject{Kind: impl.SubjectUser, Name: "sub"}
	all := impl.Subject{Kind: impl.SubjectUser, Name: "all"}
	pods := impl.Target{Namespace: "prod", Resource: "pods", Verb: "get"}
	exec := impl.Target{Namespace: "prod", Resource: "pods/exec", Verb: "get"}
	if !p.Holds(plain, pods) {
		t.Fatalf("a rule on pods must cover the pods collection")
	}
	if p.Holds(plain, exec) {
		t.Fatalf("a rule on pods must not cover the pods/exec subresource")
	}
	if !p.Holds(sub, exec) {
		t.Fatalf("a rule on pods/* must cover the pods/exec subresource")
	}
	if p.Holds(sub, pods) {
		t.Fatalf("a rule on pods/* must not cover the pods collection itself")
	}
	if !p.Holds(all, pods) || !p.Holds(all, exec) {
		t.Fatalf("a wildcard resource must cover both the collection and the subresource")
	}
}

func TestAggregatedRoleExpandsSelectorsAndRejectsCycle(t *testing.T) {
	objects := []impl.Object{
		{
			Kind:      impl.KindClusterRole,
			Name:      "logs-aggregate",
			Aggregate: map[string]string{"aggregate-to-logs": "true"},
		},
		{
			Kind:   impl.KindClusterRole,
			Name:   "logs-base",
			Labels: map[string]string{"aggregate-to-logs": "true"},
			Rules:  []impl.Rule{{Verbs: []string{"get"}, Resources: []string{"pods/log"}}},
		},
		{
			Kind:     impl.KindClusterRoleBinding,
			Name:     "crb-logs",
			RoleRef:  "logs-aggregate",
			Subjects: []impl.SubjectRef{{Kind: impl.SubjectUser, Name: "carol"}},
		},
	}
	p := impl.NewRbacEscalationPlanner(baseLimits())
	if err := p.Load(objects); err != nil {
		t.Fatalf("load graph: %v", err)
	}
	expanded := p.Aggregated("logs-aggregate")
	if len(expanded) != 1 || expanded[0] != "logs-base" {
		t.Fatalf("aggregated roles = %v, want [logs-base]", expanded)
	}
	carol := impl.Subject{Kind: impl.SubjectUser, Name: "carol"}
	logTarget := impl.Target{Namespace: "prod", Resource: "pods/log", Verb: "get"}
	if !p.Holds(carol, logTarget) {
		t.Fatalf("an aggregated role must grant the rules of the selected role")
	}
	paths, err := p.Paths(carol, logTarget)
	if err != nil {
		t.Fatalf("paths: %v", err)
	}
	if len(paths) != 1 {
		t.Fatalf("expected one aggregate path, got %d paths", len(paths))
	}
	if paths[0].Steps[0].Via != impl.ViaAggregate {
		t.Fatalf("aggregate path first step via = %q, want %q", paths[0].Steps[0].Via, impl.ViaAggregate)
	}

	cyclic := []impl.Object{
		{Kind: impl.KindClusterRole, Name: "loop-a", Aggregate: map[string]string{"loop": "yes"}},
		{Kind: impl.KindClusterRole, Name: "loop-b", Labels: map[string]string{"loop": "yes"}, Aggregate: map[string]string{"loop": "yes"}},
	}
	pc := impl.NewRbacEscalationPlanner(baseLimits())
	if err := pc.Load(cyclic); !errors.Is(err, impl.ErrAggregateCycle) {
		t.Fatalf("a cyclic aggregate selector must fail with ErrAggregateCycle, got %v", err)
	}
}

func TestPrivilegesReturnsSortedUnique(t *testing.T) {
	objects := []impl.Object{
		{
			Kind: impl.KindClusterRole,
			Name: "reader-writer",
			Rules: []impl.Rule{
				{Verbs: []string{"list", "get"}, Resources: []string{"pods"}},
			},
		},
		{
			Kind: impl.KindClusterRole,
			Name: "extra-reader",
			Rules: []impl.Rule{
				{Verbs: []string{"get"}, Resources: []string{"pods"}},
				{Verbs: []string{"get"}, Resources: []string{"configmaps"}},
			},
		},
		{
			Kind:      impl.KindRoleBinding,
			Namespace: "prod",
			Name:      "rb-rw",
			RoleRef:   "reader-writer",
			Subjects:  []impl.SubjectRef{{Kind: impl.SubjectUser, Name: "alice"}},
		},
		{
			Kind:      impl.KindRoleBinding,
			Namespace: "prod",
			Name:      "rb-extra",
			RoleRef:   "extra-reader",
			Subjects:  []impl.SubjectRef{{Kind: impl.SubjectUser, Name: "alice"}},
		},
	}
	p := impl.NewRbacEscalationPlanner(baseLimits())
	if err := p.Load(objects); err != nil {
		t.Fatalf("load graph: %v", err)
	}
	got := p.Privileges(aliceSubject())
	want := []string{"get configmaps", "get pods", "list pods"}
	if len(got) != len(want) {
		t.Fatalf("privileges = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("privileges = %v, want the sorted unique list %v", got, want)
		}
	}
}

func TestDiffReportsAddedRemovedChanged(t *testing.T) {
	prev := impl.Graph{Version: 1, ObjectCount: 2, Objects: []impl.Object{
		{Kind: impl.KindClusterRole, Name: "alpha", Rules: []impl.Rule{{Verbs: []string{"get"}, Resources: []string{"pods"}}}},
		{Kind: impl.KindClusterRole, Name: "beta"},
	}}
	next := impl.Graph{Version: 2, ObjectCount: 2, Objects: []impl.Object{
		{Kind: impl.KindClusterRole, Name: "alpha", Rules: []impl.Rule{
			{Verbs: []string{"get"}, Resources: []string{"pods"}},
			{Verbs: []string{"get"}, Resources: []string{"secrets"}},
		}},
		{Kind: impl.KindClusterRole, Name: "gamma"},
	}}
	p := impl.NewRbacEscalationPlanner(baseLimits())
	deltas, err := p.Diff(prev, next)
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	if len(deltas) != 3 {
		t.Fatalf("delta count = %d, want 3", len(deltas))
	}
	if deltas[0].Op != impl.DeltaChanged || deltas[0].Name != "alpha" {
		t.Fatalf("first delta = %+v, want the changed alpha role", deltas[0])
	}
	if deltas[0].Detail != "rules:1->2" {
		t.Fatalf("changed detail = %q, want rules:1->2", deltas[0].Detail)
	}
	if deltas[1].Op != impl.DeltaRemoved || deltas[1].Name != "beta" {
		t.Fatalf("second delta = %+v, want the removed beta role", deltas[1])
	}
	if deltas[2].Op != impl.DeltaAdded || deltas[2].Name != "gamma" {
		t.Fatalf("third delta = %+v, want the added gamma role", deltas[2])
	}
}

func TestSnapshotRestoreKeepsPathIDsAndVersion(t *testing.T) {
	p := impl.NewRbacEscalationPlanner(baseLimits())
	if err := p.Load(readerGraph()); err != nil {
		t.Fatalf("load graph: %v", err)
	}
	paths, err := p.Paths(aliceSubject(), podTarget())
	if err != nil || len(paths) != 1 {
		t.Fatalf("paths before snapshot: n=%d err=%v", len(paths), err)
	}
	data, err := p.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if len(data) == 0 {
		t.Fatalf("snapshot payload must not be empty")
	}
	restored := impl.NewRbacEscalationPlanner(baseLimits())
	if err := restored.Restore(data); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if got := restored.Graph(); got.ObjectCount != 2 || got.Version != 1 {
		t.Fatalf("restored graph count=%d version=%d, want 2 and 1", got.ObjectCount, got.Version)
	}
	paths2, err := restored.Paths(aliceSubject(), podTarget())
	if err != nil || len(paths2) != 1 {
		t.Fatalf("paths after restore: n=%d err=%v", len(paths2), err)
	}
	if paths2[0].PathID != paths[0].PathID {
		t.Fatalf("restored path id = %q, want the original %q", paths2[0].PathID, paths[0].PathID)
	}
	if _, err := restored.Explain(paths[0].PathID); err != nil {
		t.Fatalf("explain after restore: %v", err)
	}
}

func TestRestoreRejectsCorruptSnapshot(t *testing.T) {
	p := impl.NewRbacEscalationPlanner(baseLimits())
	if err := p.Load(readerGraph()); err != nil {
		t.Fatalf("load graph: %v", err)
	}
	if err := p.Restore([]byte("{not-a-graph")); !errors.Is(err, impl.ErrBadSnapshot) {
		t.Fatalf("corrupt snapshot must fail with ErrBadSnapshot, got %v", err)
	}
	if got := p.Graph().ObjectCount; got != 2 {
		t.Fatalf("a rejected restore must keep the graph, got %d objects", got)
	}
	if !p.Holds(aliceSubject(), podTarget()) {
		t.Fatalf("the live graph must keep working after a rejected restore")
	}
}

func TestObjectHopAndPathLimitsEnforced(t *testing.T) {
	tight := impl.NewRbacEscalationPlanner(impl.Limits{MaxObjects: 2, MaxHops: 4, MaxPaths: 8, MaxAggregateDepth: 4})
	if err := tight.Load(readerGraph()); err != nil {
		t.Fatalf("a graph at the object cap must load, got %v", err)
	}
	overflow := append(readerGraph(), impl.Object{Kind: impl.KindClusterRole, Name: "third"})
	if err := tight.Load(overflow); !errors.Is(err, impl.ErrObjectLimit) {
		t.Fatalf("a graph above the object cap must fail with ErrObjectLimit, got %v", err)
	}

	shallow := impl.NewRbacEscalationPlanner(impl.Limits{MaxObjects: 32, MaxHops: 1, MaxPaths: 8, MaxAggregateDepth: 4})
	if err := shallow.Load(escalateGraph()); err != nil {
		t.Fatalf("load escalate graph: %v", err)
	}
	bob := impl.Subject{Kind: impl.SubjectUser, Name: "bob"}
	target := impl.Target{Namespace: "prod", Resource: "pods/exec", Verb: "create"}
	if _, err := shallow.Paths(bob, target); !errors.Is(err, impl.ErrNoPath) {
		t.Fatalf("a chain longer than the hop limit must fail with ErrNoPath, got %v", err)
	}

	wide := append(readerGraph(), impl.Object{
		Kind:      impl.KindRoleBinding,
		Namespace: "prod",
		Name:      "rb-second",
		RoleRef:   "pod-reader",
		Subjects:  []impl.SubjectRef{{Kind: impl.SubjectUser, Name: "alice"}},
	})
	narrow := impl.NewRbacEscalationPlanner(impl.Limits{MaxObjects: 32, MaxHops: 4, MaxPaths: 1, MaxAggregateDepth: 4})
	if err := narrow.Load(wide); err != nil {
		t.Fatalf("load wide graph: %v", err)
	}
	if _, err := narrow.Paths(aliceSubject(), podTarget()); !errors.Is(err, impl.ErrPathLimit) {
		t.Fatalf("more paths than the cap must fail with ErrPathLimit, got %v", err)
	}
}

func TestConcurrentHoldsDuringReloadStaysConsistent(t *testing.T) {
	p := impl.NewRbacEscalationPlanner(baseLimits())
	if err := p.Load(readerGraph()); err != nil {
		t.Fatalf("load graph: %v", err)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	failures := 0
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				if !p.Holds(aliceSubject(), podTarget()) {
					mu.Lock()
					failures++
					mu.Unlock()
				}
			}
		}()
	}
	for i := 0; i < 5; i++ {
		if err := p.Load(readerGraph()); err != nil {
			t.Fatalf("reload %d: %v", i, err)
		}
	}
	wg.Wait()
	if failures != 0 {
		t.Fatalf("%d concurrent reads saw a torn graph", failures)
	}
	if got := p.Graph().ObjectCount; got != 2 {
		t.Fatalf("graph object count after reloads = %d, want 2", got)
	}
}
