// rename-selftest — verifica se un .exe Windows può rinominarsi a runtime.
// Output stampato su stdout, exit code 0 in ogni caso (così Marcello vede sempre il verdetto).
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

func main() {
	fmt.Println("=== rename-selftest ===")
	fmt.Println("OS/Arch:", runtime.GOOS, runtime.GOARCH)
	fmt.Println("PID:", os.Getpid())

	self, err := os.Executable()
	if err != nil {
		fmt.Println("[FATAL] cannot resolve self path:", err)
		os.Exit(1)
	}
	self, _ = filepath.Abs(self)
	fmt.Println("self:", self)

	dir := filepath.Dir(self)
	base := filepath.Base(self)
	oldPath := filepath.Join(dir, base+".old")

	// Se un .old residuo c'è da un test precedente, prova a toglierlo subito.
	if _, err := os.Stat(oldPath); err == nil {
		_ = os.Remove(oldPath)
	}

	fmt.Println()
	fmt.Println("[TEST 1] rename SELF mentre è in esecuzione →", oldPath)
	if err := os.Rename(self, oldPath); err != nil {
		fmt.Println("  RESULT: FAIL")
		fmt.Println("  error:", err)
		fmt.Println()
		fmt.Println("=== VERDETTO: PIANO A NON POSSIBILE (rename bloccato) ===")
		fmt.Println("→ usare piano B (launcher + worker)")
		return
	}
	fmt.Println("  RESULT: OK — il binario in esecuzione è stato rinominato.")

	fmt.Println()
	fmt.Println("[TEST 2] scrivere nuovo binario allo slot liberato:", self)
	data, err := os.ReadFile(oldPath)
	if err != nil {
		fmt.Println("  cannot read self.old:", err)
		fmt.Println("  RESULT: FAIL")
		return
	}
	if err := os.WriteFile(self, data, 0o755); err != nil {
		fmt.Println("  RESULT: FAIL")
		fmt.Println("  error:", err)
		fmt.Println()
		fmt.Println("=== VERDETTO: rename ok ma write nuovo binario fallisce ===")
		return
	}
	fmt.Println("  RESULT: OK — nuovo binario scritto,", len(data), "byte")

	fmt.Println()
	fmt.Println("[TEST 3] tentare delete del .old (sono ancora in esecuzione da lì)")
	if err := os.Remove(oldPath); err != nil {
		fmt.Println("  RESULT: ATTESO fallire →", err)
		fmt.Println("  → ok: il vecchio binario va cancellato al boot successivo (MOVEFILE_DELAY_UNTIL_REBOOT)")
	} else {
		fmt.Println("  RESULT: cancellato (inaspettato ma innocuo)")
	}

	fmt.Println()
	fmt.Println("=== VERDETTO: PIANO A POSSIBILE ===")
	fmt.Println("→ single-binary auto-update fattibile (rename + write a slot libero)")
	fmt.Println()
	fmt.Println("Stato file dopo il test:")
	for _, p := range []string{self, oldPath} {
		if fi, err := os.Stat(p); err == nil {
			fmt.Printf("  - %s (%d byte)\n", p, fi.Size())
		}
	}
	fmt.Println()
	fmt.Println("Nota: dopo aver chiuso questo processo puoi cancellare manualmente il .old.")
}
