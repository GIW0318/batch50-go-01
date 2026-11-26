package task

import (
	"errors"
	"testing"

	impl "gsb/batch50-go-safety-c-24"
)

func matcherOptions() impl.Options {
	return impl.Options{MaxDepth: 4, MaxNodesPerArtifact: 64, MaxChains: 8}
}

func entryPoint() impl.EntryPoint {
	return impl.EntryPoint{Class: "app.Entry", Method: "readObject"}
}

func sinkPoint() impl.Sink {
	return impl.Sink{Class: "app.Exec", Method: "run"}
}

func baseDependency() impl.Dependency {
	return impl.Dependency{
		Group:    "com.example",
		Artifact: "gadgets",
		Version:  "1.2.3",
		Classes: []impl.ClassNode{
			{Name: "app.Entry", Methods: []impl.MethodNode{{Name: "readObject", Calls: []impl.Call{{TargetClass: "app.Exec", TargetMethod: "run", Kind: impl.CallVirtual}}}}},
			{Name: "app.Exec", Methods: []impl.MethodNode{{Name: "run"}}},
			{Name: "app.Helper", Methods: []impl.MethodNode{{Name: "noop"}}},
		},
	}
}

func directDependency() impl.Dependency {
	return impl.Dependency{
		Group:    "com.example",
		Artifact: "direct",
		Version:  "1.0.0",
		Classes: []impl.ClassNode{
			{Name: "app.Entry", Methods: []impl.MethodNode{{Name: "readObject", Calls: []impl.Call{{TargetClass: "app.Exec", TargetMethod: "run", Kind: impl.CallVirtual}}}}},
			{Name: "app.Exec", Methods: []impl.MethodNode{{Name: "run"}}},
		},
	}
}

func twoHopDependency() impl.Dependency {
	return impl.Dependency{
		Group:    "com.example",
		Artifact: "chain",
		Version:  "2.1.0",
		Classes: []impl.ClassNode{
			{Name: "app.Entry", Methods: []impl.MethodNode{{Name: "readObject", Calls: []impl.Call{{TargetClass: "app.Middle", TargetMethod: "trigger", Kind: impl.CallVirtual}}}}},
			{Name: "app.Middle", Methods: []impl.MethodNode{{Name: "trigger", Calls: []impl.Call{{TargetClass: "app.Exec", TargetMethod: "run", Kind: impl.CallVirtual}}}}},
			{Name: "app.Exec", Methods: []impl.MethodNode{{Name: "run"}}},
		},
	}
}

func bothDependency() impl.Dependency {
	return impl.Dependency{
		Group:    "com.example",
		Artifact: "both",
		Version:  "3.0.1",
		Classes: []impl.ClassNode{
			{Name: "app.Entry", Methods: []impl.MethodNode{{Name: "readObject", Calls: []impl.Call{
				{TargetClass: "app.Exec", TargetMethod: "run", Kind: impl.CallVirtual},
				{TargetClass: "app.Middle", TargetMethod: "trigger", Kind: impl.CallVirtual},
			}}}},
			{Name: "app.Middle", Methods: []impl.MethodNode{{Name: "trigger", Calls: []impl.Call{{TargetClass: "app.Exec", TargetMethod: "run", Kind: impl.CallVirtual}}}}},
			{Name: "app.Exec", Methods: []impl.MethodNode{{Name: "run"}}},
		},
	}
}

func TestIndexReturnsCanonicalCoordinate(t *testing.T) {
	m := impl.NewGadgetChainMatcher(matcherOptions())
	id, err := m.Index(baseDependency())
	if err != nil {
		t.Fatalf("index: %v", err)
	}
	if id.Key() != "com.example:gadgets:1.2.3" {
		t.Fatalf("artifact key = %q, want com.example:gadgets:1.2.3", id.Key())
	}
	if got := len(m.Artifacts()); got != 1 {
		t.Fatalf("indexed artifacts = %d, want 1", got)
	}
	if got := m.NodeCount(id.Key()); got != 3 {
		t.Fatalf("node count = %d, want one node per indexed method (3)", got)
	}
}

func TestIndexRejectsBlankCoordinate(t *testing.T) {
	m := impl.NewGadgetChainMatcher(matcherOptions())
	blankGroup := baseDependency()
	blankGroup.Group = "   "
	if _, err := m.Index(blankGroup); !errors.Is(err, impl.ErrBlankArtifact) {
		t.Fatalf("blank group must fail with ErrBlankArtifact, got %v", err)
	}
	blankVersion := baseDependency()
	blankVersion.Version = ""
	if _, err := m.Index(blankVersion); !errors.Is(err, impl.ErrBlankArtifact) {
		t.Fatalf("blank version must fail with ErrBlankArtifact, got %v", err)
	}
	if got := len(m.Artifacts()); got != 0 {
		t.Fatalf("rejected artifacts must not be indexed, got %d", got)
	}
}

func TestIndexRejectsBadVersion(t *testing.T) {
	m := impl.NewGadgetChainMatcher(matcherOptions())
	dep := baseDependency()
	dep.Version = "1.2"
	if _, err := m.Index(dep); !errors.Is(err, impl.ErrBadVersion) {
		t.Fatalf("two segment version must fail with ErrBadVersion, got %v", err)
	}
}

func TestIndexEnforcesNodeLimit(t *testing.T) {
	opts := matcherOptions()
	opts.MaxNodesPerArtifact = 2
	m := impl.NewGadgetChainMatcher(opts)
	if _, err := m.Index(baseDependency()); !errors.Is(err, impl.ErrNodeLimit) {
		t.Fatalf("three methods under a cap of two must fail with ErrNodeLimit, got %v", err)
	}
	if got := len(m.Artifacts()); got != 0 {
		t.Fatalf("rejected artifact must not be indexed, got %d", got)
	}
}

func TestIndexReplacesSameCoordinate(t *testing.T) {
	m := impl.NewGadgetChainMatcher(matcherOptions())
	if _, err := m.Index(baseDependency()); err != nil {
		t.Fatalf("first index: %v", err)
	}
	shrink := impl.Dependency{
		Group:    "com.example",
		Artifact: "gadgets",
		Version:  "1.2.3",
		Classes: []impl.ClassNode{
			{Name: "app.Entry", Methods: []impl.MethodNode{{Name: "readObject"}}},
			{Name: "app.Exec", Methods: []impl.MethodNode{{Name: "run"}}},
		},
	}
	id, err := m.Index(shrink)
	if err != nil {
		t.Fatalf("replacing index: %v", err)
	}
	if got := len(m.Artifacts()); got != 1 {
		t.Fatalf("artifacts after replacement = %d, want a single coordinate", got)
	}
	if got := m.NodeCount(id.Key()); got != 2 {
		t.Fatalf("node count after replacement = %d, want 2", got)
	}
}

func TestQueryFindsDirectChain(t *testing.T) {
	m := impl.NewGadgetChainMatcher(matcherOptions())
	if _, err := m.Index(directDependency()); err != nil {
		t.Fatalf("index: %v", err)
	}
	chains, err := m.Query(entryPoint(), sinkPoint())
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(chains) != 1 {
		t.Fatalf("chains = %d, want the single direct path", len(chains))
	}
	chain := chains[0]
	if chain.ID == "" || chain.Depth != 1 || len(chain.Hops) != 2 {
		t.Fatalf("chain shape = %+v, want one hop above the entry", chain)
	}
	if chain.Status != impl.ChainUsable {
		t.Fatalf("chain status = %q, want %q", chain.Status, impl.ChainUsable)
	}
	if chain.Hops[0].Class != "app.Entry" || chain.Hops[0].Method != "readObject" {
		t.Fatalf("entry hop = %+v, want app.Entry.readObject", chain.Hops[0])
	}
	if chain.Hops[0].Artifact.Key() != "com.example:direct:1.0.0" {
		t.Fatalf("hop artifact = %q, want the declaring coordinate", chain.Hops[0].Artifact.Key())
	}
	if chain.Hops[1].Class != "app.Exec" || chain.Hops[1].Method != "run" {
		t.Fatalf("sink hop = %+v, want app.Exec.run", chain.Hops[1])
	}
	if len(chain.Artifacts) != 1 {
		t.Fatalf("chain artifacts = %v, want the single declaring artifact", chain.Artifacts)
	}
}

func TestQueryRanksShortestFirst(t *testing.T) {
	m := impl.NewGadgetChainMatcher(matcherOptions())
	if _, err := m.Index(bothDependency()); err != nil {
		t.Fatalf("index: %v", err)
	}
	chains, err := m.Query(entryPoint(), sinkPoint())
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(chains) != 2 {
		t.Fatalf("chains = %d, want the direct and the two hop path", len(chains))
	}
	if chains[0].Depth != 1 || chains[1].Depth != 2 {
		t.Fatalf("depth order = %d,%d, want 1 then 2", chains[0].Depth, chains[1].Depth)
	}
	if chains[0].ID == chains[1].ID {
		t.Fatalf("chains must get distinct ids, both are %q", chains[0].ID)
	}
	first := []string{chains[0].ID, chains[1].ID}
	again, err := m.Query(entryPoint(), sinkPoint())
	if err != nil {
		t.Fatalf("second query: %v", err)
	}
	if len(again) != 2 || again[0].ID != first[0] || again[1].ID != first[1] {
		t.Fatalf("repeated query returned %v, want stable ids %v", again, first)
	}
}

func TestQueryNoPath(t *testing.T) {
	m := impl.NewGadgetChainMatcher(matcherOptions())
	if _, err := m.Index(twoHopDependency()); err != nil {
		t.Fatalf("index: %v", err)
	}
	missing := impl.Sink{Class: "app.Missing", Method: "run"}
	if _, err := m.Query(entryPoint(), missing); !errors.Is(err, impl.ErrNoPath) {
		t.Fatalf("unreachable sink must fail with ErrNoPath, got %v", err)
	}
}

func TestQueryDepthPruning(t *testing.T) {
	opts := matcherOptions()
	opts.MaxDepth = 1
	m := impl.NewGadgetChainMatcher(opts)
	if _, err := m.Index(twoHopDependency()); err != nil {
		t.Fatalf("index: %v", err)
	}
	if _, err := m.Query(entryPoint(), sinkPoint()); !errors.Is(err, impl.ErrNoPath) {
		t.Fatalf("a path longer than the depth cap must be pruned, got %v", err)
	}
}

func TestExplainReturnsHops(t *testing.T) {
	m := impl.NewGadgetChainMatcher(matcherOptions())
	if _, err := m.Index(directDependency()); err != nil {
		t.Fatalf("index: %v", err)
	}
	chains, err := m.Query(entryPoint(), sinkPoint())
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	trace, err := m.Explain(chains[0].ID)
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	if trace.ChainID != chains[0].ID {
		t.Fatalf("trace chain = %q, want %q", trace.ChainID, chains[0].ID)
	}
	if trace.Status != impl.ChainUsable || trace.Reason != impl.ReasonUsable {
		t.Fatalf("trace status=%q reason=%q, want usable/reason_usable", trace.Status, trace.Reason)
	}
	if len(trace.Hops) != len(chains[0].Hops) {
		t.Fatalf("trace hops = %d, want %d", len(trace.Hops), len(chains[0].Hops))
	}
	if trace.Hops[1].Method != "run" || trace.Hops[0].Method != "readObject" {
		t.Fatalf("trace hop order = %+v, want entry then sink", trace.Hops)
	}
	if _, err := m.Explain("chain-404"); !errors.Is(err, impl.ErrUnknownChain) {
		t.Fatalf("unknown chain must fail with ErrUnknownChain, got %v", err)
	}
}

func TestValidateUsable(t *testing.T) {
	m := impl.NewGadgetChainMatcher(matcherOptions())
	if _, err := m.Index(directDependency()); err != nil {
		t.Fatalf("index: %v", err)
	}
	chains, err := m.Query(entryPoint(), sinkPoint())
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	env := impl.EnvSpec{Artifacts: []impl.ArtifactID{{Group: "com.example", Artifact: "direct", Version: "1.0.0"}}}
	verdict, err := m.Validate(chains[0].ID, env)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if !verdict.Usable {
		t.Fatalf("matching environment must be usable, got %+v", verdict)
	}
	if verdict.Reason != impl.ReasonUsable {
		t.Fatalf("verdict reason = %q, want %q", verdict.Reason, impl.ReasonUsable)
	}
	if len(verdict.Missing) != 0 {
		t.Fatalf("verdict missing = %v, want none", verdict.Missing)
	}
}

func TestValidateVersionMismatch(t *testing.T) {
	m := impl.NewGadgetChainMatcher(matcherOptions())
	if _, err := m.Index(directDependency()); err != nil {
		t.Fatalf("index: %v", err)
	}
	chains, err := m.Query(entryPoint(), sinkPoint())
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	env := impl.EnvSpec{Artifacts: []impl.ArtifactID{{Group: "com.example", Artifact: "direct", Version: "2.0.0"}}}
	if _, err := m.Validate(chains[0].ID, env); !errors.Is(err, impl.ErrVersionMismatch) {
		t.Fatalf("a different major version must fail with ErrVersionMismatch, got %v", err)
	}
}

func TestValidateMissingArtifact(t *testing.T) {
	m := impl.NewGadgetChainMatcher(matcherOptions())
	if _, err := m.Index(directDependency()); err != nil {
		t.Fatalf("index: %v", err)
	}
	chains, err := m.Query(entryPoint(), sinkPoint())
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if _, err := m.Validate(chains[0].ID, impl.EnvSpec{}); !errors.Is(err, impl.ErrArtifactMissing) {
		t.Fatalf("an environment without the artifact must fail with ErrArtifactMissing, got %v", err)
	}
}

func TestInvalidateRemovesArtifact(t *testing.T) {
	m := impl.NewGadgetChainMatcher(matcherOptions())
	id, err := m.Index(baseDependency())
	if err != nil {
		t.Fatalf("index: %v", err)
	}
	if err := m.Invalidate(id.Key()); err != nil {
		t.Fatalf("invalidate: %v", err)
	}
	if got := len(m.Artifacts()); got != 0 {
		t.Fatalf("artifacts after invalidation = %d, want 0", got)
	}
	if got := m.NodeCount(id.Key()); got != 0 {
		t.Fatalf("node count after invalidation = %d, want 0", got)
	}
	if err := m.Invalidate("com.example:nope:9.9.9"); !errors.Is(err, impl.ErrUnknownArtifact) {
		t.Fatalf("invalidating an unknown coordinate must fail with ErrUnknownArtifact, got %v", err)
	}
}

func TestSnapshotRestoreKeepsIndex(t *testing.T) {
	opts := matcherOptions()
	m := impl.NewGadgetChainMatcher(opts)
	if _, err := m.Index(bothDependency()); err != nil {
		t.Fatalf("index: %v", err)
	}
	chains, err := m.Query(entryPoint(), sinkPoint())
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	data, err := m.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if len(data) == 0 {
		t.Fatalf("snapshot must not be empty")
	}
	restored := impl.NewGadgetChainMatcher(opts)
	if err := restored.Restore(data); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if got := len(restored.Artifacts()); got != 1 {
		t.Fatalf("restored artifacts = %d, want 1", got)
	}
	again, err := restored.Query(entryPoint(), sinkPoint())
	if err != nil {
		t.Fatalf("query after restore: %v", err)
	}
	if len(again) != 2 || again[0].ID != chains[0].ID || again[1].ID != chains[1].ID {
		t.Fatalf("restored chains = %v, want the original ids", again)
	}
	if _, ok := restored.Chain(chains[0].ID); !ok {
		t.Fatalf("restored matcher lost a matched chain")
	}
	if err := restored.Restore([]byte("{not-a-snapshot")); !errors.Is(err, impl.ErrBadSnapshot) {
		t.Fatalf("corrupt snapshot must fail with ErrBadSnapshot, got %v", err)
	}
	if _, ok := restored.Chain(chains[0].ID); !ok {
		t.Fatalf("failed restore must not touch live state")
	}
}
