package bond

import "testing"

func TestSchedulerWeightedDistribution(t *testing.T) {
	s := newScheduler(2)
	s.setWeight(0, 3)
	s.setWeight(1, 1)
	counts := make([]int, 2)
	for i := 0; i < 4000; i++ {
		idx, ok := s.pick()
		if !ok {
			t.Fatal("expected a pick")
		}
		counts[idx]++
	}
	ratio := float64(counts[0]) / float64(counts[1])
	if ratio < 2.6 || ratio > 3.4 {
		t.Fatalf("ratio %v not ~3 (counts=%v)", ratio, counts)
	}
}

func TestSchedulerSkipsDownFlow(t *testing.T) {
	s := newScheduler(2)
	s.setWeight(0, 1)
	s.setWeight(1, 1)
	s.setDown(0, true)
	for i := 0; i < 100; i++ {
		idx, ok := s.pick()
		if !ok || idx != 1 {
			t.Fatalf("expected only flow 1, got idx=%d ok=%v", idx, ok)
		}
	}
}

func TestSchedulerNoneEligible(t *testing.T) {
	s := newScheduler(1)
	s.setDown(0, true)
	if _, ok := s.pick(); ok {
		t.Fatal("expected no pick when all flows down")
	}
}
