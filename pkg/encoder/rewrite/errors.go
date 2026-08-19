package rewrite

// ErrorCode represents an error code for rewrite operations.
type ErrorCode string

const (
	// ErrFormatNotMatch indicates the requested format is not available.
	ErrFormatNotMatch ErrorCode = "FORMAT_NOT_MATCH"

	// ErrHardwareNotSupported indicates hardware acceleration is not supported.
	ErrHardwareNotSupported ErrorCode = "HARDWARE_NOT_SUPPORTED"

	// ErrEncoderNotAvailable indicates the requested encoder is not available.
	ErrEncoderNotAvailable ErrorCode = "ENCODER_NOT_AVAILABLE"

	// ErrTranslationFailed indicates parameter translation failed.
	ErrTranslationFailed ErrorCode = "TRANSLATION_FAILED"

	// ErrInvalidRequest indicates the request is invalid.
	ErrInvalidRequest ErrorCode = "INVALID_REQUEST"

	// ErrNoSuitableEncoder indicates no suitable encoder was found.
	ErrNoSuitableEncoder ErrorCode = "NO_SUITABLE_ENCODER"

	// ErrHardwareInjectionFailed indicates hardware parameter injection failed.
	ErrHardwareInjectionFailed ErrorCode = "HW_INJECTION_FAILED"

	// ErrBlacklistedEncoder indicates the encoder is blacklisted.
	ErrBlacklistedEncoder ErrorCode = "BLACKLISTED_ENCODER"

	// ErrEncoderUnsupported indicates the encoder is completely unknown/unsupported.
	// This is different from ErrEncoderNotAvailable which indicates a known encoder
	// that is not available on this system. ErrEncoderUnsupported is for encoders
	// that are not recognized at all (e.g., typos, non-existent encoder names).
	ErrEncoderUnsupported ErrorCode = "ENCODER_UNSUPPORTED"
)

// String returns the string representation of the error code.
func (e ErrorCode) String() string {
	return string(e)
}

// Description returns a human-readable description of the error code.
func (e ErrorCode) Description() string {
	switch e {
	case ErrFormatNotMatch:
		return "The requested codec format is not available on this worker"
	case ErrHardwareNotSupported:
		return "Hardware acceleration is not supported for this operation"
	case ErrEncoderNotAvailable:
		return "The requested encoder is not available on this worker"
	case ErrTranslationFailed:
		return "Parameter translation between encoders failed"
	case ErrInvalidRequest:
		return "The rewrite request is invalid"
	case ErrNoSuitableEncoder:
		return "No suitable encoder found for the requested format"
	case ErrHardwareInjectionFailed:
		return "Failed to inject hardware-specific parameters"
	case ErrBlacklistedEncoder:
		return "The encoder is blacklisted and cannot be used"
	case ErrEncoderUnsupported:
		return "The specified encoder is not recognized (unknown encoder name)"
	default:
		return "Unknown error"
	}
}

// IsTerminal indicates whether this error should terminate the operation.
func (e ErrorCode) IsTerminal() bool {
	switch e {
	case ErrFormatNotMatch, ErrNoSuitableEncoder, ErrBlacklistedEncoder, ErrEncoderUnsupported:
		return true
	default:
		return false
	}
}
