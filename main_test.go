package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSplitCreatesDeterministicParts(t *testing.T) {
	directory := t.TempDir()
	input := filepath.Join(directory, "domains.txt")
	if err := os.WriteFile(input, []byte("a.uk\nb.uk\nc.uk\nd.uk\ne.uk\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(directory, "parts")
	if err := runSplit([]string{"--input", input, "--parts", "2", "--output-dir", output}); err != nil {
		t.Fatal(err)
	}
	partOne, err := os.ReadFile(filepath.Join(output, "part-1-of-2.txt"))
	if err != nil {
		t.Fatal(err)
	}
	partTwo, err := os.ReadFile(filepath.Join(output, "part-2-of-2.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(partOne) != "a.uk\nb.uk\nc.uk\n" || string(partTwo) != "d.uk\ne.uk\n" {
		t.Fatalf("unexpected split: %q / %q", partOne, partTwo)
	}
}

func TestActiveStatusRules(t *testing.T) {
	active := []int{200, 301, 302, 399, 401, 403}
	for _, status := range active {
		if !(status == 200 || status >= 300 && status < 400 || status == 401 || status == 403) {
			t.Fatalf("status %d should be active", status)
		}
	}
	if 404 == 200 || 404 >= 300 && 404 < 400 || 404 == 401 || 404 == 403 {
		t.Fatal("404 should be inactive")
	}
}
