package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAgentRunnerDirLayout(t *testing.T) {
	proj := "/repo/myproj"
	root := agentRunnerDir(proj)
	if root != filepath.Join("/repo/myproj", ".agent-runner") {
		t.Errorf("agentRunnerDir=%q", root)
	}
	wd := sessionWorkdir(proj, "sess-abc")
	wantWD := filepath.Join(root, "work", "sess-abc")
	if wd != wantWD {
		t.Errorf("sessionWorkdir=%q want=%q", wd, wantWD)
	}
	ad := attachmentsDirFor(proj, 42)
	if ad != filepath.Join(root, "attachments", "42") {
		t.Errorf("attachmentsDirFor=%q", ad)
	}
	tp := sessionTranscriptPath(proj, "sess-abc")
	if tp != filepath.Join(root, "sessions", "sess-abc.jsonl") {
		t.Errorf("sessionTranscriptPath=%q", tp)
	}
	sp := sessionStreamPath(proj, "sess-abc")
	if sp != filepath.Join(root, "streams", "sess-abc.ndjson") {
		t.Errorf("sessionStreamPath=%q", sp)
	}
	pp := sessionPromptPath(proj, "sess-abc")
	if pp != filepath.Join(root, "prompts", "sess-abc.md") {
		t.Errorf("sessionPromptPath=%q", pp)
	}
}

func TestSanitizeIDStripsPathSeparators(t *testing.T) {
	cases := map[string]string{
		"sess-abc":          "sess-abc",
		"":                  "nosession",
		"sess/with/slashes": "sess_with_slashes",
		`sess\with\back`:    "sess_with_back",
		"sess:with:colons":  "sess_with_colons",
		"sess.with.dots":    "sess_with_dots",
		"sess-12345_abcDEF": "sess-12345_abcDEF",
	}
	for in, want := range cases {
		if got := sanitizeID(in); got != want {
			t.Errorf("sanitizeID(%q)=%q want=%q", in, got, want)
		}
	}
}

func TestBuildFirstPrompt_NoFiles(t *testing.T) {
	p := buildFirstPrompt("Risolvi il bug X.", 100, false)
	if p != "Risolvi il bug X." {
		t.Errorf("prompt no-files=%q", p)
	}
}

func TestBuildFirstPrompt_WithFiles_PointsToAgentRunnerAttachments(t *testing.T) {
	p := buildFirstPrompt("Risolvi.", 99, true)
	if !strings.Contains(p, "Risolvi.") {
		t.Error("prompt ha perso le istruzioni")
	}
	if !strings.Contains(p, ".agent-runner/attachments/99/") {
		t.Errorf("prompt non punta alla nuova convenzione: %q", p)
	}
	// La vecchia formulazione `.tickets/99/attachments/` NON deve apparire.
	if strings.Contains(p, ".tickets/") {
		t.Errorf("prompt fa ancora riferimento a .tickets/: %q", p)
	}
}

func TestSavePromptFile_AppendsTurnsWithSeparator(t *testing.T) {
	tmp := t.TempDir()
	sid := "sess-x"
	savePromptFile(tmp, sid, "primo prompt")
	savePromptFile(tmp, sid, "secondo prompt")
	path := sessionPromptPath(tmp, sid)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	body := string(data)
	if !strings.Contains(body, "primo prompt") || !strings.Contains(body, "secondo prompt") {
		t.Errorf("prompt mancante: %q", body)
	}
	// Separatore Markdown tra i due turni.
	if !strings.Contains(body, "\n---\n") {
		t.Errorf("separatore turni mancante: %q", body)
	}
}

func TestOpenSessionStream_CreatesDirAndAppends(t *testing.T) {
	tmp := t.TempDir()
	sid := "sess-stream"
	f := openSessionStream(tmp, sid)
	if f == nil {
		t.Fatal("openSessionStream ritorna nil")
	}
	if _, err := f.WriteString(`{"a":1}` + "\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	f.Close()
	// Riapertura: O_APPEND deve aggiungere senza troncare.
	f2 := openSessionStream(tmp, sid)
	if f2 == nil {
		t.Fatal("riapertura stream fallita")
	}
	if _, err := f2.WriteString(`{"b":2}` + "\n"); err != nil {
		t.Fatalf("write2: %v", err)
	}
	f2.Close()
	data, _ := os.ReadFile(sessionStreamPath(tmp, sid))
	if want := `{"a":1}` + "\n" + `{"b":2}` + "\n"; string(data) != want {
		t.Errorf("stream=%q want=%q", data, want)
	}
}

func TestCleanupSessionWorkdir_RemovesOnlyTheWorkdir(t *testing.T) {
	tmp := t.TempDir()
	sid := "sess-cleanup"
	wd := sessionWorkdir(tmp, sid)
	if err := os.MkdirAll(wd, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// File "vicini" che NON devono essere toccati: transcript, attachments, prompts.
	transcript := sessionTranscriptPath(tmp, sid)
	_ = os.MkdirAll(filepath.Dir(transcript), 0o755)
	if err := os.WriteFile(transcript, []byte("xxx"), 0o644); err != nil {
		t.Fatalf("write transcript: %v", err)
	}
	attachments := attachmentsDirFor(tmp, 7)
	_ = os.MkdirAll(attachments, 0o755)
	if err := os.WriteFile(filepath.Join(attachments, "a.txt"), []byte("payload"), 0o644); err != nil {
		t.Fatalf("write attachment: %v", err)
	}

	cleanupSessionWorkdir(wd)

	if _, err := os.Stat(wd); !os.IsNotExist(err) {
		t.Errorf("workdir non rimosso: err=%v", err)
	}
	if _, err := os.Stat(transcript); err != nil {
		t.Errorf("transcript rimosso per sbaglio: err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(attachments, "a.txt")); err != nil {
		t.Errorf("attachments rimossi per sbaglio: err=%v", err)
	}
}
