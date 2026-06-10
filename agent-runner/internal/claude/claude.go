// Package claude lancia il CLI `claude` in modalita' non interattiva e ne
// interpreta l'output JSON, mantenendo la sessione per la continuita' della chat.
//
// A partire dal runner v0.5.0 usiamo l'output stream-json (NDJSON): leggiamo gli
// eventi riga per riga e propaghiamo i blocchi assistant come "delta" via il
// callback OnDelta, cosi' l'orchestratrice riceve feedback live invece di 130s
// di buio. L'evento finale `{"type":"result", ...}` resta l'unica fonte di verita'
// per text/session_id/cost/duration_ms/is_error.
package claude

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strings"
)

// Options configura come invocare il CLI.
type Options struct {
	ClaudePath     string // es. "claude.exe" o percorso assoluto
	UseGitBash     bool   // se true, lancia via bash.exe -lc
	GitBashPath    string // percorso di bash.exe
	Model          string // opzionale (--model)
	PermissionMode string // --permission-mode
}

// Result e' l'esito di un turno di claude. RawStdout/Stderr contengono l'output
// grezzo del CLI (NDJSON completo + stderr), sempre valorizzati anche in errore
// per finire nei log.
type Result struct {
	Text       string
	SessionID  string
	IsError    bool
	CostUSD    float64
	DurationMS int64
	RawStdout  string
	Stderr     string
}

// Runner esegue claude in una cartella di lavoro.
type Runner struct {
	opt Options
}

func New(opt Options) *Runner { return &Runner{opt: opt} }

// Run esegue un turno di claude. Se resumeSession e' vuoto avvia una nuova
// sessione, altrimenti riprende quella esistente (--resume).
//
// onDelta, se non nil, viene chiamato per ogni blocco di testo emesso dai
// frame "assistant" durante l'esecuzione (streaming). Il chiamante NON deve
// bloccare a lungo nel callback: tipicamente lo usa per inoltrare un
// chat.delta sulla WS al gateway.
func (r *Runner) Run(ctx context.Context, workdir, prompt, resumeSession string, onDelta func(text string)) (*Result, error) {
	args := r.buildArgs(prompt, resumeSession)

	var cmd *exec.Cmd
	if r.opt.UseGitBash {
		// bash.exe -lc "claude ...": utile se claude vive solo nell'ambiente Git-bash.
		cmd = exec.CommandContext(ctx, r.opt.GitBashPath, "-lc", shellJoin(append([]string{claudeBin(r.opt.ClaudePath)}, args...)))
	} else {
		cmd = exec.CommandContext(ctx, r.opt.ClaudePath, args...)
	}
	cmd.Dir = workdir

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("apertura stdout: %w", err)
	}
	var errBuf bytes.Buffer
	cmd.Stderr = &errBuf

	if err := cmd.Start(); err != nil {
		return &Result{Stderr: strings.TrimSpace(errBuf.String())},
			fmt.Errorf("avvio claude: %w", err)
	}

	res, rawOut, parseErr := parseStream(stdout, onDelta)

	runErr := cmd.Wait()
	stderr := strings.TrimSpace(errBuf.String())

	if res != nil {
		res.RawStdout = rawOut
		res.Stderr = stderr
	}

	if runErr != nil {
		// Result non-nil anche in errore: il chiamante puo' loggare l'output grezzo.
		out := res
		if out == nil {
			out = &Result{RawStdout: rawOut, Stderr: stderr}
		}
		return out, fmt.Errorf("claude fallito: %v: %s", runErr, stderr)
	}
	if parseErr != nil {
		out := res
		if out == nil {
			out = &Result{RawStdout: rawOut, Stderr: stderr}
		}
		return out, parseErr
	}
	if res == nil {
		return &Result{RawStdout: rawOut, Stderr: stderr},
			fmt.Errorf("nessun evento 'result' nell'output stream-json (primi byte: %.120q)", rawOut)
	}
	return res, nil
}

func (r *Runner) buildArgs(prompt, resumeSession string) []string {
	// --output-format stream-json + --verbose: NDJSON con eventi system/assistant/user/result.
	args := []string{"-p", prompt, "--output-format", "stream-json", "--verbose"}
	if r.opt.PermissionMode != "" {
		args = append(args, "--permission-mode", r.opt.PermissionMode)
	}
	if r.opt.Model != "" {
		args = append(args, "--model", r.opt.Model)
	}
	if resumeSession != "" {
		args = append(args, "--resume", resumeSession)
	}
	return args
}

// streamEvent e' il sottoinsieme dei campi che ci servono dagli eventi
// stream-json di claude. Tutto il resto e' ignorato e resta nel raw stdout.
type streamEvent struct {
	Type       string  `json:"type"`
	Subtype    string  `json:"subtype"`
	IsError    bool    `json:"is_error"`
	Result     string  `json:"result"`
	SessionID  string  `json:"session_id"`
	TotalCost  float64 `json:"total_cost_usd"`
	DurationMS int64   `json:"duration_ms"`
	Message    *struct {
		Content []struct {
			Type string `json:"type"` // "text" | "tool_use" | "thinking" | ...
			Text string `json:"text"`
		} `json:"content"`
	} `json:"message,omitempty"`
}

// parseStream legge NDJSON dal lettore, accumula raw, invoca onDelta per i
// blocchi assistant text ed estrae il record "result" finale. Tollera righe
// non-JSON (vengono ignorate ma restano nel raw).
//
// Ritorna sempre rawOut (anche su errore parziale), cosi' chi chiama puo'
// loggare tutto per diagnosi.
func parseStream(r io.Reader, onDelta func(text string)) (*Result, string, error) {
	var rawBuf bytes.Buffer
	tee := io.TeeReader(r, &rawBuf)
	scanner := bufio.NewScanner(tee)
	// claude puo' produrre righe lunghe (messaggi assistant interi): alziamo
	// il buffer di scanner a 4 MB per riga (default 64 KB).
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	var final *Result
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var ev streamEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			continue // riga non-JSON: ignorata, ma resta nel raw
		}
		switch ev.Type {
		case "assistant":
			if ev.Message == nil || onDelta == nil {
				continue
			}
			for _, c := range ev.Message.Content {
				if c.Type == "text" && c.Text != "" {
					onDelta(c.Text)
				}
			}
		case "result":
			final = &Result{
				Text:       ev.Result,
				SessionID:  ev.SessionID,
				IsError:    ev.IsError,
				CostUSD:    ev.TotalCost,
				DurationMS: ev.DurationMS,
			}
		}
	}
	rawOut := rawBuf.String()
	if err := scanner.Err(); err != nil {
		return final, rawOut, fmt.Errorf("lettura stream-json: %w", err)
	}
	return final, rawOut, nil
}

// claudeBin estrae il comando per la riga bash (senza path Windows con backslash).
func claudeBin(p string) string {
	if p == "" {
		return "claude.exe"
	}
	return p
}

// shellJoin fa un quoting basilare per la riga passata a bash -lc.
func shellJoin(parts []string) string {
	q := make([]string, len(parts))
	for i, p := range parts {
		q[i] = "'" + strings.ReplaceAll(p, "'", `'\''`) + "'"
	}
	return strings.Join(q, " ")
}
