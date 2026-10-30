package server

import "sync"

// Broker fans out state changes to event source subscribers.  Changes are
// keyed by account and then by type; each subscriber keeps a pending map
// of the newest state per type, so a slow reader coalesces changes instead
// of blocking the writer or missing one.  A subscriber only hears about
// the accounts it asked for.
type Broker struct {
	mu   sync.Mutex
	subs map[*subscriber]struct{}
}

// Changes maps account id to type name to state string.
type Changes map[string]map[string]string

type subscriber struct {
	mu       sync.Mutex
	accounts map[string]bool
	pending  Changes
	notify   chan struct{}
}

func NewBroker() *Broker {
	return &Broker{subs: map[*subscriber]struct{}{}}
}

// Publish records new states for the given types of one account with every
// subscriber watching that account.
func (b *Broker) Publish(accountID string, changed map[string]string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for s := range b.subs {
		if !s.accounts[accountID] {
			continue
		}
		s.mu.Lock()
		if s.pending[accountID] == nil {
			s.pending[accountID] = map[string]string{}
		}
		for typ, state := range changed {
			s.pending[accountID][typ] = state
		}
		s.mu.Unlock()
		select {
		case s.notify <- struct{}{}:
		default:
		}
	}
}

// Publisher returns a function suitable for store.DB.OnCommit that
// publishes an account's changes.
func (b *Broker) Publisher(accountID string) func(changed map[string]string) {
	return func(changed map[string]string) { b.Publish(accountID, changed) }
}

func (b *Broker) subscribe(accounts []string) *subscriber {
	s := &subscriber{accounts: map[string]bool{}, pending: Changes{}, notify: make(chan struct{}, 1)}
	for _, a := range accounts {
		s.accounts[a] = true
	}
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
func (s *subscriber) take() Changes {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.pending
	s.pending = Changes{}
	return out
}
