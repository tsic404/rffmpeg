package workerhealth

import (
	"testing"
	"time"

	"github.com/tsic404/rffmpeg/pkg/protocol"
)

func TestParseThroughputScale(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    []ThroughputScaleEntry
		wantErr bool
	}{
		{
			name: "single entry",
			raw:  "slow-node=0.05",
			want: []ThroughputScaleEntry{{Worker: "slow-node", Factor: 0.05}},
		},
		{
			name: "multiple entries with window and whitespace",
			raw:  " slow-node=0.05@2m , worker-b=0.5@30s ",
			want: []ThroughputScaleEntry{
				{Worker: "slow-node", Factor: 0.05, Window: 2 * time.Minute},
				{Worker: "worker-b", Factor: 0.5, Window: 30 * time.Second},
			},
		},
		{
			name: "factor above one is allowed",
			raw:  "fast-node=2",
			want: []ThroughputScaleEntry{{Worker: "fast-node", Factor: 2}},
		},
		{name: "missing separator", raw: "slow-node", wantErr: true},
		{name: "empty worker", raw: "=0.05", wantErr: true},
		{name: "duplicate worker", raw: "slow-node=0.05,slow-node=0.5", wantErr: true},
		{name: "zero factor", raw: "slow-node=0", wantErr: true},
		{name: "negative factor", raw: "slow-node=-1", wantErr: true},
		{name: "non-numeric factor", raw: "slow-node=fast", wantErr: true},
		{name: "NaN factor", raw: "slow-node=NaN", wantErr: true},
		{name: "Inf factor", raw: "slow-node=Inf", wantErr: true},
		{name: "lowercase infinity factor", raw: "slow-node=infinity", wantErr: true},
		{name: "invalid window", raw: "slow-node=0.5@soon", wantErr: true},
		{name: "zero window", raw: "slow-node=0.5@0s", wantErr: true},
		{name: "no entries", raw: " , ", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseThroughputScale(tt.raw)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ParseThroughputScale(%q) = %+v, want error", tt.raw, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseThroughputScale(%q) failed: %v", tt.raw, err)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("ParseThroughputScale(%q) returned %d entries, want %d", tt.raw, len(got), len(tt.want))
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("entry %d = %+v, want %+v", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestThroughputScaler_MatchByIDOrName(t *testing.T) {
	scaler := NewThroughputScaler([]ThroughputScaleEntry{
		{Worker: "worker-by-id", Factor: 0.5},
		{Worker: "slow-node", Factor: 0.25},
	})

	// A heartbeat carries only the worker ID; a rule written against the
	// registered name is matched after the handler resolves it.
	if got := scaler.Scale("worker-by-id", "", 4.0); got != 2.0 {
		t.Errorf("ID-matched throughput = %f, want 2.0", got)
	}
	if got := scaler.Scale("unrelated-uuid", "slow-node", 4.0); got != 1.0 {
		t.Errorf("name-matched throughput = %f, want 1.0", got)
	}
	if got := scaler.Scale("unrelated-uuid", "fast-node", 4.0); got != 4.0 {
		t.Errorf("unlisted worker throughput = %f, want 4.0", got)
	}
	if got := scaler.Scale("unrelated-uuid", "", 4.0); got != 4.0 {
		t.Errorf("worker with no resolvable name throughput = %f, want 4.0", got)
	}
}

func TestThroughputScaler_UnboundedWindowKeepsApplying(t *testing.T) {
	scaler := NewThroughputScaler([]ThroughputScaleEntry{{Worker: "slow-node", Factor: 0.1}})

	for range 3 {
		if got := scaler.Scale("uuid", "slow-node", 5.0); got != 0.5 {
			t.Fatalf("throughput = %f, want 0.5 for every heartbeat while no window is set", got)
		}
	}
}

func TestThroughputScaler_WindowLapseRestoresRealThroughput(t *testing.T) {
	scaler := NewThroughputScaler([]ThroughputScaleEntry{
		{Worker: "slow-node", Factor: 0.1, Window: 20 * time.Millisecond},
	})

	if got := scaler.Scale("uuid", "slow-node", 5.0); got != 0.5 {
		t.Fatalf("throughput inside window = %f, want 0.5", got)
	}

	time.Sleep(30 * time.Millisecond)

	if got := scaler.Scale("uuid", "slow-node", 5.0); got != 5.0 {
		t.Errorf("throughput after window lapse = %f, want the reported 5.0", got)
	}
	if got := scaler.Scale("uuid", "slow-node", 5.0); got != 5.0 {
		t.Errorf("throughput after lapse is sticky: got %f, want 5.0", got)
	}
}

func TestThroughputScaler_WindowStartsAtFirstNonZeroSample(t *testing.T) {
	scaler := NewThroughputScaler([]ThroughputScaleEntry{
		{Worker: "slow-node", Factor: 0.1, Window: 20 * time.Millisecond},
	})

	// Boot heartbeats report 0 jobs/sec; they must not consume the window.
	if got := scaler.Scale("uuid", "slow-node", 0); got != 0 {
		t.Fatalf("boot heartbeat throughput = %f, want 0", got)
	}
	time.Sleep(30 * time.Millisecond)

	if got := scaler.Scale("uuid", "slow-node", 5.0); got != 0.5 {
		t.Fatalf("throughput = %f, want 0.5: the window starts at the first non-zero sample", got)
	}
}

// TestThroughputScaler_DrivesEvictionAndRecovery pins the two threshold
// crossings the injection exists for: while a rule applies, the scaled worker
// falls below median/SlowNodeThreshold and is evicted; once its window lapses
// and real throughput flows again, its EWMA climbs back above
// median/RecoveryThreshold and it is recovered. Both workers report the same
// real throughput — only the injection creates the divergence.
func TestThroughputScaler_DrivesEvictionAndRecovery(t *testing.T) {
	const fastID, slowID = "worker-fast", "worker-slow"
	const realJobsPerSec = 2.0

	scaler := NewThroughputScaler([]ThroughputScaleEntry{{
		Worker: slowID,
		Factor: 0.05,
		Window: 20 * time.Millisecond,
	}})
	stateTable := NewWorkerStateTable(30 * time.Second)

	// A worker boots with no throughput; the window must not start until the
	// first real sample arrives.
	heartbeat := func(workerID string) float64 {
		recorded := scaler.Scale(workerID, "", realJobsPerSec)
		stateTable.UpdateFromHeartbeat(protocol.WorkerHeartbeatPayload{
			WorkerID:      workerID,
			Status:        string(protocol.WorkerStatusBusy),
			JobsPerSec:    recorded,
			CompletedJobs: MinJobsForEviction,
			Timestamp:     time.Now(),
		})
		return recorded
	}
	bootHeartbeat := func(workerID string) {
		stateTable.UpdateFromHeartbeat(protocol.WorkerHeartbeatPayload{
			WorkerID:      workerID,
			Status:        string(protocol.WorkerStatusIdle),
			JobsPerSec:    scaler.Scale(workerID, "", 0),
			CompletedJobs: 0,
			Timestamp:     time.Now(),
		})
	}
	bootHeartbeat(fastID)
	bootHeartbeat(slowID)
	time.Sleep(30 * time.Millisecond) // longer than the window: boot must not start it

	if got := heartbeat(fastID); got != realJobsPerSec {
		t.Fatalf("fast worker throughput = %f, want %f (no rule targets it)", got, realJobsPerSec)
	}
	slowReported := heartbeat(slowID)
	if want := realJobsPerSec * 0.05; slowReported != want {
		t.Fatalf("injected throughput = %f, want %f", slowReported, want)
	}

	result := stateTable.DetectSlowWorkers()
	if len(result.NewlyEvicted) != 1 || result.NewlyEvicted[0] != slowID {
		t.Fatalf("newly evicted = %v, want [%s] (median %f)", result.NewlyEvicted, slowID, result.Median)
	}
	if slowReported >= result.Median/SlowNodeThreshold {
		t.Fatalf("injected throughput %f did not cross median/%g (median %f)",
			slowReported, SlowNodeThreshold, result.Median)
	}

	time.Sleep(30 * time.Millisecond)

	recovered := false
	for range 5 {
		if got := heartbeat(slowID); got != realJobsPerSec {
			t.Fatalf("throughput after window lapse = %f, want %f", got, realJobsPerSec)
		}
		result = stateTable.DetectSlowWorkers()
		if len(result.Recovered) == 1 && result.Recovered[0] == slowID {
			recovered = true
			break
		}
	}
	if !recovered {
		t.Fatalf("slow worker not recovered after the injection window lapsed (median %f)", result.Median)
	}
}
