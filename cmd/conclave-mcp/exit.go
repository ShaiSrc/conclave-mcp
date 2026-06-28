package main

import "errors"

// exitClassifier maps cobra error categories to process exit codes
// per SPEC-0004 §4:
//
//	0 success
//	1 server returned 4xx/5xx (subcommand responsibility)
//	2 client-side misuse (bad flag, bad arg, unknown command)
//	3 network error (subcommand responsibility)
//	4 server is not a Conclave instance (subcommand responsibility)
//
// Subcommands that need a specific non-zero exit code wrap it in
// *exitCodeError; the wrapper is unwrapped via errors.As.
func exitClassifier(err error) int {
	if err == nil {
		return 0
	}
	var ece *exitCodeError
	if errors.As(err, &ece) {
		return ece.Code
	}
	// Cobra's own "unknown command" / "unknown flag" errors fall here.
	return 2
}

// exitCodeError lets subcommands request a specific exit code while
// still propagating up through RunE.
type exitCodeError struct {
	Code int
	Err  error
}

func (e *exitCodeError) Error() string { return e.Err.Error() }
func (e *exitCodeError) Unwrap() error { return e.Err }
