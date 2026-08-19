package inputsource

// Scheme represents the URI scheme for input sources
type Scheme string

const (
	SchemeFile  Scheme = "file"
	SchemeHTTP  Scheme = "http"
	SchemeHTTPS Scheme = "https"
	SchemeS3    Scheme = "s3"
)

// AuthType represents the authentication type for input sources
type AuthType string

const (
	AuthTypeNone   AuthType = "none"
	AuthTypeBasic  AuthType = "basic"
	AuthTypeBearer AuthType = "bearer"
	AuthTypeS3     AuthType = "s3"
)

// InputSource represents an abstract input source with URI scheme support.
// It enables Workers to fetch inputs from various sources (local files, HTTP, S3)
// based on the URI scheme.
type InputSource struct {
	// Scheme is the URI scheme (file, http, https, s3)
	Scheme Scheme `json:"scheme"`

	// URI is the complete URI string (file://, http://, https://, s3://)
	URI string `json:"uri"`

	// Metadata holds additional information about the input source
	// Reserved for future iterations
	Metadata map[string]any `json:"metadata,omitempty"`

	// AuthType specifies the authentication method required for this input
	// Reserved for future iterations (depends on TSI-710)
	AuthType AuthType `json:"auth_type,omitempty"`
}
