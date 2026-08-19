package metrics

import (
	"reflect"
	"testing"
	"time"
)

// Task #67: GPU metric aggregates must only reflect the cards a job's
// container was actually granted, not every card the runner process
// happened to see while sampling nvidia-smi/rocm-smi/ioreg on the host. The
// collector itself has no per-container view, so the fix lives in
// AllocatedGPUAggregates, a pure filter over already-collected samples. That
// makes it directly testable with fabricated samples and a fake predicate,
// no daemon, no GPU, no exec.Command involved — the same injection pattern
// internal/gpu/policy_test.go uses for its DeviceProbe.

func sample(vendor GPUVendor, deviceID int, utilization float64) GPUSample {
	return GPUSample{
		Timestamp:      time.Unix(1754784000, 0),
		Vendor:         vendor,
		DeviceID:       deviceID,
		DeviceName:     "Test GPU",
		GPUUtilization: utilization,
		MemoryUsedMB:   1024,
		MemoryTotalMB:  8192,
	}
}

// allow builds a predicate that allows only the given (vendor, deviceID)
// pairs, mirroring how handler.go wraps gpu.Policy.Allows.
func allow(pairs ...struct {
	vendor GPUVendor
	id     int
}) func(GPUVendor, int) bool {
	set := map[GPUVendor]map[int]bool{}
	for _, pair := range pairs {
		if set[pair.vendor] == nil {
			set[pair.vendor] = map[int]bool{}
		}
		set[pair.vendor][pair.id] = true
	}
	return func(vendor GPUVendor, id int) bool {
		return set[vendor][id]
	}
}

func pair(vendor GPUVendor, id int) struct {
	vendor GPUVendor
	id     int
} {
	return struct {
		vendor GPUVendor
		id     int
	}{vendor, id}
}

func TestAllocatedGPUAggregatesKeepsOnlyAllocatedDevices(t *testing.T) {
	series := &GPUTimeSeries{
		Samples: []GPUSample{
			sample(GPUVendorNVIDIA, 0, 90),
			sample(GPUVendorNVIDIA, 1, 10), // not allocated to this job
			sample(GPUVendorAMD, 0, 50),    // different vendor entirely
		},
	}

	agg := AllocatedGPUAggregates(series, allow(pair(GPUVendorNVIDIA, 0)))
	if agg == nil {
		t.Fatal("expected an aggregate for the allocated device")
	}
	if len(agg.PerGPU) != 1 {
		t.Fatalf("expected exactly one device in PerGPU, got %d: %+v", len(agg.PerGPU), agg.PerGPU)
	}
	if _, ok := agg.PerGPU["nvidia:0"]; !ok {
		t.Fatalf("expected nvidia:0 in PerGPU, got keys %v", agg.PerGPU)
	}
	if agg.PerGPU["nvidia:0"].GPUUtilizationAvg != 90 {
		t.Errorf("the retained device's stats must come only from its own samples, got %+v", agg.PerGPU["nvidia:0"])
	}
	if !reflect.DeepEqual(agg.MeasuredDevices, []string{"nvidia:0"}) {
		t.Errorf("MeasuredDevices = %v, want [nvidia:0]", agg.MeasuredDevices)
	}
}

// AC: "une exécution sans GPU alloué ne produit pas d'agrégats GPU" — nil,
// not an empty PerGPU map, so a consumer cannot mistake "measured and idle"
// for "never measured".
func TestAllocatedGPUAggregatesReturnsNilWhenNothingIsAllocated(t *testing.T) {
	series := &GPUTimeSeries{
		Samples: []GPUSample{
			sample(GPUVendorNVIDIA, 0, 90),
			sample(GPUVendorNVIDIA, 1, 10),
		},
	}

	agg := AllocatedGPUAggregates(series, func(GPUVendor, int) bool { return false })
	if agg != nil {
		t.Fatalf("expected nil when no sample is allocated to the job, got %+v", agg)
	}
}

func TestAllocatedGPUAggregatesReturnsNilForNilInputs(t *testing.T) {
	if got := AllocatedGPUAggregates(nil, func(GPUVendor, int) bool { return true }); got != nil {
		t.Errorf("expected nil for a nil series, got %+v", got)
	}
	series := &GPUTimeSeries{Samples: []GPUSample{sample(GPUVendorNVIDIA, 0, 1)}}
	if got := AllocatedGPUAggregates(series, nil); got != nil {
		t.Errorf("expected nil for a nil predicate, got %+v", got)
	}
}

// AC: "deux jobs simultanés sur deux cartes distinctes produisent des
// mesures disjointes". Both jobs' collectors would, on a shared host, sample
// the very same raw series (every card visible to the runner process); it is
// the per-job allocation predicate that must carve out disjoint results.
func TestAllocatedGPUAggregatesProducesDisjointResultsForDistinctAllocations(t *testing.T) {
	hostWideSamples := []GPUSample{
		sample(GPUVendorNVIDIA, 0, 80),
		sample(GPUVendorNVIDIA, 1, 20),
	}
	series := &GPUTimeSeries{Samples: hostWideSamples}

	jobA := AllocatedGPUAggregates(series, allow(pair(GPUVendorNVIDIA, 0)))
	jobB := AllocatedGPUAggregates(series, allow(pair(GPUVendorNVIDIA, 1)))

	if jobA == nil || jobB == nil {
		t.Fatalf("expected both jobs to get an aggregate, got jobA=%+v jobB=%+v", jobA, jobB)
	}
	for key := range jobA.PerGPU {
		if _, clash := jobB.PerGPU[key]; clash {
			t.Fatalf("device %q appears in both jobs' aggregates; measurements are not disjoint", key)
		}
	}
	if !reflect.DeepEqual(jobA.MeasuredDevices, []string{"nvidia:0"}) {
		t.Errorf("job A MeasuredDevices = %v, want [nvidia:0]", jobA.MeasuredDevices)
	}
	if !reflect.DeepEqual(jobB.MeasuredDevices, []string{"nvidia:1"}) {
		t.Errorf("job B MeasuredDevices = %v, want [nvidia:1]", jobB.MeasuredDevices)
	}
}

func TestComputeGPUAggregatesPopulatesMeasuredDevicesSortedAndDeduplicated(t *testing.T) {
	series := &GPUTimeSeries{
		Samples: []GPUSample{
			sample(GPUVendorNVIDIA, 1, 10),
			sample(GPUVendorNVIDIA, 0, 90),
			sample(GPUVendorNVIDIA, 0, 95), // second sample for device 0
			sample(GPUVendorAMD, 0, 50),
		},
	}

	agg := ComputeGPUAggregates(series)
	want := []string{"amd:0", "nvidia:0", "nvidia:1"}
	if !reflect.DeepEqual(agg.MeasuredDevices, want) {
		t.Errorf("MeasuredDevices = %v, want %v", agg.MeasuredDevices, want)
	}
}

func TestComputeGPUAggregatesOnEmptySeriesHasEmptyNotNilMeasuredDevices(t *testing.T) {
	agg := ComputeGPUAggregates(&GPUTimeSeries{})
	if agg.MeasuredDevices == nil {
		t.Error("expected a non-nil empty slice, got nil (would serialize as JSON null instead of [])")
	}
	if len(agg.MeasuredDevices) != 0 {
		t.Errorf("expected no measured devices, got %v", agg.MeasuredDevices)
	}
}
