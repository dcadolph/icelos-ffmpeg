// Command icelos-verify checks an Icelos receipt again. It re-runs every proof from the plan the
// receipt carries, against the source and output beside it, and reports whether the receipt still
// holds.
//
// Usage:
//
//	icelos-verify [--ffmpeg <dir>] [--source <path>] [--output <path>] <receipt.json>
//
// It exits 0 when the receipt holds, 1 when it does not, and 2 when the check could not run.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"

	"github.com/dcadolph/icelos-ffmpeg/verify"
)

// Exit codes and the score slack a re-measured check may show.
const (
	// exitHolds means every check re-measured as the receipt says.
	exitHolds = 0
	// exitBroken means at least one check re-measured differently.
	exitBroken = 1
	// exitCannot means the check could not run.
	exitCannot = 2
	// pictureSlack is how far a re-measured picture score may land from the receipt's.
	pictureSlack = 0.005
	// soundSlack is how many decibels a re-measured sound score may land from the receipt's.
	soundSlack = 0.5
)

// noPlan is printed for a receipt that carries no plan to re-run.
const noPlan = "receipt predates schema 2 and holds no plan; nothing to re-check"

// main runs the command and exits with its code.
func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

// run parses args, re-checks the receipt they name, writes the verdict to stdout and problems to
// stderr, and returns the exit code.
func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("icelos-verify", flag.ContinueOnError)
	flags.SetOutput(stderr)
	ffmpegDir := flags.String("ffmpeg", "", "Folder holding the ffmpeg and ffprobe to measure "+
		"with. Empty uses the ones on PATH.")
	source := flags.String("source", "", "Source file. Defaults to the receipt's source name "+
		"beside the receipt.")
	output := flags.String("output", "", "Output file. Defaults to the receipt's output name "+
		"beside the receipt.")
	flags.Usage = func() {
		_, _ = fmt.Fprintln(stderr, "usage: icelos-verify [--ffmpeg <dir>] [--source <path>] "+
			"[--output <path>] <receipt.json>")
		flags.PrintDefaults()
	}
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitHolds
		}
		return exitCannot
	}
	if flags.NArg() != 1 {
		flags.Usage()
		return exitCannot
	}
	path := flags.Arg(0)
	was, planned, err := readReceipt(path)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "icelos-verify: %v\n", err)
		return exitCannot
	}
	if !planned {
		_, _ = fmt.Fprintln(stderr, noPlan)
		return exitCannot
	}
	in := verify.Input{
		Source: beside(path, *source, was.Source),
		Output: beside(path, *output, was.Output),
		Expect: was.Expect,
	}
	if *ffmpegDir != "" {
		in.Tools = verify.Tools{
			FFmpeg:  filepath.Join(*ffmpegDir, "ffmpeg"),
			FFprobe: filepath.Join(*ffmpegDir, "ffprobe"),
		}
	}
	now, err := verify.Verify(ctx, in)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "icelos-verify: could not re-measure: %v\n", err)
		return exitCannot
	}
	if was.Tool != now.Tool {
		_, _ = fmt.Fprintf(stdout, "tool: receipt measured with %q, now %q\n", was.Tool, now.Tool)
	}
	if mismatch := compare(was, now); mismatch != "" {
		_, _ = fmt.Fprintf(stdout, "does not hold: %s\n", mismatch)
		return exitBroken
	}
	_, _ = fmt.Fprintf(stdout, "holds: %d checks re-measured, %d not checked\n",
		len(now.Checks), len(now.Unchecked))
	return exitHolds
}

// readReceipt reads the receipt at path and reports whether it carries a plan to re-run, which a
// receipt older than schema 2 or one with its plan removed does not.
func readReceipt(path string) (verify.Receipt, bool, error) {
	data, err := os.ReadFile(path) //nolint:gosec // The user names the receipt to read.
	if err != nil {
		return verify.Receipt{}, false, fmt.Errorf("read receipt: %w", err)
	}
	var r verify.Receipt
	if err := json.Unmarshal(data, &r); err != nil {
		return verify.Receipt{}, false, fmt.Errorf("parse receipt: %w", err)
	}
	var raw struct {
		// Expect is the plan exactly as written, empty when the receipt has none.
		Expect json.RawMessage `json:"expect"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return verify.Receipt{}, false, fmt.Errorf("parse receipt: %w", err)
	}
	if r.Schema < verify.SchemaVersion || len(raw.Expect) == 0 || string(raw.Expect) == "null" {
		return r, false, nil
	}
	if r.Source == "" || r.Output == "" {
		return r, true, errors.New("parse receipt: no source or output named")
	}
	return r, true, nil
}

// beside returns override when set, and else the file named like recorded in the receipt's folder.
func beside(receipt, override, recorded string) string {
	if override != "" {
		return override
	}
	return filepath.Join(filepath.Dir(receipt), filepath.Base(recorded))
}

// compare returns the first way now differs from was, or empty when it holds. Checks pair by name
// and by order among checks of that name. Pass must match, and picture and sound scores must land
// within their slack. Measured text, thresholds, methods, and the tool are not compared.
func compare(was, now verify.Receipt) string {
	seen := map[string]int{}
	for _, w := range was.Checks {
		n := nth(now.Checks, w.Name, seen[w.Name])
		seen[w.Name]++
		if n == nil {
			return fmt.Sprintf("%s was %s, now not run", w.Name, state(w))
		}
		if w.Pass != n.Pass || !scoreHolds(w, *n) {
			return fmt.Sprintf("%s was %s, now %s", w.Name, state(w), state(*n))
		}
	}
	counted := map[string]int{}
	for _, n := range now.Checks {
		counted[n.Name]++
		if counted[n.Name] > seen[n.Name] {
			return fmt.Sprintf("%s was not run, now %s", n.Name, state(n))
		}
	}
	wasNotes, nowNotes := noteNames(was.Unchecked), noteNames(now.Unchecked)
	for _, name := range wasNotes {
		if i := slices.Index(nowNotes, name); i >= 0 {
			nowNotes = slices.Delete(nowNotes, i, i+1)
			continue
		}
		return fmt.Sprintf("%s was not checked, now it has no note", name)
	}
	if len(nowNotes) > 0 {
		return fmt.Sprintf("%s had no note, now it is not checked", nowNotes[0])
	}
	return ""
}

// nth returns the check called name that comes after skip others of that name, or nil.
func nth(checks []verify.Check, name string, skip int) *verify.Check {
	for i := range checks {
		if checks[i].Name != name {
			continue
		}
		if skip == 0 {
			return &checks[i]
		}
		skip--
	}
	return nil
}

// scoreHolds reports whether a picture or sound score landed within its slack. Other checks hold
// on pass alone.
func scoreHolds(was, now verify.Check) bool {
	switch was.Name {
	case verify.CheckPicture:
		return math.Abs(was.Score-now.Score) <= pictureSlack
	case verify.CheckSound:
		return math.Abs(was.Score-now.Score) <= soundSlack
	}
	return true
}

// state returns a check's pass or fail, with its score when it has one.
func state(c verify.Check) string {
	verdict := "fail"
	if c.Pass {
		verdict = "pass"
	}
	if c.Score == 0 {
		return verdict
	}
	return verdict + " " + strconv.FormatFloat(c.Score, 'f', 4, 64)
}

// noteNames returns the names of notes, in order.
func noteNames(notes []verify.Note) []string {
	names := make([]string, 0, len(notes))
	for _, n := range notes {
		names = append(names, n.Name)
	}
	return names
}
