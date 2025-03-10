package v1alpha1

import (
	"strings"
	"testing"
)

// Helper function to create a pointer to a uint.
func uintPtr(u uint) *uint {
	return &u
}

// Helper function to create a pointer to a bool.
func boolPtr(b bool) *bool {
	return &b
}

// Helper function to create a pointer to an int32.
func int32Ptr(i int32) *int32 {
	return &i
}

func TestPipelinePerformancePropertiesValidate(t *testing.T) {
	tests := []struct {
		name           string
		buckets        *uint
		statefulset    *bool
		replicas       *int32
		expectedError  bool
		errorSubstring string
	}{
		{
			name:          "valid: Buckets in range, no statefulset ordinals",
			buckets:       uintPtr(5),
			statefulset:   boolPtr(false),
			replicas:      int32Ptr(3), // replicas value doesn't matter when statefulset ordinals is false
			expectedError: false,
		},
		{
			name:          "valid: Buckets in range and statefulset enabled with matching replicas",
			buckets:       uintPtr(3),
			statefulset:   boolPtr(true),
			replicas:      int32Ptr(3),
			expectedError: false,
		},
		{
			name:           "invalid: Buckets below minimum",
			buckets:        uintPtr(0),
			statefulset:    boolPtr(false),
			replicas:       int32Ptr(1),
			expectedError:  true,
			errorSubstring: "buckets",
		},
		{
			name:           "invalid: Buckets above maximum",
			buckets:        uintPtr(11),
			statefulset:    boolPtr(false),
			replicas:       int32Ptr(1),
			expectedError:  true,
			errorSubstring: "buckets",
		},
		{
			name:           "invalid: Statefulset enabled but replicas mismatch",
			buckets:        uintPtr(4),
			statefulset:    boolPtr(true),
			replicas:       int32Ptr(3), // mismatch: expected replicas == buckets (4)
			expectedError:  true,
			errorSubstring: "replicas",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Construct the PerformanceProperties instance using the embedded types.
			pp := &PipelinePerformanceProperties{
				PipelinePerformanceLeaderElectionConfig: PipelinePerformanceLeaderElectionConfig{
					Buckets: tc.buckets,
				},
				PipelinePerformanceStatefulsetOrdinalsConfig: PipelinePerformanceStatefulsetOrdinalsConfig{
					StatefulsetOrdinals: tc.statefulset,
				},
				Replicas: tc.replicas,
			}

			path := "spec.performance"
			err := pp.Validate(path)

			if tc.expectedError {
				if err == nil {
					t.Errorf("expected an error but got nil")
				} else if tc.errorSubstring != "" && !strings.Contains(err.Error(), tc.errorSubstring) {
					t.Errorf("expected error to contain %q, got: %v", tc.errorSubstring, err)
				}
			} else {
				if err != nil {
					t.Errorf("expected no error, but got: %v", err)
				}
			}
		})
	}
}
