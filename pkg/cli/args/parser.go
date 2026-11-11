package args

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/tsic404/rffmpeg/pkg/ffmpegopts"
)

var (
	ErrNoInputFile     = errors.New("no input file specified: pass the input with -i <file>")
	ErrNoOutputFile    = errors.New("no output file specified")
	ErrMultipleOutputs = errors.New("multiple output files are not supported")
)

// takesValue reports whether an option token consumes the following token as
// its value.
//
// Native ffmpeg options are single-dash names, and the arity table lists the
// ones that take no value; a name the table does not list is assumed to take
// one. That default is the safe direction: a value wrongly taken as an output
// candidate would be rejected as a second output, while an unknown name is
// rejected by the native tool either way. A double-dash token is not a native
// option name at all (only the bare "--" separator is), so it never consumes
// the next token — a "--flag value" pair leaves "value" as a positional.
func takesValue(arg string) bool {
	if strings.HasPrefix(arg, "--") {
		return false
	}
	return !ffmpegopts.IsBoolean(arg)
}

// ParseResult contains parsed ffmpeg arguments
type ParseResult struct {
	InputFiles      []string
	OutputFile      string
	StreamingOutput bool     // true if output is "-" (stdout)
	AllArgs         []string // args with <INPUT_FILE> placeholder replacing actual input paths, output path excluded
	RawArgs         []string
}

// Parser parses ffmpeg command-line arguments
type Parser struct{}

// NewParser creates a new argument parser
func NewParser() *Parser {
	return &Parser{}
}

// Parse parses command-line arguments into a ParseResult.
//
// The grammar is the native tool's: ffmpeg [global_options] [-i input]...
// [output_options] output. rffmpeg interprets two things only — the input
// identifier (-i and its value) and the single output target. Every other
// token, including native names whose behaviour the CLI looks at before
// submitting (-y/-n, -f, encoder names, -map), is kept verbatim in AllArgs.
//
// A positional argument is always an output candidate, never an input: only -i
// names the source, so an invocation without one is rejected before submission.
// A bare "--" ends option parsing: later tokens are never interpreted as
// options, and the non-option ones among them are output candidates too.
func (p *Parser) Parse(args []string) (*ParseResult, error) {
	result := &ParseResult{
		RawArgs: args,
	}

	if len(args) == 0 {
		return nil, ErrNoInputFile
	}

	var inputFiles []string
	var allArgs []string
	var outputFile string
	haveOutput := false
	afterSeparator := false

	// A second output candidate outranks nothing: a missing input is the more
	// actionable report, so both rejections share one helper.
	multipleOutputs := func(name string) error {
		if !hasExplicitInput(args) {
			return ErrNoInputFile
		}
		return fmt.Errorf("%w: unexpected output %q", ErrMultipleOutputs, name)
	}

	setOutput := func(tok string) error {
		if haveOutput {
			return multipleOutputs(tok)
		}
		outputFile = tok
		haveOutput = true
		return nil
	}

	for i := 0; i < len(args); i++ {
		arg := args[i]

		switch {
		case !afterSeparator && arg == "--":
			// The native separator: keep it in the job arguments so the worker's
			// ffmpeg sees the same boundary, and stop interpreting options.
			allArgs = append(allArgs, arg)
			afterSeparator = true

		case !afterSeparator && arg == "-i":
			if i+1 >= len(args) {
				return nil, ErrNoInputFile
			}
			inputFiles = append(inputFiles, args[i+1])
			// The Worker replaces the placeholder with the resolved path, so the
			// input reference keeps its position in the argument sequence.
			allArgs = append(allArgs, "-i", "<INPUT_FILE>")
			i++

		case !afterSeparator && strings.HasPrefix(arg, "-") && arg != "-":
			if takesValue(arg) {
				if i+1 >= len(args) {
					return nil, fmt.Errorf("option %s requires a value", arg)
				}
				allArgs = append(allArgs, arg, args[i+1])
				i++
				continue
			}
			allArgs = append(allArgs, arg)

		default:
			// A positional — before or after the separator.
			if err := setOutput(arg); err != nil {
				return nil, err
			}
		}
	}

	if len(inputFiles) == 0 {
		return nil, ErrNoInputFile
	}
	if outputFile == "" {
		return nil, ErrNoOutputFile
	}
	// An output that names the pipe protocol without an fd, or with fd 1, is
	// ffmpeg's stdout target — measured on the bundled tool: `-f mpegts pipe:`
	// and `-f mpegts pipe:1` both write their bytes to the process stdout and
	// create no such file. rffmpeg carries streaming output as the bare "-"
	// (C-7), so normalize every stdout alias here; otherwise the token becomes
	// a literal output filename, the worker's ffmpeg writes the stream to its
	// own stdout (never captured), and the job yields a 0-byte output the
	// downloader reports as "moov atom not found". `pipe:0` names fd 0 and
	// `pipe:2` names stderr, so neither is a stdout target and both stay
	// verbatim. Matching is case-insensitive, as the pre-existing handling was.
	if IsStdoutPipeAlias(outputFile) {
		outputFile = "-"
	}

	result.InputFiles = inputFiles
	result.OutputFile = outputFile
	result.StreamingOutput = outputFile == "-"
	result.AllArgs = allArgs

	return result, nil
}

// IsStdoutPipeAlias reports whether a token names the process stdout through
// ffmpeg's pipe protocol: the bare "pipe:" (fd 1 is the default for an output)
// or "pipe:1". "-" is the same target and is handled by callers alongside this.
func IsStdoutPipeAlias(tok string) bool {
	return strings.EqualFold(tok, "pipe:") || strings.EqualFold(tok, "pipe:1")
}

// hasExplicitInput reports whether args name an input with -i before any "--"
// separator. A post-separator -i is a positional token, not an input.
func hasExplicitInput(args []string) bool {
	for _, arg := range args {
		if arg == "--" {
			return false
		}
		if arg == "-i" {
			return true
		}
	}
	return false
}

// StripFileScheme removes the "file://" prefix from a URI and returns the raw path.
// Returns (path, true) if the input had a file:// scheme, (input, false) otherwise.
func StripFileScheme(uri string) (string, bool) {
	if strings.HasPrefix(uri, "file://") {
		return strings.TrimPrefix(uri, "file://"), true
	}
	return uri, false
}

// GetAbsPath returns absolute path for a file.
// If the path has a "file://" prefix, it is stripped before resolution.
func GetAbsPath(path string) (string, error) {
	// Strip file:// scheme before resolving
	path, _ = StripFileScheme(path)

	if filepath.IsAbs(path) {
		return path, nil
	}
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	return filepath.Abs(filepath.Join(cwd, path))
}

// ValidateFileExists checks if a file exists at the given path.
// If the path has a "file://" prefix, it is stripped before checking.
// It returns an error if the file does not exist or is a directory.
func ValidateFileExists(path string) error {
	path, _ = StripFileScheme(path)

	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("file not found: %s", path)
		}
		return fmt.Errorf("cannot access file %s: %w", path, err)
	}
	if info.IsDir() {
		return fmt.Errorf("expected a file but got a directory: %s", path)
	}
	return nil
}

// IsFfmpegCommand checks if args indicate this is an ffmpeg command
func IsFfmpegCommand(args []string) bool {
	if len(args) == 0 {
		return false
	}
	// First arg might be program name
	cmd := args[0]
	if strings.Contains(cmd, "ffmpeg") || strings.Contains(cmd, "rffmpeg") {
		return true
	}
	return false
}
