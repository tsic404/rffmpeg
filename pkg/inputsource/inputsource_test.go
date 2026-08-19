package inputsource

import (
	"testing"
)

func TestSchemeString(t *testing.T) {
	tests := []struct {
		scheme   Scheme
		expected string
	}{
		{SchemeFile, "file"},
		{SchemeHTTP, "http"},
		{SchemeHTTPS, "https"},
		{SchemeS3, "s3"},
	}

	for _, tt := range tests {
		t.Run(tt.expected, func(t *testing.T) {
			if got := tt.scheme.String(); got != tt.expected {
				t.Errorf("Scheme.String() = %v, want %v", got, tt.expected)
			}
		})
	}
}

func TestAuthTypeString(t *testing.T) {
	tests := []struct {
		authType AuthType
		expected string
	}{
		{AuthTypeNone, "none"},
		{AuthTypeBasic, "basic"},
		{AuthTypeBearer, "bearer"},
		{AuthTypeS3, "s3"},
	}

	for _, tt := range tests {
		t.Run(tt.expected, func(t *testing.T) {
			if got := tt.authType.String(); got != tt.expected {
				t.Errorf("AuthType.String() = %v, want %v", got, tt.expected)
			}
		})
	}
}

func TestParseURI(t *testing.T) {
	tests := []struct {
		name       string
		uri        string
		wantScheme Scheme
		wantURI    string
		wantErr    bool
	}{
		{
			name:       "file path without scheme",
			uri:        "/path/to/file.mp4",
			wantScheme: SchemeFile,
			wantURI:    "file:///path/to/file.mp4",
		},
		{
			name:       "file:// URI",
			uri:        "file:///path/to/file.mp4",
			wantScheme: SchemeFile,
			wantURI:    "file:///path/to/file.mp4",
		},
		{
			name:       "http:// URI",
			uri:        "http://example.com/video.mp4",
			wantScheme: SchemeHTTP,
			wantURI:    "http://example.com/video.mp4",
		},
		{
			name:       "https:// URI",
			uri:        "https://example.com/video.mp4",
			wantScheme: SchemeHTTPS,
			wantURI:    "https://example.com/video.mp4",
		},
		{
			name:       "s3:// URI",
			uri:        "s3://bucket/key/video.mp4",
			wantScheme: SchemeS3,
			wantURI:    "s3://bucket/key/video.mp4",
		},
		{
			name:    "empty URI",
			uri:     "",
			wantErr: true,
		},
		{
			name:    "unsupported scheme",
			uri:     "ftp://example.com/file.mp4",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseURI(tt.uri)
			if (err != nil) != tt.wantErr {
				t.Errorf("ParseURI() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr {
				if got.Scheme != tt.wantScheme {
					t.Errorf("ParseURI().Scheme = %v, want %v", got.Scheme, tt.wantScheme)
				}
				if got.URI != tt.wantURI {
					t.Errorf("ParseURI().URI = %v, want %v", got.URI, tt.wantURI)
				}
			}
		})
	}
}

func TestNewFileInput(t *testing.T) {
	is := NewFileInput("/path/to/file.mp4")
	if is.Scheme != SchemeFile {
		t.Errorf("NewFileInput().Scheme = %v, want %v", is.Scheme, SchemeFile)
	}
	if is.URI != "file:///path/to/file.mp4" {
		t.Errorf("NewFileInput().URI = %v, want %v", is.URI, "file:///path/to/file.mp4")
	}
	if is.AuthType != AuthTypeNone {
		t.Errorf("NewFileInput().AuthType = %v, want %v", is.AuthType, AuthTypeNone)
	}
}

func TestNewHTTPInput(t *testing.T) {
	is := NewHTTPInput("http://example.com/video.mp4")
	if is.Scheme != SchemeHTTP {
		t.Errorf("NewHTTPInput().Scheme = %v, want %v", is.Scheme, SchemeHTTP)
	}
}

func TestNewHTTPSInput(t *testing.T) {
	is := NewHTTPSInput("https://example.com/video.mp4")
	if is.Scheme != SchemeHTTPS {
		t.Errorf("NewHTTPSInput().Scheme = %v, want %v", is.Scheme, SchemeHTTPS)
	}
}

func TestNewS3Input(t *testing.T) {
	is := NewS3Input("s3://bucket/key/video.mp4")
	if is.Scheme != SchemeS3 {
		t.Errorf("NewS3Input().Scheme = %v, want %v", is.Scheme, SchemeS3)
	}
	if is.AuthType != AuthTypeS3 {
		t.Errorf("NewS3Input().AuthType = %v, want %v", is.AuthType, AuthTypeS3)
	}
}

func TestInputSourceValidate(t *testing.T) {
	tests := []struct {
		name    string
		is      InputSource
		wantErr bool
	}{
		{
			name: "valid file input",
			is:   NewFileInput("/path/to/file.mp4"),
		},
		{
			name: "valid HTTP input",
			is:   NewHTTPInput("http://example.com/video.mp4"),
		},
		{
			name: "valid S3 input",
			is:   NewS3Input("s3://bucket/key/video.mp4"),
		},
		{
			name:    "empty URI",
			is:      InputSource{Scheme: SchemeFile, URI: ""},
			wantErr: true,
		},
		{
			name:    "invalid scheme",
			is:      InputSource{Scheme: Scheme("ftp"), URI: "ftp://example.com/file"},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.is.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("InputSource.Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestInputSourceIsLocal(t *testing.T) {
	if !NewFileInput("/path").IsLocal() {
		t.Error("File input should be local")
	}
	if NewHTTPInput("http://example.com").IsLocal() {
		t.Error("HTTP input should not be local")
	}
	if NewS3Input("s3://bucket/key").IsLocal() {
		t.Error("S3 input should not be local")
	}
}

func TestInputSourceIsRemote(t *testing.T) {
	if NewFileInput("/path").IsRemote() {
		t.Error("File input should not be remote")
	}
	if !NewHTTPInput("http://example.com").IsRemote() {
		t.Error("HTTP input should be remote")
	}
	if !NewHTTPSInput("https://example.com").IsRemote() {
		t.Error("HTTPS input should be remote")
	}
	if !NewS3Input("s3://bucket/key").IsRemote() {
		t.Error("S3 input should be remote")
	}
}

func TestInputSourceGetFilePath(t *testing.T) {
	is := NewFileInput("/path/to/file.mp4")
	path, err := is.GetFilePath()
	if err != nil {
		t.Errorf("GetFilePath() error = %v", err)
	}
	if path != "/path/to/file.mp4" {
		t.Errorf("GetFilePath() = %v, want %v", path, "/path/to/file.mp4")
	}

	// Test non-file scheme
	httpInput := NewHTTPInput("http://example.com/video.mp4")
	_, err = httpInput.GetFilePath()
	if err == nil {
		t.Error("GetFilePath() should error for non-file scheme")
	}
}
