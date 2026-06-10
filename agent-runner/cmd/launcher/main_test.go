package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestApplyPendingIfAny_RotatesWorkerAndConsumesMarker(t *testing.T) {
	tmp := t.TempDir()
	worker := filepath.Join(tmp, "agent-runner")
	if err := os.WriteFile(worker, []byte("OLD"), 0o755); err != nil {
		t.Fatalf("write worker: %v", err)
	}
	if err := os.WriteFile(worker+".new", []byte("NEW"), 0o755); err != nil {
		t.Fatalf("write .new: %v", err)
	}
	mk, _ := json.Marshal(map[string]string{"version": "0.7.0", "sha256": "abc", "ts": "2026-06-10T00:00:00Z"})
	if err := os.WriteFile(worker+".update-pending", mk, 0o644); err != nil {
		t.Fatalf("write marker: %v", err)
	}

	applyPendingIfAny(worker)

	if b, _ := os.ReadFile(worker); string(b) != "NEW" {
		t.Errorf("worker non sostituito: %q", b)
	}
	if b, _ := os.ReadFile(worker + ".old"); string(b) != "OLD" {
		t.Errorf(".old non e' la versione vecchia: %q", b)
	}
	if _, err := os.Stat(worker + ".update-pending"); !os.IsNotExist(err) {
		t.Errorf("marker non consumato: %v", err)
	}
	if _, err := os.Stat(worker + ".new"); !os.IsNotExist(err) {
		t.Errorf(".new non rimosso (e' il rename del rotate)")
	}
}

func TestApplyPendingIfAny_NoOpWithoutMarker(t *testing.T) {
	tmp := t.TempDir()
	worker := filepath.Join(tmp, "agent-runner")
	_ = os.WriteFile(worker, []byte("OLD"), 0o755)
	applyPendingIfAny(worker) // niente marker -> no-op silenzioso
	if b, _ := os.ReadFile(worker); string(b) != "OLD" {
		t.Errorf("worker modificato senza marker: %q", b)
	}
}

func TestApplyPendingIfAny_RemovesStaleMarkerIfNewMissing(t *testing.T) {
	tmp := t.TempDir()
	worker := filepath.Join(tmp, "agent-runner")
	_ = os.WriteFile(worker, []byte("OLD"), 0o755)
	mk, _ := json.Marshal(map[string]string{"version": "0.7.0", "sha256": "abc", "ts": "x"})
	_ = os.WriteFile(worker+".update-pending", mk, 0o644)
	// .new mancante
	applyPendingIfAny(worker)
	if _, err := os.Stat(worker + ".update-pending"); !os.IsNotExist(err) {
		t.Errorf("marker stale non rimosso")
	}
	if b, _ := os.ReadFile(worker); string(b) != "OLD" {
		t.Errorf("worker toccato: %q", b)
	}
}

func TestRollback_RestoresOldIfPresent(t *testing.T) {
	tmp := t.TempDir()
	worker := filepath.Join(tmp, "agent-runner")
	_ = os.WriteFile(worker, []byte("BROKEN"), 0o755)
	_ = os.WriteFile(worker+".old", []byte("OLD-WORKING"), 0o755)
	if !rollback(worker) {
		t.Fatal("rollback ha restituito false")
	}
	if b, _ := os.ReadFile(worker); string(b) != "OLD-WORKING" {
		t.Errorf("worker dopo rollback=%q", b)
	}
	if b, _ := os.ReadFile(worker + ".broken"); string(b) != "BROKEN" {
		t.Errorf(".broken=%q", b)
	}
	if _, err := os.Stat(worker + ".old"); !os.IsNotExist(err) {
		t.Errorf(".old non consumato")
	}
}

func TestRollback_NoOpIfNoOld(t *testing.T) {
	tmp := t.TempDir()
	worker := filepath.Join(tmp, "agent-runner")
	_ = os.WriteFile(worker, []byte("X"), 0o755)
	if rollback(worker) {
		t.Error("rollback ha restituito true senza .old")
	}
	if b, _ := os.ReadFile(worker); string(b) != "X" {
		t.Errorf("worker toccato: %q", b)
	}
}

func TestSplitArgs_BasicSpaceSeparated(t *testing.T) {
	got := splitArgs("a b   c")
	want := []string{"a", "b", "c"}
	if len(got) != len(want) {
		t.Fatalf("got=%v want=%v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("got[%d]=%q want=%q", i, got[i], want[i])
		}
	}
	if len(splitArgs("")) != 0 {
		t.Error("vuoto -> array vuoto")
	}
}

func TestBytesTrim_StripsWhitespaceBothEnds(t *testing.T) {
	cases := map[string]string{
		"  abc  ":     "abc",
		"\n0.6.0\r\n": "0.6.0",
		"x":           "x",
		"":            "",
	}
	for in, want := range cases {
		got := string(bytesTrim([]byte(in)))
		if got != want {
			t.Errorf("bytesTrim(%q)=%q want=%q", in, got, want)
		}
	}
}
