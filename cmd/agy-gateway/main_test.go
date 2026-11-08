package main

import "testing"

func TestRunVersion(t *testing.T) {
	if err := run([]string{"version"}); err != nil {
		t.Fatalf("run: %v", err)
	}
}

func TestRunUnknownCommand(t *testing.T) {
	if err := run([]string{"bogus"}); err == nil {
		t.Fatal("expected an error")
	}
}
