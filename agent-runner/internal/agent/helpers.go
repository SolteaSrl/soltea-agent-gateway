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

// ticketDir e' la radice della cartella di sessione del ticket, dentro il repo.
// La struttura ora e':
//
//	<repo>/.tickets/<id>/                  <- radice sessione (workdir scratch del task)
//	<repo>/.tickets/<id>/attachments/      <- file allegati dall'orchestratrice
//
// La sotto-cartella `attachments/` separa cio' che il task scarica (dato di input)
// da eventuali artefatti che claude scrive durante il lavoro, evitando conflitti
// di path e rendendo chiaro nel prompt "leggi qui i file allegati".
func ticketDir(projectPath string, ticketID int) string {
	return filepath.Join(projectPath, ".tickets", strconv.Itoa(ticketID))
}

func attachmentsDir(ticketDir string) string {
	return filepath.Join(ticketDir, "attachments")
}

func cleanupTicketDir(dir string) {
	if dir == "" {
		return
	}
	_ = os.RemoveAll(dir)
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
			".tickets/%d/attachments/ (relativa alla radice del repo). "+
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
