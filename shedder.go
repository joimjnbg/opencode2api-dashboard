package main

import (
	"fmt"
)

// shedder implements semaphore-based concurrency limiting per tier using
// buffered channels. When MaxConcurrent is 0 the tier is unlimited.
type shedder struct {
	zen chan struct{}
	go_ chan struct{}
}

// newShedder creates a shedder with the given concurrency limits per tier.
// A limit of 0 means unlimited (nil channel, tryAcquire always returns true).
func newShedder(maxZen, maxGo int) *shedder {
	s := &shedder{}
	if maxZen > 0 {
		s.zen = make(chan struct{}, maxZen)
	}
	if maxGo > 0 {
		s.go_ = make(chan struct{}, maxGo)
	}
	return s
}

// tryAcquire attempts to reserve a concurrency slot for the given tier.
// Returns false when the tier is at capacity (request should be shed).
func (s *shedder) tryAcquire(tier Tier) bool {
	ch := s.channel(tier)
	if ch == nil {
		return true // unlimited
	}
	select {
	case ch <- struct{}{}:
		return true
	default:
		return false
	}
}

// release frees a concurrency slot for the given tier.
func (s *shedder) release(tier Tier) {
	ch := s.channel(tier)
	if ch == nil {
		return
	}
	<-ch
}

func (s *shedder) channel(tier Tier) chan struct{} {
	if tier == TierGo {
		return s.go_
	}
	return s.zen
}

// shedError signals that the request was rejected by the concurrency limiter.
type shedError struct {
	tier Tier
}

func (e *shedError) Error() string {
	return fmt.Sprintf("tier %q concurrency limit reached", e.tier)
}
