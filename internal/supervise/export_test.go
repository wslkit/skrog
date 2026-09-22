package supervise

import "context"

// TickForTest runs one reconcile pass; the idle tests drive ticks directly
// instead of racing a real ticker.
func (s *Supervisor) TickForTest(ctx context.Context) { s.tick(ctx) }

// PruningForTest reports whether an automatic prune is in flight, so the
// shutdown tests (#423) can wait for one to start rather than sleeping and
// hoping.
func (s *Supervisor) PruningForTest() bool { return s.pruning.Load() }
