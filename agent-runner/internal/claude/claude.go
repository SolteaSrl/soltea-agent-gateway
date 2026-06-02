// Package claude lancia il CLI `claude` in modalita' non interattiva e ne
// interpreta l'output JSON, mantenendo la sessione per la continuita' della chat.
package claude

import (
	"context"
	"encoding/json"
	"fmt"
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

// Result e' l'esito di un turno di claude.
type Result struct {
	Text       string
	SessionID  string
	IsError    bool
	CostUSD    float64
	DurationMS int64
}

// rawResult mappa i campi che ci servono dall'output `--output-format json`.
type rawResult struct {
	Type       string  `json:"type"`
	Subtype    string  `json:"subtype"`
	IsError    bool    `json:"is_error"`
	Result     string  `json:"result"`
	SessionID  string  `json:"session_id"`
	TotalCost  float64 `json:"total_cost_usd"`
	DurationMS int64   `json:"duration_ms"`
}

// Runner esegue claude in una cartella di lavoro.
type Runner struct {
	opt Options
}

func New(opt Options) *Runner { return &Runner{opt: opt} }

// Run esegue un turno. Se resumeSession e' vuoto avvia una nuova sessione,
// altrimenti riprende quella esistente (--resume).
func (r *Runner) Run(ctx context.Context, workdir, prompt, resumeSession string) (*Result, error) {
	args := r.buildArgs(prompt, resumeSession)

	var cmd *exec.Cmd
	if r.opt.UseGitBash {
		// bash.exe -lc "claude ...": utile se claude vive solo nell'ambiente Git-bash.
		cmd = exec.CommandContext(ctx, r.opt.GitBashPath, "-lc", shellJoin(append([]string{claudeBin(r.opt.ClaudePath)}, args...)))
	} else {
		cmd = exec.CommandContext(ctx, r.opt.ClaudePath, args...)
	}
	cmd.Dir = workdir

	out, err := cmd.Output()
	if err != nil {
		stderr := ""
		if ee, ok := err.(*exec.ExitError); ok {
			stderr = strings.TrimSpace(string(ee.Stderr))
		}
		return nil, fmt.Errorf("claude fallito: %v: %s", err, stderr)
	}

	var raw rawResult
	if jerr := json.Unmarshal(out, &raw); jerr != nil {
		return nil, fmt.Errorf("output claude non JSON: %w (primi byte: %.120q)", jerr, string(out))
	}
	return &Result{
		Text:       raw.Result,
		SessionID:  raw.SessionID,
		IsError:    raw.IsError,
		CostUSD:    raw.TotalCost,
		DurationMS: raw.DurationMS,
	}, nil
}

func (r *Runner) buildArgs(prompt, resumeSession string) []string {
	args := []string{"-p", prompt, "--output-format", "json"}
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
