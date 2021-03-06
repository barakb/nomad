package stream

import (
	"context"
	"errors"
	"sync/atomic"

	"github.com/hashicorp/nomad/nomad/structs"
)

const (
	// subscriptionStateOpen is the default state of a subscription. An open
	// subscription may receive new events.
	subscriptionStateOpen uint32 = 0

	// subscriptionStateClosed indicates that the subscription was closed, possibly
	// as a result of a change to an ACL token, and will not receive new events.
	// The subscriber must issue a new Subscribe request.
	subscriptionStateClosed uint32 = 1
)

// ErrSubscriptionClosed is a error signalling the subscription has been
// closed. The client should Unsubscribe, then re-Subscribe.
var ErrSubscriptionClosed = errors.New("subscription closed by server, client should resubscribe")

type Subscription struct {
	// state must be accessed atomically 0 means open, 1 means closed with reload
	state uint32

	req *SubscribeRequest

	// currentItem stores the current buffer item we are on. It
	// is mutated by calls to Next.
	currentItem *bufferItem

	// forceClosed is closed when forceClose is called. It is used by
	// EventBroker to cancel Next().
	forceClosed chan struct{}

	// unsub is a function set by EventBroker that is called to free resources
	// when the subscription is no longer needed.
	// It must be safe to call the function from multiple goroutines and the function
	// must be idempotent.
	unsub func()
}

type SubscribeRequest struct {
	Token     string
	Index     uint64
	Namespace string

	Topics map[structs.Topic][]string

	// StartExactlyAtIndex specifies if a subscription needs to
	// start exactly at the requested Index. If set to false,
	// the closest index in the buffer will be returned if there is not
	// an exact match
	StartExactlyAtIndex bool
}

func newSubscription(req *SubscribeRequest, item *bufferItem, unsub func()) *Subscription {
	return &Subscription{
		forceClosed: make(chan struct{}),
		req:         req,
		currentItem: item,
		unsub:       unsub,
	}
}

func (s *Subscription) Next(ctx context.Context) (structs.Events, error) {
	if atomic.LoadUint32(&s.state) == subscriptionStateClosed {
		return structs.Events{}, ErrSubscriptionClosed
	}

	for {
		next, err := s.currentItem.Next(ctx, s.forceClosed)
		switch {
		case err != nil && atomic.LoadUint32(&s.state) == subscriptionStateClosed:
			return structs.Events{}, ErrSubscriptionClosed
		case err != nil:
			return structs.Events{}, err
		}
		s.currentItem = next

		events := filter(s.req, next.Events.Events)
		if len(events) == 0 {
			continue
		}
		return structs.Events{Index: next.Events.Index, Events: events}, nil
	}
}

func (s *Subscription) NextNoBlock() ([]structs.Event, error) {
	if atomic.LoadUint32(&s.state) == subscriptionStateClosed {
		return nil, ErrSubscriptionClosed
	}

	for {
		next := s.currentItem.NextNoBlock()
		if next == nil {
			return nil, nil
		}
		s.currentItem = next

		events := filter(s.req, next.Events.Events)
		if len(events) == 0 {
			continue
		}
		return events, nil
	}
}

func (s *Subscription) forceClose() {
	swapped := atomic.CompareAndSwapUint32(&s.state, subscriptionStateOpen, subscriptionStateClosed)
	if swapped {
		close(s.forceClosed)
	}
}

func (s *Subscription) Unsubscribe() {
	s.unsub()
}

// filter events to only those that match a subscriptions topic/keys/namespace
func filter(req *SubscribeRequest, events []structs.Event) []structs.Event {
	if len(events) == 0 {
		return events
	}

	var count int
	for _, e := range events {
		_, allTopics := req.Topics[structs.TopicAll]
		if _, ok := req.Topics[e.Topic]; ok || allTopics {
			var keys []string
			if allTopics {
				keys = req.Topics[structs.TopicAll]
			} else {
				keys = req.Topics[e.Topic]
			}
			if req.Namespace != "" && e.Namespace != "" && e.Namespace != req.Namespace {
				continue
			}
			for _, k := range keys {
				if e.Key == k || k == string(structs.TopicAll) || filterKeyContains(e.FilterKeys, k) {
					count++
				}
			}
		}
	}

	// Only allocate a new slice if some events need to be filtered out
	switch count {
	case 0:
		return nil
	case len(events):
		return events
	}

	// Return filtered events
	result := make([]structs.Event, 0, count)
	for _, e := range events {
		_, allTopics := req.Topics[structs.TopicAll]
		if _, ok := req.Topics[e.Topic]; ok || allTopics {
			var keys []string
			if allTopics {
				keys = req.Topics[structs.TopicAll]
			} else {
				keys = req.Topics[e.Topic]
			}
			// filter out non matching namespaces
			if req.Namespace != "" && e.Namespace != "" && e.Namespace != req.Namespace {
				continue
			}
			for _, k := range keys {
				if e.Key == k || k == string(structs.TopicAll) || filterKeyContains(e.FilterKeys, k) {
					result = append(result, e)
				}
			}
		}
	}
	return result
}

func filterKeyContains(filterKeys []string, key string) bool {
	for _, fk := range filterKeys {
		if fk == key {
			return true
		}
	}
	return false
}
