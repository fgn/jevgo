package jev

import (
	"runtime"
	"strings"
)

// Version is the SDK version sent in the User-Agent and X-TypeSafe-SDK headers.
const Version = "0.1.0"

const sdkName = "jevgo"

var (
	userAgent          = sdkName + "/" + Version
	runtimeDescription = describeRuntime()
)

// describeRuntime reports the Go runtime for the X-TypeSafe-Runtime header,
// mirroring the official SDKs: "go/1.25.3 (linux; amd64)".
func describeRuntime() string {
	version := strings.TrimPrefix(runtime.Version(), "go")
	return "go/" + version + " (" + runtime.GOOS + "; " + runtime.GOARCH + ")"
}
