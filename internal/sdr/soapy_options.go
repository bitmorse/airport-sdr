package sdr

// SoapyOptions configures a hardware source. It is defined without a build tag
// so that callers compile identically whether or not SoapySDR support is built
// in; only the constructor changes.
type SoapyOptions struct {
	// DeviceArgs is a SoapySDR device argument string such as "driver=rtlsdr".
	// Empty selects the first device found.
	DeviceArgs string
	SampleRate float64
	CenterFreq float64
	Gain       float64
	AutoGain   bool
	Antenna    string
	// PPM is a crystal error correction in parts per million.
	PPM float64
	// BlockSize in samples; zero derives roughly 20 ms from the sample rate.
	BlockSize int
	// PoolSize is the number of preallocated blocks; zero uses the default.
	PoolSize int
}
