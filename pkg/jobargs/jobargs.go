// Package jobargs validates submitted ffmpeg arguments against the job trust
// boundary (C-126 / C-127): the only inputs a job may reach are the ones
// substituted from its input list, and the only local filesystem paths it may
// touch are inside its workspace (plus the pass-through target). The submission
// side and the execution side call the same function so the two judgements
// cannot drift apart.
//
// The judgement is structural: an argument is read with the option arity and
// the sub-language each option takes (filter graph, tee slave list), so a path
// hidden inside a composite value is judged as the path it is. Matching shapes
// inside a token cannot cover these positions.
package jobargs

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/tsic404/rffmpeg/pkg/ffmpegopts"
	"github.com/tsic404/rffmpeg/pkg/pathutil"
)

// InputPlaceholder is the token the CLI leaves in the argument list for every
// manifest input; the worker replaces it with the resolved input path.
const InputPlaceholder = "<INPUT_FILE>"

// Violation describes an argument that reaches a resource outside the job's
// manifest and workspace.
type Violation struct {
	Arg    string
	Reason string
}

func (v *Violation) Error() string {
	return fmt.Sprintf("argument %q %s", v.Arg, v.Reason)
}

// Policy is the set of resources the job is allowed to reach.
type Policy struct {
	// InputCount is the number of manifest inputs available for placeholder
	// substitution. More placeholders than inputs would leave a literal
	// placeholder on ffmpeg's command line.
	InputCount int
	// InputPaths are the resolved absolute input paths (execution side). They
	// are exempt from the workspace rule: a manifest input is read in place.
	InputPaths []string
	// Workspace is the job workspace. Empty on the submission side, where the
	// workspace is unknown and any absolute local path is therefore already a
	// violation.
	Workspace string
	// Target is the pass-through output target of a direct-mode job, the only
	// write outside the workspace the contract permits.
	Target string
}

// deniedOptions name a file the request chose, either to read (scripts,
// attachments) or to write (stats, logs, reports, key material) outside the
// workspace. Their values are never interpreted, so the whole option is
// refused (C-126 / C-127).
var deniedOptions = []string{
	"-filter_script",
	"-filter_complex_script",
	"-attach",
	"-dump_attachment",
	"-passlogfile",
	"-progress",
	"-report",
}

// fileValuedOptions take a path as their value. The value is judged as a path
// (a segment name or a playlist name may be relative, but it must not climb out
// of the workspace and it must not name a protocol address).
var fileValuedOptions = map[string]bool{
	"hls_segment_filename":   true,
	"hls_fmp4_init_filename": true,
	"hls_key_info_file":      true,
	"segment_list":           true,
	"vstats_file":            true,
	"sdp_file":               true,
	"master_pl_name":         true,
	"dash_segment_list":      true,
	"init_seg_name":          true,
	"media_seg_name":         true,
	"hls_subtitle_path":      true,
	"stats_file":             true,
	"mbtree_file":            true,
}

// filterOptions carry a filter graph. Their values are parsed as graphs, not
// searched for shapes: every element name and every parameter inside an element
// is judged on its own.
var filterOptions = map[string]bool{
	"vf":             true,
	"af":             true,
	"filter":         true,
	"filter_complex": true,
	"lavfi":          true,
}

// fileFilterNames open a path of their own, whatever parameters they carry.
// A filter outside this set and outside fileFilterParams cannot reach a file:
// ffmpeg either runs it on the data it already has, or reports an unknown
// filter (the job then fails as FFMPEG_ERROR at ffmpeg itself).
var fileFilterNames = map[string]bool{
	"movie":     true,
	"amovie":    true,
	"subtitles": true,
	"ass":       true,
	"subfile":   true,
	"sendcmd":   true,
	"zmq":       true,
	"zmqsend":   true,
	"ladspa":    true,
	"lv2":       true,
	"frei0r":    true,
	"lut3d":     true,
	"lut":       true,
	"drawtext2": true,
}

// fileFilterParams name a file inside a filter element's argument list. The
// element may be otherwise harmless (drawtext draws text), but these parameters
// make it open whatever path the request chose.
var fileFilterParams = map[string]bool{
	"textfile":   true,
	"fontfile":   true,
	"fontsdir":   true,
	"subfile":    true,
	"filename":   true,
	"file":       true,
	"movie":      true,
	"commands":   true,
	"stats_file": true,
	"statsfile":  true,
	"lut":        true,
	"lut_file":   true,
}

// inputDemuxers are the formats an input may be declared with: media
// containers and elementary streams, whose content is decoded rather than
// re-read as a path list. A demuxer outside this set is refused — concat,
// concatf and image2 would turn the input file into a list of other paths, and
// an unknown name cannot be relied on either.
var inputDemuxers = map[string]bool{
	"mp4": true, "mov": true, "m4a": true, "m4v": true, "3gp": true, "3g2": true,
	"matroska": true, "webm": true, "avi": true, "asf": true, "flv": true, "swf": true,
	"mpegts": true, "mpeg": true, "mpegvideo": true, "mpeg4": true, "h264": true,
	"hevc": true, "vvc": true, "dts": true, "ac3": true, "eac3": true, "truehd": true,
	"mlp": true, "aac": true, "adts": true, "mp3": true, "flac": true, "ogg": true,
	"opus": true, "wav": true, "w64": true, "aiff": true, "au": true, "caf": true,
	"voc": true, "ape": true, "wavpack": true, "alac": true, "spdif": true,
	"mxf": true, "gxf": true, "nut": true, "wtv": true, "ivf": true, "dv": true,
	"rm": true, "yuv4mpegpipe": true, "rawvideo": true, "image2pipe": true,
	"mjpeg": true, "gif": true, "apng": true, "webp": true,
	"srt": true, "ass": true, "webvtt": true, "sup": true,
	"h261": true, "h263": true, "vc1": true, "avs2": true, "cavsvideo": true,
	"s8": true, "u8": true, "s16le": true, "s16be": true, "s24le": true, "s24be": true,
	"s32le": true, "s32be": true, "f32le": true, "f32be": true, "f64le": true, "f64be": true,
	"u16le": true, "u16be": true, "u24le": true, "u24be": true, "u32le": true, "u32be": true,
}

// schemePrefix matches a URL-style token; ffmpeg would treat it as a protocol
// address rather than a local path.
var schemePrefix = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.-]*:`)

// filterNamePattern is the shape of an ffmpeg filter name: letters, digits and
// underscores. Anything else means the element boundary was misread, so the
// whole graph is refused rather than guessed at.
var filterNamePattern = regexp.MustCompile(`^[A-Za-z0-9_]+$`)

// splitFilterName reads one graph name token, which ffmpeg parses as
// "name[@instance]" from a single token and splits at the first "@". The
// instance name is whatever the token kept - "a-b", "a.b", "a:b" and the empty
// string are all accepted by ffmpeg - so it is not constrained to a character
// class. Only the filter name itself is looked up, and only it must match
// filterNamePattern.
//
// A backslash anywhere keeps the token unclassified: ffmpeg unescapes the token
// before the lookup, so "subtit\les" is the "subtitles" filter and must stay
// refused rather than guessed at.
func splitFilterName(token string) (name string, ok bool) {
	if strings.ContainsRune(token, '\\') {
		return "", false
	}
	name, _, _ = strings.Cut(token, "@")
	if !filterNamePattern.MatchString(name) {
		return "", false
	}
	return name, true
}

// Check validates args against the policy and returns the first violation, or
// nil. A nil result means the arguments reach only manifest inputs and the
// workspace.
func Check(args []string, p Policy) *Violation {
	placeholders := 0
	pendingFormat := ""
	for i := 0; i < len(args); i++ {
		arg := args[i]

		if isInputOption(arg) {
			if i+1 >= len(args) {
				return &Violation{Arg: arg, Reason: "is missing its input value"}
			}
			value := args[i+1]
			if value != InputPlaceholder {
				return &Violation{
					Arg:    value,
					Reason: "introduces an input outside the input list; only the manifest placeholder may name an input",
				}
			}
			if pendingFormat != "" {
				if !inputDemuxers[strings.ToLower(pendingFormat)] {
					return &Violation{
						Arg:    pendingFormat,
						Reason: "is not a container format an input may be declared with; a demuxer that re-reads the input file's contents is refused",
					}
				}
				pendingFormat = ""
			}
			placeholders++
			i++
			continue
		}

		// A placeholder is only meaningful as an input value, already consumed
		// above; anywhere else BuildArgs would substitute it positionally.
		if strings.Contains(arg, InputPlaceholder) {
			return &Violation{
				Arg:    arg,
				Reason: "uses the input placeholder outside an input position; only an -i value may name a manifest input",
			}
		}

		if arg == "--" {
			// End-of-options separator: the tokens after it are file
			// positions, judged as such on their own turn.
			continue
		}

		name := optionName(arg)
		inlineValue, hasInlineValue := splitInlineValue(arg)

		if name == "" {
			// A bare file position. It is an output: the pending -f decides
			// whether it carries a tee slave list or is a plain target.
			if strings.EqualFold(pendingFormat, "tee") {
				if reason := teeReason(arg, p); reason != "" {
					return &Violation{Arg: arg, Reason: reason}
				}
			} else if reason := localPathReason(arg, p); reason != "" {
				return &Violation{Arg: arg, Reason: reason}
			}
			pendingFormat = ""
			continue
		}

		if isDeniedOption(arg) {
			return &Violation{Arg: arg, Reason: "is not allowed: it reads or writes a file the request chose"}
		}

		if filterOptions[name] {
			graph := inlineValue
			if !hasInlineValue {
				if i+1 >= len(args) {
					return &Violation{Arg: arg, Reason: "is missing its filter graph"}
				}
				graph = args[i+1]
				i++
			}
			if strings.Contains(graph, InputPlaceholder) {
				return &Violation{
					Arg:    graph,
					Reason: "uses the input placeholder inside a filter graph; only an -i value may name a manifest input",
				}
			}
			if reason := filterGraphReason(graph); reason != "" {
				return &Violation{Arg: graph, Reason: reason}
			}
			continue
		}

		if name == "f" {
			value := inlineValue
			if !hasInlineValue {
				if i+1 >= len(args) {
					return &Violation{Arg: arg, Reason: "is missing its format name"}
				}
				value = args[i+1]
				i++
			}
			pendingFormat = value
			continue
		}

		if fileValuedOptions[name] {
			value := inlineValue
			if !hasInlineValue {
				if i+1 >= len(args) {
					return &Violation{Arg: arg, Reason: "is missing its file value"}
				}
				value = args[i+1]
				i++
			}
			if reason := pathReason(value, p); reason != "" {
				return &Violation{Arg: value, Reason: reason}
			}
			continue
		}

		// Any other option: consume its value by arity so the value is judged
		// as a value and never mistaken for a file position.
		if reason := localPathReason(arg, p); reason != "" {
			return &Violation{Arg: arg, Reason: reason}
		}
		if !hasInlineValue && !ffmpegopts.IsBoolean(arg) && i+1 < len(args) {
			value := args[i+1]
			if strings.Contains(value, InputPlaceholder) {
				return &Violation{
					Arg:    value,
					Reason: "uses the input placeholder outside an input position; only an -i value may name a manifest input",
				}
			}
			// An option value is not a file position, so only the shapes that
			// are a filesystem reference in any context are judged here; the
			// options that do carry a path were handled above.
			if reason := valueReferenceReason(value, p); reason != "" {
				return &Violation{Arg: value, Reason: reason}
			}
			i++
		}
	}

	if placeholders > p.InputCount {
		return &Violation{
			Arg:    InputPlaceholder,
			Reason: fmt.Sprintf("is used %d times but the input list holds %d inputs", placeholders, p.InputCount),
		}
	}
	return nil
}

// optionName returns the option name without the leading dash and without a
// stream specifier, or "" for a non-option token.
func optionName(arg string) string {
	if !strings.HasPrefix(arg, "-") || arg == "-" || arg == "--" {
		return ""
	}
	name := strings.TrimPrefix(arg, "-")
	if idx := strings.IndexByte(name, ':'); idx != -1 {
		name = name[:idx]
	}
	if idx := strings.IndexByte(name, '='); idx != -1 {
		name = name[:idx]
	}
	return name
}

// splitInlineValue splits "-opt=value" into its value and true.
func splitInlineValue(arg string) (string, bool) {
	if !strings.HasPrefix(arg, "-") {
		return "", false
	}
	idx := strings.IndexByte(arg, '=')
	if idx == -1 {
		return "", false
	}
	return arg[idx+1:], true
}

// isInputOption reports whether arg names an ffmpeg input.
func isInputOption(arg string) bool {
	return arg == "-i" || arg == "--input"
}

// isDeniedOption reports whether arg is one of the denied options, including
// its stream-specifier form (e.g. "-dump_attachment:v").
func isDeniedOption(arg string) bool {
	for _, opt := range deniedOptions {
		if arg == opt || strings.HasPrefix(arg, opt+":") {
			return true
		}
	}
	return false
}

// localPathReason returns a non-empty reason when arg reaches a local path or a
// protocol address outside the allowed set. Both the token as given and the
// text after one restore are judged (see valueForms).
func localPathReason(arg string, p Policy) string {
	for _, form := range valueForms(arg) {
		if reason := localPathForm(form, p); reason != "" {
			return reason
		}
	}
	return ""
}

func localPathForm(arg string, p Policy) string {
	if arg == "-" || strings.HasPrefix(arg, "pipe:") {
		return "" // a pipe is not a filesystem path
	}
	if schemePrefix.MatchString(arg) {
		// A protocol address (http://, concat:, file:, …) is never a manifest
		// input and never a workspace path: both are plain filesystem paths.
		return "is a protocol address outside the input list"
	}
	if !filepath.IsAbs(arg) {
		// A relative path resolves against the job workspace, but ".." climbs
		// back out of it: the workspace prefix alone does not make it safe.
		if pathutil.ContainsPathTraversal(arg) {
			return "climbs out of the job workspace with an upward path segment"
		}
		return ""
	}
	if p.Workspace != "" && pathutil.Within(p.Workspace, arg) {
		return ""
	}
	for _, allowed := range p.InputPaths {
		if allowed == arg {
			return ""
		}
	}
	if p.Target != "" && arg == p.Target {
		return ""
	}
	return "is a local path outside the job workspace"
}

// valueReferenceReason judges an arbitrary option value: an absolute host path,
// a protocol address or an upward path segment is a filesystem reference
// wherever it appears. A plain relative word may still be a name, a title or a
// number, so that form is left to the option that owns it.
func valueReferenceReason(value string, p Policy) string {
	for _, form := range valueForms(value) {
		if reason := valueReferenceForm(form, p); reason != "" {
			return reason
		}
	}
	return ""
}

func valueReferenceForm(value string, p Policy) string {
	trimmed := strings.Trim(value, `"'`)
	if trimmed == "" || trimmed == "-" || strings.HasPrefix(trimmed, "pipe:") {
		return ""
	}
	if schemePrefix.MatchString(trimmed) {
		return "is a protocol address outside the input list"
	}
	if pathutil.ContainsPathTraversal(trimmed) {
		return "climbs out of the job workspace with an upward path segment"
	}
	if !filepath.IsAbs(trimmed) {
		return ""
	}
	if p.Workspace != "" && pathutil.Within(p.Workspace, trimmed) {
		return ""
	}
	for _, allowed := range p.InputPaths {
		if allowed == trimmed {
			return ""
		}
	}
	if p.Target != "" && trimmed == p.Target {
		return ""
	}
	return "is a local path outside the job workspace"
}

// pathReason judges one file value that a dedicated option carries. It is the
// single-path rule without the token-shape shortcuts: relative values are fine
// as long as they stay inside the workspace, absolute ones must be the
// workspace, a manifest input or the pass-through target. Every form of the
// value's escape chain is judged.
func pathReason(value string, p Policy) string {
	for _, form := range valueForms(value) {
		if reason := pathForm(form, p); reason != "" {
			return reason
		}
	}
	return ""
}

func pathForm(value string, p Policy) string {
	trimmed := strings.Trim(value, `"'`)
	if trimmed == "" {
		return ""
	}
	if trimmed == "-" || strings.HasPrefix(trimmed, "pipe:") {
		return ""
	}
	if schemePrefix.MatchString(trimmed) {
		return "is a protocol address, not a workspace path"
	}
	if pathutil.ContainsPathTraversal(trimmed) {
		return "climbs out of the job workspace with an upward path segment"
	}
	if !filepath.IsAbs(trimmed) {
		return ""
	}
	if p.Workspace != "" && pathutil.Within(p.Workspace, trimmed) {
		return ""
	}
	for _, allowed := range p.InputPaths {
		if allowed == trimmed {
			return ""
		}
	}
	if p.Target != "" && trimmed == p.Target {
		return ""
	}
	return "is a local path outside the job workspace"
}

// teeReason judges a tee slave list: slaves are separated by "|", each slave is
// an optional option group in brackets followed by the output path. The slave
// list is one av_get_token pass, so a separator hidden behind an escape is
// already resolved here and a slave or an option behind one is still judged.
func teeReason(spec string, p Policy) string {
	for _, slave := range splitTokens(spec, "|") {
		slave = strings.TrimSpace(slave)
		if slave == "" {
			continue
		}
		for strings.HasPrefix(slave, "[") {
			end := indexUnescaped(slave, ']')
			if end == -1 {
				return "has an unterminated option group"
			}
			// The group carries muxer options of this slave, so its values are
			// judged exactly like the same options on the command line.
			if reason := teeGroupReason(slave[1:end], p); reason != "" {
				return reason
			}
			slave = strings.TrimSpace(slave[end+1:])
		}
		if slave == "" {
			return "names no output"
		}
		// The slave list is one av_get_token pass, so the path here is the
		// text ffmpeg opens; a second restore would judge text ffmpeg never
		// produces.
		if reason := pathForm(slave, p); reason != "" {
			return reason
		}
	}
	return ""
}

// teeGroupReason judges one "[...]" option group of a tee slave: a
// colon-separated key=value list whose values are the muxer's options. The
// group is read through the same layer model as a filter option list, because
// ffmpeg tokenises it the same way - the slave list restores a layer and the
// option parser splits and restores another.
func teeGroupReason(group string, p Policy) string {
	for _, item := range optionTokens(group) {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		key, value, ok := strings.Cut(item, "=")
		if !ok {
			// A bare value is the muxer's first option; its shape is judged
			// like any other value in this group.
			if reason := valueReferenceForm(item, p); reason != "" {
				return reason
			}
			continue
		}
		key = strings.ToLower(strings.TrimSpace(key))
		value = strings.TrimSpace(value)
		if key == "f" {
			// The slave's muxer name selects a format, not a path.
			continue
		}
		if fileValuedOptions[key] {
			if reason := pathForm(value, p); reason != "" {
				return reason
			}
			continue
		}
		if reason := valueReferenceForm(value, p); reason != "" {
			return reason
		}
	}
	return ""
}

// indexUnescaped returns the index of the first unescaped sep in s, or -1.
func indexUnescaped(s string, sep byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			i++
			continue
		}
		if s[i] == sep {
			return i
		}
	}
	return -1
}

// ffmpeg reads an argument string with one av_get_token pass per syntax stage,
// not as opaque text. A pass consumes quoting, makes a backslash escape the next
// byte, drops one escape layer from the token text, and ends a token at that
// stage's own separators. A filter argument list has two passes (the graph
// parser over the whole list, then the filter's option parser over ":"); a tee
// slave list has two (the slave list over "|", then the option group over ":");
// a plain AVOption has none. Every judgement below uses these helpers that many
// times with that syntax's separators, so the escape depth is the number of
// passes the syntax has, not a guess.
//
// These are the only tokenisation points this package models. A filter's
// instance name is part of its name token, not a new one. A syntax that adds
// another point - an option value carrying its own option dictionary, such as
// -hls_segment_options or -segment_format_options - must be specified in
// 02-contracts before it is implemented here; adding it silently would put a
// token outside every judgement below.

// restoreQuoted restores one tokeniser layer the way ffmpeg's av_get_token
// does: a quote copies its content verbatim - escapes inside it are kept for
// the next layer - and an unquoted backslash makes the next byte literal.
func restoreQuoted(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c == '\'' || c == '"':
			i++
			for i < len(s) && s[i] != c {
				b.WriteByte(s[i])
				i++
			}
		case c == '\\' && i+1 < len(s):
			i++
			b.WriteByte(s[i])
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// splitTokens splits s at any byte of seps and restores one layer of each
// token, which is exactly one av_get_token pass: a separator inside quotes or
// behind a backslash does not end the token, and the token text loses one layer
// of escapes (quoted content keeps its own for the next layer).
func splitTokens(s, seps string) []string {
	var out []string
	var b strings.Builder
	flush := func() {
		out = append(out, b.String())
		b.Reset()
	}
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c == '\'' || c == '"':
			i++
			for i < len(s) && s[i] != c {
				b.WriteByte(s[i])
				i++
			}
		case c == '\\' && i+1 < len(s):
			i++
			b.WriteByte(s[i])
		case strings.IndexByte(seps, c) >= 0:
			flush()
		default:
			b.WriteByte(c)
		}
	}
	flush()
	return out
}

// valueForms returns the spellings a value is judged in: the text as handed to
// the judgement and the text after one more restore. A value that already went
// through its syntax's tokeniser is judged in the form ffmpeg ends up using,
// plus one extra layer so a path escaped once too often is still refused.
func valueForms(s string) []string {
	if restored := restoreQuoted(s); restored != s {
		return []string{s, restored}
	}
	return []string{s}
}

// optionTokens returns the tokens of one option list ("key=value:key=value") the
// way ffmpeg's av_opt_set_from_string reads it: it splits the text at ":" and
// restores one layer per token (splitTokens does both). The caller passes text
// its own tokeniser already restored, so an escape that belonged to that outer
// layer is a plain separator here: in "fontsize=20\\:textfile=/x" the colon is
// the graph parser's, and this parser then reads two options.
func optionTokens(s string) []string {
	raw := splitTokens(s, ":")
	out := make([]string, 0, len(raw))
	for _, tok := range raw {
		if tok = strings.TrimSpace(tok); tok != "" {
			out = append(out, tok)
		}
	}
	return out
}

// filterOptionTokens returns the tokens of a filter element's argument list.
// The graph parser is the outer tokeniser: it consumes the quoting and restores
// one layer over the whole list, and the filter's own option parser then splits
// that text and restores one layer per token.
func filterOptionTokens(args string) []string {
	return optionTokens(restoreQuoted(args))
}

// filterValueReason judges one filter option value - named or positional - as
// the filter receives it, after both tokeniser stages. Only a shape that can
// name a resource outside the workspace is refused: an absolute path, an upward
// path segment, or a protocol address.
//
// The judgement does not depend on knowing the parameter name, so a filter
// parameter this package has never seen cannot carry such a path through: a
// path is a path in any position, and text that merely contains a colon or a
// backslash is not a path.
func filterValueReason(value string) string {
	v := strings.Trim(strings.TrimSpace(value), `"'`)
	if v == "" || v == "-" || strings.HasPrefix(v, "pipe:") {
		return ""
	}
	if filterValueIsAddress(v) || pathutil.ContainsPathTraversal(v) ||
		strings.HasPrefix(v, string(filepath.Separator)) {
		return v
	}
	return ""
}

// filterValueIsAddress reports whether a restored filter value is spelled as a
// protocol address. The test is the "scheme://" form, not a bare "word:" — the
// latter is ordinary text in this position ("Clock: 00:00") and the filters
// that open addresses are refused by name before their values are judged.
func filterValueIsAddress(v string) bool {
	idx := strings.Index(v, "://")
	if idx <= 0 {
		return false
	}
	scheme := v[:idx]
	for i := 0; i < len(scheme); i++ {
		c := scheme[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z':
		case i > 0 && (c >= '0' && c <= '9' || c == '+' || c == '-' || c == '.'):
		default:
			return false
		}
	}
	return true
}

// filterGraphReason parses a filter graph and judges every element in it. An
// unparseable graph is refused rather than passed through: its element
// boundaries are unknown, so no judgement can cover them.
func filterGraphReason(graph string) string {
	elements, err := parseFilterGraph(graph)
	if err != nil {
		return "is not a filter graph this job may pass to ffmpeg: " + err.Error()
	}
	for _, element := range elements {
		name := strings.ToLower(element.name)
		if fileFilterNames[name] {
			return fmt.Sprintf("uses the %q filter, which opens a file the request chose", element.name)
		}
		for _, param := range element.params() {
			if param.name != "" && fileFilterParams[strings.ToLower(param.name)] {
				return fmt.Sprintf("passes a file path to the %q filter through the %q parameter", element.name, param.name)
			}
			// The value is judged whether or not its name is known, and
			// whether it is named or positional: a path shape is refused in any
			// position, so an unenumerated parameter cannot hide one. Text that
			// merely contains a colon or a backslash stays allowed.
			if v := filterValueReason(param.value); v != "" {
				return fmt.Sprintf("passes the path %q to the %q filter", v, element.name)
			}
		}
	}
	return ""
}

// filterElement is one filter invocation inside a graph: its filter name (the
// instance name, if any, is not kept - it cannot reach a file) and its raw
// argument string.
type filterElement struct {
	name string
	args string
}

// param is one named argument of a filter element.
type param struct {
	name  string
	value string
}

// params splits the element's argument string into parameters through the
// shared layer model: the graph parser consumes quotes and the first escape
// layer, then the filter's own option parser splits on ":" at every layer of
// what remains. A positional value keeps an empty name, and its value is
// judged exactly like a named one: ffmpeg maps it onto the filter's first
// option, so it can name a file just as well.
func (e filterElement) params() []param {
	if strings.TrimSpace(e.args) == "" {
		return nil
	}
	tokens := filterOptionTokens(e.args)
	out := make([]param, 0, len(tokens))
	for _, item := range tokens {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		name, value, ok := strings.Cut(item, "=")
		if !ok {
			out = append(out, param{value: item})
			continue
		}
		out = append(out, param{name: strings.TrimSpace(name), value: strings.TrimSpace(value)})
	}
	return out
}

// parseFilterGraph walks a filtergraph and returns its elements. Supported
// syntax: chains separated by ";" , filters separated by "," , optional link
// labels in brackets before and after a filter, quoting with ' and " , and
// backslash escapes.
func parseFilterGraph(graph string) ([]filterElement, error) {
	var elements []filterElement
	i := 0
	n := len(graph)
	skipSpaces := func() {
		for i < n && isSpace(graph[i]) {
			i++
		}
	}

	for {
		skipSpaces()
		if i >= n {
			break
		}
		// Leading link labels.
		for i < n && graph[i] == '[' {
			next, err := skipLabel(graph, i)
			if err != nil {
				return nil, err
			}
			i = next
			skipSpaces()
		}
		if i >= n {
			return nil, fmt.Errorf("filter graph ends after a label")
		}

		start := i
		for i < n {
			c := graph[i]
			if c == '\\' {
				i += 2
				continue
			}
			if c == '=' || c == ',' || c == ';' || c == '[' || c == ']' || isSpace(c) {
				break
			}
			i++
		}
		token := strings.TrimSpace(graph[start:i])
		name, ok := splitFilterName(token)
		if !ok {
			return nil, fmt.Errorf("filter graph has an invalid filter name %q", token)
		}

		args := ""
		if i < n && graph[i] == '=' {
			i++
			argStart := i
			for i < n {
				c := graph[i]
				if c == '\\' && i+1 < n {
					i += 2
					continue
				}
				if c == '\'' || c == '"' {
					q := c
					i++
					closed := false
					for i < n {
						if graph[i] == '\\' && i+1 < n {
							i += 2
							continue
						}
						if graph[i] == q {
							i++
							closed = true
							break
						}
						i++
					}
					if !closed {
						return nil, fmt.Errorf("unterminated quote in filter %q", name)
					}
					continue
				}
				if c == ',' || c == ';' || c == '[' {
					break
				}
				i++
			}
			args = graph[argStart:i]
		}
		elements = append(elements, filterElement{name: name, args: args})

		skipSpaces()
		for i < n && graph[i] == '[' {
			next, err := skipLabel(graph, i)
			if err != nil {
				return nil, err
			}
			i = next
			skipSpaces()
		}
		if i < n {
			if graph[i] == ',' || graph[i] == ';' {
				i++
				continue
			}
			return nil, fmt.Errorf("unexpected %q at filter %q", string(graph[i]), name)
		}
	}
	if len(elements) == 0 {
		return nil, fmt.Errorf("filter graph is empty")
	}
	return elements, nil
}

// skipLabel consumes a "[name]" link label starting at i and returns the index
// after it.
func skipLabel(graph string, i int) (int, error) {
	i++ // the opening bracket
	for i < len(graph) {
		if graph[i] == '\\' && i+1 < len(graph) {
			i += 2
			continue
		}
		if graph[i] == ']' {
			return i + 1, nil
		}
		i++
	}
	return 0, fmt.Errorf("unterminated link label")
}

func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}
