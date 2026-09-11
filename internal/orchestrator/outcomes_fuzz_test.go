package orchestrator

import "testing"

func FuzzPartialMapOutcomes(f *testing.F) {
	f.Add(uint8(3), []byte{0, 1, 2}, true, uint8(0))
	f.Add(uint8(5), []byte{2, 0}, false, uint8(0))
	f.Add(uint8(3), []byte{1, 1}, false, uint8(0))
	f.Add(uint8(0), []byte{}, true, uint8(0))
	f.Fuzz(func(t *testing.T, countByte uint8, indices []byte, complete bool, corruption uint8) {
		if len(indices) > 64 {
			t.Skip()
		}
		count := int(countByte % 33)
		outcomes := make([]StepInvocationOutcome, len(indices))
		seen := map[int]bool{}
		valid := !complete || len(indices) == count
		for position, indexByte := range indices {
			index := int(indexByte % 34)
			if index >= count || seen[index] {
				valid = false
			}
			seen[index] = true
			outcomes[position] = StepInvocationOutcome{NodeID: "workers", Attempt: 1, Status: StatusSucceeded, Scope: &ExecutionScope{Frames: []MapItemScopeFrame{{Kind: "map-item", MapID: "workers", Index: index}}}}
		}
		if len(outcomes) > 0 {
			first := &outcomes[0]
			switch corruption % 8 {
			case 1:
				first.NodeID = "other"
			case 2:
				first.Attempt = 2
			case 3:
				first.Scope = nil
			case 4:
				first.Scope.Frames = nil
			case 5:
				first.Scope.Frames[0].Kind = "other"
			case 6:
				first.Scope.Frames[0].MapID = "other"
			case 7:
				first.Scope.Frames = append(first.Scope.Frames, first.Scope.Frames[0])
			}
			if corruption%8 != 0 {
				valid = false
			}
		}
		indexed, err := mapOutcomesByIndex("workers", outcomes, count, complete)
		if (err == nil) != valid {
			t.Fatalf("count=%d indices=%v complete=%v corruption=%d: %v", count, indices, complete, corruption%8, err)
		}
		if err != nil {
			return
		}
		if len(indexed) != len(outcomes) {
			t.Fatal("outcomes lost")
		}
		for _, outcome := range outcomes {
			index := outcome.Scope.Frames[0].Index
			if indexed[index].Scope != outcome.Scope {
				t.Fatal("outcome associated with another source index")
			}
		}
	})
}
