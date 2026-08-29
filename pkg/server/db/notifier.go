package db

import "sync"

// JobNotifier is a per-job, in-process terminal-status notifier. It lets a
// waiter (e.g. the probe handler) block on a job reaching a terminal state
// instead of polling the database on a fixed interval (TSI-2520).
//
// Notify is non-blocking: each subscriber holds a buffered channel of size 1,
// so a Notify for a subscriber that is not currently blocked is collapsed to
// a single pending wake-up. Subscribers must re-check the database after
// waking — the notification is a hint, the DB is the source of truth.
type JobNotifier struct {
	mu   sync.Mutex
	subs map[string]map[chan struct{}]struct{}
}

func newJobNotifier() *JobNotifier {
	return &JobNotifier{subs: make(map[string]map[chan struct{}]struct{})}
}

// Subscribe registers a waiter for jobID and returns a channel that receives
// a value on the next terminal-status Notify for jobID, plus a cancel func
// that unregisters the waiter. Callers must re-check the database after
// receiving: the notification is a hint, the DB is the source of truth.
func (n *JobNotifier) Subscribe(jobID string) (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)

	n.mu.Lock()
	if n.subs[jobID] == nil {
		n.subs[jobID] = make(map[chan struct{}]struct{})
	}
	n.subs[jobID][ch] = struct{}{}
	n.mu.Unlock()

	var once sync.Once
	cancel := func() {
		once.Do(func() {
			n.mu.Lock()
			if set, ok := n.subs[jobID]; ok {
				delete(set, ch)
				if len(set) == 0 {
					delete(n.subs, jobID)
				}
			}
			n.mu.Unlock()
		})
	}
	return ch, cancel
}

// Notify wakes every subscriber of jobID. It never blocks.
func (n *JobNotifier) Notify(jobID string) {
	n.mu.Lock()
	for ch := range n.subs[jobID] {
		select {
		case ch <- struct{}{}:
		default:
			// A pending wake-up is already buffered; the subscriber will
			// re-check the DB and observe the terminal state anyway.
		}
	}
	n.mu.Unlock()
}
