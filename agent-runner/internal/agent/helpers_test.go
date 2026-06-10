package agent

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestTicketDirAndAttachmentsDir(t *testing.T) {
	td := ticketDir("/repo", 42)
	want := filepath.Join("/repo", ".tickets", "42")
	if td != want {
		t.Errorf("ticketDir=%q want=%q", td, want)
	}
	ad := attachmentsDir(td)
	wantA := filepath.Join(want, "attachments")
	if ad != wantA {
		t.Errorf("attachmentsDir=%q want=%q", ad, wantA)
	}
}

func TestBuildFirstPrompt_NoFiles(t *testing.T) {
	p := buildFirstPrompt("Risolvi il bug X.", 100, false)
	if p != "Risolvi il bug X." {
		t.Errorf("prompt no-files=%q", p)
	}
}

func TestBuildFirstPrompt_WithFiles_PointsToAttachments(t *testing.T) {
	p := buildFirstPrompt("Risolvi.", 99, true)
	if !strings.Contains(p, "Risolvi.") {
		t.Error("prompt ha perso le istruzioni")
	}
	if !strings.Contains(p, ".tickets/99/attachments/") {
		t.Errorf("prompt non punta a attachments/: %q", p)
	}
	// La vecchia formulazione (".tickets/99/") senza attachments NON deve apparire
	// perche' altrimenti claude andrebbe a leggere nella radice della sessione.
	if strings.Contains(p, ".tickets/99/ ") {
		t.Errorf("prompt punta ancora alla radice sessione: %q", p)
	}
}

func TestBuildFirstPrompt_EmptyInstructionsWithFiles(t *testing.T) {
	p := buildFirstPrompt("", 7, true)
	if !strings.HasPrefix(p, "Gli allegati") {
		t.Errorf("prompt con istruzioni vuote=%q", p)
	}
	if !strings.Contains(p, ".tickets/7/attachments/") {
		t.Errorf("path mancante: %q", p)
	}
}
