package intent

// The agent loop is good at understanding what the user meant and bad at retyping a
// list. Asked "我目前部署的实例" with a 13-instance payload in front of it, ds-v4-flash
// wrote a table naming three — it dropped eleven real machines and invented a twelfth,
// `uhost-1exampleaa05`, borrowing the prefix of a real ID it had just been shown and
// confabulating the suffix, complete with a plausible GPU/CPU/image row. The direct-
// dispatch path cannot do this: it renders the same payload through Go string
// formatting, and across the same capture it listed instances six times with zero
// invented IDs.
//
// So the enumeration does not go through the model at all. The agent decides WHEN to
// show the list; this renders WHAT it shows, from the payload, deterministically. The
// stiffness objection to the fast path was about prose — nobody wants a fluent instance
// table, they want a correct one, and that is exactly where determinism is free.

// RenderInstanceTableFromDescribe renders the canonical instance table from a raw
// DescribeCompShareInstance payload, applying the same sort and display truncation the
// direct-dispatch handler applies, so the agent loop and the fast path show the user
// byte-identical tables.
//
// Returns ok=false when the payload holds no instances — the caller must then leave the
// agent to answer in its own words rather than emit an empty table.
func RenderInstanceTableFromDescribe(raw map[string]any) (string, bool) {
	data, err := instancesFromDescribeResult(raw)
	if err != nil || len(data.Instances) == 0 {
		return "", false
	}
	instances, shown, truncated := TruncateInstancesForDisplay(data.Instances, 0)
	meta := ResourceEnvelopeMeta{
		TotalCount: data.TotalCount,
		Shown:      shown,
		Truncated:  truncated,
	}
	return RenderResourceSummary(instances, meta), true
}
