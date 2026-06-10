// Package selfupdate gestisce il check periodico verso il gateway per la
// disponibilita' di una nuova versione del runner e l'apply ad alto livello.
//
// L'apply NON sostituisce il binario in esecuzione (sarebbe rischioso sotto
// Windows e dipende dal Service Control Manager): scarica `agent-runner.exe.new`
// accanto al binario corrente, ne verifica lo SHA256, e scrive un marker
// `agent-runner.exe.update-pending` con {version, sha256, ts}.
//
// Tocca al LAUNCHER (`agent-launcher.exe`) consumare quel marker al prossimo
// ciclo: rinomina worker -> .old, .new -> worker, rimuove il marker e
// rilancia. Vedi DESIGN.md sezione "Auto-update Piano B".
package selfupdate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// Manifest e' la risposta del gateway su /runner/latest.
type Manifest struct {
	Version  string `json:"version"`
	AssetURL string `json:"asset_url"`
	SHA256   string `json:"sha256"`
}

// PendingMarker e' il contenuto del file <exe>.update-pending che il launcher
// trova al boot per sapere che il worker ha gia' scaricato un .new.
type PendingMarker struct {
	Version string `json:"version"`
	SHA256  string `json:"sha256"`
	TS      string `json:"ts"`
}

// Client interroga il gateway. timeout su tutto il giro Fetch+Download.
type Client struct {
	GatewayBase string        // es. "https://projectopen.soltea.it/agents"
	HTTP        *http.Client  // se nil, usa default con timeout 60s
	Timeout     time.Duration // timeout totale per ogni operazione, default 5m
}

func (c *Client) http() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 60 * time.Second}
}

func (c *Client) timeout() time.Duration {
	if c.Timeout > 0 {
		return c.Timeout
	}
	return 5 * time.Minute
}

// Fetch interroga GET /runner/latest. Ritorna (nil, nil) se il gateway risponde
// 404 (auto-update disabilitato dall'admin), (manifest, nil) se ok, error in
// tutti gli altri casi.
func (c *Client) Fetch(ctx context.Context) (*Manifest, error) {
	url := c.GatewayBase + "/runner/latest"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http().Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch /runner/latest: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, nil // auto-update disabilitato lato gateway
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch /runner/latest: HTTP %d", resp.StatusCode)
	}
	var m Manifest
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		return nil, fmt.Errorf("decode manifest: %w", err)
	}
	if m.Version == "" || m.AssetURL == "" || m.SHA256 == "" {
		return nil, fmt.Errorf("manifest incompleto: %+v", m)
	}
	return &m, nil
}

// IsNewerThan e' true se m.Version != current. Niente semver: il gateway
// dichiara la versione "consigliata" e basta — se differisce, aggiorniamo.
// Cosi' supportiamo anche downgrade voluti (rollback admin).
func (m *Manifest) IsNewerThan(current string) bool {
	return m != nil && m.Version != "" && m.Version != current
}

// Download scarica AssetURL in <exePath>.new, verifica lo SHA256, e scrive il
// marker pending. Ritorna l'errore appena qualcosa va male — i file parziali
// vengono ripuliti (no <exe>.new sporco al prossimo giro).
func (c *Client) Download(ctx context.Context, exePath string, m Manifest) error {
	newPath := exePath + ".new"
	marker := exePath + ".update-pending"

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.AssetURL, nil)
	if err != nil {
		return err
	}
	cli := *c.http()
	cli.Timeout = c.timeout()
	resp, err := cli.Do(req)
	if err != nil {
		return fmt.Errorf("download asset: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download asset: HTTP %d", resp.StatusCode)
	}

	tmp, err := os.CreateTemp(filepath.Dir(exePath), "agent-runner-*.new.tmp")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	tmpName := tmp.Name()
	hash := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, hash), resp.Body); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("write asset: %w", err)
	}
	tmp.Close()
	got := hex.EncodeToString(hash.Sum(nil))
	if got != m.SHA256 {
		os.Remove(tmpName)
		return fmt.Errorf("sha256 mismatch: got %s, want %s", got, m.SHA256)
	}

	if err := os.Chmod(tmpName, 0o755); err != nil {
		// Su Windows il chmod e' nop, ignoriamo.
	}
	if err := os.Rename(tmpName, newPath); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("rename .new: %w", err)
	}

	mk := PendingMarker{Version: m.Version, SHA256: m.SHA256, TS: time.Now().UTC().Format(time.RFC3339)}
	mkData, _ := json.Marshal(mk)
	// scrittura atomica del marker: tmp + rename
	tmpMarker := marker + ".tmp"
	if err := os.WriteFile(tmpMarker, mkData, 0o644); err != nil {
		os.Remove(newPath)
		return fmt.Errorf("write marker: %w", err)
	}
	if err := os.Rename(tmpMarker, marker); err != nil {
		os.Remove(tmpMarker)
		os.Remove(newPath)
		return fmt.Errorf("rename marker: %w", err)
	}
	return nil
}

// HasPending true se esiste il marker per exePath.
func HasPending(exePath string) bool {
	_, err := os.Stat(exePath + ".update-pending")
	return err == nil
}

// ReadPending legge il marker (usato dal launcher per logging).
func ReadPending(exePath string) (*PendingMarker, error) {
	data, err := os.ReadFile(exePath + ".update-pending")
	if err != nil {
		return nil, err
	}
	var m PendingMarker
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	return &m, nil
}
