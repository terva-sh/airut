//go:build !goolm

package airut

// airut must be built with -tags goolm, which makes mautrix use its
// pure-Go Olm backend instead of cgo libolm. This import fails at load
// time with "build constraints exclude all Go files in
// .../internal/buildwithtagsgoolm", so the build output names the fix.
// The guard sits in the root package so a build fails here, with this
// message, whether or not it reaches the Matrix adapter.
import _ "terva.sh/airut/internal/buildwithtagsgoolm"
