package simkit

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/RCX1t7/plexus/tests/fakeharness"
)

// ReadLedger parses the side-effect ledger written by the fake harnesses.
func ReadLedger(path string) ([]fakeharness.LedgerEntry, error) {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []fakeharness.LedgerEntry
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var e fakeharness.LedgerEntry
		if json.Unmarshal(sc.Bytes(), &e) == nil {
			out = append(out, e)
		}
	}
	return out, sc.Err()
}

// CountPhase counts ledger entries per "bot:action" for a phase.
func CountPhase(entries []fakeharness.LedgerEntry, phase string) map[string]int {
	m := map[string]int{}
	for _, e := range entries {
		if e.Phase == phase {
			m[e.Bot+":"+e.Action]++
		}
	}
	return m
}

// ReadJSONL reads a generic JSON-lines file (e.g. the hub event log).
func ReadJSONL(path string) ([]map[string]any, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []map[string]any
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for sc.Scan() {
		var m map[string]any
		if json.Unmarshal(sc.Bytes(), &m) == nil {
			out = append(out, m)
		}
	}
	return out, sc.Err()
}

// ---------------------------------------------------------------- golden

// InputTxt is the exact scenario input (35 bytes, LF line endings, UTF-8, no BOM).
const InputTxt = "harness hub\nclaude codex dsh\nslack\n"

func sha(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }

// Expected holds the golden artifact contents of the main scenario.
type Expected struct {
	Files    map[string]string // out/<name> -> exact content
	Manifest string
	SHA      map[string]string // out/<name> -> sha256
}

// ExpectedArtifacts derives the golden outputs from InputTxt and the answer
// "lines/words/bytes" (long field names).
func ExpectedArtifacts() Expected {
	c := fakeharness.CountBytes([]byte(InputTxt))
	result := fakeharness.ResultJSON(c, true)
	report := fmt.Sprintf("PASS lines=%d words=%d bytes=%d\n", c.Lines, c.Words, c.Bytes)
	review := fakeharness.ReviewMD(sha(result), sha(report))
	files := map[string]string{"result.json": result, "test-report.txt": report, "REVIEW.md": review}
	names := append([]string(nil), fakeharness.ManifestFiles...)
	sort.Strings(names)
	var sb strings.Builder
	for _, n := range names {
		fmt.Fprintf(&sb, "%s  %s\n", sha(files[n]), n)
	}
	files["MANIFEST.sha256"] = sb.String()
	e := Expected{Files: files, Manifest: sb.String(), SHA: map[string]string{}}
	for n, v := range files {
		e.SHA[n] = sha(v)
	}
	return e
}
