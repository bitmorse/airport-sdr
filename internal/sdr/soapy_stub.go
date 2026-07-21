//go:build !soapy

package sdr

import "errors"

// ErrNoSoapy is returned when hardware support was not compiled in.
var ErrNoSoapy = errors.New(
	"this binary was built without SoapySDR support; rebuild with `make build-full` " +
		"(needs libsoapysdr-dev) or use a recorded .cf32 file as the source")

// NewSoapySource always fails in a pure-Go build. Keeping the symbol present in
// both builds means the rest of the program has no build tags of its own.
func NewSoapySource(_ SoapyOptions) (Source, error) {
	return nil, ErrNoSoapy
}

// ProbeDevice always fails in a pure-Go build.
func ProbeDevice(_ string) (DeviceCaps, error) {
	return DeviceCaps{}, ErrNoSoapy
}
