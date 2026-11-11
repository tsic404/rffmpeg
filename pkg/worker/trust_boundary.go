package worker

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/tsic404/rffmpeg/pkg/addrpolicy"
	"github.com/tsic404/rffmpeg/pkg/pathutil"
	"github.com/tsic404/rffmpeg/pkg/protocol"
)

// boundaryFailure is an execution-side trust-boundary violation: the job fails
// with INFRA, ffmpeg never starts, and nothing outside the workspace is
// created, overwritten or removed (C-124 / C-126 / C-127).
type boundaryFailure struct {
	msg     string
	details string
}

// pathSpace returns the declared shared filesystem space, never nil so callers
// can ask for membership unconditionally. An undeclared space has no legal
// path (ADR-018 option A).
func (w *Worker) pathSpace() *pathutil.Space {
	if w.sharedFS == nil {
		return pathutil.NewSpace("")
	}
	return w.sharedFS
}

// jobInput is one planned input of a job: its classification form plus the
// identifier itself.
type jobInput struct {
	form       pathutil.InputForm
	identifier string
}

// planJobInputs classifies every input identifier of a job (C-123 / C-124).
// Classification is the only basis on which the execution side obtains inputs,
// so it runs before the job workspace exists: an identifier that matches no
// form must not leave a single file behind. A direct-mode job takes its inputs
// from the pass-through field (C-125), and each of those must classify as a
// shared filesystem path as well.
func (w *Worker) planJobInputs(job protocol.JobInfo, directMode bool) ([]jobInput, *boundaryFailure) {
	space := w.pathSpace()
	planned := make([]jobInput, 0, len(job.InputFiles))
	for _, identifier := range job.InputFiles {
		form, err := pathutil.ClassifyInput(identifier, space)
		if err != nil {
			return nil, &boundaryFailure{
				msg:     "input identifier cannot be classified: " + identifier,
				details: err.Error(),
			}
		}
		if directMode && form != pathutil.InputSharedPath {
			return nil, &boundaryFailure{
				msg: fmt.Sprintf("a pass-through job accepts only shared filesystem paths as inputs: %q is a %s",
					identifier, form),
				details: "input form does not match the pass-through mode",
			}
		}
		planned = append(planned, jobInput{form: form, identifier: identifier})
	}

	if directMode {
		// C-125: the pass-through field is the input list of a direct-mode job.
		planned = planned[:0]
		for _, directPath := range job.DirectPaths {
			if err := pathutil.ValidateSharedPath(space, directPath); err != nil {
				return nil, &boundaryFailure{
					msg:     "pass-through input is outside the declared shared filesystem space",
					details: err.Error(),
				}
			}
			planned = append(planned, jobInput{form: pathutil.InputSharedPath, identifier: directPath})
		}
	}
	return planned, nil
}

// jobOutput is the judged output target of a job: the class it belongs to and
// the value ffmpeg receives. A protocol address target is handed to ffmpeg
// verbatim; every other target is a local path (C-125).
type jobOutput struct {
	target string
	form   pathutil.OutputTargetForm
}

// planJobOutput judges the output target before anything is written (C-125 /
// C-129). It returns the single file name to materialise inside the workspace,
// the absolute target of a pass-through job, the stdout pipe, or the protocol
// address a network output writes to. Rules that only the execution side can
// judge — the resolved pass-through target landing outside the declared space,
// or the resolved push target landing in a denied address class — fail the job
// here instead of connecting or writing anywhere.
func (w *Worker) planJobOutput(ctx context.Context, job protocol.JobInfo, directMode bool) (jobOutput, *boundaryFailure) {
	if job.StreamingOutput {
		// The target is the stdout pipe, not a filesystem path (C-7).
		return jobOutput{target: "-", form: pathutil.OutputTargetFile}, nil
	}
	name := job.OutputFilename
	if directMode {
		if v := w.validateDirectOutputPath(name); v != nil {
			return jobOutput{}, &boundaryFailure{msg: v.msg, details: v.details}
		}
		return jobOutput{target: name, form: pathutil.OutputTargetFile}, nil
	}
	if name == "" {
		// Legacy rows created before the output-name rule existed: the default
		// is a workspace-internal name, so it cannot point outside the job.
		name = "output"
	}
	form, err := pathutil.ClassifyOutputTarget(name, false, w.pathSpace())
	if err != nil {
		return jobOutput{}, &boundaryFailure{
			msg:     "output name is neither a single file name nor a protocol address: " + name,
			details: err.Error(),
		}
	}
	if form == pathutil.OutputTargetProtocolAddress {
		// C-129: the whole target is judged by its resolved address before
		// ffmpeg starts, so a denied class never receives a connection. A
		// target whose address cannot be determined is refused as well
		// (fail-closed): the judgement would be on an address nobody vouched
		// for, and ffmpeg would connect with a resolver this side never saw.
		if judgeErr := addrpolicy.CheckOutputTarget(ctx, name); judgeErr != nil {
			return jobOutput{}, &boundaryFailure{
				msg:     "output target address is not permitted: " + name,
				details: judgeErr.Error(),
			}
		}
	}
	return jobOutput{target: name, form: form}, nil
}

// inputLandingPath returns the workspace path a downloaded input is written to.
// The name is derived from the input's position, never from the identifier text
// (C-126), and the result is verified to stay inside the workspace.
func inputLandingPath(jobDir string, index int) (string, bool) {
	landing := filepath.Join(jobDir, fmt.Sprintf("input-%d", index))
	return landing, pathutil.Within(jobDir, landing)
}
