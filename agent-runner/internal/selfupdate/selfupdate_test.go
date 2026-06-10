package selfupdate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestFetch_NotConfigured_Returns404AsNil(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"not_configured"}`, http.StatusNotFound)
	}))
	defer srv.Close()
	c := &Client{GatewayBase: srv.URL}
	m, err := c.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if m != nil {
		t.Errorf("manifest=%+v, atteso nil su 404", m)
	}
}

func TestFetch_HappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/runner/latest" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(Manifest{
			Version: "0.7.0", AssetURL: "https://x/y", SHA256: "aa",
		})
	}))
	defer srv.Close()
	c := &Client{GatewayBase: srv.URL}
	m, err := c.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if m == nil || m.Version != "0.7.0" {
		t.Errorf("manifest=%+v", m)
	}
}

func TestFetch_RejectsIncompleteManifest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(Manifest{Version: "0.7.0"}) // url + sha256 mancanti
	}))
	defer srv.Close()
	c := &Client{GatewayBase: srv.URL}
	if _, err := c.Fetch(context.Background()); err == nil {
		t.Error("atteso errore su manifest incompleto")
	}
}

func TestIsNewerThan(t *testing.T) {
	m := &Manifest{Version: "0.7.0"}
	if !m.IsNewerThan("0.6.0") {
		t.Error("0.7.0 != 0.6.0 should be newer")
	}
	if m.IsNewerThan("0.7.0") {
		t.Error("stessa versione non e' newer")
	}
	var nilm *Manifest
	if nilm.IsNewerThan("0.6.0") {
		t.Error("nil manifest non puo' essere newer")
	}
}

func TestDownload_VerifiesSHA256_WritesNewAndMarker(t *testing.T) {
	payload := []byte("nuovo binario fittizio")
	sum := sha256.Sum256(payload)
	wantHash := hex.EncodeToString(sum[:])

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(payload)
	}))
	defer srv.Close()

	tmp := t.TempDir()
	exe := filepath.Join(tmp, "agent-runner.exe")
	if err := os.WriteFile(exe, []byte("vecchio binario"), 0o755); err != nil {
		t.Fatalf("write exe: %v", err)
	}
	c := &Client{GatewayBase: "ignored"}
	err := c.Download(context.Background(), exe, Manifest{
		Version: "0.7.0", AssetURL: srv.URL + "/asset", SHA256: wantHash,
	})
	if err != nil {
		t.Fatalf("Download: %v", err)
	}

	newBytes, err := os.ReadFile(exe + ".new")
	if err != nil {
		t.Fatalf("read .new: %v", err)
	}
	if string(newBytes) != string(payload) {
		t.Errorf(".new payload diverso")
	}
	if !HasPending(exe) {
		t.Errorf("marker pending non creato")
	}
	mk, err := ReadPending(exe)
	if err != nil {
		t.Fatalf("ReadPending: %v", err)
	}
	if mk.Version != "0.7.0" || mk.SHA256 != wantHash || mk.TS == "" {
		t.Errorf("marker=%+v", mk)
	}
}

func TestDownload_SHA256Mismatch_LeavesNoFiles(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("payload reale"))
	}))
	defer srv.Close()

	tmp := t.TempDir()
	exe := filepath.Join(tmp, "agent-runner.exe")
	_ = os.WriteFile(exe, []byte("vecchio"), 0o755)
	c := &Client{GatewayBase: "x"}
	err := c.Download(context.Background(), exe, Manifest{
		Version: "0.7.0", AssetURL: srv.URL, SHA256: "deadbeef" + "00000000000000000000000000000000000000000000000000000000",
	})
	if err == nil {
		t.Fatal("atteso errore sha256")
	}
	if _, err := os.Stat(exe + ".new"); !os.IsNotExist(err) {
		t.Errorf(".new non ripulito: %v", err)
	}
	if HasPending(exe) {
		t.Errorf("marker creato nonostante mismatch")
	}
}

func TestDownload_HTTP500_NoArtifacts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()
	tmp := t.TempDir()
	exe := filepath.Join(tmp, "agent-runner.exe")
	_ = os.WriteFile(exe, []byte("x"), 0o755)
	c := &Client{GatewayBase: "x"}
	err := c.Download(context.Background(), exe, Manifest{
		Version: "0.7.0", AssetURL: srv.URL, SHA256: "aa",
	})
	if err == nil {
		t.Fatal("atteso errore HTTP 500")
	}
	if _, err := os.Stat(exe + ".new"); !os.IsNotExist(err) {
		t.Errorf(".new dovrebbe non esistere")
	}
}
