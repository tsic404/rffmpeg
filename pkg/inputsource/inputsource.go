package inputsource

import (
	"errors"
	"net/url"
	"strings"
)

// String returns the string representation of a Scheme
func (s Scheme) String() string {
	return string(s)
}

// String returns the string representation of an AuthType
func (a AuthType) String() string {
	return string(a)
}

// ParseURI parses a URI string and returns an InputSource.
// The URI must have a valid scheme (file, http, https, s3).
func ParseURI(uri string) (InputSource, error) {
	if uri == "" {
		return InputSource{}, errors.New("empty URI")
	}

	// Handle file paths without explicit file:// prefix
	if !strings.Contains(uri, "://") {
		// Treat as local file path
		return InputSource{
			Scheme:   SchemeFile,
			URI:      "file://" + uri,
			AuthType: AuthTypeNone,
		}, nil
	}

	parsed, err := url.Parse(uri)
	if err != nil {
		return InputSource{}, err
	}

	scheme := Scheme(strings.ToLower(parsed.Scheme))
	switch scheme {
	case SchemeFile, SchemeHTTP, SchemeHTTPS, SchemeS3:
		return InputSource{
			Scheme:   scheme,
			URI:      uri,
			AuthType: AuthTypeNone,
		}, nil
	default:
		return InputSource{}, errors.New("unsupported scheme: " + parsed.Scheme)
	}
}

// NewFileInput creates an InputSource for a local file path.
func NewFileInput(filepath string) InputSource {
	// Normalize the URI format
	uri := filepath
	if !strings.HasPrefix(uri, "file://") {
		uri = "file://" + filepath
	}
	return InputSource{
		Scheme:   SchemeFile,
		URI:      uri,
		AuthType: AuthTypeNone,
	}
}

// NewHTTPInput creates an InputSource for an HTTP URL.
func NewHTTPInput(url string) InputSource {
	return InputSource{
		Scheme:   SchemeHTTP,
		URI:      url,
		AuthType: AuthTypeNone,
	}
}

// NewHTTPSInput creates an InputSource for an HTTPS URL.
func NewHTTPSInput(url string) InputSource {
	return InputSource{
		Scheme:   SchemeHTTPS,
		URI:      url,
		AuthType: AuthTypeNone,
	}
}

// NewS3Input creates an InputSource for an S3 URI.
func NewS3Input(uri string) InputSource {
	return InputSource{
		Scheme:   SchemeS3,
		URI:      uri,
		AuthType: AuthTypeS3,
	}
}

// Validate checks if the InputSource has valid fields.
func (is InputSource) Validate() error {
	if is.URI == "" {
		return errors.New("empty URI")
	}

	switch is.Scheme {
	case SchemeFile, SchemeHTTP, SchemeHTTPS, SchemeS3:
		// Valid scheme
	default:
		return errors.New("invalid scheme: " + string(is.Scheme))
	}

	return nil
}

// IsLocal returns true if the input source is a local file.
func (is InputSource) IsLocal() bool {
	return is.Scheme == SchemeFile
}

// IsRemote returns true if the input source requires network access.
func (is InputSource) IsRemote() bool {
	return is.Scheme == SchemeHTTP || is.Scheme == SchemeHTTPS || is.Scheme == SchemeS3
}

// GetFilePath extracts the file path from a file:// URI.
// Returns an error if the scheme is not file.
func (is InputSource) GetFilePath() (string, error) {
	if is.Scheme != SchemeFile {
		return "", errors.New("not a file scheme")
	}

	// Remove file:// prefix
	path := strings.TrimPrefix(is.URI, "file://")
	return path, nil
}
