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
	ErrNoInputFile  = errors.New("no input file specified")
	ErrNoOutputFile = errors.New("no output file specified")
)

// isBooleanFlag checks if an option is a boolean flag (doesn't take a value),
// using the generated FFmpeg arity table in pkg/ffmpegopts.
func isBooleanFlag(arg string) bool {
	return ffmpegopts.IsBoolean(arg)
}

// isKnownOption reports whether arg names a known FFmpeg option (boolean or
// value-taking), using the generated arity table in pkg/ffmpegopts. It
// disambiguates a real "-i"-prefixed option such as "-itsoffset" from a
// concatenated "-i<input>" input path (TSI-2907).
func isKnownOption(arg string) bool {
	return ffmpegopts.IsKnown(arg)
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

// Parse parses command-line arguments into a ParseResult
// ffmpeg syntax: ffmpeg [global_options] [-i input_file]... [output_options] output_file
func (p *Parser) Parse(args []string) (*ParseResult, error) {
	result := &ParseResult{
		RawArgs: args,
	}

	if len(args) == 0 {
		return nil, ErrNoInputFile
	}

	// Track state
	var inputFiles []string
	var outputFile string
	var allArgs []string

	// Find the last argument that looks like an output file (typically the
	// last non-option argument, or an explicit -o value).
	outputCandidateIdx := findOutputCandidate(args)

	// Parse arguments
	i := 0
	for i < len(args) {
		arg := args[i]

		// Handle -i option for input files
		if arg == "-i" || arg == "--input" {
			if i+1 >= len(args) {
				return nil, ErrNoInputFile
			}
			inputFile := args[i+1]
			inputFiles = append(inputFiles, inputFile)
			// Add -i <INPUT_FILE> to allArgs - Worker will replace <INPUT_FILE> with actual path
			allArgs = append(allArgs, "-i", "<INPUT_FILE>")
			i += 2
			continue
		}

		// Handle concatenated -i option: -iinputfile. A known "-i"-prefixed
		// option such as "-itsoffset" is NOT an inline input; it falls through
		// to the generic option handling below (TSI-2907).
		if strings.HasPrefix(arg, "-i") && len(arg) > 2 && !isKnownOption(arg) {
			inputFile := arg[2:]
			inputFiles = append(inputFiles, inputFile)
			// Add -i <INPUT_FILE> to allArgs - Worker will replace <INPUT_FILE> with actual path
			allArgs = append(allArgs, "-i", "<INPUT_FILE>")
			i++
			continue
		}

		// Handle -o option for explicit output file specification (e.g. -o - for streaming output)
		if arg == "-o" {
			if i+1 >= len(args) {
				return nil, ErrNoOutputFile
			}
			outputFile = args[i+1]
			i += 2
			continue
		}

		// Check if this is the output file
		// "-" is stdout (streaming output) — treat as output file, not a flag
		if outputCandidateIdx >= 0 && i == outputCandidateIdx && (!strings.HasPrefix(arg, "-") || arg == "-") {
			outputFile = arg
			i++
			continue
		}
		// Handle other options
		if strings.HasPrefix(arg, "-") && arg != "-" {
			// Boolean flag: takes no value; leave the following argument
			// (often the output file) untouched.
			if isBooleanFlag(arg) {
				allArgs = append(allArgs, arg)
				i++
				continue
			}
			// Value-taking option: consume the next token unconditionally,
			// matching ffmpeg — a value may itself begin with "-" (e.g.
			// "-map -1", "-ss -10", "-itsoffset -5"). Only a missing next
			// token is a hard error, so "-c:v" with no codec reports the
			// real problem instead of a misleading
			// "no input/output file specified" (TSI-2907).
			if i+1 >= len(args) {
				return nil, fmt.Errorf("option %s requires a value", arg)
			}
			allArgs = append(allArgs, arg, args[i+1])
			i += 2
			continue
		}

		// Non-option argument (could be input or output)
		if len(inputFiles) == 0 {
			// This might be an input file without -i (some ffmpeg versions allow this)
			inputFiles = append(inputFiles, arg)
			// Rewrite the implicit input into the same -i <INPUT_FILE> form the
			// worker substitutes, so omitting -i still yields a real input stream.
			allArgs = append(allArgs, "-i", "<INPUT_FILE>")
		} else if outputFile == "" && i == outputCandidateIdx {
			outputFile = arg
		}
		i++
	}

	// Validate
	if len(inputFiles) == 0 {
		return nil, ErrNoInputFile
	}
	if outputFile == "" {
		return nil, ErrNoOutputFile
	}
	// ffmpeg treats "-" as shorthand for "pipe:1" (stdout) on output. rffmpeg
	// only recognizes "-" as a streaming job, so normalize "pipe:1" here:
	// otherwise it is treated as a literal output filename, the worker's
	// ffmpeg writes the stream to its own stdout (never captured), and the
	// job yields a 0-byte output the downloader reports as "moov atom not
	// found" (TSI-3038). The comparison is case-insensitive because ffmpeg
	// protocol names are matched without regard to case ("PIPE:1" == "pipe:1").
	if strings.EqualFold(outputFile, "pipe:1") {
		outputFile = "-"
	}

	// Detect streaming output mode (output is "-")
	streamingOutput := outputFile == "-"

	result.InputFiles = inputFiles
	result.OutputFile = outputFile
	result.StreamingOutput = streamingOutput
	result.AllArgs = allArgs

	return result, nil
}

// findOutputCandidate returns the index of the last positional argument,
// which ffmpeg treats as the output file. It scans left-to-right so a
// value-taking option consumes its next token even when that value begins
// with "-" (e.g. "-map -1"); a right-to-left scan mistakes "-1" for an
// option and skips the real output file (TSI-2907).
func findOutputCandidate(args []string) int {
	candidate := -1
	for i := 0; i < len(args); {
		arg := args[i]
		switch {
		case arg == "-i" || arg == "--input":
			i += 2 // input flag and its value
		case strings.HasPrefix(arg, "-i") && len(arg) > 2 && !isKnownOption(arg):
			i++ // -iinput inline form
		case arg == "-o":
			i += 2 // explicit output flag and its value
		case strings.HasPrefix(arg, "-") && arg != "-":
			if isBooleanFlag(arg) {
				i++
			} else {
				i += 2 // value-taking option and its value
			}
		default:
			candidate = i // positional argument
			i++
		}
	}
	return candidate
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
