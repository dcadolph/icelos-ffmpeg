// Package sim writes test media from lavfi at test time, each file paired with the facts it was
// built to hold. The expected answers are computed from the recipe, never recorded from a run,
// and nothing it writes is committed.
package sim

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/dcadolph/icelos-ffmpeg/proc"
)

// Recipe describes one simulated source and the facts it holds.
type Recipe struct {
	// Name identifies the recipe in test names and in Find.
	Name string
	// File is the name of the file written inside the target folder.
	File string
	// Sidecars maps file names to text the steps read, written inside the target folder first.
	Sidecars map[string]string
	// Steps returns the ffmpeg argument lists to run in order, given the target folder and the
	// final path. A recipe with no steps relies on Finish to write its file.
	Steps func(dir, out string) [][]string
	// Finish changes the final file after the steps ran, such as cutting it short.
	Finish func(out string) error
	// Truth holds what the finished file contains.
	Truth Truth
}

// Generate writes the recipe's file under dir, which must exist, and returns its path and Truth.
func Generate(ctx context.Context, recipe Recipe, dir string) (string, Truth, error) {
	return generate(ctx, "ffmpeg", recipe, dir)
}

// generate writes the recipe's file with the ffmpeg binary named by bin.
func generate(ctx context.Context, bin string, recipe Recipe, dir string) (string, Truth, error) {
	info, err := os.Stat(dir)
	if err != nil {
		return "", Truth{}, fmt.Errorf("%w: %s: %w", ErrSim, recipe.Name, err)
	}
	if !info.IsDir() {
		return "", Truth{}, fmt.Errorf("%w: %s: %s is not a folder", ErrSim, recipe.Name, dir)
	}
	for name, text := range recipe.Sidecars {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o600); err != nil {
			return "", Truth{}, fmt.Errorf("%w: %s: sidecar: %w", ErrSim, recipe.Name, err)
		}
	}
	out := filepath.Join(dir, recipe.File)
	if recipe.Steps != nil {
		for _, args := range recipe.Steps(dir, out) {
			if err := runStep(ctx, bin, args); err != nil {
				return "", Truth{}, fmt.Errorf("%w: %s: %w", ErrSim, recipe.Name, err)
			}
		}
	}
	if recipe.Finish != nil {
		if err := recipe.Finish(out); err != nil {
			return "", Truth{}, fmt.Errorf("%w: %s: finish: %w", ErrSim, recipe.Name, err)
		}
	}
	if _, err := os.Stat(out); err != nil {
		return "", Truth{}, fmt.Errorf("%w: %s: %w", ErrSim, recipe.Name, err)
	}
	return out, recipe.Truth, nil
}

// runStep runs one ffmpeg command quietly and reports the first error line it logged.
func runStep(ctx context.Context, bin string, args []string) error {
	full := slices.Concat([]string{"-hide_banner", "-nostdin", "-v", "error", "-y"}, args)
	var stderr bytes.Buffer
	cmd := proc.Command(ctx, bin, full...)
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		line, _, _ := strings.Cut(strings.TrimSpace(stderr.String()), "\n")
		if line == "" {
			return fmt.Errorf("ffmpeg step: %w", err)
		}
		return fmt.Errorf("ffmpeg step: %w: %s", err, line)
	}
	return nil
}

// Find returns the recipe named name, looking through Recipes and LongRecipes.
func Find(name string) (Recipe, bool) {
	for _, r := range slices.Concat(Recipes(), LongRecipes()) {
		if r.Name == name {
			return r, true
		}
	}
	return Recipe{}, false
}
