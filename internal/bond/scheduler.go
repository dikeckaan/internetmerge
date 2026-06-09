package bond

import "sync"

// scheduler chooses which flow index each outbound segment travels on, using
// smooth weighted round-robin over the flows that are up. Safe for concurrent use.
type scheduler struct {
	mu      sync.Mutex
	weight  []int
	current []int
	down    []bool
}

func newScheduler(nFlows int) *scheduler {
	s := &scheduler{
		weight:  make([]int, nFlows),
		current: make([]int, nFlows),
		down:    make([]bool, nFlows),
	}
	for i := range s.weight {
		s.weight[i] = 1
	}
	return s
}

func (s *scheduler) setWeight(i, w int) {
	if w < 0 {
		w = 0
	}
	s.mu.Lock()
	s.weight[i] = w
	s.mu.Unlock()
}

func (s *scheduler) setDown(i int, down bool) {
	s.mu.Lock()
	s.down[i] = down
	s.mu.Unlock()
}

// pick returns the next eligible flow index, or ok=false if none are eligible.
func (s *scheduler) pick() (int, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	best, total := -1, 0
	for i := range s.weight {
		if s.down[i] || s.weight[i] <= 0 {
			continue
		}
		s.current[i] += s.weight[i]
		total += s.weight[i]
		if best == -1 || s.current[i] > s.current[best] {
			best = i
		}
	}
	if best == -1 {
		return 0, false
	}
	s.current[best] -= total
	return best, true
}

// eligibleCount returns how many flows are currently up with positive weight.
func (s *scheduler) eligibleCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for i := range s.weight {
		if !s.down[i] && s.weight[i] > 0 {
			n++
		}
	}
	return n
}
