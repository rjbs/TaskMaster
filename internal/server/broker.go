package server

import "sync"

// Broker fans out state changes to event source subscribers.  Each
// subscriber keeps a pending map of type to newest state, so a slow reader
// coalesces changes instead of blocking the writer or missing one.
type Broker struct {
	mu   sync.Mutex
	subs map[*subscriber]struct{}
}

type subscriber struct {
	mu      sync.Mutex
	pending map[string]string
	notify  chan struct{}
}

func NewBroker() *Broker {
	return &Broker{subs: map[*subscriber]struct{}{}}
}

// Publish records new states for the given types with every subscriber.
func (b *Broker) Publish(changed map[string]string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for s := range b.subs {
		s.mu.Lock()
		for typ, state := range changed {
			s.pending[typ] = state
		}
		s.mu.Unlock()
		select {
		case s.notify <- struct{}{}:
		default:
		}
	}
}

func (b *Broker) subscribe() *subscriber {
	s := &subscriber{pending: map[string]string{}, notify: make(chan struct{}, 1)}
	b.mu.Lock()
	b.subs[s] = struct{}{}
	b.mu.Unlock()
	return s
}

func (b *Broker) unsubscribe(s *subscriber) {
	b.mu.Lock()
	delete(b.subs, s)
	b.mu.Unlock()
}

// take returns and clears the pending changes.
func (s *subscriber) take() map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.pending
	s.pending = map[string]string{}
	return out
}
