package verify

import "errors"

// ErrVerify is returned when the proofs could not run at all: a missing tool, an unreadable
// source, or a canceled context. An output that fails a proof is a failed check, not this error.
var ErrVerify = errors.New("verify failed")
