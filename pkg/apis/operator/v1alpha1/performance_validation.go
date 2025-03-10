package v1alpha1

import (
	"fmt"

	"knative.dev/pkg/apis"
)

func (prof *PipelinePerformanceProperties) Validate(path string) *apis.FieldError {
	var errs *apis.FieldError

	bucketsPath := fmt.Sprintf("%s.buckets", path)
	// Minimum and maximum allowed buckets value
	if prof.Buckets != nil {
		if *prof.Buckets < 1 || *prof.Buckets > 10 {
			errs = errs.Also(apis.ErrOutOfBoundsValue(*prof.Buckets, 1, 10, bucketsPath))
		}
	}

	// Check for StatefulsetOrdinals and Replicas
	if prof.StatefulsetOrdinals != nil && *prof.StatefulsetOrdinals {
		if prof.Replicas != nil {
			replicas := uint(*prof.Replicas)
			if *prof.Buckets != replicas {
				errs = errs.Also(apis.ErrInvalidValue(*prof.Replicas, fmt.Sprintf("%s.replicas", path),
					"spec.performance.replicas must equal spec.performance.buckets for statefulset ordinals"))
			}
		}
	}

	return errs
}
