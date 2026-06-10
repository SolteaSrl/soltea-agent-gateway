package claude

import (
	"strings"
	"testing"
)

func TestParseStream_HappyPath(t *testing.T) {
	// Sequenza tipica: system init, due assistant chunk di testo, un tool_use
	// (che NON deve diventare delta), un altro assistant text, e il result finale.
	input := strings.Join([]string{
		`{"type":"system","subtype":"init","session_id":"sess-1"}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"Ciao "}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"Marcello,"}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Read"}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":" il fix e' pronto."}]}}`,
		`{"type":"result","subtype":"success","is_error":false,"result":"Ciao Marcello, il fix e' pronto.","session_id":"sess-1","total_cost_usd":0.42,"duration_ms":1234}`,
	}, "\n") + "\n"

	var deltas []string
	res, raw, err := parseStream(strings.NewReader(input), func(t string) { deltas = append(deltas, t) }, nil)
	if err != nil {
		t.Fatalf("parseStream: %v", err)
	}
	if res == nil {
		t.Fatal("result nil")
	}
	if res.Text != "Ciao Marcello, il fix e' pronto." {
		t.Errorf("Text=%q", res.Text)
	}
	if res.SessionID != "sess-1" {
		t.Errorf("SessionID=%q", res.SessionID)
	}
	if res.IsError {
		t.Error("IsError true, atteso false")
	}
	if res.CostUSD != 0.42 {
		t.Errorf("CostUSD=%v", res.CostUSD)
	}
	if res.DurationMS != 1234 {
		t.Errorf("DurationMS=%v", res.DurationMS)
	}
	// 3 blocchi text (NON quello tool_use).
	wantDeltas := []string{"Ciao ", "Marcello,", " il fix e' pronto."}
	if len(deltas) != len(wantDeltas) {
		t.Fatalf("delta count=%d want=%d (%v)", len(deltas), len(wantDeltas), deltas)
	}
	for i, d := range deltas {
		if d != wantDeltas[i] {
			t.Errorf("delta[%d]=%q want=%q", i, d, wantDeltas[i])
		}
	}
	// Raw output preservato per intero (per i log).
	if !strings.Contains(raw, "Ciao Marcello") {
		t.Errorf("raw stdout incompleto: %q", raw)
	}
}

func TestParseStream_NoDeltaCallback(t *testing.T) {
	// Senza callback non si rompe nulla: il result viene comunque parsato.
	input := `{"type":"assistant","message":{"content":[{"type":"text","text":"x"}]}}` + "\n" +
		`{"type":"result","is_error":false,"result":"x","session_id":"s"}` + "\n"
	res, _, err := parseStream(strings.NewReader(input), nil, nil)
	if err != nil {
		t.Fatalf("parseStream: %v", err)
	}
	if res == nil || res.Text != "x" {
		t.Fatalf("res=%+v", res)
	}
}

func TestParseStream_IsErrorTrue(t *testing.T) {
	input := `{"type":"result","is_error":true,"result":"fallito","session_id":"s2"}` + "\n"
	res, _, err := parseStream(strings.NewReader(input), nil, nil)
	if err != nil {
		t.Fatalf("parseStream: %v", err)
	}
	if !res.IsError {
		t.Error("atteso IsError true")
	}
	if res.SessionID != "s2" {
		t.Errorf("SessionID=%q", res.SessionID)
	}
}

func TestParseStream_IgnoresNonJSONLines(t *testing.T) {
	// claude potrebbe emettere log su stdout o righe vuote: non devono rompere.
	input := strings.Join([]string{
		"",               // riga vuota
		"WARN: qualcosa", // riga non-JSON
		`{"type":"assistant","message":{"content":[{"type":"text","text":"ok"}]}}`,
		`{"type":"result","is_error":false,"result":"ok","session_id":"s3"}`,
	}, "\n") + "\n"
	var deltas []string
	res, _, err := parseStream(strings.NewReader(input), func(t string) { deltas = append(deltas, t) }, nil)
	if err != nil {
		t.Fatalf("parseStream: %v", err)
	}
	if res == nil || res.Text != "ok" {
		t.Fatalf("res=%+v", res)
	}
	if len(deltas) != 1 || deltas[0] != "ok" {
		t.Errorf("deltas=%v", deltas)
	}
}

func TestParseStream_NoResultReturnsNilResult(t *testing.T) {
	// Se claude muore prima del result, il parser ritorna res=nil senza errore di parsing.
	input := `{"type":"system","subtype":"init"}` + "\n" +
		`{"type":"assistant","message":{"content":[{"type":"text","text":"a"}]}}` + "\n"
	res, raw, err := parseStream(strings.NewReader(input), nil, nil)
	if err != nil {
		t.Fatalf("parseStream: %v", err)
	}
	if res != nil {
		t.Errorf("res=%+v, atteso nil", res)
	}
	if !strings.Contains(raw, `"type":"assistant"`) {
		t.Errorf("raw incompleto: %q", raw)
	}
}

func TestParseStream_LongLineAboveDefaultScannerBuffer(t *testing.T) {
	// Un blocco assistant da 200 KB supera il default Scanner buffer (64 KB):
	// vogliamo che il parser non si pianti.
	big := strings.Repeat("x", 200*1024)
	input := `{"type":"assistant","message":{"content":[{"type":"text","text":"` + big + `"}]}}` + "\n" +
		`{"type":"result","is_error":false,"result":"final","session_id":"s4"}` + "\n"
	var deltas []string
	res, _, err := parseStream(strings.NewReader(input), func(t string) { deltas = append(deltas, t) }, nil)
	if err != nil {
		t.Fatalf("parseStream: %v", err)
	}
	if res == nil || res.Text != "final" {
		t.Fatalf("res=%+v", res)
	}
	if len(deltas) != 1 || len(deltas[0]) != len(big) {
		t.Errorf("delta lungo non ricevuto: len=%d", len(deltas))
	}
}
