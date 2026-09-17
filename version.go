package jev

import (
	"runtime"
	"strings"
)

// Version is sent in the User-Agent and X-Typesafe-Sdk headers.
const Version = "0.3.0"

var (
	userAgent          = "jevgo/" + Version
	runtimeDescription = "go/" + strings.TrimPrefix(
		runtime.Version(),
		"go",
	) + " (" + runtime.GOOS + "; " + runtime.GOARCH + ")"
)
