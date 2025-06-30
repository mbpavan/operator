/*
Copyright 2020 The Tekton Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package tektonconfig

import (
	"context"
	"fmt"

	mf "github.com/manifestival/manifestival"
	"github.com/tektoncd/operator/pkg/apis/operator/v1alpha1"
	"github.com/tektoncd/operator/pkg/client/clientset/versioned"
	operatorclient "github.com/tektoncd/operator/pkg/client/injection/client"
	"github.com/tektoncd/operator/pkg/reconciler/common"
	"github.com/tektoncd/operator/pkg/reconciler/kubernetes/tektonconfig/extension"
)

func KubernetesExtension(ctx context.Context) common.Extension {
	return kubernetesExtension{
		operatorClientSet: operatorclient.Get(ctx),
	}
}

type kubernetesExtension struct {
	operatorClientSet versioned.Interface
}

func (oe kubernetesExtension) Transformers(comp v1alpha1.TektonComponent) []mf.Transformer {
	return []mf.Transformer{}
}
func (oe kubernetesExtension) PreReconcile(context.Context, v1alpha1.TektonComponent) error {
	return nil
}
func (oe kubernetesExtension) PostReconcile(ctx context.Context, comp v1alpha1.TektonComponent) error {
	configInstance := comp.(*v1alpha1.TektonConfig)

	if configInstance.Spec.Profile == v1alpha1.ProfileAll {
		if _, err := extension.EnsureTektonDashboardExists(ctx, oe.operatorClientSet.OperatorV1alpha1().TektonDashboards(), configInstance); err != nil {
			configInstance.Status.MarkPostInstallFailed(fmt.Sprintf("TektonDashboard: %s", err.Error()))
			return v1alpha1.REQUEUE_EVENT_AFTER
		}
	}

	if configInstance.Spec.Profile == v1alpha1.ProfileLite || configInstance.Spec.Profile == v1alpha1.ProfileBasic {
		return extension.EnsureTektonDashboardCRNotExists(ctx, oe.operatorClientSet.OperatorV1alpha1().TektonDashboards())
	}

	// ──────────────────────────────────────────────
	// Pipelines-as-Code: seed the CR on *all* platforms
	//cfg := comp.(*v1alpha1.TektonConfig)
	//pac := cfg.Spec.Platforms.OpenShift.PipelinesAsCode
	//if pac != nil && *pac.Enable {
	//	// Note: operatorVersion is the same string you pass into the OCP extension.
	//	// If you have it in scope use that; otherwise use cfg.Status.AppliedVersion
	//	// or hard‑code a version string.
	//	operatorVersion := cfg.Status.AppliedVersion
	//	if _, err := extension.EnsureOpenShiftPipelinesAsCodeExists(
	//		ctx,
	//		oe.operatorClientSet.OperatorV1alpha1().OpenShiftPipelinesAsCodes(),
	//		cfg,
	//		operatorVersion,
	//	); err != nil {
	//		cfg.Status.MarkComponentNotReady(fmt.Sprintf("PipelinesAsCode: %s", err))
	//		return v1alpha1.REQUEUE_EVENT_AFTER
	//	}
	//}

	return nil
}
func (oe kubernetesExtension) Finalize(ctx context.Context, comp v1alpha1.TektonComponent) error {
	configInstance := comp.(*v1alpha1.TektonConfig)
	if configInstance.Spec.Profile == v1alpha1.ProfileAll {
		return extension.EnsureTektonDashboardCRNotExists(ctx, oe.operatorClientSet.OperatorV1alpha1().TektonDashboards())
	}
	return nil
}
