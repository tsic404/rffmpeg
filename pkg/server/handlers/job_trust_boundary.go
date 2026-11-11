package handlers

import (
	"fmt"
	"strings"

	"github.com/tsic404/rffmpeg/pkg/addrpolicy"
	"github.com/tsic404/rffmpeg/pkg/jobargs"
	"github.com/tsic404/rffmpeg/pkg/pathutil"
	"github.com/tsic404/rffmpeg/pkg/protocol"
)

// validateSubmitRequest enforces the job request trust boundary on the
// submission side: every input identifier is classified by its whole string
// (C-123), the pass-through fields and the output target are judged against the
// declared path space (C-125), and arguments that can only reach resources
// outside the manifest are rejected before a job row exists (C-127).
//
// It returns the user-facing rejection, or nil when the request may be
// persisted.
func (h *Handler) validateSubmitRequest(req *protocol.JobSubmitRequest) *protocol.ProtocolError {
	directMode := len(req.DirectPath) > 0
	space := h.sharedFSSpace()

	for _, identifier := range req.InputFiles {
		form, err := pathutil.ClassifyInput(identifier, space)
		if err != nil {
			return invalidRequestError(err.Error())
		}
		if directMode && form != pathutil.InputSharedPath {
			return invalidRequestError(fmt.Sprintf(
				"a pass-through request accepts only shared filesystem absolute paths as inputs: %q is a %s",
				identifier, form))
		}
		if form == pathutil.InputUpload {
			exists, err := h.storage.TouchFile(identifier)
			if err != nil || !exists {
				return protocol.NewProtocolError(
					protocol.ErrCodeNotFound, "Input file not found: "+identifier, err,
				)
			}
		}
	}

	if directMode {
		for _, path := range req.DirectPath {
			if err := pathutil.ValidateSharedPath(space, path); err != nil {
				return invalidRequestError(err.Error())
			}
		}
	}

	switch {
	case req.StreamingOutput:
		// The output target is stdout: C-7 normalizes a pipe target to the bare
		// pipe, so the name field carries no filesystem target and its shape
		// rules do not apply. Only the two pipe spellings are accepted, so a
		// streaming request can never smuggle a file path in this field.
		if req.OutputFilename != "" && req.OutputFilename != "-" &&
			!strings.EqualFold(req.OutputFilename, "pipe:1") {
			return invalidRequestError(fmt.Sprintf(
				"a streaming request must not name an output file: %q", req.OutputFilename))
		}
	default:
		form, err := pathutil.ClassifyOutputTarget(req.OutputFilename, directMode, space)
		if err != nil {
			return invalidRequestError(err.Error())
		}
		if form == pathutil.OutputTargetProtocolAddress {
			// C-129: a literal address is judged here; a host name is left to
			// the execution side, which resolves it before ffmpeg starts.
			if err := addrpolicy.CheckOutputLiteral(req.OutputFilename); err != nil {
				return invalidRequestError(err.Error())
			}
		}
	}

	if violation := jobargs.Check(req.Args, jobargs.Policy{InputCount: len(req.InputFiles)}); violation != nil {
		return invalidRequestError(violation.Error())
	}

	return nil
}

// invalidRequestError builds the protocol-consistent 400 for a trust-boundary
// rejection.
func invalidRequestError(message string) *protocol.ProtocolError {
	return protocol.NewProtocolError(protocol.ErrCodeInvalidRequest, message, nil)
}
