package rewrite

import (
	"testing"

	"github.com/tsix404/rffmpeg/pkg/encoder"
)

func TestFFmpegParser_ParseArgs(t *testing.T) {
	parser := NewFFmpegParser()

	tests := []struct {
		name        string
		args        []string
		wantEncoder encoder.EncoderFamily
		wantCodec   encoder.CodecFormat
		wantErr     bool
	}{
		{
			name:        "Detect encoder with -c:v",
			args:        []string{"-c:v", "h264_nvenc", "-b:v", "5000k"},
			wantEncoder: encoder.EncoderH264NVENC,
			wantCodec:   encoder.CodecH264,
		},
		{
			name:        "Detect encoder with -codec:v",
			args:        []string{"-codec:v", "libx264", "-crf", "23"},
			wantEncoder: encoder.EncoderLibX264,
			wantCodec:   encoder.CodecH264,
		},
		{
			name:        "Detect encoder with -vcodec",
			args:        []string{"-vcodec", "hevc_nvenc"},
			wantEncoder: encoder.EncoderHEVCNVENC,
			wantCodec:   encoder.CodecHEVC,
		},
		{
			name:        "No encoder specified",
			args:        []string{"-b:v", "5000k", "-crf", "23"},
			wantEncoder: "",
			wantCodec:   "",
		},
		{
			name:        "Invalid encoder",
			args:        []string{"-c:v", "invalid_encoder"},
			wantEncoder: "",
			wantCodec:   "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotEncoder, gotCodec, gotParams, err := parser.ParseArgs(tt.args)
			if (err != nil) != tt.wantErr {
				t.Errorf("FFmpegParser.ParseArgs() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if gotEncoder != tt.wantEncoder {
				t.Errorf("FFmpegParser.ParseArgs() gotEncoder = %v, want %v", gotEncoder, tt.wantEncoder)
			}
			if gotCodec != tt.wantCodec {
				t.Errorf("FFmpegParser.ParseArgs() gotCodec = %v, want %v", gotCodec, tt.wantCodec)
			}
			if gotParams == nil {
				t.Error("FFmpegParser.ParseArgs() gotParams should not be nil")
			}
		})
	}
}

func TestFFmpegParser_CreateEncoderRewriteRequest(t *testing.T) {
	parser := NewFFmpegParser()

	hwCaps := &HardwareCapabilities{
		AvailableEncoders: []encoder.EncoderFamily{
			encoder.EncoderLibX264,
			encoder.EncoderH264NVENC,
		},
		HardwareEncoders: []encoder.EncoderFamily{
			encoder.EncoderH264NVENC,
		},
		SoftwareEncoders: []encoder.EncoderFamily{
			encoder.EncoderLibX264,
		},
		SupportedCodecs: []encoder.CodecFormat{
			encoder.CodecH264,
		},
		EncoderPriority: DefaultEncoderPriority(),
	}

	tests := []struct {
		name      string
		args      []string
		autoHW    bool
		requestID string
		wantErr   bool
		checkFunc func(*EncoderRewriteRequest) bool
	}{
		{
			name:      "Valid request with encoder",
			args:      []string{"-c:v", "h264_nvenc", "-b:v", "5000k"},
			autoHW:    false,
			requestID: "test-123",
			checkFunc: func(req *EncoderRewriteRequest) bool {
				return req.SpecifiedEncoder == encoder.EncoderH264NVENC &&
					req.TargetCodec == encoder.CodecH264 &&
					req.AutoHW == false &&
					req.RequestID == "test-123" &&
					len(req.OriginalArgs) == 4
			},
		},
		{
			name:      "Valid request without encoder",
			args:      []string{"-b:v", "5000k"},
			autoHW:    true,
			requestID: "test-456",
			checkFunc: func(req *EncoderRewriteRequest) bool {
				return req.SpecifiedEncoder == "" &&
					req.AutoHW == true &&
					req.RequestID == "test-456"
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, err := parser.CreateEncoderRewriteRequest(tt.args, hwCaps, tt.autoHW, tt.requestID)
			if (err != nil) != tt.wantErr {
				t.Errorf("FFmpegParser.CreateEncoderRewriteRequest() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if err == nil && req == nil {
				t.Error("FFmpegParser.CreateEncoderRewriteRequest() returned nil request without error")
				return
			}
			if err == nil && tt.checkFunc != nil && !tt.checkFunc(req) {
				t.Error("FFmpegParser.CreateEncoderRewriteRequest() request validation failed")
			}
		})
	}
}

func TestFFmpegParser_EmptyArgs(t *testing.T) {
	parser := NewFFmpegParser()
	_, err := parser.CreateEncoderRewriteRequest(nil, &HardwareCapabilities{}, false, "test")
	if err == nil {
		t.Error("Expected error for empty arguments")
	}
}
