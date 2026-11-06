package logging

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
)

func TestOrDiscardKeepsALoggerAndFillsInNil(t *testing.T) {
	OrDiscard(nil).Info("Wrote nothing")
	var lines []string
	log := Lines(func(line string) { lines = append(lines, line) }, slog.LevelInfo)
	OrDiscard(log).Info("Kept logger")
	if len(lines) != 1 {
		t.Fatalf("lines = %q", lines)
	}
}

func TestFromContext(t *testing.T) {
	FromContext(context.Background()).Info("Wrote nothing")
	var lines []string
	log := Lines(func(line string) { lines = append(lines, line) }, slog.LevelInfo)
	FromContext(NewContext(context.Background(), log)).Info("Found logger")
	if len(lines) != 1 {
		t.Fatalf("lines = %q", lines)
	}
}

func TestLinesHasNoNewlineAndKeepsToItsLevel(t *testing.T) {
	var lines []string
	log := Lines(func(line string) { lines = append(lines, line) }, slog.LevelInfo)
	log.Debug("Too quiet")
	log.Error("Failed to open Device", "err", errors.New("gone"))
	if len(lines) != 1 || !strings.Contains(lines[0], `msg="Failed to open Device" err=gone`) ||
		strings.Contains(lines[0], "\n") || strings.Contains(lines[0], "time=") {
		t.Fatalf("lines = %q", lines)
	}
}
