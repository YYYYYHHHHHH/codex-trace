package rollout

import (
	"testing"
	"time"
)

func TestMergeCombinesIncrementalFacts(t *testing.T) {
	t0 := time.Unix(100, 0)
	a := TurnFacts{
		TurnID:      "t",
		Model:       "m1",
		StartedAt:   t0,
		Generations: []Generation{{ResponseID: "r1"}},
		Items:       map[string]Item{"i1": {ID: "i1", Status: "inProgress"}},
	}
	b := TurnFacts{
		TurnID:      "t",
		Completed:   true,
		CompletedAt: t0.Add(time.Second),
		DurationMs:  1000,
		Generations: []Generation{{ResponseID: "r1"}, {ResponseID: "r2"}},
		Items:       map[string]Item{"i1": {ID: "i1", Status: "completed"}, "i2": {ID: "i2"}},
	}

	got := Merge(a, b)

	if got.Model != "m1" || !got.StartedAt.Equal(t0) || !got.Completed || got.DurationMs != 1000 {
		t.Errorf("scalar merge wrong: %+v", got)
	}
	if len(got.Generations) != 2 {
		t.Errorf("generations must be deduped by response id, got %d", len(got.Generations))
	}
	if got.Items["i1"].Status != "completed" || len(got.Items) != 2 {
		t.Errorf("items merge wrong: %+v", got.Items)
	}
	if len(a.Items) != 1 || len(a.Generations) != 1 {
		t.Error("Merge must not modify its inputs")
	}
}

func TestMergeIntoEmpty(t *testing.T) {
	got := Merge(TurnFacts{}, TurnFacts{TurnID: "t", Aborted: true, AbortReason: "interrupted"})
	if got.TurnID != "t" || !got.Aborted || got.AbortReason != "interrupted" || got.Items == nil {
		t.Errorf("merge into empty = %+v", got)
	}
}
