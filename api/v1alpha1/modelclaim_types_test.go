package v1alpha1

import (
	"encoding/json"
	"testing"
	"time"
)

// The controller (internal/modelclaim) writes status via unstructured maps —
// setCondition builds "metav1.Condition-shaped" entries by hand. This test
// pins the seam: a controller-shaped status document must decode losslessly
// into the typed ModelClaimStatus, and the constants here must match the
// strings the controller emits.
func TestControllerShapedStatusDecodes(t *testing.T) {
	now := time.Now().UTC().Format(time.RFC3339)
	doc := map[string]any{
		"observedGeneration": int64(3),
		"resolved": map[string]any{
			"memoryGi":        int64(24),
			"minBandwidthGBs": int64(640),
			"quant":           "Q4_K_M",
			"weightsGb":       "18.4",
			"resolverVersion": "0.9.38",
		},
		"templateRef": map[string]any{"name": "qwen"},
		"candidates":  map[string]any{"devices": int64(2), "nodes": int64(2), "available": int64(1)},
		"conditions": []any{
			map[string]any{
				"type":               ConditionResolved,
				"status":             "True",
				"reason":             ReasonResolved,
				"message":            "resolved",
				"observedGeneration": int64(3),
				"lastTransitionTime": now,
			},
			map[string]any{
				"type":               ConditionSatisfiable,
				"status":             "True",
				"reason":             ReasonDevicesAvailable,
				"message":            "2 device(s) on 2 node(s) satisfy the bounds (1 currently unallocated)",
				"observedGeneration": int64(3),
				"lastTransitionTime": now,
			},
		},
	}

	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var status ModelClaimStatus
	if err := json.Unmarshal(raw, &status); err != nil {
		t.Fatalf("controller-shaped status does not decode into typed status: %v", err)
	}

	if status.ObservedGeneration != 3 {
		t.Errorf("observedGeneration = %d, want 3", status.ObservedGeneration)
	}
	if status.Resolved == nil || status.Resolved.MinBandwidthGBs != 640 || status.Resolved.WeightsGb != "18.4" {
		t.Errorf("resolved physics did not round-trip: %+v", status.Resolved)
	}
	if status.Candidates == nil || status.Candidates.Available != 1 {
		t.Errorf("candidates did not round-trip: %+v", status.Candidates)
	}
	if len(status.Conditions) != 2 {
		t.Fatalf("conditions = %d, want 2", len(status.Conditions))
	}
	if status.Conditions[0].Type != ConditionResolved || status.Conditions[0].Reason != ReasonResolved {
		t.Errorf("Resolved condition mismatch: %+v", status.Conditions[0])
	}
	if status.Conditions[1].Type != ConditionSatisfiable || status.Conditions[1].Reason != ReasonDevicesAvailable {
		t.Errorf("Satisfiable condition mismatch: %+v", status.Conditions[1])
	}
	if status.Conditions[1].LastTransitionTime.IsZero() {
		t.Error("lastTransitionTime failed to parse as metav1.Time")
	}
}

// Spec defaults are applied by the API server (from the CRD schema), not by
// client-side decoding — pin that an empty spec decodes with nil/zero values
// so consumers don't assume defaulted fields are populated on read paths
// that bypass the server.
func TestSpecOptionalFieldsStayNilWithoutServerDefaulting(t *testing.T) {
	var spec ModelClaimSpec
	if err := json.Unmarshal([]byte(`{"model":"qwen3.6-30b-a3b"}`), &spec); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if spec.Model != "qwen3.6-30b-a3b" {
		t.Errorf("model = %q", spec.Model)
	}
	if spec.MinTps != nil || spec.EfficiencyPct != nil || spec.DeviceClassName != "" {
		t.Errorf("optional fields unexpectedly populated client-side: %+v", spec)
	}
}
