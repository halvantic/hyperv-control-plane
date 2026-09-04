package store

import "testing"

func TestObservedGenerationsSurviveReopen(t *testing.T) {
	// The whole point: a restart must not look like a host that has never
	// enforced anything. Reopening the file is the closest a test gets to the
	// service being restarted under it.
	path := t.TempDir() + "/agent.db"

	s, err := openAt(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, ok, err := s.LoadObserved(); err != nil || ok {
		t.Fatalf("a fresh store has honoured nothing; got ok=%v err=%v", ok, err)
	}
	want := Observed{Host: 7, VMs: map[string]int64{"HyperV_DC": 3, "BallastNewCentre": 1}}
	if err := s.SaveObserved(want); err != nil {
		t.Fatalf("save: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	s2, err := openAt(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()
	got, ok, err := s2.LoadObserved()
	if err != nil || !ok {
		t.Fatalf("after reopen: ok=%v err=%v", ok, err)
	}
	if got.Host != want.Host {
		t.Errorf("host generation: got %d, want %d", got.Host, want.Host)
	}
	for name, gen := range want.VMs {
		if got.VMs[name] != gen {
			t.Errorf("vm %s: got %d, want %d", name, got.VMs[name], gen)
		}
	}
}

// A VM held on an operator action never reaches "fully honoured", so its
// generation is written once and must then survive every restart that follows.
// This is the case an in-memory map got permanently wrong.
func TestObservedGenerationOfAHeldVMIsNotLost(t *testing.T) {
	path := t.TempDir() + "/agent.db"
	s, err := openAt(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := s.SaveObserved(Observed{VMs: map[string]int64{"HyperV_DC": 2}}); err != nil {
		t.Fatalf("save: %v", err)
	}
	s.Close()

	for i := 0; i < 3; i++ {
		s, err := openAt(path)
		if err != nil {
			t.Fatalf("reopen %d: %v", i, err)
		}
		got, ok, err := s.LoadObserved()
		s.Close()
		if err != nil || !ok || got.VMs["HyperV_DC"] != 2 {
			t.Fatalf("restart %d lost it: ok=%v gen=%d err=%v", i, ok, got.VMs["HyperV_DC"], err)
		}
	}
}

func TestSaveObservedReplacesRatherThanMerges(t *testing.T) {
	// A pruned VM must actually leave the file. Merging would let a deleted VM
	// linger for the life of the host.
	s, err := openAt(t.TempDir() + "/agent.db")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()
	if err := s.SaveObserved(Observed{VMs: map[string]int64{"gone": 1, "kept": 2}}); err != nil {
		t.Fatalf("save: %v", err)
	}
	if err := s.SaveObserved(Observed{VMs: map[string]int64{"kept": 2}}); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, _, err := s.LoadObserved()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if _, still := got.VMs["gone"]; still {
		t.Error("a pruned VM is still in the store")
	}
	if got.VMs["kept"] != 2 {
		t.Errorf("kept: got %d, want 2", got.VMs["kept"])
	}
}
