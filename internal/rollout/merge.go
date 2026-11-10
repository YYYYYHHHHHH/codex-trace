package rollout

// Merge 把同一轮次在两次增量扫描中得到的事实合并：后到的非零字段覆盖，
// generation 按 response_id 去重追加，item 按 id 覆盖。不修改入参。
func Merge(a, b TurnFacts) TurnFacts {
	out := a
	out.Items = make(map[string]Item, len(a.Items)+len(b.Items))
	for k, v := range a.Items {
		out.Items[k] = v
	}
	for k, v := range b.Items {
		out.Items[k] = v
	}
	if out.TurnID == "" {
		out.TurnID = b.TurnID
	}
	setStr(&out.RootTurnID, b.RootTurnID)
	setStr(&out.ParentTurnID, b.ParentTurnID)
	setStr(&out.Model, b.Model)
	setStr(&out.Effort, b.Effort)
	setStr(&out.AbortReason, b.AbortReason)
	if !b.StartedAt.IsZero() {
		out.StartedAt = b.StartedAt
	}
	if !b.CompletedAt.IsZero() {
		out.CompletedAt = b.CompletedAt
	}
	if b.DurationMs != 0 {
		out.DurationMs = b.DurationMs
	}
	if b.TTFTMs != 0 {
		out.TTFTMs = b.TTFTMs
	}
	out.Completed = a.Completed || b.Completed
	out.Aborted = a.Aborted || b.Aborted

	seen := make(map[string]bool, len(a.Generations))
	out.Generations = make([]Generation, 0, len(a.Generations)+len(b.Generations))
	for _, g := range a.Generations {
		seen[g.ResponseID] = true
		out.Generations = append(out.Generations, g)
	}
	for _, g := range b.Generations {
		if !seen[g.ResponseID] {
			seen[g.ResponseID] = true
			out.Generations = append(out.Generations, g)
		}
	}
	return out
}

func setStr(dst *string, v string) {
	if v != "" {
		*dst = v
	}
}
