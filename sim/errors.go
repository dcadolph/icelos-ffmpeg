package sim

import "errors"

// ErrSim is returned when a recipe could not be written: a missing tool, a folder that cannot be
// written, or an ffmpeg step that failed.
var ErrSim = errors.New("simulation failed")
