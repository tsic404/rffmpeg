package rewrite

import (
	"context"
	"fmt"

	"github.com/tsix404/rffmpeg/pkg/encoder"
)

// Example demonstrates a complete workflow of the scenario classifier.
func Example() {
	// Create a parser to parse FFmpeg arguments
	parser := NewFFmpegParser()

	// Simulate hardware capabilities of a worker with NVIDIA GPU
	hwCaps := &HardwareCapabilities{
		AvailableEncoders: []encoder.EncoderFamily{
			encoder.EncoderLibX264,
			encoder.EncoderH264NVENC,
			encoder.EncoderHEVCNVENC,
			encoder.EncoderLibVPX,
		},
		HardwareEncoders: []encoder.EncoderFamily{
			encoder.EncoderH264NVENC,
			encoder.EncoderHEVCNVENC,
		},
		SoftwareEncoders: []encoder.EncoderFamily{
			encoder.EncoderLibX264,
			encoder.EncoderLibVPX,
		},
		SupportedCodecs: []encoder.CodecFormat{
			encoder.CodecH264,
			encoder.CodecHEVC,
			encoder.CodecVP9,
		},
		EncoderPriority: DefaultEncoderPriority(),
	}

	// Create scenario classifier
	classifier := NewScenarioClassifier()

	// Test case 1: User specifies h264_nvenc, which is available
	args1 := []string{"-c:v", "h264_nvenc", "-b:v", "5000k", "-crf", "23"}
	req1, _ := parser.CreateEncoderRewriteRequest(args1, hwCaps, false, "req-1")
	scenario1, _ := classifier.Classify(context.Background(), req1)
	info1 := classifier.GetScenarioInfo(scenario1)
	fmt.Printf("Test 1 - Specified h264_nvenc (available): %s - %s\n", scenario1.String(), info1.Description)

	// Test case 2: User specifies h264_qsv, which is not available but h264_nvenc is
	args2 := []string{"-c:v", "h264_qsv", "-b:v", "5000k"}
	req2, _ := parser.CreateEncoderRewriteRequest(args2, hwCaps, false, "req-2")
	scenario2, _ := classifier.Classify(context.Background(), req2)
	info2 := classifier.GetScenarioInfo(scenario2)
	fmt.Printf("Test 2 - Specified h264_qsv (not available): %s - %s\n", scenario2.String(), info2.Description)

	// Test case 3: User doesn't specify encoder, hardware available
	args3 := []string{"-b:v", "5000k", "-crf", "23"}
	req3, _ := parser.CreateEncoderRewriteRequest(args3, hwCaps, false, "req-3")
	scenario3, _ := classifier.Classify(context.Background(), req3)
	info3 := classifier.GetScenarioInfo(scenario3)
	fmt.Printf("Test 3 - No encoder specified (hardware available): %s - %s\n", scenario3.String(), info3.Description)

	// Test case 4: User specifies unsupported format
	args4 := []string{"-c:v", "av1_nvenc", "-b:v", "5000k"}
	req4, _ := parser.CreateEncoderRewriteRequest(args4, hwCaps, false, "req-4")
	scenario4, _ := classifier.Classify(context.Background(), req4)
	info4 := classifier.GetScenarioInfo(scenario4)
	fmt.Printf("Test 4 - Specified av1_nvenc (format not available): %s - %s\n", scenario4.String(), info4.Description)

	// Output:
	// Test 1 - Specified h264_nvenc (available): specified_encoder_supported - Pass through unchanged
	// Test 2 - Specified h264_qsv (not available): specified_encoder_unsupported_with_alternative - Translate to local hardware encoder
	// Test 3 - No encoder specified (hardware available): unspecified_encoder_with_hw - Auto-upgrade to best hardware encoder
	// Test 4 - Specified av1_nvenc (format not available): format_not_available - Format not available (error)
}
