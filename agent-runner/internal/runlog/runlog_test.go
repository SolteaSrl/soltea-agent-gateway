package runlog

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSessionTranscript(t *testing.T) {
	dir := t.TempDir()
	lg, err := New(dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer lg.Close()

	lg.Info("avvio test")
	s := lg.Session("sess-abc123", 69010)
	s.Log(Event{Kind: "task.start", Direction: "in", Text: "istruzioni"})
	s.Log(Event{Kind: "prompt", Direction: "out", Text: "prompt a claude"})
	s.Log(Event{Kind: "claude.raw", Stdout: `{"result":"ok"}`, Stderr: "warning x"})
	s.Log(Event{Kind: "result", Direction: "out", Text: "fatto", CostUSD: 0.5, DurationMS: 1234})

	// Il transcript esiste e ha 4 righe JSON valide, con session/ticket propagati.
	f := s.File()
	if !strings.Contains(filepath.Base(f), "ticket-69010-sess-abc123") {
		t.Fatalf("nome file inatteso: %s", f)
	}
	fh, err := os.Open(f)
	if err != nil {
		t.Fatalf("open transcript: %v", err)
	}
	defer fh.Close()

	var kinds []string
	sc := bufio.NewScanner(fh)
	for sc.Scan() {
		var ev Event
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			t.Fatalf("riga non JSON: %v (%q)", err, sc.Text())
		}
		if ev.Session != "sess-abc123" || ev.Ticket != 69010 {
			t.Errorf("session/ticket non propagati: %+v", ev)
		}
		if ev.Time == "" {
			t.Errorf("timestamp mancante: %+v", ev)
		}
		kinds = append(kinds, ev.Kind)
	}
	want := []string{"task.start", "prompt", "claude.raw", "result"}
	if strings.Join(kinds, ",") != strings.Join(want, ",") {
		t.Fatalf("kinds = %v, voglio %v", kinds, want)
	}

	// Il log generale è stato scritto.
	gen, err := os.ReadFile(filepath.Join(dir, "runner.log"))
	if err != nil {
		t.Fatalf("read runner.log: %v", err)
	}
	if !strings.Contains(string(gen), "avvio test") {
		t.Fatalf("runner.log non contiene la riga attesa: %q", gen)
	}
}

// I metodi devono essere sicuri anche su Logger/Session nil (logging degradato).
func TestNilSafe(t *testing.T) {
	var lg *Logger
	lg.Info("niente")
	lg.Close()
	s := lg.Session("x", 1)
	s.Log(Event{Kind: "result"})
	if s.File() != "" {
		t.Fatalf("File() su sessione senza logger dovrebbe essere vuoto")
	}
	// Writer() su nil-logger NON deve panicare e deve essere nil.
	if w := lg.Writer(); w != nil {
		t.Errorf("Writer() su nil logger=%v want=nil", w)
	}
}

func TestWriter_MirrorsToGeneralFile(t *testing.T) {
	dir := t.TempDir()
	lg, err := New(dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer lg.Close()

	w := lg.Writer()
	if w == nil {
		t.Fatal("Writer() ritorna nil su logger valido")
	}
	for _, line := range []string{"riga 1\n", "riga 2\n", "riga 3 con dati\n"} {
		n, err := w.Write([]byte(line))
		if err != nil {
			t.Fatalf("Write: %v", err)
		}
		if n != len(line) {
			t.Errorf("Write n=%d want=%d", n, len(line))
		}
	}
	data, err := os.ReadFile(filepath.Join(dir, "runner.log"))
	if err != nil {
		t.Fatalf("read runner.log: %v", err)
	}
	body := string(data)
	for _, want := range []string{"riga 1", "riga 2", "riga 3 con dati"} {
		if !strings.Contains(body, want) {
			t.Errorf("runner.log non contiene %q: %q", want, body)
		}
	}
}

func TestWriter_IsConcurrencySafe(t *testing.T) {
	dir := t.TempDir()
	lg, err := New(dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer lg.Close()

	w := lg.Writer()
	if w == nil {
		t.Fatal("Writer() nil")
	}
	// 50 goroutine scrivono insieme: niente race, niente panic. Non controlliamo
	// l'ordine (e' best-effort) ma il file deve contenere tutte le firme.
	done := make(chan struct{})
	for i := 0; i < 50; i++ {
		i := i
		go func() {
			defer func() { done <- struct{}{} }()
			_, _ = w.Write([]byte(fmt.Sprintf("worker-%02d\n", i)))
		}()
	}
	for i := 0; i < 50; i++ {
		<-done
	}
	data, _ := os.ReadFile(filepath.Join(dir, "runner.log"))
	body := string(data)
	for i := 0; i < 50; i++ {
		want := fmt.Sprintf("worker-%02d", i)
		if !strings.Contains(body, want) {
			t.Errorf("manca %q nel file", want)
		}
	}
}
