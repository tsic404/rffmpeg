package pathutil

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// uploadIDPattern is the identifier shape the server issues for an uploaded
// input: the lowercase hexadecimal content hash. Both the submission side and
// the execution side classify with this single definition, so a string can
// never be a file identifier on one side and a filesystem path on the other.
var uploadIDPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// IsUploadID reports whether s is a server-issued upload identifier.
func IsUploadID(s string) bool {
	return uploadIDPattern.MatchString(s)
}

// IsHTTPRemoteURL reports whether s is, as a whole string, an absolute http or
// https URL carrying a host name. Other schemes are not input identifiers at
// all: the input taxonomy admits only http/https URLs, and treating e.g.
// "rtmp://" as a remote input would hand the worker an unprejudiced network
// reach (C-123).
func IsHTTPRemoteURL(s string) bool {
	if s == "" || strings.TrimSpace(s) != s {
		return false
	}
	u, err := url.Parse(s)
	if err != nil || !u.IsAbs() {
		return false
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
	default:
		return false
	}
	return u.Host != "" && u.Hostname() != ""
}

// InputForm is the shape of one input identifier (C-123).
type InputForm int

const (
	// InputInvalid marks a string that matches no form, matches more than one,
	// or names a path outside every declared root.
	InputInvalid InputForm = iota
	// InputUpload is a server-issued upload identifier.
	InputUpload
	// InputRemoteURL is an absolute http/https URL with a host.
	InputRemoteURL
	// InputSharedPath is an absolute path inside the declared shared-FS space.
	InputSharedPath
)

// String returns the form name used in error messages and logs.
func (f InputForm) String() string {
	switch f {
	case InputUpload:
		return "upload identifier"
	case InputRemoteURL:
		return "remote URL"
	case InputSharedPath:
		return "shared filesystem path"
	default:
		return "unclassified identifier"
	}
}

// ClassifyInput classifies one input identifier by its whole string (C-123).
// The three forms are mutually exclusive; a string that matches none, or more
// than one, is InputInvalid. Classification is the only basis on which the
// execution side may obtain an input (C-124), so both sides call this function
// with their own copy of the same space declaration.
func ClassifyInput(identifier string, space *Space) (InputForm, error) {
	matches := 0
	form := InputInvalid
	if IsUploadID(identifier) {
		matches++
		form = InputUpload
	}
	if IsHTTPRemoteURL(identifier) {
		matches++
		form = InputRemoteURL
	}
	if space != nil && space.Contains(identifier) {
		matches++
		form = InputSharedPath
	}
	switch {
	case matches == 1:
		return form, nil
	case matches > 1:
		return InputInvalid, fmt.Errorf(
			"input identifier matches more than one form: %q", identifier)
	default:
		return InputInvalid, fmt.Errorf(
			"input identifier is neither an upload identifier, an http/https URL, nor an absolute path in the declared shared filesystem space: %q",
			identifier)
	}
}

// ValidateSharedPath validates a whole-string path against the declared space:
// absolute, no ".." component, member of a declared root under the C-107
// membership judgement. It covers the C-123 third form and the C-125
// pass-through input and target fields.
func ValidateSharedPath(space *Space, p string) error {
	if p == "" {
		return fmt.Errorf("path is empty")
	}
	if !filepath.IsAbs(p) {
		return fmt.Errorf("path must be absolute: %q", p)
	}
	if ContainsPathTraversal(p) {
		return fmt.Errorf("path must not contain a '..' component: %q", p)
	}
	if !space.Declared() {
		return fmt.Errorf("shared filesystem mode is not enabled: no root is declared")
	}
	if !space.Contains(p) {
		return fmt.Errorf("path is outside the declared shared filesystem space: %q", p)
	}
	return nil
}

// OutputTargetForm is the shape of a job's output target (C-125).
type OutputTargetForm int

const (
	// OutputTargetInvalid marks a target that belongs to neither class.
	OutputTargetInvalid OutputTargetForm = iota
	// OutputTargetFile is a local file target: a single file name inside the
	// job workspace, or the absolute path of a pass-through job.
	OutputTargetFile
	// OutputTargetProtocolAddress is an absolute protocol address with a host:
	// the job writes to that address and nothing lands in its workspace.
	OutputTargetProtocolAddress
)

// String returns the form name used in error messages and logs.
func (f OutputTargetForm) String() string {
	switch f {
	case OutputTargetFile:
		return "file target"
	case OutputTargetProtocolAddress:
		return "protocol address target"
	default:
		return "unclassified output target"
	}
}

// protocolAddressHost returns the host of s when the whole string is an
// absolute protocol address: an RFC 3986 scheme followed by "://" and a host
// name. The "file" scheme addresses a local path rather than a network
// endpoint, so it is not a protocol address target (C-125).
func protocolAddressHost(s string) (string, bool) {
	if s == "" || strings.TrimSpace(s) != s || strings.ContainsRune(s, 0) {
		return "", false
	}
	if !remoteURLPattern.MatchString(s) {
		return "", false
	}
	u, err := url.Parse(s)
	if err != nil || !u.IsAbs() || strings.EqualFold(u.Scheme, "file") {
		return "", false
	}
	host := u.Hostname()
	if host == "" {
		return "", false
	}
	return host, true
}

// ClassifyOutputTarget classifies the output target of a submission by its
// whole string (C-125). Direct mode accepts only an absolute path without a
// ".." component that lies inside the declared space. Any other mode accepts
// exactly one of two mutually exclusive classes: a single file name, or a
// protocol address target (an absolute protocol address with a host, the file
// scheme excluded). The empty string, an absolute path, a directory separator
// and a ".." component are rejected as file targets. The returned error is
// user-facing.
func ClassifyOutputTarget(name string, directMode bool, space *Space) (OutputTargetForm, error) {
	if directMode {
		if err := ValidateSharedPath(space, name); err != nil {
			return OutputTargetInvalid, err
		}
		return OutputTargetFile, nil
	}
	if _, ok := protocolAddressHost(name); ok {
		return OutputTargetProtocolAddress, nil
	}
	if name == "" {
		return OutputTargetInvalid, fmt.Errorf("output name must not be empty")
	}
	if filepath.IsAbs(name) || strings.HasPrefix(name, string(os.PathSeparator)) {
		return OutputTargetInvalid, fmt.Errorf("output name must not be an absolute path: %q", name)
	}
	if strings.ContainsAny(name, `/\`) {
		return OutputTargetInvalid, fmt.Errorf("output name must not contain a directory separator: %q", name)
	}
	if name == "." || name == ".." || ContainsPathTraversal(name) {
		return OutputTargetInvalid, fmt.Errorf("output name must not contain a '..' component: %q", name)
	}
	if strings.ContainsRune(name, 0) {
		return OutputTargetInvalid, fmt.Errorf("output name contains a NUL byte")
	}
	return OutputTargetFile, nil
}
