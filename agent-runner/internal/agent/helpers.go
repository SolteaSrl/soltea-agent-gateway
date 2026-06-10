package agent

import (
	"archive/zip"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Struttura .agent-runner/ (radice del repo del progetto, vedi DESIGN):
//
//	<repo>/.agent-runner/
//	├── sessions/<session_id>.jsonl     <- transcript della sessione (JSONL)
//	├── streams/<session_id>.ndjson     <- output stream-json raw di claude (include thinking)
//	├── prompts/<session_id>.md         <- prompt inviato a claude (debug)
//	├── work/<session_id>/              <- scratch del task (rimosso a fine sessione)
//	└── attachments/<ticket_id>/        <- file dall'orchestratrice (--attach), persistenti
//
// Un solo posto per tutto cio' che il runner produce per quel progetto.
// Consiglio: aggiungere `.agent-runner/` al .gitignore del repo cliente.
func agentRunnerDir(projectPath string) string {
	return filepath.Join(projectPath, ".agent-runner")
}

func sessionWorkdir(projectPath, sessionID string) string {
	return filepath.Join(agentRunnerDir(projectPath), "work", sanitizeID(sessionID))
}

func attachmentsDirFor(projectPath string, ticketID int) string {
	return filepath.Join(agentRunnerDir(projectPath), "attachments", strconv.Itoa(ticketID))
}

func sessionTranscriptPath(projectPath, sessionID string) string {
	return filepath.Join(agentRunnerDir(projectPath), "sessions", sanitizeID(sessionID)+".jsonl")
}

func sessionStreamPath(projectPath, sessionID string) string {
	return filepath.Join(agentRunnerDir(projectPath), "streams", sanitizeID(sessionID)+".ndjson")
}

func sessionPromptPath(projectPath, sessionID string) string {
	return filepath.Join(agentRunnerDir(projectPath), "prompts", sanitizeID(sessionID)+".md")
}

// cleanupSessionWorkdir rimuove SOLO la sotto-cartella work/<session_id>/.
// Lascia intatti transcript, stream, prompt e attachments: utili dopo la fine.
func cleanupSessionWorkdir(workdir string) {
	if workdir == "" {
		return
	}
	_ = os.RemoveAll(workdir)
}

// sanitizeID rende un id sicuro come nome file/cartella (no slash, no colon).
func sanitizeID(s string) string {
	if s == "" {
		return "nosession"
	}
	out := make([]rune, 0, len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			out = append(out, r)
		default:
			out = append(out, '_')
		}
	}
	return string(out)
}

// savePromptFile scrive il prompt ricevuto su file (debug / ispezione). Non e'
// un errore se fallisce: il logging persistente in transcript resta autorevole.
func savePromptFile(projectPath, sessionID, prompt string) {
	path := sessionPromptPath(projectPath, sessionID)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	// O_APPEND: turni successivi (chat.send) si accodano allo stesso file con
	// un separatore visibile, cosi' il prompt diventa la storia completa.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	if fi, err := f.Stat(); err == nil && fi.Size() > 0 {
		_, _ = f.WriteString("\n\n---\n\n")
	}
	_, _ = f.WriteString(prompt)
	_, _ = f.WriteString("\n")
}

// openSessionStream apre/crea il file streams/<sid>.ndjson per scrivere il raw
// NDJSON di claude (include eventi assistant text, thinking, tool_use, result).
// Ritorna nil se non si riesce ad aprire: il claude.raw nel transcript resta.
func openSessionStream(projectPath, sessionID string) *os.File {
	path := sessionStreamPath(projectPath, sessionID)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil
	}
	return f
}

// buildFirstPrompt compone il prompt del primo turno: istruzioni + (se presenti)
// dove trovare i file allegati al ticket. La riga sugli allegati viene aggiunta
// solo quando uno zip e' stato effettivamente scaricato e scompattato (hasFiles),
// altrimenti indicheremmo a claude una cartella inesistente.
func buildFirstPrompt(instructions string, ticketID int, hasFiles bool) string {
	var b strings.Builder
	if instructions != "" {
		b.WriteString(instructions)
		if hasFiles {
			b.WriteString("\n\n")
		}
	}
	if hasFiles {
		fmt.Fprintf(&b, "Gli allegati del ticket #%d sono nella cartella "+
			".agent-runner/attachments/%d/ (relativa alla radice del repo). "+
			"Leggili prima di iniziare.", ticketID, ticketID)
	}
	return b.String()
}

// fetchTicketZip scarica lo zip dal gateway e lo scompatta in destDir.
func (a *Agent) fetchTicketZip(blobID, destDir string) error {
	url := a.cfg.HTTPBase() + "/blobs/" + blobID
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+a.cfg.Token)

	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("download blob: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download blob: HTTP %d", resp.StatusCode)
	}

	tmp, err := os.CreateTemp("", "ticket-*.zip")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := io.Copy(tmp, resp.Body); err != nil {
		tmp.Close()
		return err
	}
	tmp.Close()

	return unzip(tmpName, destDir)
}

// unzip estrae un archivio zip in destDir, prevenendo path traversal (Zip Slip).
func unzip(zipPath, destDir string) error {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return err
	}
	defer zr.Close()

	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return err
	}
	destAbs, err := filepath.Abs(destDir)
	if err != nil {
		return err
	}

	for _, f := range zr.File {
		target := filepath.Join(destDir, f.Name)
		targetAbs, err := filepath.Abs(target)
		if err != nil {
			return err
		}
		// Difesa Zip Slip: il target deve restare dentro destDir.
		if targetAbs != destAbs && !strings.HasPrefix(targetAbs, destAbs+string(os.PathSeparator)) {
			return fmt.Errorf("voce zip fuori dalla cartella: %q", f.Name)
		}
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		if err := writeZipEntry(f, target); err != nil {
			return err
		}
	}
	return nil
}

func writeZipEntry(f *zip.File, target string) error {
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer rc.Close()
	out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, rc)
	return err
}
