package main

import (
	"testing"
)

func TestCatalogReplaceNilPreservesExisting(t *testing.T) {
	c := newModelCatalog(TierZen, nil, ModeOpenAI)
	c.Replace([]string{"gemini-3.7-flash", "gemini-2.5-pro"}, []string{"big-pickle", "hy3"})

	// Nil should preserve both tiers.
	c.Replace(nil, nil)
	snap := c.Snapshot()
	if snap.Zen != 2 {
		t.Errorf("zen preserved: got %d, want 2", snap.Zen)
	}
	if snap.Go != 2 {
		t.Errorf("go preserved: got %d, want 2", snap.Go)
	}
	if _, err := c.Route("gemini-3.7-flash", true, true); err != nil {
		t.Errorf("zen model still routable after nil replace: %v", err)
	}
	if _, err := c.Route("big-pickle", true, true); err != nil {
		t.Errorf("go model still routable after nil replace: %v", err)
	}
}

func TestCatalogReplaceNilZenUpdatesGo(t *testing.T) {
	c := newModelCatalog(TierZen, nil, ModeOpenAI)
	c.Replace([]string{"gemini-3.7-flash"}, []string{"big-pickle"})

	// Only zen updates; go preserved.
	c.Replace([]string{"gemini-3.7-flash", "gemini-2.5-flash"}, nil)
	snap := c.Snapshot()
	if snap.Zen != 2 {
		t.Errorf("zen updated: got %d, want 2", snap.Zen)
	}
	if snap.Go != 1 {
		t.Errorf("go preserved: got %d, want 1", snap.Go)
	}
}

func TestCatalogReplaceNilGoUpdatesZen(t *testing.T) {
	c := newModelCatalog(TierZen, nil, ModeOpenAI)
	c.Replace([]string{"gemini-3.7-flash"}, []string{"big-pickle"})

	// Only go updates; zen preserved.
	c.Replace(nil, []string{"big-pickle", "hy3"})
	snap := c.Snapshot()
	if snap.Zen != 1 {
		t.Errorf("zen preserved: got %d, want 1", snap.Zen)
	}
	if snap.Go != 2 {
		t.Errorf("go updated: got %d, want 2", snap.Go)
	}
}

func TestCatalogReplaceFullUpdate(t *testing.T) {
	c := newModelCatalog(TierZen, nil, ModeOpenAI)
	c.Replace([]string{"gemini-3.7-flash"}, []string{"big-pickle"})

	// Both tiers update.
	c.Replace([]string{"gemini-3.7-flash", "gemini-2.5-flash"}, []string{"big-pickle", "hy3"})
	snap := c.Snapshot()
	if snap.Zen != 2 || snap.Go != 2 {
		t.Errorf("both updated: zen=%d go=%d, want 2,2", snap.Zen, snap.Go)
	}
}
