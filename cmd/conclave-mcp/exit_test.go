package main

import (
	"errors"
	"testing"
)

func TestExitClassifier_NilIsZero(t *testing.T) {
	if got := exitClassifier(nil); got != 0 {
		t.Errorf("nil err: got %d, want 0", got)
	}
}

func TestExitClassifier_PlainErrorIsTwo(t *testing.T) {
	// Any unknown cobra error is client misuse per SPEC-0004 §4
	if got := exitClassifier(errors.New("unknown command: frobnicate")); got != 2 {
		t.Errorf("plain err: got %d, want 2", got)
	}
}

func TestExitClassifier_ExitCodeErrorPropagates(t *testing.T) {
	wrapped := &exitCodeError{Code: 3, Err: errors.New("network down")}
	if got := exitClassifier(wrapped); got != 3 {
		t.Errorf("exitCodeError: got %d, want 3", got)
	}
}

func TestExitClassifier_DeeplyWrapped(t *testing.T) {
	inner := &exitCodeError{Code: 4, Err: errors.New("not conclave")}
	wrapped := errors.Join(errors.New("while starting"), inner)
	if got := exitClassifier(wrapped); got != 4 {
		t.Errorf("deeply wrapped: got %d, want 4", got)
	}
}
