package jobargs

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCheckSubmission(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		inputs  int
		wantErr bool
	}{
		{
			name:   "placeholder input",
			args:   []string{"-i", InputPlaceholder, "-c:v", "libx264", "-preset", "fast"},
			inputs: 1,
		},
		{
			name:   "no input option at all",
			args:   []string{"-c:v", "libx264", "-preset", "fast"},
			inputs: 1,
		},
		{
			name:   "bare relative output token stays in the workspace",
			args:   []string{"-i", InputPlaceholder, "out.mp4"},
			inputs: 1,
		},
		{
			name:    "extra input path",
			args:    []string{"-f", "data", "-i", "/etc/passwd"},
			inputs:  1,
			wantErr: true,
		},
		{
			name:    "synthetic input",
			args:    []string{"-f", "lavfi", "-i", "testsrc=duration=5"},
			inputs:  1,
			wantErr: true,
		},
		{
			name:    "more placeholders than manifest inputs",
			args:    []string{"-i", InputPlaceholder, "-i", InputPlaceholder},
			inputs:  1,
			wantErr: true,
		},
		{
			name:    "filter movie source",
			args:    []string{"-i", InputPlaceholder, "-vf", "movie=/etc/passwd"},
			inputs:  1,
			wantErr: true,
		},
		{
			name:    "filter source after a chain separator",
			args:    []string{"-i", InputPlaceholder, "-filter_complex", "[0:v]scale=10:10[bg];movie=/etc/passwd[fg]"},
			inputs:  1,
			wantErr: true,
		},
		{
			name:    "subtitles filter source",
			args:    []string{"-i", InputPlaceholder, "-vf", "subtitles=/etc/passwd"},
			inputs:  1,
			wantErr: true,
		},
		{
			name:    "filter script option",
			args:    []string{"-i", InputPlaceholder, "-filter_complex_script", "graph.txt"},
			inputs:  1,
			wantErr: true,
		},
		{
			name:    "pass log file",
			args:    []string{"-i", InputPlaceholder, "-passlogfile", "/tmp/x"},
			inputs:  1,
			wantErr: true,
		},
		{
			name:    "progress file",
			args:    []string{"-i", InputPlaceholder, "-progress", "/tmp/x"},
			inputs:  1,
			wantErr: true,
		},
		{
			name:    "absolute path token",
			args:    []string{"-i", InputPlaceholder, "/etc/passwd"},
			inputs:  1,
			wantErr: true,
		},
		{
			name:    "file protocol token",
			args:    []string{"-i", InputPlaceholder, "file:/etc/passwd"},
			inputs:  1,
			wantErr: true,
		},
		{
			name:    "network output address",
			args:    []string{"-i", InputPlaceholder, "rtmp://example.com/live"},
			inputs:  1,
			wantErr: true,
		},
		{
			name:   "plain filter graph is untouched",
			args:   []string{"-i", InputPlaceholder, "-vf", "scale=1280:720", "-metadata", "title=holiday"},
			inputs: 1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Check(tc.args, Policy{InputCount: tc.inputs})
			if (got != nil) != tc.wantErr {
				t.Fatalf("Check(%v) = %v, wantErr %v", tc.args, got, tc.wantErr)
			}
		})
	}
}

func TestCheckExecution(t *testing.T) {
	workspace := t.TempDir()
	inputPath := filepath.Join(workspace, "input-0")
	if err := os.WriteFile(inputPath, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "target.mp4")

	base := Policy{
		InputCount: 1,
		InputPaths: []string{inputPath},
		Workspace:  workspace,
		Target:     target,
	}

	ok := []struct {
		name string
		args []string
	}{
		{"placeholder and workspace output", []string{"-i", InputPlaceholder, "out.mp4"}},
		{"relative progress report", []string{"-i", InputPlaceholder, "-vf", "scale=10:10"}},
		{"direct target passed as the output", []string{"-i", InputPlaceholder, target}},
	}
	for _, tc := range ok {
		t.Run(tc.name, func(t *testing.T) {
			if v := Check(tc.args, base); v != nil {
				t.Fatalf("Check(%v) = %v, want nil", tc.args, v)
			}
		})
	}

	bad := []struct {
		name string
		args []string
	}{
		{"absolute path outside the workspace", []string{"-i", InputPlaceholder, "/etc/cron.d/x"}},
		{"extra input", []string{"-i", InputPlaceholder, "-i", "/etc/passwd"}},
		{"filter file source", []string{"-i", InputPlaceholder, "-vf", "movie=/etc/passwd"}},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			if v := Check(tc.args, base); v == nil {
				t.Fatalf("Check(%v) = nil, want a violation", tc.args)
			}
		})
	}
}

// TestCheckStructuredBoundaries covers the C-126 / C-127 bypasses that live in
// argument syntax rather than in a single token's shape: a file source hidden
// behind a filter label, a relative path that climbs out of the workspace, a
// file-valued muxer option whose value is a composite spec, a demuxer that
// re-interprets the input file's contents, and a placeholder used outside an
// input position.
func TestCheckStructuredBoundaries(t *testing.T) {
	workspace := t.TempDir()
	inputPath := filepath.Join(workspace, "input-0")
	if err := os.WriteFile(inputPath, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	submission := Policy{InputCount: 2}
	execution := Policy{InputCount: 2, InputPaths: []string{inputPath}, Workspace: workspace}

	cases := []struct {
		name string
		args []string
		want bool // true = must be refused
	}{
		// 1. Filter file sources, in every syntactic position.
		{"subtitles behind a label prefix", []string{"-i", InputPlaceholder, "-filter_complex", "[0:v]subtitles=/etc/passwd[v]"}, true},
		{"ass behind two label prefixes", []string{"-i", InputPlaceholder, "-filter_complex", "[0:v][0:a]ass=/etc/passwd[v]"}, true},
		{"drawtext textfile", []string{"-i", InputPlaceholder, "-vf", "drawtext=textfile=/etc/passwd"}, true},
		{"drawtext fontfile", []string{"-i", InputPlaceholder, "-vf", "[in]drawtext=fontfile=/etc/passwd:fontsize=12[out]"}, true},
		{"movie behind a label prefix", []string{"-i", InputPlaceholder, "-filter_complex", "[0:v]movie=/etc/passwd[fg]"}, true},
		{"amovie behind a label prefix", []string{"-i", InputPlaceholder, "-filter_complex", "[0:a]amovie=/etc/passwd[a]"}, true},
		{"subfile filter", []string{"-i", InputPlaceholder, "-filter_complex", "subtitles=subfile=/etc/passwd"}, true},
		{"filter value that is an absolute path", []string{"-i", InputPlaceholder, "-vf", "drawtext=text=/etc/passwd"}, true},
		{"filter graph that cannot be parsed", []string{"-i", InputPlaceholder, "-vf", "scale=1:1,'unterminated"}, true},

		// 2. Relative paths with upward segments (both modes).
		{"relative output climbing out", []string{"-i", InputPlaceholder, "-c", "copy", "../outside/PWN.mp4"}, true},
		{"relative segment list climbing out", []string{"-i", InputPlaceholder, "-segment_list", "../leak.csv"}, true},
		{"dash init segment climbing out", []string{"-i", InputPlaceholder, "-f", "dash", "-init_seg_name", "../x%03d.m4s"}, true},

		// 3. Muxer options with file values, including composite specs.
		{"tee slave with an absolute path", []string{"-i", InputPlaceholder, "-f", "tee", "[f=mp4]/etc/cron.d/x"}, true},
		{"tee slave with an upward segment", []string{"-i", InputPlaceholder, "-f", "tee", "[f=mp4]../out/x.mp4"}, true},
		{"tee slave with a protocol address", []string{"-i", InputPlaceholder, "-f", "tee", "[f=mp4]udp://127.0.0.1:9"}, true},
		{"hls segment filename outside", []string{"-i", InputPlaceholder, "-hls_segment_filename", "../out/x%03d.ts"}, true},
		{"segment list outside", []string{"-i", InputPlaceholder, "-segment_list", "../../../../etc/cron.d/x"}, true},
		{"hls key info file", []string{"-i", InputPlaceholder, "-hls_key_info_file", "/etc/passwd"}, true},
		{"vstats file", []string{"-i", InputPlaceholder, "-vstats_file", "/etc/passwd"}, true},
		{"sdp file", []string{"-i", InputPlaceholder, "-sdp_file", "/etc/passwd"}, true},

		// 4. Demuxers that re-interpret the input file's contents.
		{"concat demuxer", []string{"-f", "concat", "-safe", "0", "-i", InputPlaceholder}, true},
		{"concatf demuxer", []string{"-f", "concatf", "-i", InputPlaceholder}, true},
		{"image2 demuxer", []string{"-f", "image2", "-i", InputPlaceholder}, true},
		{"lavfi demuxer", []string{"-f", "lavfi", "-i", InputPlaceholder}, true},

		// 1b. The same file sources spelled with ffmpeg's backslash escapes:
		// ffmpeg restores the escaped text before it reads the parameter, so
		// the judgement must run on the restored form.
		{"escaped drawtext textfile", []string{"-i", InputPlaceholder, "-vf", `drawtext=textfile\=\/etc\/passwd`}, true},
		{"escaped drawtext textfile behind a label", []string{"-i", InputPlaceholder, "-vf", `[in]drawtext=textfile\=\/etc\/passwd[out]`}, true},
		{"escaped drawtext fontfile", []string{"-i", InputPlaceholder, "-vf", `drawtext=fontfile\=\/etc\/passwd:fontsize=12`}, true},
		{"escaped movie filter", []string{"-i", InputPlaceholder, "-filter_complex", `[0:v]movie\=\/etc\/passwd[fg]`}, true},
		{"escaped subtitles parameter", []string{"-i", InputPlaceholder, "-filter_complex", `[0:v]subtitles=filename\=\/etc\/passwd[v]`}, true},
		// The filter option value goes through ffmpeg's graph tokeniser and its
		// own option parser, so "\\/" becomes "/" there. One restored layer is
		// not enough to see the absolute path.
		{"double-escaped filter write path", []string{"-i", InputPlaceholder, "-vf", `vidstabdetect=result=\\/etc\\/passwd`}, true},
		{"double-escaped filter read path", []string{"-i", InputPlaceholder, "-vf", `vidstabtransform=input=\\/etc\\/passwd`}, true},
		{"double-escaped filter value with an upward segment", []string{"-i", InputPlaceholder, "-vf", `vidstabdetect=result=\\..\\/..\\/etc\\/passwd`}, true},
		{"double-escaped protocol-shaped filter value", []string{"-i", InputPlaceholder, "-vf", `foo=url=\\/\\/169.254.169.254/x`}, true},
		// An escaped separator moves the parameter name into the *next*
		// tokeniser layer: ffmpeg restores "\:" to ":" and then splits, so
		// textfile is a parameter of its own and not part of fontsize's value.
		{"escaped separator hides a file parameter", []string{"-i", InputPlaceholder, "-vf", `drawtext=fontsize=20\:textfile=/etc/passwd`}, true},
		{"escaped separator hides a file parameter after a valid one", []string{"-i", InputPlaceholder, "-vf", `drawtext=x=1\:y=2\:textfile=\/etc\/passwd`}, true},
		// A positional value is the filter's first option, which for these
		// filters is a file path.
		{"positional filter read path", []string{"-i", InputPlaceholder, "-vf", `vidstabtransform=/etc/passwd`}, true},
		{"positional filter write path", []string{"-i", InputPlaceholder, "-vf", `vidstabdetect=/tmp/outside/written.txt`}, true},
		{"positional filter upward path", []string{"-i", InputPlaceholder, "-vf", `vidstabtransform=../../etc/passwd`}, true},
		{"escaped positional filter path", []string{"-i", InputPlaceholder, "-vf", `vidstabtransform=\/etc\/passwd`}, true},
		{"positional filter protocol address", []string{"-i", InputPlaceholder, "-vf", `vidstabtransform=http://169.254.169.254/x`}, true},

		// 3b. tee: the option group's own file-valued options and the slave
		// path are both restored from backslash escapes by ffmpeg.
		{"tee option group with segment_list", []string{"-i", InputPlaceholder, "-f", "tee", `[f=segment:segment_list=/tmp/outside/EVIL.csv]seg%d.ts`}, true},
		{"tee option group with hls_segment_filename", []string{"-i", InputPlaceholder, "-f", "tee", `[f=hls:hls_segment_filename=/tmp/outside/seg%d.ts]out.m3u8`}, true},
		{"escaped tee option group file value", []string{"-i", InputPlaceholder, "-f", "tee", `[f=segment:segment_list=\/tmp\/outside/EVIL.csv]seg%d.ts`}, true},
		{"escaped tee slave path", []string{"-i", InputPlaceholder, "-f", "tee", `[f=mpegts]\/tmp\/outside/esc.ts`}, true},
		{"escaped tee group separator and value", []string{"-i", InputPlaceholder, "-f", "tee", `[f=segment:segment_list\=\/tmp\/outside/EVIL.csv]seg%d.ts`}, true},
		{"escaped tee group separator before a file option", []string{"-i", InputPlaceholder, "-f", "tee", `[f=segment:segment_time=1\:segment_list=/tmp/outside/EVIL.csv]seg%d.ts`}, true},

		// 5. Placeholder position.
		{"bare placeholder in the output position", []string{"-i", InputPlaceholder, "-c", "copy", "-y", InputPlaceholder}, true},
		{"placeholder as a filter value", []string{"-i", InputPlaceholder, "-vf", "drawtext=text=" + InputPlaceholder}, true},

		// 6. Filter instance names: ffmpeg reads "name[@instance]" as one graph
		// token and splits it at the first "@". The instance name is whatever
		// the token kept, so the shape rules apply to the filter name only.
		{"file filter with an instance name", []string{"-i", InputPlaceholder, "-vf", `movie@x=/etc/passwd`}, true},
		{"file parameter through an instance-named filter", []string{"-i", InputPlaceholder, "-vf", `drawtext@t=textfile=\/etc\/passwd`}, true},
		{"positional path through an instance-named filter", []string{"-i", InputPlaceholder, "-vf", `vidstabdetect@x=/tmp/outside/written.txt`}, true},
		{"escaped filter name", []string{"-i", InputPlaceholder, "-vf", `subtit\les=/etc/hostname`}, true},
		{"escaped file filter name", []string{"-i", InputPlaceholder, "-vf", `mov\ie=/etc/passwd`}, true},
		{"escaped filter name with an instance", []string{"-i", InputPlaceholder, "-vf", `movi\e@x=/etc/passwd`}, true},

		// Legitimate shapes that must keep working.
		{"filter instance name", []string{"-i", InputPlaceholder, "-vf", "scale@0=64:64"}, false},
		{"filter instance name behind a label", []string{"-i", InputPlaceholder, "-filter_complex", "[0:v]scale@0=64:64[v]"}, false},
		{"split with an instance name", []string{"-i", InputPlaceholder, "-vf", "split@a=3"}, false},
		{"instance name with a hyphen", []string{"-i", InputPlaceholder, "-vf", "scale@a-b=64:64"}, false},
		{"instance name with a dot", []string{"-i", InputPlaceholder, "-vf", "scale@a.b=64:64"}, false},
		{"instance name with a colon", []string{"-i", InputPlaceholder, "-vf", "scale@a:b=64:64"}, false},
		{"empty instance name", []string{"-i", InputPlaceholder, "-vf", "scale@=64:64"}, false},
		{"label-prefixed filter chain", []string{"-i", InputPlaceholder, "-filter_complex", "[0:v]scale=10:10[bg];[bg][0:v]overlay[v]"}, false},
		{"drawtext with literal text", []string{"-i", InputPlaceholder, "-vf", "scale=640:480,drawtext=text='acceptance':fontsize=32:fontcolor=white:x=20:y=20"}, false},
		{"unknown filter reaches ffmpeg", []string{"-i", InputPlaceholder, "-vf", "no_such_filter_xyz=1"}, false},
		{"container input format", []string{"-f", "mp4", "-i", InputPlaceholder}, false},
		{"output format", []string{"-i", InputPlaceholder, "-f", "hls", "out.m3u8"}, false},
		{"hls segment filename inside the workspace", []string{"-i", InputPlaceholder, "-f", "hls", "-hls_segment_filename", "seg%03d.ts", "out.m3u8"}, false},
		{"tee slave inside the workspace", []string{"-i", InputPlaceholder, "-f", "tee", "[f=mp4]out.mp4"}, false},
		{"tee option group with a non-path value", []string{"-i", InputPlaceholder, "-f", "tee", "[f=segment:segment_time=5]seg%d.ts"}, false},
		{"escaped non-path filter text", []string{"-i", InputPlaceholder, "-vf", `drawtext=text=100\%:fontsize=32`}, false},
		// Text with a colon is text: ffmpeg renders it, and a bare "word:" is
		// not a protocol address in a filter option value (C-7 / AC-06).
		{"drawtext text with an escaped colon", []string{"-i", InputPlaceholder, "-vf", `drawtext=text='Clock\: %{pts\:hms}':fontsize=32`}, false},
		{"drawtext text with an unescaped colon", []string{"-i", InputPlaceholder, "-vf", `drawtext=text='Clock: 00:00':fontsize=32`}, false},
		{"drawtext chapter text with an escaped colon", []string{"-i", InputPlaceholder, "-vf", `drawtext=text='Chapter\: 1':fontsize=32`}, false},
		{"relative output name", []string{"-i", InputPlaceholder, "-c", "copy", "out.mp4"}, false},
	}

	for _, tc := range cases {
		t.Run(tc.name+"/submission", func(t *testing.T) {
			if got := Check(tc.args, submission); (got != nil) != tc.want {
				t.Fatalf("Check(%v) = %v, want violation %v", tc.args, got, tc.want)
			}
		})
		t.Run(tc.name+"/execution", func(t *testing.T) {
			if got := Check(tc.args, execution); (got != nil) != tc.want {
				t.Fatalf("Check(%v) = %v, want violation %v", tc.args, got, tc.want)
			}
		})
	}
}
