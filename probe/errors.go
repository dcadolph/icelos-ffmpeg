package probe

import "errors"

// ErrProbe is returned when ffprobe fails or its output cannot be read.
var ErrProbe = errors.New("probe failed")
